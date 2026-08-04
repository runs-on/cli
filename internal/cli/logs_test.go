package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

func TestApplicationFilterPatternUsesJobIDByDefault(t *testing.T) {
	facts := &jobFactsProvider{}
	facts.set(&workflowJobFacts{CurrentInstanceID: "i-123"})

	filterPattern, err := jobApplicationFilterPattern("42", facts, nil)
	if err != nil {
		t.Fatalf("applicationFilterPattern returned error: %v", err)
	}
	if !strings.Contains(filterPattern, `$.job_id = "42"`) {
		t.Fatalf("expected job ID filter, got %q", filterPattern)
	}
	if !strings.Contains(filterPattern, `$.workflow_job_id = "42"`) {
		t.Fatalf("expected workflow job ID filter, got %q", filterPattern)
	}
	if !strings.Contains(filterPattern, `$.message = "*i-123*"`) {
		t.Fatalf("expected instance ID message filter, got %q", filterPattern)
	}
}

func TestApplicationFilterPatternDoesNotUseRunIDByDefault(t *testing.T) {
	facts := testJobFactsProvider(jobDiagnosticsResponse{
		Status:  "ambiguous",
		Product: "fleet",
		Request: jobDiagnosticsRequest{WorkflowRunID: 1234, WorkflowJobID: 42},
	})
	if err := facts.refresh(context.Background()); err != nil {
		t.Fatalf("refresh returned error: %v", err)
	}

	filterPattern, err := jobApplicationFilterPattern("42", facts, nil)
	if err != nil {
		t.Fatalf("applicationFilterPattern returned error: %v", err)
	}
	if strings.Contains(filterPattern, `$.run_id = "1234"`) {
		t.Fatalf("did not expect default job filter to include run ID, got %q", filterPattern)
	}
	if !strings.Contains(filterPattern, `$.job_id = "42"`) || !strings.Contains(filterPattern, `$.workflow_job_id = "42"`) {
		t.Fatalf("expected default job filter to include job IDs, got %q", filterPattern)
	}
}

func TestApplicationFilterPatternUsesRunIDWhenRequested(t *testing.T) {
	facts := &jobFactsProvider{}
	facts.set(&workflowJobFacts{
		RunID:             1234,
		CurrentInstanceID: "i-123",
	})

	filterPattern, err := jobApplicationFilterPattern("42", facts, []string{"run"})
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

func TestFleetStreamedLogSessionUsesGitHubRunnerWhenClaimMissing(t *testing.T) {
	cwl := &mockCloudWatchLogsClient{}
	streamer := &jobLogStreamer{
		cwl: cwl,
		outputs: &StackOutputs{
			ServiceLogGroupName:    "/aws/ecs/runs-on/fleetd",
			EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
		},
	}
	facts := testJobFactsProvider(jobDiagnosticsResponse{
		Status:  "partial",
		Product: "fleet",
		Request: jobDiagnosticsRequest{WorkflowRunID: 1234, WorkflowJobID: 42},
		GitHub: jobDiagnosticsGitHub{
			WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "runs-on--i-github--job"},
		},
		Fleet: &jobDiagnosticsFleet{ClaimCount: 75, MatchBasis: "runner_not_found"},
	})

	if err := streamer.Stream(context.Background(), "42", facts, nil, &LogOptions{StartTime: 1, NoColor: true}); err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}

	var sawInstance, sawApplication bool
	for _, input := range cwl.inputs {
		switch {
		case aws.ToString(input.LogStreamNamePrefix) == "i-github/":
			sawInstance = true
		case strings.Contains(aws.ToString(input.FilterPattern), `$.job_id = "42"`):
			sawApplication = strings.Contains(aws.ToString(input.FilterPattern), `$.message = "*i-github*"`)
		}
	}
	if !sawInstance || !sawApplication {
		t.Fatalf("expected GitHub runner-derived instance and application filters, got %#v", cwl.inputs)
	}
}

func TestStreamRejectsCrossProductDiagnosticsBeforeCloudWatch(t *testing.T) {
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
			cwl := &mockCloudWatchLogsClient{}
			streamer := &jobLogStreamer{
				cwl: cwl,
				outputs: &StackOutputs{
					ServiceLogGroupName:    "/aws/ecs/runs-on/flexd",
					EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
				},
			}
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

			err := streamer.Stream(context.Background(), "42", facts, nil, &LogOptions{StartTime: 1, NoColor: true})
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Stream error = %v, want %q", err, tt.want)
			}
			if len(cwl.inputs) != 0 {
				t.Fatalf("expected no CloudWatch fetches before product mismatch error, got %d", len(cwl.inputs))
			}
		})
	}
}

