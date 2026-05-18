package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type mockWorkflowJobsClient struct {
	getItem func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

func (m *mockWorkflowJobsClient) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return m.getItem(ctx, params, optFns...)
}

func marshalWorkflowJobItem(t *testing.T, record workflowJobFactsRecord) map[string]dynamodbtypes.AttributeValue {
	t.Helper()

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		t.Fatalf("failed to marshal workflow job item: %v", err)
	}
	return item
}

func TestFindWorkflowJobFactsReturnsRunAndInstance(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobFactsRecord{
					RunID:           1234,
					Status:          "queued",
					SchedulingState: "active",
					ActiveAttempt: &struct {
						InstanceID string `dynamodbav:"instance_id"`
					}{
						InstanceID: "i-123",
					},
				}),
			}, nil
		},
	}

	facts, err := findWorkflowJobFacts(context.Background(), client, "runs-on-workflow-jobs", "42")
	if err != nil {
		t.Fatalf("findWorkflowJobFacts returned error: %v", err)
	}
	if facts.RunID != 1234 {
		t.Fatalf("expected run ID 1234, got %d", facts.RunID)
	}
	if facts.CurrentInstanceID != "i-123" {
		t.Fatalf("expected instance ID i-123, got %q", facts.CurrentInstanceID)
	}
}

func TestFindWorkflowJobFactsFallsBackToRunnerNameInstanceID(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobFactsRecord{
					RunID:      1234,
					RunnerName: "org--i-123--job",
					Status:     "completed",
				}),
			}, nil
		},
	}

	facts, err := findWorkflowJobFacts(context.Background(), client, "runs-on-workflow-jobs", "42")
	if err != nil {
		t.Fatalf("findWorkflowJobFacts returned error: %v", err)
	}
	if facts.CurrentInstanceID != "i-123" {
		t.Fatalf("expected runner-name instance ID i-123, got %q", facts.CurrentInstanceID)
	}
}

func TestFindWorkflowJobFactsFallsBackToAttemptHistoryInstanceID(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobFactsRecord{
					RunID:  1234,
					Status: "completed",
					AttemptHistory: []struct {
						InstanceID string `dynamodbav:"instance_id"`
					}{
						{InstanceID: "i-old"},
						{InstanceID: "i-123"},
					},
				}),
			}, nil
		},
	}

	facts, err := findWorkflowJobFacts(context.Background(), client, "runs-on-workflow-jobs", "42")
	if err != nil {
		t.Fatalf("findWorkflowJobFacts returned error: %v", err)
	}
	if facts.CurrentInstanceID != "i-123" {
		t.Fatalf("expected attempt-history instance ID i-123, got %q", facts.CurrentInstanceID)
	}
}

func TestFindWorkflowJobFactsDerivesAttemptedInstancesAndCreatedAt(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 30, 0, 0, time.UTC)
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobFactsRecord{
					JobID:         42,
					RunID:         1234,
					CreatedAt:     &createdAt,
					CreatedAtUnix: createdAt.Add(-time.Hour).Unix(),
					RunnerName:    "runs-on--i-runner--job",
					ActiveAttempt: &struct {
						InstanceID string `dynamodbav:"instance_id"`
					}{InstanceID: "i-active"},
					AttemptHistory: []struct {
						InstanceID string `dynamodbav:"instance_id"`
					}{
						{InstanceID: "i-old"},
						{InstanceID: "i-active"},
					},
				}),
			}, nil
		},
	}

	facts, err := findWorkflowJobFacts(context.Background(), client, "runs-on-workflow-jobs", "https://github.com/runs-on/server/actions/runs/100/job/42?pr=1")
	if err != nil {
		t.Fatalf("findWorkflowJobFacts returned error: %v", err)
	}
	if got := strings.Join(facts.AttemptedInstanceIDs, ","); got != "i-active,i-old,i-runner" {
		t.Fatalf("expected attempted instance IDs to be deduplicated, got %q", got)
	}
	if !facts.CreatedAt.Equal(createdAt) {
		t.Fatalf("expected created_at %s, got %s", createdAt, facts.CreatedAt)
	}
	if facts.CreatedAtSource != "created_at" {
		t.Fatalf("expected created_at source, got %q", facts.CreatedAtSource)
	}
	if _, err := facts.rawDynamoDBItemJSON(); err != nil {
		t.Fatalf("rawDynamoDBItemJSON returned error: %v", err)
	}
}

