package cli

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
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
)

type mockCloudWatchLogsClient struct {
	mu      sync.Mutex
	inputs  []*cloudwatchlogs.FilterLogEventsInput
	onInput func(*cloudwatchlogs.FilterLogEventsInput)
}

func (m *mockCloudWatchLogsClient) FilterLogEvents(ctx context.Context, params *cloudwatchlogs.FilterLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	m.mu.Lock()
	m.inputs = append(m.inputs, params)
	onInput := m.onInput
	m.mu.Unlock()

	if onInput != nil {
		onInput(params)
	}
	message := "server"
	if strings.Contains(aws.ToString(params.FilterPattern), "$.run_id") {
		message = "run"
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
	jobPattern := fullJobFilterPattern("42", 1234, []string{"i-1", "i-2"})
	for _, want := range []string{`$.job_id = "42"`, `$.workflow_job_id = "42"`, `$.run_id = "1234"`, `$.message = "*i-1*"`, `$.message = "*i-2*"`} {
		if !strings.Contains(jobPattern, want) {
			t.Fatalf("expected job filter pattern %q to contain %q", jobPattern, want)
		}
	}
	if got := runFilterPattern(1234); !strings.Contains(got, `$.run_id = "1234"`) {
		t.Fatalf("expected run filter, got %q", got)
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
	cwl := &mockCloudWatchLogsClient{}
	trail := &mockCloudTrailClient{}
	ec2Client := &mockEC2ConsoleClient{}
	resolverClient := &mockJobDiagnosticsLambda{
		response: jobDiagnosticsResponse{
			Status:  "found",
			Product: "flex",
			GitHub: jobDiagnosticsGitHub{
				WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "runs-on--i-runner--job"},
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
		cwl:        cwl,
		resolver:   &jobDiagnosticsResolver{client: resolverClient, functionName: "job-diagnostics"},
		ec2:        ec2Client,
		cloudtrail: trail,
		stackName:  "runs-on-dev",
		region:     "us-east-1",
		outputs: &StackOutputs{
			ServiceLogGroupName:    "/aws/ecs/runs-on/flexd",
			EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
		},
	}

	t.Chdir(t.TempDir())
	zipPath, err := exporter.Export(context.Background(), "https://github.com/runs-on/server/actions/runs/1234/job/42")
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}

	files := readZipFiles(t, zipPath)
	for _, path := range []string{
		"manifest.json",
		"dynamodb/job-42.ddb.json",
		"diagnostics/resolver.json",
		"server/job-42.jsonl",
		"server/run-1234.jsonl",
		"instances/i-active/cloudtrail.json",
		"instances/i-active/console.log",
		"instances/i-active/agent.jsonl",
		"instances/i-old/cloudtrail.json",
		"instances/i-runner/agent.jsonl",
	} {
		if _, ok := files[path]; !ok {
			t.Fatalf("expected archive to contain %s; files: %v", path, sortedZipFileNames(files))
		}
	}
	if !strings.Contains(files["manifest.json"], `"window_start": "2026-05-08T11:00:00Z"`) ||
		!strings.Contains(files["manifest.json"], `"window_end": "2026-05-08T13:00:00Z"`) {
		t.Fatalf("manifest did not contain derived window: %s", files["manifest.json"])
	}
	if !strings.Contains(files["instances/i-active/console.log"], "console line") {
		t.Fatalf("console log missing decoded output: %q", files["instances/i-active/console.log"])
	}

	if len(cwl.inputs) != 5 {
		t.Fatalf("expected job, run, and 3 agent CloudWatch fetches, got %d", len(cwl.inputs))
	}
	if got := aws.ToInt64(cwl.inputs[0].StartTime); got != createdAt.Add(-time.Hour).UnixMilli() {
		t.Fatalf("expected derived CloudWatch start time, got %d", got)
	}
	if len(trail.inputs) != 3 {
		t.Fatalf("expected CloudTrail lookup per instance, got %d", len(trail.inputs))
	}
	if len(ec2Client.inputs) != 3 {
		t.Fatalf("expected console output per instance, got %d", len(ec2Client.inputs))
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
