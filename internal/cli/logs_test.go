package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestApplicationFilterPatternUsesJobIDByDefault(t *testing.T) {
	fetcher := &LogFetcher{
		jobID:      "42",
		instanceID: "i-123",
	}

	filterPattern, err := fetcher.applicationFilterPattern()
	if err != nil {
		t.Fatalf("applicationFilterPattern returned error: %v", err)
	}
	if !strings.Contains(filterPattern, `$.job_id = "42"`) {
		t.Fatalf("expected job ID filter, got %q", filterPattern)
	}
	if !strings.Contains(filterPattern, `$.message = "*i-123*"`) {
		t.Fatalf("expected instance ID message filter, got %q", filterPattern)
	}
}

func TestApplicationFilterPatternUsesRunIDWhenRequested(t *testing.T) {
	fetcher := &LogFetcher{
		jobID:        "42",
		runID:        1234,
		instanceID:   "i-123",
		includeTypes: []string{"run"},
	}

	filterPattern, err := fetcher.applicationFilterPattern()
	if err != nil {
		t.Fatalf("applicationFilterPattern returned error: %v", err)
	}
	if !strings.Contains(filterPattern, `$.run_id = "1234"`) {
		t.Fatalf("expected run ID filter, got %q", filterPattern)
	}
	if strings.Contains(filterPattern, `$.job_id = "42"`) {
		t.Fatalf("did not expect job ID filter in run-scoped pattern, got %q", filterPattern)
	}
	if strings.Contains(filterPattern, `$.message = "*i-123*"`) {
		t.Fatalf("did not expect instance ID message filter in run-scoped pattern, got %q", filterPattern)
	}
}

func TestApplicationLogGroupIdentifierPrefersServiceLogGroup(t *testing.T) {
	outputs := &StackOutputs{
		ServiceLogGroupName: "/aws/ecs/runs-on-preview-v3/flexd",
	}

	logGroup, err := outputs.applicationLogGroupIdentifier()
	if err != nil {
		t.Fatalf("applicationLogGroupIdentifier returned error: %v", err)
	}
	if logGroup != outputs.ServiceLogGroupName {
		t.Fatalf("expected service log group %q, got %q", outputs.ServiceLogGroupName, logGroup)
	}
}

func TestApplicationLogGroupIdentifierRequiresServiceLogGroup(t *testing.T) {
	if _, err := (&StackOutputs{}).applicationLogGroupIdentifier(); err == nil {
		t.Fatal("expected applicationLogGroupIdentifier to fail without a service log group")
	}
}

func TestRefreshJobLookupHandlesMissingRow(t *testing.T) {
	fetcher := &LogFetcher{
		jobs: &mockWorkflowJobsClient{
			getItem: func(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{}, nil
			},
		},
		jobsTable:  "runs-on-workflow-jobs",
		jobID:      "42",
		instanceID: "i-existing",
		runID:      1234,
	}

	if err := fetcher.refreshJobLookup(context.Background()); err != nil {
		t.Fatalf("refreshJobLookup returned error: %v", err)
	}
	if fetcher.instanceID != "" {
		t.Fatalf("expected empty instance ID, got %q", fetcher.instanceID)
	}
	if fetcher.runID != 0 {
		t.Fatalf("expected run ID to be cleared, got %d", fetcher.runID)
	}
}