func TestWorkflowJobFactsCreatedAtFallsBackToUnix(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 30, 0, 0, time.UTC)
	facts := workflowJobFactsFromRecord(workflowJobFactsRecord{
		JobID:         43,
		CreatedAtUnix: createdAt.Unix(),
	}, nil)

	got, err := facts.createdAtOrError()
	if err != nil {
		t.Fatalf("createdAtOrError returned error: %v", err)
	}
	if !got.Equal(createdAt) {
		t.Fatalf("expected created_at_unix %s, got %s", createdAt, got)
	}
	if facts.CreatedAtSource != "created_at_unix" {
		t.Fatalf("expected created_at_unix source, got %q", facts.CreatedAtSource)
	}
}

func TestFindWorkflowJobFactsMissingRow(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{}, nil
		},
	}

	facts, err := findWorkflowJobFacts(context.Background(), client, "runs-on-workflow-jobs", "42")
	if err != nil {
		t.Fatalf("findWorkflowJobFacts returned error: %v", err)
	}
	if facts != nil {
		t.Fatalf("expected missing row to return nil facts, got %+v", facts)
	}
}

func TestWaitForWorkflowJobFactsWaitsForInstanceID(t *testing.T) {
	var calls int
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			calls++
			if calls == 1 {
				return &dynamodb.GetItemOutput{
					Item: marshalWorkflowJobItem(t, workflowJobFactsRecord{
						RunID:           1234,
						Status:          "queued",
						SchedulingState: "launching",
					}),
				}, nil
			}
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobFactsRecord{
					RunID:           1234,
					Status:          "queued",
					SchedulingState: "active",
					ActiveAttempt: &struct {
						InstanceID string `dynamodbav:"instance_id"`
					}{
						InstanceID: "i-123",
					},
				}),
			}, nil
		},
	}

	facts, err := waitForWorkflowJobFactsWithInterval(context.Background(), client, "runs-on-workflow-jobs", "42", true, nil, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("waitForWorkflowJobFactsWithInterval returned error: %v", err)
	}
	if facts.CurrentInstanceID != "i-123" {
		t.Fatalf("expected instance ID i-123, got %q", facts.CurrentInstanceID)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 GetItem calls, got %d", calls)
	}
}

func TestWaitForWorkflowJobFactsNoWatchReturnsPendingState(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobFactsRecord{
					RunID:           1234,
					Status:          "queued",
					SchedulingState: "launching",
				}),
			}, nil
		},
	}

	_, err := waitForWorkflowJobFactsWithInterval(context.Background(), client, "runs-on-workflow-jobs", "42", false, nil, time.Millisecond)
	if err == nil {
		t.Fatal("expected waitForWorkflowJobFactsWithInterval to return an error")
	}
	if !strings.Contains(err.Error(), "status=queued") || !strings.Contains(err.Error(), "scheduling_state=launching") {
		t.Fatalf("expected status and scheduling state in error, got %v", err)
	}
}

func TestWaitForWorkflowJobFactsHonorsContext(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{}, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := waitForWorkflowJobFactsWithInterval(ctx, client, "runs-on-workflow-jobs", "42", true, nil, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected context deadline error")
	}
	if ctx.Err() == nil || !strings.Contains(err.Error(), ctx.Err().Error()) {
		t.Fatalf("expected context error %v, got %v", ctx.Err(), err)
	}
}
