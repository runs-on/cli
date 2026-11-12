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

// Without --watch, roc returns the instance only once the selected stack's own
// record names it, and otherwise says why it has none.
func TestWaitForJobFactsNoWatch(t *testing.T) {
	fleetClaim := func(instanceIDs ...string) *jobDiagnosticsLocal {
		return &jobDiagnosticsLocal{Source: "fleet_claims", WorkflowJobID: 42, WorkflowRunID: 1234, Status: "queued", InstanceIDs: instanceIDs}
	}
	githubRunner := func(name string) jobDiagnosticsGitHub {
		return jobDiagnosticsGitHub{WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: name}}
	}
	tests := []struct {
		name         string
		resolve      func(jobDiagnosticsRequest) jobDiagnosticsResponse
		github       localGitHubWorkflowJobFetcher
		wantInstance string
		wantErr      string
	}{
		{
			name: "fleet claim without an instance yet",
			resolve: func(jobDiagnosticsRequest) jobDiagnosticsResponse {
				return jobDiagnosticsResponse{Status: "found", Product: "fleet", GitHub: githubRunner(""), Local: fleetClaim()}
			},
			wantErr: `instance ID for job 42 not available yet job_found_in="job diagnostics resolver Lambda job-diagnostics" status=queued`,
		},
		{
			name: "flex: GitHub runner without a stack record",
			resolve: func(jobDiagnosticsRequest) jobDiagnosticsResponse {
				return jobDiagnosticsResponse{Status: "partial", Product: "flex", StackName: "prod", GitHub: githubRunner("my-self-hosted--i-0aaa")}
			},
			wantErr: `job 42 runs on instance i-0aaa according to GitHub, but the selected RunsOn stack "prod" has no record of that instance`,
		},
		{
			name: "flex: stack record without the GitHub runner's instance",
			resolve: func(jobDiagnosticsRequest) jobDiagnosticsResponse {
				return jobDiagnosticsResponse{Status: "found", Product: "flex", StackName: "prod", GitHub: githubRunner("runs-on--i-0ccc--abc"),
					Local: &jobDiagnosticsLocal{Source: "flex_workflow_jobs", WorkflowJobID: 42, Status: "queued"}}
			},
			wantErr: `job 42 runs on instance i-0ccc according to GitHub, but the selected RunsOn stack "prod" has no record of that instance`,
		},
		{
			name: "flex: stack record names the instance",
			resolve: func(jobDiagnosticsRequest) jobDiagnosticsResponse {
				return jobDiagnosticsResponse{Status: "found", Product: "flex", StackName: "prod", GitHub: githubRunner("runs-on--i-flex--abc"),
					Local: &jobDiagnosticsLocal{Source: "flex_workflow_jobs", WorkflowJobID: 42, RunnerName: "runs-on--i-flex--abc"}}
			},
			wantInstance: "i-flex",
		},
		{
			name: "fleet: local gh runner without a claim",
			resolve: func(jobDiagnosticsRequest) jobDiagnosticsResponse {
				return jobDiagnosticsResponse{Status: "ambiguous", Product: "fleet", StackName: "prod",
					Request: jobDiagnosticsRequest{Owner: "runs-on", Repo: "server", WorkflowRunID: 1234, WorkflowJobID: 42}}
			},
			github:  fakeLocalGitHubWorkflowJobFetcher{job: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "attacker--i-0bbb"}},
			wantErr: `job 42 runs on instance i-0bbb according to GitHub, but the selected RunsOn stack "prod" has no record of that instance`,
		},
		{
			name: "fleet: claim matched on the local gh runner",
			resolve: func(request jobDiagnosticsRequest) jobDiagnosticsResponse {
				response := jobDiagnosticsResponse{Status: "ambiguous", Product: "fleet", StackName: "prod",
					Request: jobDiagnosticsRequest{Owner: "runs-on", Repo: "server", WorkflowRunID: 1234, WorkflowJobID: 42}}
				if request.GitHubCached && request.RunnerName == "runs-on--i-gh--job" {
					response.Status, response.Local = "found", fleetClaim("i-gh")
				}
				return response
			},
			github:       fakeLocalGitHubWorkflowJobFetcher{job: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "runs-on--i-gh--job"}},
			wantInstance: "i-gh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolverClient := &mockJobDiagnosticsLambda{}
			resolverClient.invoke = func(_ context.Context, input *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
				var request jobDiagnosticsRequest
				if err := json.Unmarshal(input.Payload, &request); err != nil {
					return nil, err
				}
				payload, err := json.Marshal(tt.resolve(request))
				return &lambda.InvokeOutput{Payload: payload}, err
			}
			factsProvider := testJobFactsProvider(jobDiagnosticsResponse{})
			factsProvider.resolver.client = resolverClient
			factsProvider.stackName, factsProvider.github = "prod", tt.github

			facts, err := waitForJobFacts(context.Background(), factsProvider, false, time.Millisecond)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("waitForJobFacts returned error: %v", err)
			}
			if facts.CurrentInstanceID != tt.wantInstance {
				t.Fatalf("current instance ID = %q, want %q", facts.CurrentInstanceID, tt.wantInstance)
			}
		})
	}
}
