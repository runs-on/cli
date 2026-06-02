package cli

import (
	"context"
	"fmt"
	"log"
	"net/url"
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
	CompletedAt          time.Time
	CompletedAtSource    string
	rawItem              map[string]dynamodbtypes.AttributeValue
	rawJSON              []byte
}

type workflowJobFactsRecord struct {
	JobID           int64      `dynamodbav:"job_id"`
	RunID           int64      `dynamodbav:"run_id"`
	RunnerName      string     `dynamodbav:"runner_name"`
	Status          string     `dynamodbav:"status"`
	SchedulingState string     `dynamodbav:"scheduling_state"`
	CreatedAt       *time.Time `dynamodbav:"created_at"`
	CreatedAtUnix   int64      `dynamodbav:"created_at_unix"`
	CompletedAt     *time.Time `dynamodbav:"completed_at"`
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

type jobFactsProvider struct {
	product     string
	stackName   string
	resolver    *jobDiagnosticsResolver
	github      localGitHubWorkflowJobFetcher
	jobID       string
	jobRef      string
	logger      *log.Logger
	diagnostics *jobDiagnosticsResponse

	mu    sync.RWMutex
	facts *workflowJobFacts
}

func newJobFactsProvider(config *RunsOnConfig, jobRef string, logger *log.Logger) *jobFactsProvider {
	return &jobFactsProvider{
		product:   strings.TrimSpace(config.Product),
		stackName: strings.TrimSpace(config.StackName),
		resolver:  newJobDiagnosticsResolver(config),
		github:    ghCLIWorkflowJobFetcher{},
		jobID:     extractJobID(strings.TrimSpace(jobRef)),
		jobRef:    strings.TrimSpace(jobRef),
		logger:    logger,
	}
}

func (p *jobFactsProvider) refresh(ctx context.Context) error {
	facts, err := p.find(ctx)
	if err != nil {
		if p.logger != nil {
			p.logger.Printf("Error discovering job facts: %v", err)
		}
		p.set(nil)
		return err
	}
	p.set(facts)
	return nil
}

func (p *jobFactsProvider) find(ctx context.Context) (*workflowJobFacts, error) {
	response, err := p.resolver.Resolve(ctx, p.jobRef)
	if err != nil {
		return nil, err
	}
	if response != nil && p.logger != nil {
		response.logDebug(p.logger)
	}
	if p.shouldUseLocalGitHubFallback(response) {
		p.enrichWithLocalGitHub(ctx, response)
	}
	p.setDiagnostics(response)
	if err := response.validateStackProduct(p.product, p.stackName, parsedJobIDOrZero(p.jobID)); err != nil {
		return nil, err
	}
	return response.workflowFacts(parsedJobIDOrZero(p.jobID)), nil
}

func (p *jobFactsProvider) shouldUseLocalGitHubFallback(response *jobDiagnosticsResponse) bool {
	if response == nil || p.github == nil || !strings.EqualFold(response.Product, "fleet") {
		return false
	}
	if response.GitHub.WorkflowJob != nil {
		return false
	}
	if response.Request.WorkflowJobID == 0 || response.Request.Owner == "" || response.Request.Repo == "" {
		return false
	}
	return response.Local == nil || response.Status == "ambiguous"
}

func (p *jobFactsProvider) enrichWithLocalGitHub(ctx context.Context, response *jobDiagnosticsResponse) {
	response.Diagnostics = append(response.Diagnostics, jobDiagnosticsDiagnostic{
		Level:   "info",
		Code:    "local_gh_workflow_job_fetch_attempt",
		Message: "resolver could not resolve the Fleet workflow job; trying local gh CLI",
	})
	if p.logger != nil {
		p.logger.Printf("Job diagnostics info local_gh_workflow_job_fetch_attempt: resolver could not resolve the Fleet workflow job; trying local gh CLI")
	}
	workflowJob, err := p.github.FetchWorkflowJob(ctx, response.Request)
	if err != nil {
		response.Diagnostics = append(response.Diagnostics, jobDiagnosticsDiagnostic{
			Level:   "warn",
			Code:    "local_gh_workflow_job_fetch_failed",
			Message: err.Error(),
		})
		if p.logger != nil {
			p.logger.Printf("Job diagnostics warn local_gh_workflow_job_fetch_failed: %s", err.Error())
		}
		return
	}
	response.GitHub.WorkflowJob = workflowJob
	response.Status = "partial"
	response.Diagnostics = append(response.Diagnostics, jobDiagnosticsDiagnostic{
		Level:   "info",
		Code:    "local_gh_workflow_job_fetched",
		Message: "workflow job details fetched with local gh CLI fallback",
	})
	if p.logger != nil {
		p.logger.Printf("Job diagnostics info local_gh_workflow_job_fetched: workflow job details fetched with local gh CLI fallback")
	}
}

func (p *jobFactsProvider) productType() string {
	if strings.EqualFold(strings.TrimSpace(p.product), "fleet") {
		return "fleet"
	}
	return "flex"
}

func (p *jobFactsProvider) lookupTarget() string {
	return fmt.Sprintf("job diagnostics resolver Lambda %s", displayValue(p.resolverName()))
}

func (p *jobFactsProvider) resolverName() string {
	if p == nil || p.resolver == nil {
		return ""
	}
	return p.resolver.functionName
}

func displayValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "(not configured)"
	}
	return value
}

