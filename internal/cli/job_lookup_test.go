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

func marshalWorkflowJobItem(t *testing.T, record workflowJobLookupRecord) map[string]dynamodbtypes.AttributeValue {
	t.Helper()

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		t.Fatalf("failed to marshal workflow job item: %v", err)
	}
	return item
}

func TestFindJobLookupReturnsRunAndInstance(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobLookupRecord{
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

	lookup, err := findJobLookup(context.Background(), client, "runs-on-workflow-jobs", "42")
	if err != nil {
		t.Fatalf("findJobLookup returned error: %v", err)
	}
	if lookup.RunID != 1234 {
		t.Fatalf("expected run ID 1234, got %d", lookup.RunID)
	}
	if lookup.InstanceID != "i-123" {
		t.Fatalf("expected instance ID i-123, got %q", lookup.InstanceID)
	}
}

func TestFindJobLookupFallsBackToRunnerNameInstanceID(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobLookupRecord{
					RunID:      1234,
					RunnerName: "org--i-123--job",
					Status:     "completed",
				}),
			}, nil
		},
	}

	lookup, err := findJobLookup(context.Background(), client, "runs-on-workflow-jobs", "42")
	if err != nil {
		t.Fatalf("findJobLookup returned error: %v", err)
	}
	if lookup.InstanceID != "i-123" {
		t.Fatalf("expected runner-name instance ID i-123, got %q", lookup.InstanceID)
	}
}

func TestFindJobLookupFallsBackToAttemptHistoryInstanceID(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobLookupRecord{
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

	lookup, err := findJobLookup(context.Background(), client, "runs-on-workflow-jobs", "42")
	if err != nil {
		t.Fatalf("findJobLookup returned error: %v", err)
	}
	if lookup.InstanceID != "i-123" {
		t.Fatalf("expected attempt-history instance ID i-123, got %q", lookup.InstanceID)
	}
}

func TestFindJobLookupMissingRow(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{}, nil
		},
	}

	lookup, err := findJobLookup(context.Background(), client, "runs-on-workflow-jobs", "42")
	if err != nil {
		t.Fatalf("findJobLookup returned error: %v", err)
	}
	if lookup != nil {
		t.Fatalf("expected missing row to return nil lookup, got %+v", lookup)
	}
}

func TestWaitForJobLookupWaitsForInstanceID(t *testing.T) {
	var calls int
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			calls++
			if calls == 1 {
				return &dynamodb.GetItemOutput{
					Item: marshalWorkflowJobItem(t, workflowJobLookupRecord{
						RunID:           1234,
						Status:          "queued",
						SchedulingState: "launching",
					}),
				}, nil
			}
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobLookupRecord{
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

	lookup, err := waitForJobLookupWithInterval(context.Background(), client, "runs-on-workflow-jobs", "42", true, nil, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("waitForJobLookupWithInterval returned error: %v", err)
	}
	if lookup.InstanceID != "i-123" {
		t.Fatalf("expected instance ID i-123, got %q", lookup.InstanceID)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 GetItem calls, got %d", calls)
	}
}

func TestWaitForJobLookupNoWatchReturnsPendingState(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: marshalWorkflowJobItem(t, workflowJobLookupRecord{
					RunID:           1234,
					Status:          "queued",
					SchedulingState: "launching",
				}),
			}, nil
		},
	}

	_, err := waitForJobLookupWithInterval(context.Background(), client, "runs-on-workflow-jobs", "42", false, nil, time.Millisecond)
	if err == nil {
		t.Fatal("expected waitForJobLookupWithInterval to return an error")
	}
	if !strings.Contains(err.Error(), "status=queued") || !strings.Contains(err.Error(), "scheduling_state=launching") {
		t.Fatalf("expected status and scheduling state in error, got %v", err)
	}
}

func TestWaitForJobLookupHonorsContext(t *testing.T) {
	client := &mockWorkflowJobsClient{
		getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{}, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := waitForJobLookupWithInterval(ctx, client, "runs-on-workflow-jobs", "42", true, nil, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected context deadline error")
	}
	if ctx.Err() == nil || !strings.Contains(err.Error(), ctx.Err().Error()) {
		t.Fatalf("expected context error %v, got %v", ctx.Err(), err)
	}
}