func TestStreamRejectsWrongDiagnosticsStackBeforeCloudWatch(t *testing.T) {
	t.Parallel()

	cwl := &mockCloudWatchLogsClient{}
	streamer := &jobLogStreamer{
		cwl: cwl,
		outputs: &StackOutputs{
			ServiceLogGroupName:    "/aws/ecs/runs-on/fleetd",
			EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
		},
	}
	facts := testJobFactsProvider(jobDiagnosticsResponse{
		Status:    "found",
		Product:   "fleet",
		StackName: "runs-on-fleet-stage-v3",
		Request:   jobDiagnosticsRequest{WorkflowRunID: 1234, WorkflowJobID: 42},
		Local: &jobDiagnosticsLocal{
			Source:        "fleet_claims",
			WorkflowJobID: 42,
			WorkflowRunID: 1234,
			InstanceIDs:   []string{"i-fleet"},
		},
	})
	facts.product = "fleet"
	facts.stackName = "runs-on-fleet-preview-v3"

	err := streamer.Stream(context.Background(), "42", facts, nil, &LogOptions{StartTime: 1, NoColor: true})
	want := `job 42 diagnostics resolved stack "runs-on-fleet-stage-v3", but CLI selected stack "runs-on-fleet-preview-v3"`
	if err == nil || err.Error() != want {
		t.Fatalf("Stream error = %v, want %q", err, want)
	}
	if len(cwl.inputs) != 0 {
		t.Fatalf("expected no CloudWatch fetches before stack mismatch error, got %d", len(cwl.inputs))
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
	facts := testJobFactsProvider(jobDiagnosticsResponse{Status: "not_found", Product: "flex"})
	facts.facts = &workflowJobFacts{CurrentInstanceID: "i-existing", RunID: 1234}

	if err := facts.refresh(context.Background()); err != nil {
		t.Fatalf("refresh returned error: %v", err)
	}
	if got := facts.currentInstanceID(); got != "" {
		t.Fatalf("expected empty instance ID, got %q", got)
	}
	if got := facts.runID(); got != 0 {
		t.Fatalf("expected run ID to be cleared, got %d", got)
	}
}

func TestStreamedLogSessionUsesExpectedJobAndStackFilters(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	cwl := &mockCloudWatchLogsClient{}
	streamer := &jobLogStreamer{
		cwl: cwl,
		outputs: &StackOutputs{
			ServiceLogGroupName:    "/aws/ecs/runs-on/flexd",
			EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
		},
	}
	facts := testJobFactsProvider(jobDiagnosticsResponse{
		Status:  "found",
		Product: "flex",
		GitHub: jobDiagnosticsGitHub{
			WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234},
		},
		Local: &jobDiagnosticsLocal{
			Source:        "flex_workflow_jobs",
			WorkflowJobID: 42,
			WorkflowRunID: 1234,
			InstanceIDs:   []string{"i-active"},
			CreatedAt:     createdAt.Format(time.RFC3339),
		},
	})

	if err := streamer.Stream(context.Background(), "42", facts, nil, &LogOptions{StartTime: 1, NoColor: true}); err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}

	var sawInstance, sawApplication bool
	for _, input := range cwl.inputs {
		switch {
		case aws.ToString(input.LogStreamNamePrefix) == "i-active/":
			sawInstance = true
		case strings.Contains(aws.ToString(input.FilterPattern), `$.job_id = "42"`):
			sawApplication = true
		}
	}
	if !sawInstance || !sawApplication {
		t.Fatalf("expected job stream to register instance and application filters, got %#v", cwl.inputs)
	}

	cwl.inputs = nil
	applicationStreamer := &applicationLogStreamer{
		cwl:     cwl,
		outputs: streamer.outputs,
	}
	if err := applicationStreamer.Stream(context.Background(), &LogOptions{StartTime: 1, NoColor: true}); err != nil {
		t.Fatalf("application Stream returned error: %v", err)
	}
	if len(cwl.inputs) != 1 {
		t.Fatalf("expected one stack application log fetch, got %d", len(cwl.inputs))
	}
	if got := aws.ToString(cwl.inputs[0].FilterPattern); got != "" {
		t.Fatalf("expected stack application logs to use empty filter, got %q", got)
	}
}

