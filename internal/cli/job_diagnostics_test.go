package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

type mockJobDiagnosticsLambda struct {
	invocations int
	requests    []jobDiagnosticsRequest
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

// queuedThenAssignedGitHub reports the job queued, then assigned to a runner.
type queuedThenAssignedGitHub struct{ calls int }

func (f *queuedThenAssignedGitHub) FetchWorkflowJob(context.Context, jobDiagnosticsRequest) (*jobDiagnosticsWorkflowJob, error) {
	f.calls++
	job := &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234}
	if f.calls > 1 {
		job.RunnerName = "runs-on--i-gh--job"
	}
	return job, nil
}

func (m *mockJobDiagnosticsLambda) Invoke(ctx context.Context, input *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	m.invocations++
	var request jobDiagnosticsRequest
	if err := json.Unmarshal(input.Payload, &request); err != nil {
		return nil, err
	}
	m.requests = append(m.requests, request)
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

// discardLogger stands in for the --debug logger when --debug is off.
var discardLogger = log.New(io.Discard, "", 0)

// testJob is the job the resolver fakes describe.
var testJob = parsedGitHubJobURL{
	URL:   "https://github.com/runs-on/server/actions/runs/1234/job/42",
	Host:  "github.com",
	Owner: "runs-on",
	Repo:  "server",
	RunID: 1234,
	JobID: 42,
}

func testJobFactsProvider(response jobDiagnosticsResponse) *jobFactsProvider {
	client := &mockJobDiagnosticsLambda{response: response}
	return &jobFactsProvider{
		product: parseProduct(response.Product),
		resolver: &jobDiagnosticsResolver{
			client:       client,
			functionName: "job-diagnostics",
		},
		job:    testJob,
		logger: discardLogger,
	}
}

// A Flex resolver skips GitHub on cached polls, so roc keeps reusing the job,
// queued or not; only Fleet refetches a job GitHub has not named a runner for.
func TestJobFactsProviderReusesGitHubDetailsOnRepeatPolls(t *testing.T) {
	tests := []struct {
		name         string
		runnerName   string
		wantInstance string
	}{
		{name: "runner named", runnerName: "runs-on--i-flex--job", wantInstance: "i-flex"},
		{name: "still queued"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &mockJobDiagnosticsLambda{}
			client.invoke = func(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
				response := jobDiagnosticsResponse{
					Status:  "found",
					Product: "flex",
					Local:   &jobDiagnosticsLocal{Source: "flex_workflow_jobs", WorkflowJobID: 42, WorkflowRunID: 1234},
				}
				// A cached poll gets no GitHub details back, as from the resolver.
				if !client.requests[len(client.requests)-1].GitHubCached {
					response.GitHub.WorkflowJob = &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: tt.runnerName}
				}
				payload, err := json.Marshal(response)
				return &lambda.InvokeOutput{Payload: payload}, err
			}
			provider := testJobFactsProvider(jobDiagnosticsResponse{Product: "flex"})
			provider.resolver.client = client

			for range 3 {
				if err := provider.refresh(context.Background()); err != nil {
					t.Fatalf("refresh returned error: %v", err)
				}
			}

			for _, repeat := range client.requests[1:] {
				if !repeat.GitHubCached || repeat.RunnerName != tt.runnerName {
					t.Fatalf("repeat poll must send the cached runner %q, got %+v", tt.runnerName, repeat)
				}
			}
			for _, request := range client.requests {
				if request.IncludeDeliveryMetadata {
					t.Fatalf("job fact polls must not request delivery metadata: %+v", request)
				}
			}
			if got := provider.current().CurrentInstanceID; got != tt.wantInstance {
				t.Fatalf("current instance = %q, want %q from the reused GitHub runner", got, tt.wantInstance)
			}
		})
	}
}

