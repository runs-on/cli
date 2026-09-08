package cli

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cloudtrailtypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type mockCloudWatchLogsClient struct {
	mu      sync.Mutex
	inputs  []*cloudwatchlogs.FilterLogEventsInput
	onInput func(*cloudwatchlogs.FilterLogEventsInput)
	output  func(*cloudwatchlogs.FilterLogEventsInput) (*cloudwatchlogs.FilterLogEventsOutput, error)
}

func (m *mockCloudWatchLogsClient) FilterLogEvents(ctx context.Context, params *cloudwatchlogs.FilterLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	m.mu.Lock()
	m.inputs = append(m.inputs, cloneFilterLogEventsInput(params))
	onInput := m.onInput
	output := m.output
	m.mu.Unlock()

	if onInput != nil {
		onInput(params)
	}
	if output != nil {
		return output(params)
	}
	message := "server"
	if strings.Contains(aws.ToString(params.FilterPattern), "$.run_id") {
		return &cloudwatchlogs.FilterLogEventsOutput{
			Events: []cwltypes.FilteredLogEvent{
				{Message: aws.String(`{"run_id":1234,"job_url":"https://github.com/runs-on/server/actions/runs/1234/job/42","job_id":42,"message":"target"}`), Timestamp: aws.Int64(123), EventId: aws.String("target-event"), LogStreamName: aws.String("run-stream")},
				{Message: aws.String(`{"run_id":1234,"job_url":"https://github.com/runs-on/server/actions/runs/1234/job/43","job_id":43,"message":"other"}`), Timestamp: aws.Int64(124), EventId: aws.String("other-event"), LogStreamName: aws.String("run-stream")},
			},
		}, nil
	}
	if aws.ToString(params.LogStreamNamePrefix) != "" {
		message = "agent"
	}
	return &cloudwatchlogs.FilterLogEventsOutput{
		Events: []cwltypes.FilteredLogEvent{
			{
				Message:       aws.String(`{"message":"` + message + `"}`),
				Timestamp:     aws.Int64(123),
				EventId:       aws.String(message + "-event"),
				LogStreamName: aws.String(message + "-stream"),
			},
		},
	}, nil
}

func cloneFilterLogEventsInput(input *cloudwatchlogs.FilterLogEventsInput) *cloudwatchlogs.FilterLogEventsInput {
	if input == nil {
		return nil
	}
	cloned := *input
	return &cloned
}

type mockCloudTrailClient struct {
	inputs []*cloudtrail.LookupEventsInput
}

func (m *mockCloudTrailClient) LookupEvents(ctx context.Context, params *cloudtrail.LookupEventsInput, optFns ...func(*cloudtrail.Options)) (*cloudtrail.LookupEventsOutput, error) {
	m.inputs = append(m.inputs, params)
	return &cloudtrail.LookupEventsOutput{
		Events: []cloudtrailtypes.Event{
			{
				EventName:       aws.String("RunInstances"),
				CloudTrailEvent: aws.String(`{"eventName":"RunInstances"}`),
			},
		},
	}, nil
}

type mockEC2ConsoleClient struct {
	inputs []*ec2.GetConsoleOutputInput
}

type mockMetricsS3Client struct {
	objects    map[string]string
	listInputs []*s3.ListObjectsV2Input
	getInputs  []*s3.GetObjectInput
}

func (m *mockMetricsS3Client) ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	m.listInputs = append(m.listInputs, params)
	output := &s3.ListObjectsV2Output{}
	for key := range m.objects {
		if strings.HasPrefix(key, aws.ToString(params.Prefix)) {
			output.Contents = append(output.Contents, s3types.Object{Key: aws.String(key)})
		}
	}
	return output, nil
}

func (m *mockMetricsS3Client) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	m.getInputs = append(m.getInputs, params)
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(m.objects[aws.ToString(params.Key)]))}, nil
}

func (m *mockEC2ConsoleClient) GetConsoleOutput(ctx context.Context, params *ec2.GetConsoleOutputInput, optFns ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error) {
	m.inputs = append(m.inputs, params)
	now := time.Now()
	return &ec2.GetConsoleOutputOutput{
		Output:    aws.String(base64.StdEncoding.EncodeToString([]byte("console line\n"))),
		Timestamp: &now,
	}, nil
}

