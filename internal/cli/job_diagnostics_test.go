package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

type mockJobDiagnosticsLambda struct {
	invocations int
	response    jobDiagnosticsResponse
	payload     []byte
	invoke      func(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

type fakeLocalGitHubWorkflowJobFetcher struct {
	job *jobDiagnosticsWorkflowJob
	err error
}

func (f fakeLocalGitHubWorkflowJobFetcher) FetchWorkflowJob(context.Context, jobDiagnosticsRequest) (*jobDiagnosticsWorkflowJob, error) {
	return f.job, f.err
}

func (m *mockJobDiagnosticsLambda) Invoke(ctx context.Context, input *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	m.invocations++
	if m.invoke != nil {
		return m.invoke(ctx, input, optFns...)
	}
	payload := m.payload
	if len(payload) == 0 {
		var err error
		payload, err = json.Marshal(m.response)
		if err != nil {
			return nil, err
		}
	}
	return &lambda.InvokeOutput{Payload: payload}, nil
}

func testJobFactsProvider(response jobDiagnosticsResponse) *jobFactsProvider {
	client := &mockJobDiagnosticsLambda{response: response}
	return &jobFactsProvider{
		product: response.Product,
		resolver: &jobDiagnosticsResolver{
			client:       client,
			functionName: "job-diagnostics",
		},
		jobID:  "42",
		jobRef: "https://github.com/runs-on/server/actions/runs/1234/job/42",
	}
}

func TestBuildJobDiagnosticsRequestParsesGitHubJobURL(t *testing.T) {
	request, err := buildJobDiagnosticsRequest("https://github.com/runs-on/server/actions/runs/1234/job/42?pr=1")
	if err != nil {
		t.Fatalf("buildJobDiagnosticsRequest returned error: %v", err)
	}
	if request.Owner != "runs-on" || request.Repo != "server" || request.WorkflowRunID != 1234 || request.WorkflowJobID != 42 {
		t.Fatalf("unexpected request: %+v", request)
	}
}

func TestBuildJobDiagnosticsRequestParsesGHESJobURL(t *testing.T) {
	request, err := buildJobDiagnosticsRequest("https://github.example.com/runs-on/server/actions/runs/1234/job/42")
	if err != nil {
		t.Fatalf("buildJobDiagnosticsRequest returned error: %v", err)
	}
	if request.GitHubHost != "github.example.com" || request.Owner != "runs-on" || request.Repo != "server" || request.WorkflowRunID != 1234 || request.WorkflowJobID != 42 {
		t.Fatalf("unexpected request: %+v", request)
	}
}

func TestBuildJobDiagnosticsRequestRequiresGitHubJobURL(t *testing.T) {
	_, err := buildJobDiagnosticsRequest("42")
	if err == nil {
		t.Fatal("expected numeric job ID to be rejected")
	}
	if !strings.Contains(err.Error(), "GitHub Actions job URL is required") {
		t.Fatalf("expected job URL error, got %v", err)
	}
}

func TestJobDiagnosticsResolverInvokesLambda(t *testing.T) {
	client := &mockJobDiagnosticsLambda{
		response: jobDiagnosticsResponse{
			Status:  "found",
			Product: "flex",
			GitHub: jobDiagnosticsGitHub{
				WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "runs-on--i-123--job"},
			},
			Local: &jobDiagnosticsLocal{
				Source:      "flex_workflow_jobs",
				InstanceIDs: []string{"i-123"},
			},
		},
	}
	resolver := &jobDiagnosticsResolver{client: client, functionName: "resolver"}

	response, err := resolver.Resolve(context.Background(), "https://github.com/runs-on/server/actions/runs/1234/job/42")
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if client.invocations != 1 {
		t.Fatalf("invocations = %d, want 1", client.invocations)
	}
	if response.workflowFacts(42).CurrentInstanceID != "i-123" {
		t.Fatalf("unexpected facts: %+v", response.workflowFacts(42))
	}
}

func TestJobDiagnosticsWorkflowFactsPreferGitHubTimestamps(t *testing.T) {
	localCreatedAt := time.Date(2026, 5, 8, 11, 0, 0, 0, time.UTC)
	githubCreatedAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	githubCompletedAt := time.Date(2026, 5, 8, 12, 30, 0, 0, time.UTC)
	response := &jobDiagnosticsResponse{
		Status:  "found",
		Product: "flex",
		GitHub: jobDiagnosticsGitHub{
			WorkflowJob: &jobDiagnosticsWorkflowJob{
				ID:          42,
				RunID:       1234,
				CreatedAt:   githubCreatedAt.Format(time.RFC3339),
				CompletedAt: githubCompletedAt.Format(time.RFC3339),
			},
		},
		Local: &jobDiagnosticsLocal{
			Source:        "flex_workflow_jobs",
			WorkflowJobID: 42,
			WorkflowRunID: 1234,
			CreatedAt:     localCreatedAt.Format(time.RFC3339),
		},
	}

	facts := response.workflowFacts(42)
	if !facts.CreatedAt.Equal(githubCreatedAt) || facts.CreatedAtSource != "github.workflow_job.created_at" {
		t.Fatalf("unexpected created_at facts: %s source=%q", facts.CreatedAt, facts.CreatedAtSource)
	}
	if !facts.CompletedAt.Equal(githubCompletedAt) || facts.CompletedAtSource != "github.workflow_job.completed_at" {
		t.Fatalf("unexpected completed_at facts: %s source=%q", facts.CompletedAt, facts.CompletedAtSource)
	}
}

func TestJobDiagnosticsWorkflowFactsUseLocalCompletedAtFallback(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	completedAt := time.Date(2026, 5, 8, 12, 30, 0, 0, time.UTC)
	response := &jobDiagnosticsResponse{
		Status:  "found",
		Product: "fleet",
		GitHub: jobDiagnosticsGitHub{
			WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234},
		},
		Local: &jobDiagnosticsLocal{
			Source:        "fleet_claims",
			WorkflowJobID: 42,
			WorkflowRunID: 1234,
			CreatedAt:     createdAt.Format(time.RFC3339),
			CompletedAt:   completedAt.Format(time.RFC3339),
		},
	}

	facts := response.workflowFacts(42)
	if !facts.CreatedAt.Equal(createdAt) || facts.CreatedAtSource != "local.created_at" {
		t.Fatalf("unexpected created_at facts: %s source=%q", facts.CreatedAt, facts.CreatedAtSource)
	}
	if !facts.CompletedAt.Equal(completedAt) || facts.CompletedAtSource != "local.completed_at" {
		t.Fatalf("unexpected completed_at facts: %s source=%q", facts.CompletedAt, facts.CompletedAtSource)
	}
}