func TestJobFactsRefreshStopsOnceLocalRecordCompleted(t *testing.T) {
	for _, tc := range []struct {
		name            string
		localCompleted  string
		githubCompleted string
		wantRefreshes   int
	}{
		{name: "local record completed", localCompleted: "2026-05-08T12:30:00Z", wantRefreshes: 1},
		{name: "only GitHub completed", githubCompleted: "2026-05-08T12:30:00Z", wantRefreshes: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := testJobFactsProvider(jobDiagnosticsResponse{
				Status:  "found",
				Product: "flex",
				GitHub: jobDiagnosticsGitHub{
					WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, CompletedAt: tc.githubCompleted},
				},
				Local: &jobDiagnosticsLocal{
					Source:        "flex_workflow_jobs",
					WorkflowJobID: 42,
					CompletedAt:   tc.localCompleted,
				},
			})
			ticks := make(chan time.Time, 3)
			for range cap(ticks) {
				ticks <- time.Now()
			}
			close(ticks)

			provider.refreshUntilCompleted(context.Background(), ticks, func() {})

			if got := provider.resolver.client.(*mockJobDiagnosticsLambda).invocations; got != tc.wantRefreshes {
				t.Fatalf("resolver invocations = %d, want %d", got, tc.wantRefreshes)
			}
		})
	}
}

