package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

func TestJobLogLineMatchesCanonicalAndFallbackIdentities(t *testing.T) {
	jobURL := "https://github.com/runs-on/server/actions/runs/1234/job/42"
	instanceIDs := []string{"i-123"}
	tests := []struct {
		name string
		line string
		want bool
	}{
		{name: "job URL", line: `{"job_url":"https://github.com/runs-on/server/actions/runs/1234/job/42"}`, want: true},
		{name: "Flex job ID", line: `{"job_id":42}`, want: true},
		{name: "workflow job ID string", line: `{"workflow_job_id":"42"}`, want: true},
		{name: "Fleet scaleset job ID", line: `{"scaleset_job_id":"fleet-job-opaque"}`, want: true},
		{name: "scaleset job ID naming the workflow job", line: `{"scaleset_job_id":"42"}`, want: true},
		{name: "other scaleset job", line: `{"scaleset_job_id":"43"}`, want: false},
		{name: "structured instance ID", line: `{"instance_id":"i-123"}`, want: true},
		{name: "legacy instance message", line: `{"message":"launched i-123"}`, want: true},
		{name: "other job", line: `{"job_url":"https://github.com/runs-on/server/actions/runs/1234/job/43","job_id":43}`, want: false},
		{name: "job URL in another case with a trailing slash", line: `{"job_url":"HTTPS://GITHUB.COM/runs-on/server/actions/runs/1234/job/42/"}`, want: true},
		{name: "plain text job URL", line: "started https://github.com/runs-on/server/actions/runs/1234/job/42", want: true},
		{name: "plain text instance ID", line: "launched i-123", want: true},
		{name: "plain text naming neither", line: "launched i-456", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jobLogLineMatches(tt.line, jobURL, "42", "fleet-job-opaque", instanceIDs); got != tt.want {
				t.Fatalf("jobLogLineMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStreamCloudWatchLogsReplaysAfterFleetIdentityResolves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scalesetJobID := ""
	calls := 0
	startTimes := make([]int64, 0, 2)
	cwl := &mockCloudWatchLogsClient{}
	cwl.output = func(input *cloudwatchlogs.FilterLogEventsInput) (*cloudwatchlogs.FilterLogEventsOutput, error) {
		calls++
		startTimes = append(startTimes, aws.ToInt64(input.StartTime))
		if calls == 2 {
			cancel()
		}
		return &cloudwatchlogs.FilterLogEventsOutput{Events: []cwltypes.FilteredLogEvent{{
			Message:       aws.String(`{"scaleset_job_id":"fleet-job-opaque","message":"queued"}`),
			Timestamp:     aws.Int64(120000),
			EventId:       aws.String("fleet-event"),
			LogStreamName: aws.String("run-stream"),
		}}}, nil
	}
	session := newStreamedLogSession(&LogOptions{StartTime: 100, Watch: true, WatchInterval: time.Millisecond, NoColor: true}, discardLogger, io.Discard, io.Discard)
	accept := func(line string) bool {
		matched := jobLogLineMatches(line, "", "42", scalesetJobID, nil)
		if !matched {
			scalesetJobID = "fleet-job-opaque"
		}
		return matched
	}
	err := session.streamCloudWatchLogs(ctx, cwl, cloudWatchStream{
		prefix: "application",
		update: func(input *cloudwatchlogs.FilterLogEventsInput) error {
			applyLogTimeBounds(input, session.opts)
			return nil
		},
		accept: accept,
		correlationKey: func() string {
			return jobLogCorrelationKey("", scalesetJobID, nil)
		},
	}, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("streamCloudWatchLogs returned %v, want context cancellation", err)
	}
	if len(startTimes) != 2 || startTimes[0] != 100 || startTimes[1] != 100 {
		t.Fatalf("expected identity change to replay initial start time, got %v", startTimes)
	}
	if len(session.collector.events) != 1 || session.collector.events[0].eventId != "fleet-event" {
		t.Fatalf("expected replayed Fleet event after identity resolution, got %+v", session.collector.events)
	}
}

// A filtered watch stream re-reads the last minute for late events, even after
// an empty poll, and keeps only its job's events. An unfiltered stream resumes
// right after the last event.
func TestStreamCloudWatchLogsWatchCursor(t *testing.T) {
	event := func(id, message string, timestamp int64) cwltypes.FilteredLogEvent {
		return cwltypes.FilteredLogEvent{EventId: aws.String(id), Message: aws.String(message), Timestamp: aws.Int64(timestamp)}
	}
	late := time.Now().Add(-30 * time.Second).UnixMilli()
	tests := []struct {
		name       string
		filtered   bool
		startTime  int64
		polls      [2][]cwltypes.FilteredLogEvent
		wantStarts [2]int64
		wantEvent  string
	}{
		{
			name:       "filtered: late event",
			filtered:   true,
			startTime:  100,
			polls:      [2][]cwltypes.FilteredLogEvent{{event("other-event", `{"job_id":43}`, 120000)}, {event("target-event", `{"job_id":42}`, 119000)}},
			wantStarts: [2]int64{100, 60000},
			wantEvent:  "target-event",
		},
		{
			name:       "filtered: late event after an empty poll",
			filtered:   true,
			startTime:  late - 10000,
			polls:      [2][]cwltypes.FilteredLogEvent{nil, {event("target-event", `{"job_id":42}`, late)}},
			wantStarts: [2]int64{late - 10000, late - 10000},
			wantEvent:  "target-event",
		},
		{
			name:       "unfiltered",
			startTime:  100,
			polls:      [2][]cwltypes.FilteredLogEvent{{event("stack-event", `{"message":"stack event"}`, 120000)}, nil},
			wantStarts: [2]int64{100, 120001},
			wantEvent:  "stack-event",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var starts []int64
			cwl := &mockCloudWatchLogsClient{}
			cwl.output = func(input *cloudwatchlogs.FilterLogEventsInput) (*cloudwatchlogs.FilterLogEventsOutput, error) {
				starts = append(starts, aws.ToInt64(input.StartTime))
				if len(starts) == 2 {
					cancel()
				}
				return &cloudwatchlogs.FilterLogEventsOutput{Events: tt.polls[len(starts)-1]}, nil
			}
			session := newStreamedLogSession(&LogOptions{StartTime: tt.startTime, Watch: true, WatchInterval: time.Millisecond, NoColor: true}, discardLogger, io.Discard, io.Discard)
			stream := cloudWatchStream{prefix: "application", update: func(input *cloudwatchlogs.FilterLogEventsInput) error {
				applyLogTimeBounds(input, session.opts)
				return nil
			}}
			if tt.filtered {
				stream.accept = func(line string) bool { return jobLogLineMatches(line, "", "42", "", nil) }
			}

			if err := session.streamCloudWatchLogs(ctx, cwl, stream, func() {}); !errors.Is(err, context.Canceled) {
				t.Fatalf("streamCloudWatchLogs returned %v, want context cancellation", err)
			}
			if len(starts) != 2 || [2]int64(starts) != tt.wantStarts {
				t.Fatalf("start times = %v, want %v", starts, tt.wantStarts)
			}
			if len(session.collector.events) != 1 || session.collector.events[0].eventId != tt.wantEvent {
				t.Fatalf("expected only %s, got %+v", tt.wantEvent, session.collector.events)
			}
		})
	}
}

// requestScopedAPIError is shaped like an AWS API error, whose text carries a
// different request ID on every call.
type requestScopedAPIError struct{ requestID int }

func (e requestScopedAPIError) Error() string {
	return fmt.Sprintf("operation error CloudWatch Logs: FilterLogEvents, RequestID: %d, api error AccessDeniedException: not authorized", e.requestID)
}
func (requestScopedAPIError) ErrorCode() string    { return "AccessDeniedException" }
func (requestScopedAPIError) ErrorMessage() string { return "not authorized" }

func TestStreamedLogSessionReportsFailedSource(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprintf("watch=%t", watch), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			calls := 0
			denied := &mockCloudWatchLogsClient{}
			denied.output = func(*cloudwatchlogs.FilterLogEventsInput) (*cloudwatchlogs.FilterLogEventsOutput, error) {
				// Watch mode re-polls; stop after the same failure repeats.
				calls++
				if calls == 3 {
					cancel()
				}
				return nil, requestScopedAPIError{requestID: calls}
			}
			var stdout, stderr bytes.Buffer
			session := newStreamedLogSession(&LogOptions{StartTime: 1, Watch: watch, WatchInterval: time.Millisecond, NoColor: true}, discardLogger, &stdout, &stderr)
			updateInput := func(input *cloudwatchlogs.FilterLogEventsInput) error {
				applyLogTimeBounds(input, session.opts)
				return nil
			}
			session.start(ctx, denied, cloudWatchStream{prefix: "instance i-denied", update: updateInput})
			session.start(ctx, &mockCloudWatchLogsClient{events: runLogEvents()}, cloudWatchStream{prefix: "application", update: updateInput})

			err := session.drainAndWatch(ctx)

			if watch && !errors.Is(err, context.Canceled) {
				t.Fatalf("drainAndWatch returned %v, want to keep watching until cancelled", err)
			}
			if !watch && (err == nil || errors.Is(err, context.Canceled)) {
				t.Fatalf("drainAndWatch returned %v, want an incomplete-output error", err)
			}
			if warnings := stderr.String(); strings.Count(warnings, "\n") != 1 || !strings.Contains(warnings, "instance i-denied") {
				t.Fatalf("expected one warning naming the failed source, got %q", warnings)
			}
			if !strings.Contains(stdout.String(), `"message":"target"`) {
				t.Fatalf("expected the application events despite the failed source, got %q", stdout.String())
			}
		})
	}
}