func TestJobDiagnosticsWorkflowFactsUsesGitHubRunnerWhenLocalClaimMissing(t *testing.T) {
	response := &jobDiagnosticsResponse{
		Status:  "partial",
		Product: "fleet",
		Request: jobDiagnosticsRequest{WorkflowRunID: 1234, WorkflowJobID: 42},
		GitHub: jobDiagnosticsGitHub{
			WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "runs-on--i-github--job"},
		},
		Fleet: &jobDiagnosticsFleet{
			ClaimCount:      75,
			MatchBasis:      "runner_not_found",
			ExactMatchCount: 0,
		},
	}

	facts := response.workflowFacts(42)
	if facts == nil {
		t.Fatal("expected workflow facts from GitHub runner")
	}
	if facts.CurrentInstanceID != "i-github" {
		t.Fatalf("current instance ID = %q, want i-github", facts.CurrentInstanceID)
	}
	if facts.RunID != 1234 {
		t.Fatalf("run ID = %d, want 1234", facts.RunID)
	}
}

func TestJobDiagnosticsClassifiesJobProduct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response *jobDiagnosticsResponse
		want     string
	}{
		{
			name: "fleet label",
			response: &jobDiagnosticsResponse{GitHub: jobDiagnosticsGitHub{
				WorkflowJob: &jobDiagnosticsWorkflowJob{Labels: []string{"self-hosted", "runs-on/fleet=linux-small/env=production"}},
			}},
			want: "fleet",
		},
		{
			name: "flex equals label",
			response: &jobDiagnosticsResponse{GitHub: jobDiagnosticsGitHub{
				WorkflowJob: &jobDiagnosticsWorkflowJob{Labels: []string{"runs-on=123/runner=1cpu-linux-x64/env=production"}},
			}},
			want: "flex",
		},
		{
			name: "flex slash label",
			response: &jobDiagnosticsResponse{GitHub: jobDiagnosticsGitHub{
				WorkflowJob: &jobDiagnosticsWorkflowJob{Labels: []string{"runs-on/pool=default/env=production"}},
			}},
			want: "flex",
		},
		{
			name:     "local flex source",
			response: &jobDiagnosticsResponse{Local: &jobDiagnosticsLocal{Source: "flex_workflow_jobs"}},
			want:     "flex",
		},
		{
			name:     "local fleet source",
			response: &jobDiagnosticsResponse{Local: &jobDiagnosticsLocal{Source: "fleet_claims"}},
			want:     "fleet",
		},
		{
			name: "unknown labels",
			response: &jobDiagnosticsResponse{GitHub: jobDiagnosticsGitHub{
				WorkflowJob: &jobDiagnosticsWorkflowJob{Labels: []string{"self-hosted", "linux"}},
			}},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.response.classifiedJobProduct(); got != tt.want {
				t.Fatalf("classifiedJobProduct() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJobFactsProviderRejectsCrossProductDiagnostics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		stackProduct string
		stackName    string
		labels       []string
		want         string
	}{
		{
			name:         "fleet stack flex job",
			stackProduct: "fleet",
			stackName:    "runs-on-fleet-dev-v3",
			labels:       []string{"runs-on=123/runner=1cpu-linux-x64/env=dev"},
			want:         `job 42 is a Flex job, but stack "runs-on-fleet-dev-v3" is a Fleet stack`,
		},
		{
			name:         "flex stack fleet job",
			stackProduct: "flex",
			stackName:    "runs-on-flex-dev-v3",
			labels:       []string{"runs-on/fleet=linux-small/env=dev"},
			want:         `job 42 is a Fleet job, but stack "runs-on-flex-dev-v3" is a Flex stack`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			facts := testJobFactsProvider(jobDiagnosticsResponse{
				Status:  "partial",
				Product: tt.stackProduct,
				Request: jobDiagnosticsRequest{WorkflowRunID: 1234, WorkflowJobID: 42},
				GitHub: jobDiagnosticsGitHub{
					WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, Labels: tt.labels},
				},
			})
			facts.product = tt.stackProduct
			facts.stackName = tt.stackName

			err := facts.refresh(context.Background())
			if err == nil || err.Error() != tt.want {
				t.Fatalf("refresh error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestFleetWorkflowFactsUsesLatestAttemptWhenClaimHasNoCurrentInstance(t *testing.T) {
	response := &jobDiagnosticsResponse{
		Status:  "found",
		Product: "fleet",
		Request: jobDiagnosticsRequest{WorkflowRunID: 1234, WorkflowJobID: 42},
		Local: &jobDiagnosticsLocal{
			Source:        "fleet_claims",
			WorkflowJobID: 42,
			WorkflowRunID: 1234,
			InstanceIDs:   []string{"i-z-old", "i-a-new"},
			Status:        "job_claimed",
			Record: json.RawMessage(`{
				"attempted_instance_ids":["i-z-old","i-a-new"],
				"state":"job_claimed"
			}`),
		},
	}

	facts := response.workflowFacts(42)
	if facts == nil {
		t.Fatal("expected workflow facts")
	}
	if facts.CurrentInstanceID != "i-a-new" {
		t.Fatalf("current instance ID = %q, want latest attempted instance i-a-new", facts.CurrentInstanceID)
	}
	if got := strings.Join(facts.AttemptedInstanceIDs, ","); got != "i-z-old,i-a-new" {
		t.Fatalf("attempted instance IDs = %q, want preserved attempt order", got)
	}
}

func TestJobFactsProviderFallsBackToLocalGHForFleetWorkflowJob(t *testing.T) {
	facts := testJobFactsProvider(jobDiagnosticsResponse{
		Status:  "ambiguous",
		Product: "fleet",
		Request: jobDiagnosticsRequest{
			GitHubHost:    "github.com",
			Owner:         "runs-on-demo",
			Repo:          "scaleset-demo",
			WorkflowRunID: 1234,
			WorkflowJobID: 42,
		},
		Fleet: &jobDiagnosticsFleet{ClaimCount: 75, MatchBasis: "workflow_run_id"},
	})
	facts.github = fakeLocalGitHubWorkflowJobFetcher{
		job: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "runs-on--i-gh--job"},
	}

	if err := facts.refresh(context.Background()); err != nil {
		t.Fatalf("refresh returned error: %v", err)
	}
	got := facts.current()
	if got == nil {
		t.Fatal("expected workflow facts")
	}
	if got.CurrentInstanceID != "i-gh" {
		t.Fatalf("current instance ID = %q, want i-gh", got.CurrentInstanceID)
	}
	if facts.diagnostics.GitHub.WorkflowJob == nil {
		t.Fatal("expected diagnostics to include local gh workflow job")
	}
	codes := diagnosticCodes(facts.diagnostics.Diagnostics)
	for _, want := range []string{"local_gh_workflow_job_fetch_attempt", "local_gh_workflow_job_fetched"} {
		if !slices.Contains(codes, want) {
			t.Fatalf("expected diagnostic %q in %v", want, codes)
		}
	}
}

func TestJobFactsProviderReportsLocalGHSetupFailure(t *testing.T) {
	facts := testJobFactsProvider(jobDiagnosticsResponse{
		Status:  "ambiguous",
		Product: "fleet",
		Request: jobDiagnosticsRequest{
			Owner:         "runs-on-demo",
			Repo:          "scaleset-demo",
			WorkflowRunID: 1234,
			WorkflowJobID: 42,
		},
		Fleet: &jobDiagnosticsFleet{ClaimCount: 75, MatchBasis: "workflow_run_id"},
	})
	facts.github = fakeLocalGitHubWorkflowJobFetcher{err: fmt.Errorf("GitHub CLI fallback requires gh; install GitHub CLI and run `gh auth login` with repository Actions read access")}

	if err := facts.refresh(context.Background()); err != nil {
		t.Fatalf("refresh returned error: %v", err)
	}
	if got := facts.current(); got != nil {
		t.Fatalf("expected no facts, got %+v", got)
	}
	last := facts.diagnostics.Diagnostics[len(facts.diagnostics.Diagnostics)-1]
	if last.Code != "local_gh_workflow_job_fetch_failed" {
		t.Fatalf("last diagnostic code = %q, want local_gh_workflow_job_fetch_failed", last.Code)
	}
	if !strings.Contains(last.Message, "gh auth login") {
		t.Fatalf("expected setup hint in diagnostic, got %q", last.Message)
	}
	if !slices.Contains(diagnosticCodes(facts.diagnostics.Diagnostics), "local_gh_workflow_job_fetch_attempt") {
		t.Fatalf("expected local gh attempt diagnostic in %+v", facts.diagnostics.Diagnostics)
	}
}

func diagnosticCodes(diagnostics []jobDiagnosticsDiagnostic) []string {
	codes := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		codes = append(codes, diagnostic.Code)
	}
	return codes
}

func TestJobDiagnosticsResolverSendsRequestPayload(t *testing.T) {
	client := &mockJobDiagnosticsLambda{
		invoke: func(_ context.Context, input *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
			if got := aws.ToString(input.FunctionName); got != "resolver" {
				t.Fatalf("function name = %q", got)
			}
			var request jobDiagnosticsRequest
			if err := json.Unmarshal(input.Payload, &request); err != nil {
				t.Fatalf("request payload was invalid: %v", err)
			}
			if request.WorkflowJobID != 42 || request.WorkflowRunID != 1234 || request.Owner != "runs-on" || request.Repo != "server" {
				t.Fatalf("unexpected request payload: %+v", request)
			}
			return &lambda.InvokeOutput{Payload: []byte(`{"status":"not_found","product":"flex"}`)}, nil
		},
	}
	resolver := &jobDiagnosticsResolver{client: client, functionName: "resolver"}
	if _, err := resolver.Resolve(context.Background(), "https://github.com/runs-on/server/actions/runs/1234/job/42"); err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
}