func TestBuildJobDiagnosticsRequestFromJobURL(t *testing.T) {
	tests := []struct {
		input string
		want  jobDiagnosticsRequest // zero when the input is rejected
	}{
		{
			input: "https://github.com/runs-on/server/actions/runs/1234/job/42?pr=1",
			want:  jobDiagnosticsRequest{JobURL: "https://github.com/runs-on/server/actions/runs/1234/job/42?pr=1", GitHubHost: "github.com", Owner: "runs-on", Repo: "server", WorkflowRunID: 1234, WorkflowJobID: 42},
		},
		{
			// The job ID the commands print and match comes from here.
			input: "https://github.com/runs-on/server/actions/runs/1234/job/42/",
			want:  jobDiagnosticsRequest{JobURL: "https://github.com/runs-on/server/actions/runs/1234/job/42/", GitHubHost: "github.com", Owner: "runs-on", Repo: "server", WorkflowRunID: 1234, WorkflowJobID: 42},
		},
		{
			// job_url is the trimmed input.
			input: " https://github.example.com/runs-on/server/actions/runs/1234/job/42\n",
			want:  jobDiagnosticsRequest{JobURL: "https://github.example.com/runs-on/server/actions/runs/1234/job/42", GitHubHost: "github.example.com", Owner: "runs-on", Repo: "server", WorkflowRunID: 1234, WorkflowJobID: 42},
		},
		{input: "42"},
	}
	for _, tt := range tests {
		job, err := parseGitHubJobURL(tt.input)
		if tt.want == (jobDiagnosticsRequest{}) {
			if err == nil || !strings.Contains(err.Error(), "GitHub Actions job URL is required") {
				t.Errorf("parseGitHubJobURL(%q) error = %v, want job URL error", tt.input, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseGitHubJobURL(%q) returned error: %v", tt.input, err)
		}
		if got := buildJobDiagnosticsRequest(job); got != tt.want {
			t.Errorf("request for %q = %+v, want %+v", tt.input, got, tt.want)
		}
	}
}

func TestJobDiagnosticsResolverInvokesLambda(t *testing.T) {
	var functionName string
	client := &mockJobDiagnosticsLambda{}
	client.invoke = func(_ context.Context, input *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
		functionName = aws.ToString(input.FunctionName)
		return &lambda.InvokeOutput{Payload: []byte(`{"status":"not_found","product":"flex"}`)}, nil
	}
	resolver := &jobDiagnosticsResolver{client: client, functionName: "resolver"}

	response, err := resolver.resolveRequest(context.Background(), buildJobDiagnosticsRequest(testJob))
	if err != nil {
		t.Fatalf("resolveRequest returned error: %v", err)
	}
	if functionName != "resolver" || len(client.requests) != 1 || client.requests[0] != buildJobDiagnosticsRequest(testJob) {
		t.Fatalf("invoked %q with %+v", functionName, client.requests)
	}
	if response.Status != "not_found" || response.Product != "flex" {
		t.Fatalf("decoded response = %+v", response)
	}
}

// The run ID falls back from the GitHub workflow job to the workflow run, the
// local record and the request, and the job URL from GitHub to the request to
// the URL given on the command line.
func TestJobFactsIdentifiers(t *testing.T) {
	tests := []struct {
		name     string
		response jobDiagnosticsResponse
		want     string
	}{
		{
			name:     "not found: the request's run ID and job URL",
			response: jobDiagnosticsResponse{Request: jobDiagnosticsRequest{WorkflowRunID: 4, JobURL: " https://request/job/42 "}},
			want:     "found=false run=4 url=https://request/job/42 scaleset=",
		},
		{
			name:     "not found: the parsed job URL",
			response: jobDiagnosticsResponse{Request: jobDiagnosticsRequest{WorkflowRunID: 4}},
			want:     "found=false run=4 url=" + testJob.URL + " scaleset=",
		},
		{
			name: "GitHub workflow job first",
			response: jobDiagnosticsResponse{
				Request: jobDiagnosticsRequest{WorkflowRunID: 4, JobURL: "https://request/job/42"},
				GitHub: jobDiagnosticsGitHub{
					WorkflowJob: &jobDiagnosticsWorkflowJob{RunID: 1, HTMLURL: "https://github/job/42"},
					WorkflowRun: &jobDiagnosticsWorkflowRun{ID: 2},
				},
				Local: &jobDiagnosticsLocal{WorkflowRunID: 3},
			},
			want: "found=true run=1 url=https://github/job/42 scaleset=",
		},
		{
			name: "then the workflow run",
			response: jobDiagnosticsResponse{
				Request: jobDiagnosticsRequest{WorkflowRunID: 4, JobURL: "https://request/job/42"},
				GitHub: jobDiagnosticsGitHub{
					WorkflowJob: &jobDiagnosticsWorkflowJob{HTMLURL: " "},
					WorkflowRun: &jobDiagnosticsWorkflowRun{ID: 2},
				},
				Local: &jobDiagnosticsLocal{WorkflowRunID: 3},
			},
			want: "found=true run=2 url=https://request/job/42 scaleset=",
		},
		{
			name: "then the local record",
			response: jobDiagnosticsResponse{
				Request: jobDiagnosticsRequest{WorkflowRunID: 4},
				Local:   &jobDiagnosticsLocal{WorkflowRunID: 3, ScalesetJobID: " fleet-job-opaque "},
			},
			want: "found=true run=3 url=" + testJob.URL + " scaleset=fleet-job-opaque",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.response.jobFacts(testJob)
			if got := fmt.Sprintf("found=%t run=%d url=%s scaleset=%s", f.Found, f.RunID, f.JobURL, f.ScalesetJobID); got != tt.want {
				t.Fatalf("facts = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJobFactsInstances(t *testing.T) {
	githubRunner := jobDiagnosticsGitHub{WorkflowJob: &jobDiagnosticsWorkflowJob{RunnerName: "runs-on--i-github--job"}}
	tests := []struct {
		name    string
		product string
		github  jobDiagnosticsGitHub
		local   *jobDiagnosticsLocal
		want    string
	}{
		{
			name:    "GitHub runner without a local record",
			product: "fleet",
			github:  githubRunner,
			want:    "current=i-github instances=i-github",
		},
		{
			name:    "flex: the first local instance; the local runner before GitHub's",
			product: "flex",
			github:  githubRunner,
			local:   &jobDiagnosticsLocal{InstanceIDs: []string{"i-a", "i-b"}, RunnerName: "runs-on--i-runner--job", Record: json.RawMessage(`{"instance_id":"i-b"}`)},
			want:    "current=i-a instances=i-a,i-b,i-runner",
		},
		{
			name:    "fleet: the claim's instance",
			product: "fleet",
			local:   &jobDiagnosticsLocal{InstanceIDs: []string{"i-a", "i-b"}, Record: json.RawMessage(`{"instance_id":"i-b","desired_runner_name":"runs-on--i-a--job"}`)},
			want:    "current=i-b instances=i-a,i-b",
		},
		{
			name:    "fleet: then the claim's desired runner",
			product: "fleet",
			local:   &jobDiagnosticsLocal{InstanceIDs: []string{"i-b", "i-a"}, Record: json.RawMessage(`{"desired_runner_name":"runs-on--i-a--job","attempted_instance_ids":["i-b"]}`)},
			want:    "current=i-a instances=i-b,i-a",
		},
		{
			name:    "fleet: then the claim's latest attempt",
			product: "fleet",
			local:   &jobDiagnosticsLocal{InstanceIDs: []string{"i-z-old", "i-a-new"}, RunnerName: "runs-on--i-z-old--job", Record: json.RawMessage(`{"attempted_instance_ids":["i-z-old","i-a-new"]}`)},
			want:    "current=i-a-new instances=i-z-old,i-a-new",
		},
		{
			name:    "fleet: then the local runner",
			product: "fleet",
			local:   &jobDiagnosticsLocal{InstanceIDs: []string{"i-r", "i-b"}, RunnerName: "runs-on--i-r--job"},
			want:    "current=i-r instances=i-r,i-b",
		},
		{
			name:    "fleet: then the latest local instance",
			product: "fleet",
			local:   &jobDiagnosticsLocal{InstanceIDs: []string{"i-a", "i-b"}},
			want:    "current=i-b instances=i-a,i-b",
		},
		{
			name:    "fleet: the product in any case",
			product: "Fleet",
			local:   &jobDiagnosticsLocal{InstanceIDs: []string{"i-a", "i-b"}},
			want:    "current=i-b instances=i-a,i-b",
		},
		{
			name:    "fleet: the current instance joins the attempts",
			product: "fleet",
			local:   &jobDiagnosticsLocal{InstanceIDs: []string{"i-a"}, Record: json.RawMessage(`{"instance_id":"i-x"}`)},
			want:    "current=i-x instances=i-a,i-x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := jobDiagnosticsResponse{Product: tt.product, GitHub: tt.github, Local: tt.local}
			f := response.jobFacts(testJob)
			if got := fmt.Sprintf("current=%s instances=%s", f.CurrentInstanceID, strings.Join(f.InstanceIDs, ",")); got != tt.want {
				t.Fatalf("facts = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJobFactsTimestamps(t *testing.T) {
	local := &jobDiagnosticsLocal{CreatedAt: "2026-05-08T11:00:00Z", CompletedAt: "2026-05-08T11:30:00Z"}
	tests := []struct {
		name   string
		github *jobDiagnosticsWorkflowJob
		want   string
	}{
		{
			name:   "GitHub first",
			github: &jobDiagnosticsWorkflowJob{CreatedAt: "2026-05-08T12:00:00Z", CompletedAt: "2026-05-08T12:30:00Z"},
			want:   "created=2026-05-08T12:00:00Z (github.workflow_job.created_at) completed=2026-05-08T12:30:00Z (github.workflow_job.completed_at)",
		},
		{
			name:   "then the local record",
			github: &jobDiagnosticsWorkflowJob{},
			want:   "created=2026-05-08T11:00:00Z (local.created_at) completed=2026-05-08T11:30:00Z (local.completed_at)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := jobDiagnosticsResponse{GitHub: jobDiagnosticsGitHub{WorkflowJob: tt.github}, Local: local}
			f := response.jobFacts(testJob)
			got := fmt.Sprintf("created=%s (%s) completed=%s (%s)", f.CreatedAt.Format(time.RFC3339), f.CreatedAtSource, f.CompletedAt.Format(time.RFC3339), f.CompletedAtSource)
			if got != tt.want {
				t.Fatalf("timestamps = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJobDiagnosticsClassifiesJobProduct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response *jobDiagnosticsResponse
		want     runsOnProduct
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
			name: "local flex source overrides fleet-shaped label",
			response: &jobDiagnosticsResponse{
				GitHub: jobDiagnosticsGitHub{
					WorkflowJob: &jobDiagnosticsWorkflowJob{Labels: []string{"runs-on/fleet=linux-small/env=production"}},
				},
				Local: &jobDiagnosticsLocal{Source: "flex_workflow_jobs"},
			},
			want: "flex",
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

// A job resolved from another stack, or belonging to the other product, is
// rejected before any CloudWatch read or archive write. `roc logs` goes
// through the job facts provider shared with the other job commands.
func TestJobCommandsRejectMismatchedStack(t *testing.T) {
	t.Chdir(t.TempDir()) // `roc logs --full` writes its archive to the working dir

	flexLabels := []string{"runs-on=123/runner=1cpu-linux-x64/env=dev"}
	fleetLabels := []string{"runs-on/fleet=linux-small/env=dev"}
	tests := []struct {
		name          string
		command       string // "logs" (logStreamer) or "logs --full" (fullLogExporter)
		stackProduct  runsOnProduct
		stackName     string // stack selected by the CLI
		resolvedStack string // stack the diagnostics resolver answered from
		labels        []string
		want          string
	}{
		{
			name:         "logs: fleet stack, flex job",
			command:      "logs",
			stackProduct: "fleet",
			stackName:    "runs-on-fleet-dev-v3",
			labels:       flexLabels,
			want:         `job 42 is a Flex job, but stack "runs-on-fleet-dev-v3" is a Fleet stack`,
		},
		{
			name:         "logs: flex stack, fleet job",
			command:      "logs",
			stackProduct: "flex",
			stackName:    "runs-on-flex-dev-v3",
			labels:       fleetLabels,
			want:         `job 42 is a Fleet job, but stack "runs-on-flex-dev-v3" is a Flex stack`,
		},
		{
			name:          "logs: diagnostics from another stack",
			command:       "logs",
			stackProduct:  "fleet",
			stackName:     "runs-on-fleet-preview-v3",
			resolvedStack: "runs-on-fleet-stage-v3",
			labels:        fleetLabels,
			want:          `job 42 diagnostics resolved stack "runs-on-fleet-stage-v3", but CLI selected stack "runs-on-fleet-preview-v3"`,
		},
		{
			name:         "logs --full: fleet stack, flex job",
			command:      "logs --full",
			stackProduct: "fleet",
			stackName:    "runs-on-fleet-dev-v3",
			labels:       flexLabels,
			want:         `job 42 is a Flex job, but stack "runs-on-fleet-dev-v3" is a Fleet stack`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := jobDiagnosticsResponse{
				Status:    "partial",
				Product:   string(tt.stackProduct),
				StackName: tt.resolvedStack,
				Request:   jobDiagnosticsRequest{WorkflowRunID: 1234, WorkflowJobID: 42},
				GitHub: jobDiagnosticsGitHub{
					WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, Labels: tt.labels},
				},
			}
			cwl := &mockCloudWatchLogsClient{}
			var err error
			switch tt.command {
			case "logs":
				streamer := testLogStreamer(cwl, &RunsOnConfig{
					ServiceLogGroupName:    "/aws/ecs/runs-on/fleetd",
					EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
				})
				facts := testJobFactsProvider(response)
				facts.product, facts.stackName = tt.stackProduct, tt.stackName
				err = streamer.StreamJob(context.Background(), facts, nil, &LogOptions{StartTime: 1, NoColor: true})
			case "logs --full":
				exporter := &fullLogExporter{
					cwl:      cwl,
					resolver: &jobDiagnosticsResolver{client: &mockJobDiagnosticsLambda{response: response}, functionName: "job-diagnostics"},
					config:   &RunsOnConfig{StackName: tt.stackName, Product: tt.stackProduct},
				}
				var zipPath string
				zipPath, err = exporter.Export(context.Background(), testJob)
				if zipPath != "" {
					t.Errorf("zipPath = %q, want empty before archive creation", zipPath)
				}
			default:
				t.Fatalf("unknown command %q", tt.command)
			}

			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if len(cwl.inputs) != 0 {
				t.Errorf("CloudWatch fetches = %d, want none before the mismatch error", len(cwl.inputs))
			}
			if entries, readErr := os.ReadDir("."); readErr != nil || len(entries) != 0 {
				t.Errorf("working dir entries = %v (err %v), want no archive", entries, readErr)
			}
		})
	}
}

func TestJobFactsProviderAcceptsMatchingStack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		product  runsOnProduct
		stack    string
		response jobDiagnosticsResponse
		want     string
	}{
		{
			name:    "diagnostics from the selected stack",
			product: "fleet",
			stack:   "runs-on-fleet-stage-v3",
			response: jobDiagnosticsResponse{
				Status:    "found",
				Product:   "fleet",
				StackName: "runs-on-fleet-stage-v3",
				Local:     &jobDiagnosticsLocal{Source: "fleet_claims", InstanceIDs: []string{"i-fleet"}},
			},
			want: "i-fleet",
		},
		{
			name:    "flex source with a fleet-shaped label",
			product: "flex",
			stack:   "runs-on-flex-dev-v3",
			response: jobDiagnosticsResponse{
				Status:    "found",
				Product:   "flex",
				StackName: "runs-on-flex-dev-v3",
				GitHub: jobDiagnosticsGitHub{
					WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, Labels: []string{"runs-on/fleet=linux-small/env=dev"}},
				},
				Local: &jobDiagnosticsLocal{Source: "flex_workflow_jobs", InstanceIDs: []string{"i-flex"}},
			},
			want: "i-flex",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			facts := testJobFactsProvider(tt.response)
			facts.product, facts.stackName = tt.product, tt.stack

			if err := facts.refresh(context.Background()); err != nil {
				t.Fatalf("refresh returned error: %v", err)
			}
			if got := facts.current().CurrentInstanceID; got != tt.want {
				t.Fatalf("current instance ID = %q, want %q", got, tt.want)
			}
		})
	}
}

// When the resolver cannot resolve a Fleet workflow job, roc asks the local gh
// CLI and records the attempt either way.
func TestJobFactsProviderFallsBackToLocalGHForFleetWorkflowJob(t *testing.T) {
	tests := []struct {
		name      string
		github    localGitHubWorkflowJobFetcher
		polls     int
		want      string
		wantCodes []string
	}{
		{
			name:      "fetched",
			github:    fakeLocalGitHubWorkflowJobFetcher{job: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "runs-on--i-gh--job"}},
			polls:     1,
			want:      "i-gh",
			wantCodes: []string{"local_gh_workflow_job_fetch_attempt", "local_gh_workflow_job_fetched"},
		},
		{
			name:      "gh unavailable",
			github:    fakeLocalGitHubWorkflowJobFetcher{err: errGHMissing},
			polls:     1,
			wantCodes: []string{"local_gh_workflow_job_fetch_attempt", "local_gh_workflow_job_fetch_failed"},
		},
		{
			name:      "runner assigned after a queued poll",
			github:    &queuedThenAssignedGitHub{},
			polls:     2,
			want:      "i-gh",
			wantCodes: []string{"local_gh_workflow_job_fetch_attempt", "local_gh_workflow_job_fetched"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts := testJobFactsProvider(jobDiagnosticsResponse{
				Status:  "ambiguous",
				Product: "fleet",
				Request: jobDiagnosticsRequest{Owner: "runs-on-demo", Repo: "scaleset-demo", WorkflowRunID: 1234, WorkflowJobID: 42},
				Fleet:   &jobDiagnosticsFleet{ClaimCount: 75, MatchBasis: "workflow_run_id"},
			})
			facts.github = tt.github

			for range tt.polls {
				if err := facts.refresh(context.Background()); err != nil {
					t.Fatalf("refresh returned error: %v", err)
				}
			}
			if got := facts.current().CurrentInstanceID; got != tt.want {
				t.Fatalf("current instance ID = %q, want %q", got, tt.want)
			}
			if codes := diagnosticCodes(facts.currentDiagnostics().Diagnostics); !slices.Equal(codes, tt.wantCodes) {
				t.Fatalf("diagnostic codes = %v, want %v", codes, tt.wantCodes)
			}
		})
	}
}

// Cleanup and the local gh fallback share one gh runner but keep their own
// setup hints.
func TestGHCallersExplainMissingGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	if _, err := (ghCLIRunner{}).Get(context.Background(), "github.com", "repos/runs-on/server"); err == nil || !strings.HasPrefix(err.Error(), "cleanup requires the GitHub CLI") {
		t.Errorf("cleanup error = %v", err)
	}
	if _, err := (ghCLIWorkflowJobFetcher{}).FetchWorkflowJob(context.Background(), buildJobDiagnosticsRequest(testJob)); err == nil || !strings.HasPrefix(err.Error(), "GitHub CLI fallback requires gh") {
		t.Errorf("fallback error = %v", err)
	}
}

func diagnosticCodes(diagnostics []jobDiagnosticsDiagnostic) []string {
	codes := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		codes = append(codes, diagnostic.Code)
	}
	return codes
}