func TestJobLogsIncludeRunWatchUsesOrdinaryCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	createdAt := time.Now().Add(-time.Minute).UTC()
	eventTimestamp := createdAt.UnixMilli()
	startTimes := make([]int64, 0, 2)
	cwl := &mockCloudWatchLogsClient{}
	cwl.output = func(input *cloudwatchlogs.FilterLogEventsInput) (*cloudwatchlogs.FilterLogEventsOutput, error) {
		startTimes = append(startTimes, aws.ToInt64(input.StartTime))
		if len(startTimes) == 2 {
			cancel()
			return &cloudwatchlogs.FilterLogEventsOutput{}, nil
		}
		return &cloudwatchlogs.FilterLogEventsOutput{Events: []cwltypes.FilteredLogEvent{{
			Message:   aws.String(`{"run_id":1234,"message":"run event"}`),
			Timestamp: aws.Int64(eventTimestamp),
			EventId:   aws.String("run-event"),
		}}}, nil
	}
	streamer := testLogStreamer(cwl, &RunsOnConfig{
		ServiceLogGroupName: "/aws/ecs/runs-on/flexd",
	})
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
			CreatedAt:     createdAt.Format(time.RFC3339Nano),
		},
	})
	err := streamer.StreamJob(ctx, facts, []string{"run"}, &LogOptions{Watch: true, WatchInterval: time.Millisecond, NoColor: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream returned %v, want context cancellation", err)
	}
	if len(startTimes) != 2 || startTimes[1] != eventTimestamp+1 {
		t.Fatalf("expected --include=run watch cursor without overlap, got %v", startTimes)
	}
}

