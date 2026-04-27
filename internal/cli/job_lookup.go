package cli

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type workflowJobsAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

type workflowJobLookup struct {
	RunID           int64
	Status          string
	SchedulingState string
	InstanceID      string
}

type workflowJobLookupRecord struct {
	RunID           int64  `dynamodbav:"run_id"`
	RunnerName      string `dynamodbav:"runner_name"`
	Status          string `dynamodbav:"status"`
	SchedulingState string `dynamodbav:"scheduling_state"`
	ActiveAttempt   *struct {
		InstanceID string `dynamodbav:"instance_id"`
	} `dynamodbav:"active_attempt"`
	AttemptHistory []struct {
		InstanceID string `dynamodbav:"instance_id"`
	} `dynamodbav:"attempt_history"`
}

func findJobLookup(ctx context.Context, jobsClient workflowJobsAPI, tableName, jobID string) (*workflowJobLookup, error) {
	if jobsClient == nil {
		return nil, fmt.Errorf("workflow jobs client is required")
	}
	if tableName == "" {
		return nil, fmt.Errorf("workflow jobs table is not configured")
	}

	parsedJobID, err := strconv.ParseInt(strings.TrimSpace(jobID), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid job ID %q: %w", jobID, err)
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

	var record workflowJobLookupRecord
	if err := attributevalue.UnmarshalMap(output.Item, &record); err != nil {
		return nil, fmt.Errorf("failed to unmarshal workflow job record: %w", err)
	}

	lookup := &workflowJobLookup{
		RunID:           record.RunID,
		Status:          record.Status,
		SchedulingState: record.SchedulingState,
	}
	lookup.InstanceID = workflowJobLookupInstanceID(record)

	return lookup, nil
}

func workflowJobLookupInstanceID(record workflowJobLookupRecord) string {
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

func waitForJobLookup(ctx context.Context, jobsClient workflowJobsAPI, tableName, jobID string, watch bool, logger *log.Logger) (*workflowJobLookup, error) {
	return waitForJobLookupWithInterval(ctx, jobsClient, tableName, jobID, watch, logger, 5*time.Second)
}

func waitForJobLookupWithInterval(ctx context.Context, jobsClient workflowJobsAPI, tableName, jobID string, watch bool, logger *log.Logger, interval time.Duration) (*workflowJobLookup, error) {
	for {
		lookup, err := findJobLookup(ctx, jobsClient, tableName, jobID)
		if err != nil {
			return nil, err
		}
		if lookup != nil && lookup.InstanceID != "" {
			return lookup, nil
		}
		if !watch {
			return nil, lookupError(lookup, jobID)
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

func lookupError(lookup *workflowJobLookup, jobID string) error {
	switch {
	case lookup == nil:
		return fmt.Errorf("job %s not found in workflow jobs table", jobID)
	case lookup.InstanceID != "":
		return nil
	default:
		parts := []string{fmt.Sprintf("instance ID for job %s not available yet", jobID)}
		if lookup.Status != "" {
			parts = append(parts, fmt.Sprintf("status=%s", lookup.Status))
		}
		if lookup.SchedulingState != "" {
			parts = append(parts, fmt.Sprintf("scheduling_state=%s", lookup.SchedulingState))
		}
		return fmt.Errorf("%s", strings.Join(parts, " "))
	}
}