func TestJobLogsUseDiagnosticsWindowForNonWatch(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	completedAt := createdAt.Add(30 * time.Minute)
	cwl := &mockCloudWatchLogsClient{}
	streamer := &jobLogStreamer{
		cwl: cwl,
		outputs: &StackOutputs{
			ServiceLogGroupName:    "/aws/ecs/runs-on/flexd",
			EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
		},
	}
	facts := testJobFactsProvider(jobDiagnosticsResponse{
		Status:  "found",
		Product: "flex",
		GitHub: jobDiagnosticsGitHub{
			WorkflowJob: &jobDiagnosticsWorkflowJob{
				ID:          42,
				RunID:       1234,
				CreatedAt:   createdAt.Format(time.RFC3339),
				CompletedAt: completedAt.Format(time.RFC3339),
			},
		},
		Local: &jobDiagnosticsLocal{
			Source:        "flex_workflow_jobs",
			WorkflowJobID: 42,
			WorkflowRunID: 1234,
			InstanceIDs:   []string{"i-active"},
		},
	})

	if err := streamer.Stream(context.Background(), "42", facts, nil, &LogOptions{StartTime: 1, NoColor: true}); err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}

	wantStart := createdAt.Add(-fullLogWindowBefore).UnixMilli()
	wantEnd := completedAt.Add(fullLogWindowAfter).UnixMilli()
	if len(cwl.inputs) == 0 {
		t.Fatal("expected CloudWatch inputs")
	}
	for _, input := range cwl.inputs {
		if got := aws.ToInt64(input.StartTime); got != wantStart {
			t.Fatalf("StartTime = %d, want %d for input %#v", got, wantStart, input)
		}
		if got := aws.ToInt64(input.EndTime); got != wantEnd {
			t.Fatalf("EndTime = %d, want %d for input %#v", got, wantEnd, input)
		}
	}
}

func TestJobLogsMissingDiagnosticsTimestampsPreserveFallbackWindow(t *testing.T) {
	cwl := &mockCloudWatchLogsClient{}
	streamer := &jobLogStreamer{
		cwl: cwl,
		outputs: &StackOutputs{
			ServiceLogGroupName:    "/aws/ecs/runs-on/fleetd",
			EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
		},
	}
	facts := testJobFactsProvider(jobDiagnosticsResponse{
		Status:  "found",
		Product: "fleet",
		GitHub: jobDiagnosticsGitHub{
			WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234},
		},
		Local: &jobDiagnosticsLocal{
			Source:        "fleet_claims",
			WorkflowJobID: 42,
			WorkflowRunID: 1234,
			InstanceIDs:   []string{"i-fleet"},
		},
	})

	if err := streamer.Stream(context.Background(), "42", facts, nil, &LogOptions{StartTime: 99, NoColor: true}); err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	if len(cwl.inputs) == 0 {
		t.Fatal("expected CloudWatch inputs")
	}
	for _, input := range cwl.inputs {
		if got := aws.ToInt64(input.StartTime); got != 99 {
			t.Fatalf("StartTime = %d, want fallback 99 for input %#v", got, input)
		}
		if input.EndTime != nil {
			t.Fatalf("EndTime = %d, want unset for input %#v", aws.ToInt64(input.EndTime), input)
		}
	}
}

func TestJobLogsWatchUsesDiagnosticsStartWithoutEndTime(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	completedAt := createdAt.Add(30 * time.Minute)
	facts := &jobFactsProvider{}
	facts.set(&workflowJobFacts{
		JobID:       42,
		CreatedAt:   createdAt,
		CompletedAt: completedAt,
	})
	opts := &LogOptions{
		Watch:     true,
		StartTime: 99,
		EndTime:   100,
	}

	(&jobLogStreamer{}).applyJobLogWindow(facts, opts)

	if got := opts.StartTime; got != createdAt.Add(-fullLogWindowBefore).UnixMilli() {
		t.Fatalf("StartTime = %d, want diagnostics start", got)
	}
	if opts.EndTime != 0 {
		t.Fatalf("EndTime = %d, want unset for watch", opts.EndTime)
	}
}