func parsedJobIDOrZero(jobID string) int64 {
	parsed, _ := strconv.ParseInt(strings.TrimSpace(jobID), 10, 64)
	return parsed
}

func (p *jobFactsProvider) logLookupSnapshot() {
	if p.logger == nil {
		return
	}
	p.logger.Printf("Stack product detected: %s", p.productType())
	p.logger.Printf("Job lookup target: %s", p.lookupTarget())
	p.logger.Printf("%s", p.lookupStatusLine())
}

func (p *jobFactsProvider) lookupStatusLine() string {
	jobID := extractJobID(strings.TrimSpace(p.jobID))
	facts := p.current()
	if facts == nil {
		return fmt.Sprintf("Job %s not found in %s", jobID, p.lookupTarget())
	}

	instanceIDs := jobFactsInstanceIDs(facts)
	currentInstanceID := displayValue(facts.CurrentInstanceID)
	attemptedInstanceIDs := "(none)"
	if len(instanceIDs) > 0 {
		attemptedInstanceIDs = strings.Join(instanceIDs, ",")
	}

	if p.productType() == "fleet" {
		return fmt.Sprintf(
			"Job %s found in %s: workflow_run_id=%d state=%s current_instance_id=%s attempted_instance_ids=%s",
			jobID,
			p.lookupTarget(),
			facts.RunID,
			displayValue(facts.Status),
			currentInstanceID,
			attemptedInstanceIDs,
		)
	}

	return fmt.Sprintf(
		"Job %s found in %s: run_id=%d status=%s scheduling_state=%s current_instance_id=%s attempted_instance_ids=%s",
		jobID,
		p.lookupTarget(),
		facts.RunID,
		displayValue(facts.Status),
		displayValue(facts.SchedulingState),
		currentInstanceID,
		attemptedInstanceIDs,
	)
}

func (p *jobFactsProvider) instanceUnavailableError() error {
	return jobFactsInstanceError(p.current(), extractJobID(strings.TrimSpace(p.jobID)), p.lookupTarget())
}

func lookupWorkflowJobFacts(ctx context.Context, config *RunsOnConfig, jobRef string, watch bool, logger *log.Logger) (*workflowJobFacts, error) {
	if config == nil {
		return nil, fmt.Errorf("runs-on config is required")
	}
	if strings.EqualFold(config.Product, "fleet") || strings.TrimSpace(config.JobDiagnosticsResolver) != "" {
		return waitForJobFactsProviderWithInterval(ctx, newJobFactsProvider(config, jobRef, logger), watch, logger, 5*time.Second)
	}
	jobsClient := dynamodb.NewFromConfig(config.AWSConfig)
	return waitForWorkflowJobFacts(ctx, jobsClient, config.WorkflowJobsTable, jobRef, watch, logger)
}

func waitForJobFactsProviderWithInterval(ctx context.Context, factsProvider *jobFactsProvider, watch bool, logger *log.Logger, interval time.Duration) (*workflowJobFacts, error) {
	if factsProvider == nil {
		return nil, fmt.Errorf("workflow job facts provider is required")
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	jobID := extractJobID(strings.TrimSpace(factsProvider.jobID))
	for {
		if err := factsProvider.refresh(ctx); err != nil {
			return nil, err
		}
		facts := factsProvider.current()
		if facts != nil && facts.CurrentInstanceID != "" {
			return facts, nil
		}
		if !watch {
			return nil, factsProvider.instanceUnavailableError()
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

func (p *jobFactsProvider) startRefreshWithInterval(ctx context.Context, interval time.Duration) {
	if p == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.refresh(ctx); err != nil && p.logger != nil {
					p.logger.Printf("Error refreshing job facts: %v", err)
				}
			}
		}
	}()
}

func (p *jobFactsProvider) set(facts *workflowJobFacts) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.facts = facts
}