// A job's logs stream every instance the job used and its run's application
// logs, all within the job's window when it has timestamps.
func TestJobLogsStreamInstanceAndRunLogs(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	completedAt := createdAt.Add(30 * time.Minute)
	tests := []struct {
		name          string
		response      jobDiagnosticsResponse
		wantInstances []string
		wantWindow    [2]int64 // start and end of every CloudWatch read
	}{
		{
			name: "flex local record",
			response: jobDiagnosticsResponse{
				Status:  "found",
				Product: "flex",
				GitHub: jobDiagnosticsGitHub{WorkflowJob: &jobDiagnosticsWorkflowJob{
					ID:          42,
					RunID:       1234,
					CreatedAt:   createdAt.Format(time.RFC3339),
					CompletedAt: completedAt.Format(time.RFC3339),
				}},
				Local: &jobDiagnosticsLocal{Source: "flex_workflow_jobs", InstanceIDs: []string{"i-active"}, Status: "running", SchedulingState: "launched"},
			},
			wantInstances: []string{"i-active"},
			wantWindow:    [2]int64{createdAt.Add(-fullLogWindowBefore).UnixMilli(), completedAt.Add(fullLogWindowAfter).UnixMilli()},
		},
		{
			name: "fleet claim missing, GitHub runner",
			response: jobDiagnosticsResponse{
				Status:  "partial",
				Product: "fleet",
				Request: jobDiagnosticsRequest{WorkflowRunID: 1234, WorkflowJobID: 42},
				GitHub:  jobDiagnosticsGitHub{WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "runs-on--i-github--job"}},
				Fleet:   &jobDiagnosticsFleet{ClaimCount: 75, MatchBasis: "runner_not_found"},
			},
			wantInstances: []string{"i-github"},
			wantWindow:    [2]int64{1, 0},
		},
		{
			name: "fleet claim with attempted instances",
			response: jobDiagnosticsResponse{
				Status:  "found",
				Product: "fleet",
				GitHub:  jobDiagnosticsGitHub{WorkflowJob: &jobDiagnosticsWorkflowJob{ID: 42, RunID: 1234, RunnerName: "runs-on--i-active--job"}},
				Local:   &jobDiagnosticsLocal{Source: "fleet_claims", InstanceIDs: []string{"i-old", "i-active"}, Status: "job_claimed", SchedulingState: "job_claimed"},
			},
			wantInstances: []string{"i-old", "i-active"},
			wantWindow:    [2]int64{1, 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwl := &mockCloudWatchLogsClient{}
			streamer := testLogStreamer(cwl, &RunsOnConfig{
				ServiceLogGroupName:    "/aws/ecs/runs-on/server",
				EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
			})
			facts := testJobFactsProvider(tt.response)

			if err := streamer.StreamJob(context.Background(), facts, nil, &LogOptions{StartTime: 1, NoColor: true}); err != nil {
				t.Fatalf("Stream returned error: %v", err)
			}

			var instances []string
			sawApplication := false
			for _, input := range cwl.inputs {
				if window := [2]int64{aws.ToInt64(input.StartTime), aws.ToInt64(input.EndTime)}; window != tt.wantWindow {
					t.Fatalf("read window = %v, want %v for input %#v", window, tt.wantWindow, input)
				}
				if prefix := aws.ToString(input.LogStreamNamePrefix); prefix != "" {
					instances = append(instances, strings.TrimSuffix(prefix, "/"))
				} else if aws.ToString(input.FilterPattern) == runFilterPattern(1234) {
					sawApplication = true
				}
			}
			slices.Sort(instances)
			if want := slices.Sorted(slices.Values(tt.wantInstances)); !slices.Equal(instances, want) || !sawApplication {
				t.Fatalf("streamed instances %v and application logs %t, want %v and true", instances, sawApplication, want)
			}
		})
	}
}