func TestWorkflowJobCreatedAtUsesCreatedAtThenUnix(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 30, 0, 0, time.UTC)
	facts := workflowJobFactsFromRecord(workflowJobFactsRecord{
		JobID:         42,
		CreatedAt:     &createdAt,
		CreatedAtUnix: createdAt.Add(-time.Hour).Unix(),
	}, nil)
	got, err := facts.createdAtOrError()
	if err != nil {
		t.Fatalf("createdAtOrError returned error: %v", err)
	}
	if !got.Equal(createdAt) {
		t.Fatalf("expected created_at %s, got %s", createdAt, got)
	}

	facts = workflowJobFactsFromRecord(workflowJobFactsRecord{
		JobID:         43,
		CreatedAtUnix: createdAt.Unix(),
	}, nil)
	got, err = facts.createdAtOrError()
	if err != nil {
		t.Fatalf("createdAtOrError fallback returned error: %v", err)
	}
	if !got.Equal(createdAt) {
		t.Fatalf("expected created_at_unix %s, got %s", createdAt, got)
	}
}

func TestWorkflowJobAttemptedInstanceIDsDeduplicatesSources(t *testing.T) {
	record := workflowJobFactsRecord{
		RunnerName: "runs-on--i-runner--job",
		ActiveAttempt: &struct {
			InstanceID string `dynamodbav:"instance_id"`
		}{InstanceID: "i-active"},
		AttemptHistory: []struct {
			InstanceID string `dynamodbav:"instance_id"`
		}{
			{InstanceID: "i-old"},
			{InstanceID: "i-active"},
		},
	}

	got := strings.Join(workflowJobAttemptedInstanceIDs(record), ",")
	want := "i-active,i-old,i-runner"
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestWorkflowJobAttemptedInstanceIDsPreservesAttemptOrder(t *testing.T) {
	record := workflowJobFactsRecord{
		AttemptHistory: []struct {
			InstanceID string `dynamodbav:"instance_id"`
		}{
			{InstanceID: "i-z-old"},
			{InstanceID: "i-a-new"},
		},
	}

	got := strings.Join(workflowJobAttemptedInstanceIDs(record), ",")
	want := "i-z-old,i-a-new"
	if got != want {
		t.Fatalf("expected attempt order %s, got %s", want, got)
	}
	if current := workflowJobCurrentInstanceID(record); current != "i-a-new" {
		t.Fatalf("expected current instance to use latest attempt i-a-new, got %q", current)
	}
}

func TestFullLogFilterPatterns(t *testing.T) {
	if got := runFilterPattern(1234); !strings.Contains(got, `$.run_id = "1234"`) {
		t.Fatalf("expected run filter, got %q", got)
	}
}

func TestFilterJobLogLinesKeepsFleetIdentityWithoutJobURL(t *testing.T) {
	data := []byte("{\"run_id\":1234,\"scaleset_job_id\":\"fleet-job-opaque\",\"message\":\"Fleet job started\"}\n" +
		"{\"run_id\":1234,\"scaleset_job_id\":\"43\",\"message\":\"Fleet job started\"}\n")
	got := string(filterJobLogLines(data, "https://github.com/runs-on/server/actions/runs/1234/job/42", "42", "fleet-job-opaque", nil))
	if !strings.Contains(got, `"scaleset_job_id":"fleet-job-opaque"`) || strings.Contains(got, `"scaleset_job_id":"43"`) {
		t.Fatalf("unexpected Fleet job log subset: %q", got)
	}
}

func TestLogsCommandFullModeValidationAndFlags(t *testing.T) {
	cmd := NewLogsCmd(&Stack{})
	cmd.SetArgs([]string{"https://github.com/runs-on/server/actions/runs/1234/job/42", "--full", "--watch"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected --full --watch to be rejected")
	}

	if cmd.Flags().Lookup("since") != nil {
		t.Fatal("did not expect job-specific logs command to expose --since")
	}
	if cmd.Flags().Lookup("full") == nil {
		t.Fatal("expected job-specific logs command to expose --full")
	}
}

func TestFetchFullLogsCreatesArchive(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	completedAt := time.Date(2026, 5, 8, 12, 30, 0, 0, time.UTC)
	cwl := &mockCloudWatchLogsClient{}
	trail := &mockCloudTrailClient{}
	ec2Client := &mockEC2ConsoleClient{}
	metricsClient := &mockMetricsS3Client{objects: map[string]string{
		"cache/metrics/v1/runs-on/server/1234/unit-tests/i-active/metrics.jsonl":   "{\"cpu\":42}\n",
		"cache/metrics/v1/runs-on/server/1234/other-job/i-unrelated/metrics.jsonl": "{\"cpu\":99}\n",
	}}
	resolverClient := &mockJobDiagnosticsLambda{
		response: jobDiagnosticsResponse{
			Status:  "found",
			Product: "flex",
			Request: jobDiagnosticsRequest{Owner: "runs-on", Repo: "server", WorkflowRunID: 1234, WorkflowJobID: 42},
			GitHub: jobDiagnosticsGitHub{
				WorkflowJob: &jobDiagnosticsWorkflowJob{
					ID:          42,
					RunID:       1234,
					HTMLURL:     "https://github.com/runs-on/server/actions/runs/1234/job/42",
					RunnerName:  "runs-on--i-runner--job",
					CreatedAt:   createdAt.Format(time.RFC3339),
					CompletedAt: completedAt.Format(time.RFC3339),
				},
			},
			Local: &jobDiagnosticsLocal{
				Source:          "flex_workflow_jobs",
				WorkflowJobID:   42,
				WorkflowRunID:   1234,
				RunnerName:      "runs-on--i-runner--job",
				InstanceIDs:     []string{"i-active", "i-old", "i-runner"},
				Status:          "completed",
				SchedulingState: "completed",
				CreatedAt:       createdAt.Format(time.RFC3339),
				Record:          json.RawMessage(`{"job_id":42,"run_id":1234}`),
			},
		},
	}
	exporter := &fullLogExporter{
		cwl:         cwl,
		resolver:    &jobDiagnosticsResolver{client: resolverClient, functionName: "job-diagnostics"},
		ec2:         ec2Client,
		cloudtrail:  trail,
		s3:          metricsClient,
		cacheBucket: "runs-on-cache",
		stackName:   "runs-on-dev",
		region:      "us-east-1",
		outputs: &StackOutputs{
			ServiceLogGroupName:    "/aws/ecs/runs-on/flexd",
			EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
		},
	}

	t.Chdir(t.TempDir())
	zipPath, err := exporter.Export(context.Background(), "https://github.com/runs-on/server/actions/runs/9999/job/42")
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}

	files := readZipFiles(t, zipPath)
	for _, path := range []string{
		"manifest.json",
		"dynamodb/job-42.ddb.json",
		"diagnostics/resolver.json",
		"server/ecs.jsonl",
		"server/job-42.jsonl",
		"server/run-1234.jsonl",
		"instances/i-active/cloudtrail.json",
		"instances/i-active/console.log",
		"instances/i-active/agent.jsonl",
		"instances/i-active/metrics.jsonl",
		"instances/i-old/cloudtrail.json",
		"instances/i-runner/agent.jsonl",
	} {
		if _, ok := files[path]; !ok {
			t.Fatalf("expected archive to contain %s; files: %v", path, sortedZipFileNames(files))
		}
	}
	if !strings.Contains(files["manifest.json"], `"job_start": "2026-05-08T12:00:00Z"`) ||
		!strings.Contains(files["manifest.json"], `"job_end": "2026-05-08T12:30:00Z"`) ||
		!strings.Contains(files["manifest.json"], `"job_start_source": "github.workflow_job.created_at"`) ||
		!strings.Contains(files["manifest.json"], `"job_end_source": "github.workflow_job.completed_at"`) ||
		!strings.Contains(files["manifest.json"], `"window_start": "2026-05-08T11:55:00Z"`) ||
		!strings.Contains(files["manifest.json"], `"window_end": "2026-05-08T12:40:00Z"`) {
		t.Fatalf("manifest did not contain derived window: %s", files["manifest.json"])
	}
	if !strings.Contains(files["instances/i-active/console.log"], "console line") {
		t.Fatalf("console log missing decoded output: %q", files["instances/i-active/console.log"])
	}
	if files["instances/i-active/metrics.jsonl"] != "{\"cpu\":42}\n" {
		t.Fatalf("unexpected metrics content: %q", files["instances/i-active/metrics.jsonl"])
	}
	if files["server/job-42.jsonl"] != "{\"run_id\":1234,\"job_url\":\"https://github.com/runs-on/server/actions/runs/1234/job/42\",\"job_id\":42,\"message\":\"target\"}\n" {
		t.Fatalf("job logs unexpectedly contain run-wide logs: %q", files["server/job-42.jsonl"])
	}
	if !strings.Contains(files["server/run-1234.jsonl"], `"message":"target"`) || !strings.Contains(files["server/run-1234.jsonl"], `"message":"other"`) {
		t.Fatalf("unexpected run logs: %q", files["server/run-1234.jsonl"])
	}
	if _, ok := files["instances/i-old/metrics.jsonl"]; ok {
		t.Fatal("did not expect an archive entry when metrics are absent")
	}

	if len(cwl.inputs) != 5 {
		t.Fatalf("expected ecs, run, and 3 agent CloudWatch fetches, got %d", len(cwl.inputs))
	}
	if got := aws.ToString(cwl.inputs[0].FilterPattern); got != "" {
		t.Fatalf("expected ECS server logs to use empty filter pattern, got %q", got)
	}
	if got := aws.ToInt64(cwl.inputs[0].StartTime); got != createdAt.Add(-5*time.Minute).UnixMilli() {
		t.Fatalf("expected derived CloudWatch start time, got %d", got)
	}
	if got := aws.ToInt64(cwl.inputs[0].EndTime); got != completedAt.Add(10*time.Minute).UnixMilli() {
		t.Fatalf("expected derived CloudWatch end time, got %d", got)
	}
	if got := aws.ToString(cwl.inputs[1].FilterPattern); got != runFilterPattern(1234) {
		t.Fatalf("expected the only scoped server fetch to use run ID, got %q", got)
	}
	if len(trail.inputs) != 3 {
		t.Fatalf("expected CloudTrail lookup per instance, got %d", len(trail.inputs))
	}
	if len(ec2Client.inputs) != 3 {
		t.Fatalf("expected console output per instance, got %d", len(ec2Client.inputs))
	}
	if len(metricsClient.listInputs) != 1 {
		t.Fatalf("expected one S3 metrics listing, got %d", len(metricsClient.listInputs))
	}
	if got := aws.ToString(metricsClient.listInputs[0].Prefix); got != "cache/metrics/v1/runs-on/server/1234/" {
		t.Fatalf("unexpected metrics prefix %q", got)
	}
	if len(metricsClient.getInputs) != 1 {
		t.Fatalf("expected one matching metrics download, got %d", len(metricsClient.getInputs))
	}
}

func TestCanonicalMetricsRequestUsesResolvedJobURLCasing(t *testing.T) {
	fallback, err := buildJobDiagnosticsRequest("https://github.com/RUNS-ON/SERVER/actions/runs/1234/job/42")
	if err != nil {
		t.Fatalf("buildJobDiagnosticsRequest returned error: %v", err)
	}
	diagnostics := &jobDiagnosticsResponse{GitHub: jobDiagnosticsGitHub{
		WorkflowJob: &jobDiagnosticsWorkflowJob{HTMLURL: "https://github.com/runs-on/server/actions/runs/1234/job/42"},
	}}

	got := canonicalMetricsRequest(diagnostics, fallback)
	if got.Owner != "runs-on" || got.Repo != "server" {
		t.Fatalf("canonical metrics repository = %s/%s, want runs-on/server", got.Owner, got.Repo)
	}
}

func TestCanonicalMetricsRequestFallsBackToResolvedRunAndLocalRecords(t *testing.T) {
	fallback, err := buildJobDiagnosticsRequest("https://github.com/RUNS-ON/SERVER/actions/runs/1234/job/42")
	if err != nil {
		t.Fatalf("buildJobDiagnosticsRequest returned error: %v", err)
	}

	tests := []struct {
		name        string
		diagnostics *jobDiagnosticsResponse
		wantOwner   string
		wantRepo    string
	}{
		{
			name: "workflow run URL",
			diagnostics: &jobDiagnosticsResponse{GitHub: jobDiagnosticsGitHub{
				WorkflowRun: &jobDiagnosticsWorkflowRun{HTMLURL: "https://github.com/runs-on/server/actions/runs/1234"},
			}},
			wantOwner: "runs-on",
			wantRepo:  "server",
		},
		{
			name: "Flex local record",
			diagnostics: &jobDiagnosticsResponse{Local: &jobDiagnosticsLocal{
				Record: json.RawMessage(`{"org_name":"Runs-On","repo_name":"Server"}`),
			}},
			wantOwner: "Runs-On",
			wantRepo:  "Server",
		},
		{
			name: "Fleet local record",
			diagnostics: &jobDiagnosticsResponse{Local: &jobDiagnosticsLocal{
				Record: json.RawMessage(`{"owner_name":"runs-on-demo","repository_name":"Scaleset-Demo"}`),
			}},
			wantOwner: "runs-on-demo",
			wantRepo:  "Scaleset-Demo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalMetricsRequest(tt.diagnostics, fallback)
			if got.Owner != tt.wantOwner || got.Repo != tt.wantRepo {
				t.Fatalf("canonical metrics repository = %s/%s, want %s/%s", got.Owner, got.Repo, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

func TestFullLogWindowFallsBackToLocalTimestamps(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	completedAt := time.Date(2026, 5, 8, 12, 45, 0, 0, time.UTC)
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

	window, err := facts.fullLogWindow()
	if err != nil {
		t.Fatalf("fullLogWindow returned error: %v", err)
	}
	if !window.Start.Equal(createdAt.Add(-5 * time.Minute)) {
		t.Fatalf("window start = %s, want %s", window.Start, createdAt.Add(-5*time.Minute))
	}
	if !window.End.Equal(completedAt.Add(10 * time.Minute)) {
		t.Fatalf("window end = %s, want %s", window.End, completedAt.Add(10*time.Minute))
	}
	if window.JobStartSource != "local.created_at" || window.JobEndSource != "local.completed_at" {
		t.Fatalf("unexpected timestamp sources: %+v", window)
	}
}

func TestFullLogWindowUsesDefaultDurationWithoutCompletion(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	facts := &workflowJobFacts{
		JobID:           42,
		CreatedAt:       createdAt,
		CreatedAtSource: "github.workflow_job.created_at",
	}

	window, err := facts.fullLogWindow()
	if err != nil {
		t.Fatalf("fullLogWindow returned error: %v", err)
	}
	if !window.JobEnd.Equal(createdAt.Add(30 * time.Minute)) {
		t.Fatalf("job end = %s, want %s", window.JobEnd, createdAt.Add(30*time.Minute))
	}
	if !window.End.Equal(createdAt.Add(40 * time.Minute)) {
		t.Fatalf("window end = %s, want %s", window.End, createdAt.Add(40*time.Minute))
	}
	if window.JobEndSource != "github.workflow_job.created_at+30m" {
		t.Fatalf("job end source = %q", window.JobEndSource)
	}
}

func TestFullLogExportRejectsCrossProductDiagnosticsBeforeArchive(t *testing.T) {
	t.Chdir(t.TempDir())

	resolverClient := &mockJobDiagnosticsLambda{
		response: jobDiagnosticsResponse{
			Status:  "partial",
			Product: "fleet",
			Request: jobDiagnosticsRequest{WorkflowRunID: 1234, WorkflowJobID: 42},
			GitHub: jobDiagnosticsGitHub{
				WorkflowJob: &jobDiagnosticsWorkflowJob{
					ID:     42,
					RunID:  1234,
					Labels: []string{"runs-on=123/runner=1cpu-linux-x64/env=dev"},
				},
			},
		},
	}
	exporter := &fullLogExporter{
		resolver:  &jobDiagnosticsResolver{client: resolverClient, functionName: "job-diagnostics"},
		stackName: "runs-on-fleet-dev-v3",
		product:   "fleet",
	}

	zipPath, err := exporter.Export(context.Background(), "https://github.com/runs-on/server/actions/runs/1234/job/42")
	if err == nil {
		t.Fatal("expected cross-product full log export to fail")
	}
	want := `job 42 is a Flex job, but stack "runs-on-fleet-dev-v3" is a Fleet stack`
	if err.Error() != want {
		t.Fatalf("Export error = %v, want %q", err, want)
	}
	if zipPath != "" {
		t.Fatalf("zipPath = %q, want empty before archive creation", zipPath)
	}
	entries, readErr := os.ReadDir(".")
	if readErr != nil {
		t.Fatalf("read temp dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no archive files, got %v", entries)
	}
}

func readZipFiles(t *testing.T, zipPath string) map[string]string {
	t.Helper()

	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer reader.Close()

	files := make(map[string]string)
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open zip file %s: %v", file.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read zip file %s: %v", file.Name, err)
		}
		files[filepath.ToSlash(file.Name)] = string(data)
	}
	return files
}

func sortedZipFileNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