func (p *jobFactsProvider) setDiagnostics(diagnostics *jobDiagnosticsResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.diagnostics = diagnostics
}

func (p *jobFactsProvider) current() *workflowJobFacts {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.facts
}

func (p *jobFactsProvider) currentDiagnostics() *jobDiagnosticsResponse {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.diagnostics
}

func (p *jobFactsProvider) currentInstanceID() string {
	facts := p.current()
	if facts == nil {
		return ""
	}
	return facts.CurrentInstanceID
}

func (p *jobFactsProvider) currentInstanceIDs() []string {
	facts := p.current()
	if facts == nil {
		return nil
	}
	return jobFactsInstanceIDs(facts)
}

func (p *jobFactsProvider) runID() int64 {
	facts := p.current()
	if facts != nil && facts.RunID != 0 {
		return facts.RunID
	}
	if diagnostics := p.currentDiagnostics(); diagnostics != nil {
		if diagnostics.Request.WorkflowRunID != 0 {
			return diagnostics.Request.WorkflowRunID
		}
		if diagnostics.GitHub.WorkflowJob != nil && diagnostics.GitHub.WorkflowJob.RunID != 0 {
			return diagnostics.GitHub.WorkflowJob.RunID
		}
		if diagnostics.GitHub.WorkflowRun != nil {
			return diagnostics.GitHub.WorkflowRun.ID
		}
	}
	return 0
}

func jobFactsInstanceIDs(facts *workflowJobFacts) []string {
	if facts == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(facts.AttemptedInstanceIDs)+1)
	ids := make([]string, 0, len(facts.AttemptedInstanceIDs)+1)
	add := func(instanceID string) {
		instanceID = strings.TrimSpace(instanceID)
		if instanceID == "" {
			return
		}
		if _, ok := seen[instanceID]; ok {
			return
		}
		seen[instanceID] = struct{}{}
		ids = append(ids, instanceID)
	}
	for _, instanceID := range facts.AttemptedInstanceIDs {
		add(instanceID)
	}
	add(facts.CurrentInstanceID)
	return ids
}

func workflowJobFactsFromRecord(record workflowJobFactsRecord, rawItem map[string]dynamodbtypes.AttributeValue) *workflowJobFacts {
	createdAt, createdAtSource := workflowJobCreatedAtFromRecord(record)
	var completedAt time.Time
	completedAtSource := ""
	if record.CompletedAt != nil && !record.CompletedAt.IsZero() {
		completedAt = record.CompletedAt.UTC()
		completedAtSource = "completed_at"
	}
	return &workflowJobFacts{
		JobID:                record.JobID,
		RunID:                record.RunID,
		Status:               record.Status,
		SchedulingState:      record.SchedulingState,
		CurrentInstanceID:    workflowJobCurrentInstanceID(record),
		AttemptedInstanceIDs: workflowJobAttemptedInstanceIDs(record),
		CreatedAt:            createdAt,
		CreatedAtSource:      createdAtSource,
		CompletedAt:          completedAt,
		CompletedAtSource:    completedAtSource,
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
	if len(f.rawJSON) > 0 {
		return append([]byte(nil), f.rawJSON...), nil
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
			return nil, jobFactsInstanceError(facts, jobID, fmt.Sprintf("workflow jobs table %s", displayValue(tableName)))
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

func jobFactsInstanceError(facts *workflowJobFacts, jobID string, lookupTarget string) error {
	switch {
	case facts == nil:
		return fmt.Errorf("job %s not found in %s", jobID, lookupTarget)
	case facts.CurrentInstanceID != "":
		return nil
	default:
		parts := []string{fmt.Sprintf("instance ID for job %s not available yet", jobID)}
		if lookupTarget != "" {
			parts = append(parts, fmt.Sprintf("job_found_in=%q", lookupTarget))
		}
		if facts.Status != "" {
			parts = append(parts, fmt.Sprintf("status=%s", facts.Status))
		}
		if facts.SchedulingState != "" {
			parts = append(parts, fmt.Sprintf("scheduling_state=%s", facts.SchedulingState))
		}
		return fmt.Errorf("%s", strings.Join(parts, " "))
	}
}
