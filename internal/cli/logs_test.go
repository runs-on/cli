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
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
)

func TestApplicationFilterPatternUsesRunIDByDefault(t *testing.T) {
	facts := &jobFactsProvider{}
	facts.set(&workflowJobFacts{RunID: 1234, CurrentInstanceID: "i-123"})

	filterPattern, err := jobApplicationFilterPattern("42", facts)
	if err != nil {
		t.Fatalf("applicationFilterPattern returned error: %v", err)
	}
	if filterPattern != runFilterPattern(1234) {
		t.Fatalf("expected run ID filter, got %q", filterPattern)
	}
	if filterPattern != `{ ( $.run_id = 1234 ) || ( $.run_id = "1234" ) }` {
		t.Fatalf("expected numeric and string run ID filter, got %q", filterPattern)
	}
}

func TestApplicationFilterPatternUsesResolvedRunIDByDefault(t *testing.T) {
	facts := testJobFactsProvider(jobDiagnosticsResponse{
		Status:  "ambiguous",
		Product: "fleet",
		Request: jobDiagnosticsRequest{WorkflowRunID: 1234, WorkflowJobID: 42},
	})
	if err := facts.refresh(context.Background()); err != nil {
		t.Fatalf("refresh returned error: %v", err)
	}

	filterPattern, err := jobApplicationFilterPattern("42", facts)
	if err != nil {
		t.Fatalf("applicationFilterPattern returned error: %v", err)
	}
	if filterPattern != runFilterPattern(1234) {
		t.Fatalf("expected run ID filter, got %q", filterPattern)
	}
}

