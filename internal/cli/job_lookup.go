package cli

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type workflowJobsAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

type workflowJobFacts struct {
	JobID                int64
	RunID                int64
	Status               string
	SchedulingState      string
	CurrentInstanceID    string
	AttemptedInstanceIDs []string
	CreatedAt            time.Time
	CreatedAtSource      string
	rawItem              map[string]dynamodbtypes.AttributeValue
}

type workflowJobFactsRecord struct {
	JobID           int64      `dynamodbav:"job_id"`
	RunID           int64      `dynamodbav:"run_id"`
	RunnerName      string     `dynamodbav:"runner_name"`
	Status          string     `dynamodbav:"status"`
	SchedulingState string     `dynamodbav:"scheduling_state"`
	CreatedAt       *time.Time `dynamodbav:"created_at"`
	CreatedAtUnix   int64      `dynamodbav:"created_at_unix"`
	ActiveAttempt   *struct {
		InstanceID string `dynamodbav:"instance_id"`
	} `dynamodbav:"active_attempt"`
	AttemptHistory []struct {
		InstanceID string `dynamodbav:"instance_id"`
	} `dynamodbav:"attempt_history"`
}

func extractJobID(input string) string {
	url, err := url.Parse(input)
	if err == nil && url.Scheme == "https" {
		// Extract job ID from URLs like:
		//
		// - https://github.com/runs-on/runs-on/actions/runs/12312372848/job/34368864490
		// - https://github.com/runs-on/runs-on/actions/runs/12312372848/job/34368864490?pr=123
		parts := strings.Split(url.Path, "/")
		if len(parts) > 1 {
			return parts[len(parts)-1]
		}
	}
	return input
}

func findWorkflowJobFacts(ctx context.Context, jobsClient workflowJobsAPI, tableName, jobRef string) (*workflowJobFacts, error) {
	if jobsClient == nil {
		return nil, fmt.Errorf("workflow jobs client is required")
	}
	if tableName == "" {
		return nil, fmt.Errorf("workflow jobs table is not configured")
	}

	jobID := extractJobID(strings.TrimSpace(jobRef))
	parsedJobID, err := strconv.ParseInt(jobID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid job ID %q: %w", jobRef, err)
	}

	output, err := jobsClient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]dynamodbtypes.AttributeValue{
			"job_id": &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(parsedJobID, 10)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow job record: %w", err)
	}
	if output.Item == nil {
		return nil, nil
	}

	var record workflowJobFactsRecord
	if err := attributevalue.UnmarshalMap(output.Item, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal workflow job record: %w", err)
	}
	if record.JobID == 0 {
		record.JobID = parsedJobID
	}

	return workflowJobFactsFromRecord(record, output.Item), nil
}

type workflowJobFactsProvider struct {
	jobs      workflowJobsAPI
	tableName string
	jobID     string
	logger    *log.Logger

	mu    sync.RWMutex
	facts *workflowJobFacts
}

func newWorkflowJobFactsProvider(config *RunsOnConfig, jobID string, logger *log.Logger) *workflowJobFactsProvider {
	return &workflowJobFactsProvider{
		jobs:      dynamodb.NewFromConfig(config.AWSConfig),
		tableName: config.WorkflowJobsTable,
		jobID:     jobID,
		logger:    logger,
	}
}

func (p *workflowJobFactsProvider) refresh(ctx context.Context) {
	facts, err := findWorkflowJobFacts(ctx, p.jobs, p.tableName, p.jobID)
	if err != nil {
		if p.logger != nil {
			p.logger.Printf("Error discovering workflow job facts: %v", err)
		}
		p.set(nil)
		return
	}
	p.set(facts)
}

func (p *workflowJobFactsProvider) startRefresh(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.refresh(ctx)
				if instanceID := p.currentInstanceID(); instanceID != "" && p.logger != nil {
					p.logger.Printf("Instance ID for job %s: %s", p.jobID, instanceID)
				}
			}
		}
	}()
}

func (p *workflowJobFactsProvider) set(facts *workflowJobFacts) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.facts = facts
}

func (p *workflowJobFactsProvider) current() *workflowJobFacts {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.facts
}

func (p *workflowJobFactsProvider) currentInstanceID() string {
	facts := p.current()
	if facts == nil {
		return ""
	}
	return facts.CurrentInstanceID
}

func (p *workflowJobFactsProvider) runID() int64 {
	facts := p.current()
	if facts == nil {
		return 0
	}
	return facts.RunID
}

func workflowJobFactsFromRecord(record workflowJobFactsRecord, rawItem map[string]dynamodbtypes.AttributeValue) *workflowJobFacts {
	createdAt, createdAtSource := workflowJobCreatedAtFromRecord(record)
	return &workflowJobFacts{
		JobID:                record.JobID,
		RunID:                record.RunID,
		Status:               record.Status,
		SchedulingState:      record.SchedulingState,
		CurrentInstanceID:    workflowJobCurrentInstanceID(record),
		AttemptedInstanceIDs: workflowJobAttemptedInstanceIDs(record),
		CreatedAt:            createdAt,
		CreatedAtSource:      createdAtSource,
		rawItem:              rawItem,
	}
}