func TestRefreshJobLookupHandlesMissingRow(t *testing.T) {
	facts := testJobFactsProvider(jobDiagnosticsResponse{Status: "not_found", Product: "flex"})
	facts.facts = &workflowJobFacts{Found: true, CurrentInstanceID: "i-existing", RunID: 1234}

	if err := facts.refresh(context.Background()); err != nil {
		t.Fatalf("refresh returned error: %v", err)
	}
	if got := facts.current(); got.Found || got.CurrentInstanceID != "" || got.RunID != 0 {
		t.Fatalf("expected the missing row to clear the facts, got %+v", got)
	}
}

func TestStackLogsStreamAllApplicationLogs(t *testing.T) {
	cwl := &mockCloudWatchLogsClient{}
	streamer := testLogStreamer(cwl, &RunsOnConfig{ServiceLogGroupName: "/aws/ecs/runs-on/flexd"})
	if err := streamer.StreamStack(context.Background(), &LogOptions{StartTime: 1, NoColor: true}); err != nil {
		t.Fatalf("application Stream returned error: %v", err)
	}
	if len(cwl.inputs) != 1 || aws.ToString(cwl.inputs[0].FilterPattern) != "" {
		t.Fatalf("expected one unfiltered stack application log fetch, got %#v", cwl.inputs)
	}
}

// Watch mode starts at the job's window but keeps reading past its end.
func TestApplyJobLogWindowInWatchMode(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	facts := &workflowJobFacts{Found: true, CreatedAt: createdAt, CreatedAtSource: "local.created_at", CompletedAt: createdAt.Add(30 * time.Minute)}
	opts := &LogOptions{Watch: true, StartTime: 99, EndTime: 100}

	testLogStreamer(nil, nil).applyJobLogWindow(facts, 42, opts)

	if opts.StartTime != createdAt.Add(-fullLogWindowBefore).UnixMilli() || opts.EndTime != 0 {
		t.Fatalf("window = %d..%d, want the job's start and no end", opts.StartTime, opts.EndTime)
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

	streamer := testLogStreamer(cwl, &RunsOnConfig{
		ServiceLogGroupName:    "/aws/ecs/runs-on/fleetd",
		EC2InstanceLogGroupArn: "arn:aws:logs:us-east-1:123456789012:log-group:runs-on/ec2/instances",
	})
	facts := &jobFactsProvider{
		product: "fleet",
		resolver: &jobDiagnosticsResolver{
			client:       resolverClient,
			functionName: "job-diagnostics",
		},
		job:    testJob,
		logger: discardLogger,
	}

	err := streamer.StreamJob(ctx, facts, nil, &LogOptions{
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

func TestLogColorDisabled(t *testing.T) {
	tests := []struct {
		name     string
		flag     bool
		env      string
		terminal bool
		want     bool
	}{
		{name: "terminal", terminal: true, want: false},
		{name: "redirected stdout", terminal: false, want: true},
		{name: "NO_COLOR set", env: "1", terminal: true, want: true},
		{name: "--no-color", flag: true, terminal: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := logColorDisabled(tt.flag, tt.env, tt.terminal); got != tt.want {
				t.Fatalf("logColorDisabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// testLogStreamer streams from cwl and discards its output.
func testLogStreamer(cwl cloudWatchLogsAPI, config *RunsOnConfig) *logStreamer {
	return &logStreamer{cwl: cwl, config: config, logger: discardLogger, stdout: io.Discard, stderr: io.Discard}
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