func TestApplicationFilterPatternOnlyUsesRunID(t *testing.T) {
	facts := &jobFactsProvider{}
	facts.set(&workflowJobFacts{
		RunID:             1234,
		CurrentInstanceID: "i-123",
	})

	filterPattern, err := jobApplicationFilterPattern("42", facts)
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
		{name: "structured instance ID", line: `{"instance_id":"i-123"}`, want: true},
		{name: "legacy instance message", line: `{"message":"launched i-123"}`, want: true},
		{name: "other job", line: `{"job_url":"https://github.com/runs-on/server/actions/runs/1234/job/43","job_id":43}`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jobLogLineMatches(tt.line, jobURL, "42", "fleet-job-opaque", instanceIDs); got != tt.want {
				t.Fatalf("jobLogLineMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStreamCloudWatchLogsFiltersRunLinesLocally(t *testing.T) {
	cwl := &mockCloudWatchLogsClient{}
	session := newStreamedLogSession(&LogOptions{NoColor: true}, nil)
	accept := func(line string) bool {
		return jobLogLineMatches(line, "https://github.com/runs-on/server/actions/runs/1234/job/42", "42", "", nil)
	}
	err := session.streamCloudWatchLogs(context.Background(), "application", cwl, func(input *cloudwatchlogs.FilterLogEventsInput) error {
		input.FilterPattern = aws.String(runFilterPattern(1234))
		return nil
	}, accept, nil, 0, func() {})
	if err != nil {
		t.Fatalf("streamCloudWatchLogs returned error: %v", err)
	}
	if len(session.collector.events) != 1 || !strings.Contains(session.collector.events[0].message, `"job_id":42`) {
		t.Fatalf("expected only the selected job line, got %+v", session.collector.events)
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
	session := newStreamedLogSession(&LogOptions{StartTime: 100, Watch: true, WatchInterval: time.Millisecond, NoColor: true}, nil)
	accept := func(line string) bool {
		matched := jobLogLineMatches(line, "", "42", scalesetJobID, nil)
		if !matched {
			scalesetJobID = "fleet-job-opaque"
		}
		return matched
	}
	err := session.streamCloudWatchLogs(ctx, "application", cwl, func(input *cloudwatchlogs.FilterLogEventsInput) error {
		applyLogTimeBounds(input, session.opts)
		return nil
	}, accept, func() string {
		return jobLogCorrelationKey("", scalesetJobID, nil)
	}, cloudWatchWatchReplayOverlap, func() {})
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

func TestStreamCloudWatchLogsOverlapsWatchCursorForLateEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	startTimes := make([]int64, 0, 2)
	cwl := &mockCloudWatchLogsClient{}
	cwl.output = func(input *cloudwatchlogs.FilterLogEventsInput) (*cloudwatchlogs.FilterLogEventsOutput, error) {
		calls++
		startTimes = append(startTimes, aws.ToInt64(input.StartTime))
		if calls == 1 {
			return &cloudwatchlogs.FilterLogEventsOutput{Events: []cwltypes.FilteredLogEvent{{
				Message:   aws.String(`{"job_id":43,"message":"other"}`),
				Timestamp: aws.Int64(120000),
				EventId:   aws.String("other-event"),
			}}}, nil
		}
		cancel()
		return &cloudwatchlogs.FilterLogEventsOutput{Events: []cwltypes.FilteredLogEvent{{
			Message:   aws.String(`{"job_id":42,"message":"late target"}`),
			Timestamp: aws.Int64(119000),
			EventId:   aws.String("target-event"),
		}}}, nil
	}
	session := newStreamedLogSession(&LogOptions{StartTime: 100, Watch: true, WatchInterval: time.Millisecond, NoColor: true}, nil)
	err := session.streamCloudWatchLogs(ctx, "application", cwl, func(input *cloudwatchlogs.FilterLogEventsInput) error {
		applyLogTimeBounds(input, session.opts)
		return nil
	}, func(line string) bool {
		return jobLogLineMatches(line, "", "42", "", nil)
	}, nil, cloudWatchWatchReplayOverlap, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("streamCloudWatchLogs returned %v, want context cancellation", err)
	}
	if len(startTimes) != 2 || startTimes[0] != 100 || startTimes[1] != 60000 {
		t.Fatalf("expected one-minute watch overlap, got start times %v", startTimes)
	}
	if len(session.collector.events) != 1 || session.collector.events[0].eventId != "target-event" {
		t.Fatalf("expected late target event from overlap, got %+v", session.collector.events)
	}
}

func TestStreamCloudWatchLogsOverlapsWatchCursorAfterEmptyPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	targetTimestamp := time.Now().Add(-30 * time.Second).UnixMilli()
	startTimes := make([]int64, 0, 2)
	cwl := &mockCloudWatchLogsClient{}
	cwl.output = func(input *cloudwatchlogs.FilterLogEventsInput) (*cloudwatchlogs.FilterLogEventsOutput, error) {
		startTimes = append(startTimes, aws.ToInt64(input.StartTime))
		if len(startTimes) == 1 {
			return &cloudwatchlogs.FilterLogEventsOutput{}, nil
		}
		cancel()
		return &cloudwatchlogs.FilterLogEventsOutput{Events: []cwltypes.FilteredLogEvent{{
			Message:   aws.String(`{"job_id":42,"message":"late target"}`),
			Timestamp: aws.Int64(targetTimestamp),
			EventId:   aws.String("target-event"),
		}}}, nil
	}
	session := newStreamedLogSession(&LogOptions{
		StartTime:     targetTimestamp - 120000,
		Watch:         true,
		WatchInterval: time.Millisecond,
		NoColor:       true,
	}, nil)
	err := session.streamCloudWatchLogs(ctx, "application", cwl, func(input *cloudwatchlogs.FilterLogEventsInput) error {
		applyLogTimeBounds(input, session.opts)
		return nil
	}, func(line string) bool {
		return jobLogLineMatches(line, "", "42", "", nil)
	}, nil, cloudWatchWatchReplayOverlap, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("streamCloudWatchLogs returned %v, want context cancellation", err)
	}
	if len(startTimes) != 2 || startTimes[1] > targetTimestamp {
		t.Fatalf("expected empty poll to retain watch overlap for timestamp %d, got %v", targetTimestamp, startTimes)
	}
	if len(session.collector.events) != 1 || session.collector.events[0].eventId != "target-event" {
		t.Fatalf("expected late target event after empty poll, got %+v", session.collector.events)
	}
}

func TestStreamCloudWatchLogsDoesNotOverlapOrdinaryWatchCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTimes := make([]int64, 0, 2)
	cwl := &mockCloudWatchLogsClient{}
	cwl.output = func(input *cloudwatchlogs.FilterLogEventsInput) (*cloudwatchlogs.FilterLogEventsOutput, error) {
		startTimes = append(startTimes, aws.ToInt64(input.StartTime))
		if len(startTimes) == 2 {
			cancel()
			return &cloudwatchlogs.FilterLogEventsOutput{}, nil
		}
		return &cloudwatchlogs.FilterLogEventsOutput{Events: []cwltypes.FilteredLogEvent{{
			Message:   aws.String(`{"message":"stack event"}`),
			Timestamp: aws.Int64(120000),
			EventId:   aws.String("stack-event"),
		}}}, nil
	}
	session := newStreamedLogSession(&LogOptions{StartTime: 100, Watch: true, WatchInterval: time.Millisecond, NoColor: true}, nil)
	err := session.streamCloudWatchLogs(ctx, "application", cwl, func(input *cloudwatchlogs.FilterLogEventsInput) error {
		applyLogTimeBounds(input, session.opts)
		return nil
	}, nil, nil, 0, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("streamCloudWatchLogs returned %v, want context cancellation", err)
	}
	if len(startTimes) != 2 || startTimes[0] != 100 || startTimes[1] != 120001 {
		t.Fatalf("expected ordinary watch cursor to advance without overlap, got %v", startTimes)
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
	streamer := &jobLogStreamer{
		cwl: cwl,
		outputs: &StackOutputs{
			ServiceLogGroupName: "/aws/ecs/runs-on/flexd",
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
			CreatedAt:     createdAt.Format(time.RFC3339Nano),
		},
	})
	err := streamer.Stream(ctx, "42", facts, []string{"run"}, &LogOptions{Watch: true, WatchInterval: time.Millisecond, NoColor: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream returned %v, want context cancellation", err)
	}
	if len(startTimes) != 2 || startTimes[1] != eventTimestamp+1 {
		t.Fatalf("expected --include=run watch cursor without overlap, got %v", startTimes)
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
		case aws.ToString(input.FilterPattern) == runFilterPattern(1234):
			sawApplication = true
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
		case aws.ToString(input.FilterPattern) == runFilterPattern(1234):
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
		case aws.ToString(input.FilterPattern) == runFilterPattern(1234):
			sawApplication = true
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