func TestFleetStreamedLogSessionUsesClaimFactsAndAttemptedInstances(t *testing.T) {
	cwl := &mockCloudWatchLogsClient{}
	var debug bytes.Buffer
	logger := log.New(&debug, "", 0)
	streamer := &jobLogStreamer{
		cwl:    cwl,
		logger: logger,
		outputs: &StackOutputs{
			ServiceLogGroupName:    "/aws/ecs/runs-on/fleetd",
			EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
		},
	}
	facts := testJobFactsProvider(jobDiagnosticsResponse{
		Status:  "found",
		Product: "fleet",
		GitHub: jobDiagnosticsGitHub{
			WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "runs-on--i-active--job"},
		},
		Local: &jobDiagnosticsLocal{
			Source:          "fleet_claims",
			WorkflowJobID:   42,
			WorkflowRunID:   1234,
			InstanceIDs:     []string{"i-old", "i-active"},
			Status:          "job_claimed",
			SchedulingState: "job_claimed",
		},
	})
	facts.logger = logger

	if err := streamer.Stream(context.Background(), "42", facts, nil, &LogOptions{StartTime: 1, NoColor: true}); err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}

	var sawOldInstance, sawActiveInstance, sawApplication bool
	for _, input := range cwl.inputs {
		switch {
		case aws.ToString(input.LogStreamNamePrefix) == "i-old/":
			sawOldInstance = true
		case aws.ToString(input.LogStreamNamePrefix) == "i-active/":
			sawActiveInstance = true
		case strings.Contains(aws.ToString(input.FilterPattern), `$.job_id = "42"`):
			sawApplication = strings.Contains(aws.ToString(input.FilterPattern), `$.message = "*i-old*"`) &&
				strings.Contains(aws.ToString(input.FilterPattern), `$.message = "*i-active*"`)
		}
	}
	if !sawOldInstance || !sawActiveInstance || !sawApplication {
		t.Fatalf("expected Fleet stream to register all attempted instance and application filters, got %#v", cwl.inputs)
	}

	debugOutput := debug.String()
	for _, want := range []string{
		"Stack product detected: fleet",
		"Job lookup target: job diagnostics resolver Lambda job-diagnostics",
		"Job 42 found in job diagnostics resolver Lambda job-diagnostics",
		"state=job_claimed",
	} {
		if !strings.Contains(debugOutput, want) {
			t.Fatalf("expected debug output to contain %q:\n%s", want, debugOutput)
		}
	}
}

func TestWatchStartsInstanceStreamWhenFactsGainInstanceID(t *testing.T) {
	cwl := &mockCloudWatchLogsClient{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	lateInstanceStreamStarted := make(chan struct{})
	var closeLateInstanceStreamStarted sync.Once
	cwl.onInput = func(input *cloudwatchlogs.FilterLogEventsInput) {
		if aws.ToString(input.LogStreamNamePrefix) != "i-late/" {
			return
		}
		closeLateInstanceStreamStarted.Do(func() {
			close(lateInstanceStreamStarted)
			cancel()
		})
	}

	var mu sync.Mutex
	invocations := 0
	resolverClient := &mockJobDiagnosticsLambda{}
	resolverClient.invoke = func(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
		mu.Lock()
		invocations++
		invocation := invocations
		mu.Unlock()

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
		if invocation > 1 {
			response.Local.InstanceIDs = []string{"i-late"}
			response.Local.Status = "job_claimed"
		}
		payload, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		return &lambda.InvokeOutput{Payload: payload}, nil
	}

	streamer := &jobLogStreamer{
		cwl: cwl,
		outputs: &StackOutputs{
			ServiceLogGroupName:    "/aws/ecs/runs-on/fleetd",
			EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
		},
	}
	facts := &jobFactsProvider{
		product: "fleet",
		resolver: &jobDiagnosticsResolver{
			client:       resolverClient,
			functionName: "job-diagnostics",
		},
		jobID:  "42",
		jobRef: "https://github.com/runs-on/server/actions/runs/1234/job/42",
	}

	err := streamer.Stream(ctx, "42", facts, nil, &LogOptions{
		Watch:         true,
		WatchInterval: 5 * time.Millisecond,
		StartTime:     1,
		NoColor:       true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream returned %v, want context cancellation after late instance stream starts", err)
	}

	select {
	case <-lateInstanceStreamStarted:
	default:
		t.Fatal("expected watch mode to start a CloudWatch stream for the late Fleet instance")
	}
}

func TestNoColorFlagIsLogCommandOnly(t *testing.T) {
	rootHelp := rootCommandHelp(t, "--help")
	if strings.Contains(rootHelp, "--no-color") {
		t.Fatalf("did not expect root help to advertise --no-color:\n%s", rootHelp)
	}

	logsHelp := rootCommandHelp(t, "logs", "--help")
	assertHelpContains(t, logsHelp, "--no-color")
	assertHelpContains(t, logsHelp, "Disable color output for streamed logs")

	stackLogsHelp := rootCommandHelp(t, "stack", "logs", "--help")
	assertHelpContains(t, stackLogsHelp, "--no-color")
	assertHelpContains(t, stackLogsHelp, "Disable color output for streamed logs")
}

func rootCommandHelp(t *testing.T, args ...string) string {
	t.Helper()

	cmd := NewRootCmd(&Stack{})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("command help failed for args %v: %v", args, err)
	}
	return output.String()
}

func assertHelpContains(t *testing.T, help, want string) {
	t.Helper()
	if !strings.Contains(help, want) {
		t.Fatalf("expected help to contain %q:\n%s", want, help)
	}
}
