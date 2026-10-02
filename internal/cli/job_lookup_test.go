package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

func TestWaitForJobFactsUntilFleetInstanceIDAppears(t *testing.T) {
	var calls int
	resolverClient := &mockJobDiagnosticsLambda{}
	resolverClient.invoke = func(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
		calls++
		response := jobDiagnosticsResponse{
			Status:  "found",
			Product: "fleet",
			GitHub: jobDiagnosticsGitHub{
				WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234},
			},
			Local: &jobDiagnosticsLocal{
				Source:        "fleet_claims",
				WorkflowJobID: 42,
				WorkflowRunID: 1234,
				Status:        "queued",
			},
		}
		if calls > 1 {
			response.Local.InstanceIDs = []string{"i-fleet"}
			response.Local.Status = "job_claimed"
		}
		payload, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		return &lambda.InvokeOutput{Payload: payload}, nil
	}
	factsProvider := testJobFactsProvider(jobDiagnosticsResponse{Product: "fleet"})
	factsProvider.resolver.client = resolverClient

	facts, err := waitForJobFacts(context.Background(), factsProvider, true, time.Millisecond)
	if err != nil {
		t.Fatalf("waitForJobFacts returned error: %v", err)
	}
	if facts.CurrentInstanceID != "i-fleet" {
		t.Fatalf("expected Fleet instance ID i-fleet, got %q", facts.CurrentInstanceID)
	}
	if calls < 2 {
		t.Fatalf("expected resolver to be retried, got %d calls", calls)
	}
}

func TestWaitForJobFactsNoWatchReturnsResolverState(t *testing.T) {
	factsProvider := testJobFactsProvider(jobDiagnosticsResponse{
		Status:  "found",
		Product: "fleet",
		GitHub: jobDiagnosticsGitHub{
			WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234},
		},
		Local: &jobDiagnosticsLocal{
			Source:        "fleet_claims",
			WorkflowJobID: 42,
			WorkflowRunID: 1234,
			Status:        "queued",
		},
	})

	_, err := waitForJobFacts(context.Background(), factsProvider, false, time.Millisecond)
	if err == nil {
		t.Fatal("expected missing Fleet instance ID to return an error")
	}
	if !strings.Contains(err.Error(), "job_found_in=\"job diagnostics resolver Lambda job-diagnostics\"") || !strings.Contains(err.Error(), "status=queued") {
		t.Fatalf("expected resolver target and status in error, got %v", err)
	}
}