func workflowJobCreatedAtFromRecord(record workflowJobFactsRecord) (time.Time, string) {
	if record.CreatedAt != nil && !record.CreatedAt.IsZero() {
		return record.CreatedAt.UTC(), "created_at"
	}
	if record.CreatedAtUnix > 0 {
		return time.Unix(record.CreatedAtUnix, 0).UTC(), "created_at_unix"
	}
	return time.Time{}, ""
}

func (f *workflowJobFacts) createdAtOrError() (time.Time, error) {
	if f == nil {
		return time.Time{}, fmt.Errorf("workflow job facts are required")
	}
	if !f.CreatedAt.IsZero() {
		return f.CreatedAt, nil
	}
	return time.Time{}, fmt.Errorf("workflow job %d has no usable created_at or created_at_unix timestamp", f.JobID)
}

func (f *workflowJobFacts) rawDynamoDBItemJSON() ([]byte, error) {
	if f == nil {
		return nil, fmt.Errorf("workflow job facts are required")
	}
	data, err := attributevalue.MarshalMapJSON(f.rawItem)
	if err != nil {
		return nil, fmt.Errorf("marshal DynamoDB job record: %w", err)
	}
	return data, nil
}

func workflowJobCurrentInstanceID(record workflowJobFactsRecord) string {
	if record.ActiveAttempt != nil && strings.TrimSpace(record.ActiveAttempt.InstanceID) != "" {
		return strings.TrimSpace(record.ActiveAttempt.InstanceID)
	}
	if instanceID := parseRunnerNameInstanceID(record.RunnerName); instanceID != "" {
		return instanceID
	}
	for i := len(record.AttemptHistory) - 1; i >= 0; i-- {
		if instanceID := strings.TrimSpace(record.AttemptHistory[i].InstanceID); instanceID != "" {
			return instanceID
		}
	}
	return ""
}

func workflowJobAttemptedInstanceIDs(record workflowJobFactsRecord) []string {
	seen := make(map[string]struct{})
	var ids []string
	add := func(instanceID string) {
		instanceID = strings.TrimSpace(instanceID)
		if instanceID == "" {
			return
		}
		if _, exists := seen[instanceID]; exists {
			return
		}
		seen[instanceID] = struct{}{}
		ids = append(ids, instanceID)
	}

	if record.ActiveAttempt != nil {
		add(record.ActiveAttempt.InstanceID)
	}
	for _, attempt := range record.AttemptHistory {
		add(attempt.InstanceID)
	}
	add(parseRunnerNameInstanceID(record.RunnerName))

	sort.Strings(ids)
	return ids
}

func parseRunnerNameInstanceID(runnerName string) string {
	parts := strings.Split(strings.TrimSpace(runnerName), "--")
	if len(parts) < 2 {
		return ""
	}
	instanceID := strings.TrimSpace(parts[1])
	if strings.HasPrefix(instanceID, "i-") {
		return instanceID
	}
	return ""
}

func waitForWorkflowJobFacts(ctx context.Context, jobsClient workflowJobsAPI, tableName, jobRef string, watch bool, logger *log.Logger) (*workflowJobFacts, error) {
	return waitForWorkflowJobFactsWithInterval(ctx, jobsClient, tableName, jobRef, watch, logger, 5*time.Second)
}

func waitForWorkflowJobFactsWithInterval(ctx context.Context, jobsClient workflowJobsAPI, tableName, jobRef string, watch bool, logger *log.Logger, interval time.Duration) (*workflowJobFacts, error) {
	jobID := extractJobID(strings.TrimSpace(jobRef))
	for {
		facts, err := findWorkflowJobFacts(ctx, jobsClient, tableName, jobID)
		if err != nil {
			return nil, err
		}
		if facts != nil && facts.CurrentInstanceID != "" {
			return facts, nil
		}
		if !watch {
			return nil, workflowJobFactsInstanceError(facts, jobID)
		}
		if logger != nil {
			logger.Printf("Waiting for instance ID for job %s...\n", jobID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func workflowJobFactsInstanceError(facts *workflowJobFacts, jobID string) error {
	switch {
	case facts == nil:
		return fmt.Errorf("job %s not found in workflow jobs table", jobID)
	case facts.CurrentInstanceID != "":
		return nil
	default:
		parts := []string{fmt.Sprintf("instance ID for job %s not available yet", jobID)}
		if facts.Status != "" {
			parts = append(parts, fmt.Sprintf("status=%s", facts.Status))
		}
		if facts.SchedulingState != "" {
			parts = append(parts, fmt.Sprintf("scheduling_state=%s", facts.SchedulingState))
		}
		return fmt.Errorf("%s", strings.Join(parts, " "))
	}
}
