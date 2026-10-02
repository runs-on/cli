package cli

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
)

type LogOptions struct {
	Watch         bool
	WatchInterval time.Duration
	StartTime     int64
	EndTime       int64
	Format        string
	NoColor       bool
}

const cloudWatchWatchReplayOverlap = time.Minute

type cloudWatchLogsAPI interface {
	FilterLogEvents(ctx context.Context, params *cloudwatchlogs.FilterLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error)
}

type ec2ConsoleAPI interface {
	GetConsoleOutput(ctx context.Context, params *ec2.GetConsoleOutputInput, optFns ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error)
}

// logStreamer prints a stack's or a job's logs.
type logStreamer struct {
	cwl    cloudWatchLogsAPI
	ec2    ec2ConsoleAPI
	config *RunsOnConfig
	logger *log.Logger
	stdout io.Writer
	stderr io.Writer
}

func newLogStreamer(cmd *cobra.Command, config *RunsOnConfig) *logStreamer {
	return &logStreamer{
		cwl:    cloudwatchlogs.NewFromConfig(config.AWSConfig),
		ec2:    ec2.NewFromConfig(config.AWSConfig),
		config: config,
		logger: debugLogger(cmd),
		stdout: cmd.OutOrStdout(),
		stderr: cmd.ErrOrStderr(),
	}
}

func (s *logStreamer) StreamJob(ctx context.Context, facts *jobFactsProvider, includeTypes []string, opts *LogOptions) error {
	jobID := strconv.FormatInt(facts.job.JobID, 10)
	s.logger.Printf("Fetching logs for job ID: %s (include types: %v)", jobID, includeTypes)

	if err := facts.refresh(ctx); err != nil {
		return err
	}
	s.applyJobLogWindow(facts.current(), facts.job.JobID, opts)
	facts.logLookupSnapshot()
	facts.currentDiagnostics().writeSummary(s.stderr)

	session := newStreamedLogSession(opts, s.logger, s.stdout, s.stderr)
	if current := facts.current(); len(current.InstanceIDs) == 0 && current.Found {
		fmt.Fprintf(s.stderr, "Instance logs are not available yet for job %s; streaming application logs.\n", jobID)
	}
	streamed := make(map[string]bool)
	streamNewInstances := func() {
		for _, instanceID := range facts.current().InstanceIDs {
			if streamed[instanceID] {
				continue
			}
			streamed[instanceID] = true
			session.start(ctx, s.cwl, cloudWatchStream{prefix: "instance " + instanceID, update: func(input *cloudwatchlogs.FilterLogEventsInput) error {
				input.LogGroupIdentifier = &s.config.EC2InstanceLogGroupArn
				input.FilterPattern = aws.String("")
				applyLogTimeBounds(input, opts)
				input.LogStreamNamePrefix = aws.String(instanceID + "/")
				s.logger.Printf("Streaming instance logs with arn: %s, prefix: %s", *input.LogGroupIdentifier, *input.LogStreamNamePrefix)
				return nil
			}})
		}
	}
	streamNewInstances()
	if opts.Watch {
		go func() {
			ticker := time.NewTicker(opts.WatchInterval)
			defer ticker.Stop()
			facts.refreshUntilCompleted(ctx, ticker.C, func() {
				// A queued Fleet job can gain an instance after the initial
				// drain; its stream then delivers through the live channel.
				select {
				case <-ctx.Done():
				case <-session.drained:
					streamNewInstances()
				}
			})
		}()
	}
	if slices.Contains(includeTypes, "console") {
		session.startOnce("console", func(collector *logCollector) error {
			return s.collectConsoleLogs(ctx, facts, collector, opts)
		})
	}
	application := cloudWatchStream{prefix: "application", update: func(input *cloudwatchlogs.FilterLogEventsInput) error {
		input.LogGroupIdentifier = aws.String(s.config.ServiceLogGroupName)
		applyLogTimeBounds(input, opts)
		runID := facts.current().RunID
		if runID == 0 {
			return fmt.Errorf("workflow run ID for job %s not available yet", jobID)
		}
		input.FilterPattern = aws.String(runFilterPattern(runID))
		s.logger.Printf("Filter pattern: %s", *input.FilterPattern)
		return nil
	}}
	if !slices.Contains(includeTypes, "run") {
		application.accept = func(message string) bool {
			f := facts.current()
			return jobLogLineMatches(message, f.JobURL, jobID, f.ScalesetJobID, f.InstanceIDs)
		}
		application.correlationKey = func() string {
			f := facts.current()
			return jobLogCorrelationKey(f.JobURL, f.ScalesetJobID, f.InstanceIDs)
		}
	}
	session.start(ctx, s.cwl, application)
	return session.drainAndWatch(ctx)
}

// applyJobLogWindow bounds a job's logs by its timestamps; watch mode keeps
// reading past the end.
func (s *logStreamer) applyJobLogWindow(facts *workflowJobFacts, jobID int64, opts *LogOptions) {
	if !facts.Found {
		return
	}
	window, err := facts.fullLogWindow(jobID)
	if err != nil {
		s.logger.Printf("Job timestamps unavailable, using fallback log start time: %v", err)
		return
	}
	opts.StartTime = window.Start.UnixMilli()
	if opts.Watch {
		opts.EndTime = 0
		return
	}
	opts.EndTime = window.End.UnixMilli()
}

// StreamStack streams all of the stack's application logs.
func (s *logStreamer) StreamStack(ctx context.Context, opts *LogOptions) error {
	session := newStreamedLogSession(opts, s.logger, s.stdout, s.stderr)
	session.start(ctx, s.cwl, cloudWatchStream{prefix: "application", update: func(input *cloudwatchlogs.FilterLogEventsInput) error {
		input.LogGroupIdentifier = aws.String(s.config.ServiceLogGroupName)
		input.FilterPattern = aws.String("")
		applyLogTimeBounds(input, opts)
		return nil
	}})
	return session.drainAndWatch(ctx)
}

type jobLogFields struct {
	JobURL        string          `json:"job_url"`
	JobID         json.RawMessage `json:"job_id"`
	WorkflowJobID json.RawMessage `json:"workflow_job_id"`
	ScalesetJobID json.RawMessage `json:"scaleset_job_id"`
	InstanceID    string          `json:"instance_id"`
	Message       string          `json:"message"`
}

func jobLogLineMatches(line, jobURL, jobID, scalesetJobID string, instanceIDs []string) bool {
	var fields jobLogFields
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		return (jobURL != "" && strings.Contains(line, jobURL)) || containsAny(line, instanceIDs)
	}
	if jobURL != "" && strings.EqualFold(strings.TrimSuffix(fields.JobURL, "/"), strings.TrimSuffix(jobURL, "/")) {
		return true
	}
	for _, rawID := range []json.RawMessage{fields.JobID, fields.WorkflowJobID} {
		if jobID != "" && strings.Trim(strings.TrimSpace(string(rawID)), `"`) == jobID {
			return true
		}
	}
	loggedScalesetJobID := strings.Trim(strings.TrimSpace(string(fields.ScalesetJobID)), `"`)
	if loggedScalesetJobID != "" && (loggedScalesetJobID == scalesetJobID || loggedScalesetJobID == jobID) {
		return true
	}
	for _, instanceID := range instanceIDs {
		if instanceID != "" && (fields.InstanceID == instanceID || strings.Contains(fields.Message, instanceID)) {
			return true
		}
	}
	return false
}

func containsAny(value string, candidates []string) bool {
	for _, candidate := range candidates {
		if candidate != "" && strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func jobLogCorrelationKey(jobURL, scalesetJobID string, instanceIDs []string) string {
	identifiers := append([]string{jobURL, scalesetJobID}, instanceIDs...)
	sort.Strings(identifiers)
	return strings.Join(identifiers, "\x00")
}

type logEvent struct {
	message   string
	prefix    string
	stream    string
	timestamp int64
	eventId   string
	noColor   bool
}

type applicationLogEvent struct {
	Message    string    `json:"message"`
	AppVersion string    `json:"app_version"`
	RunID      int64     `json:"run_id"`
	Label      []string  `json:"labels"`
	Timestamp  time.Time `json:"time"`
}

func (e *logEvent) print(w io.Writer, format string) {
	message := e.message
	localTime := time.UnixMilli(e.timestamp).Local().Format("2006-01-02T15:04:05.000Z07:00")

	if format == "short" && e.prefix == "application" {
		applicationLogEvent := &applicationLogEvent{}
		err := json.Unmarshal([]byte(message), applicationLogEvent)
		if err == nil {
			message = applicationLogEvent.Message
			localTime = applicationLogEvent.Timestamp.Local().Format("2006-01-02T15:04:05.000Z07:00")
		}
	}

	if e.noColor {
		fmt.Fprintf(w, "%s [%s] %s\n", localTime, e.stream, message)
		return
	}

	// Default "long" format

	color := "\033[34m" // blue for instance
	stream := e.stream
	switch e.prefix {
	case "application":
		color = "\033[33m" // yellow for application
		stream = e.prefix
	case "console":
		color = "\033[35m" // magenta for console
		stream = e.prefix
	}
	fmt.Fprintf(w, "\033[90m%s\033[0m %s[%s]\033[0m %s\n", localTime, color, stream, message)
}

type logCollector struct {
	events              []logEvent
	mu                  sync.Mutex
	eventCh             chan logEvent
	wg                  sync.WaitGroup
	pastEventsCollected bool
	seenEvents          map[string]struct{}
}

func newLogCollector() *logCollector {
	return &logCollector{
		events:     make([]logEvent, 0),
		eventCh:    make(chan logEvent, 100),
		seenEvents: make(map[string]struct{}),
	}
}

func (c *logCollector) add(event logEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, seen := c.seenEvents[event.eventId]; seen {
		return
	}
	c.seenEvents[event.eventId] = struct{}{}

	if !c.pastEventsCollected {
		c.events = append(c.events, event)
	} else {
		c.eventCh <- event
	}
}

type streamedLogSession struct {
	collector *logCollector
	opts      *LogOptions
	logger    *log.Logger
	stdout    io.Writer
	stderr    io.Writer
	drained   chan struct{} // closed once the past events are printed

	failuresMu    sync.Mutex
	failedSources map[string]struct{}
	shownFailures map[string]struct{}
}

func newStreamedLogSession(opts *LogOptions, logger *log.Logger, stdout, stderr io.Writer) *streamedLogSession {
	return &streamedLogSession{
		collector:     newLogCollector(),
		opts:          opts,
		logger:        logger,
		stdout:        stdout,
		stderr:        stderr,
		drained:       make(chan struct{}),
		failedSources: make(map[string]struct{}),
		shownFailures: make(map[string]struct{}),
	}
}

// reportFailure warns that a log source could not be read. Watch mode
// re-polls every interval, so each distinct error is shown once per source.
func (s *streamedLogSession) reportFailure(source string, err error) {
	s.logger.Printf("[%s]: Error fetching logs: %v", source, err)
	key := source + "\x00" + logFailureKey(err)
	s.failuresMu.Lock()
	defer s.failuresMu.Unlock()
	s.failedSources[source] = struct{}{}
	if _, shown := s.shownFailures[key]; shown {
		return
	}
	s.shownFailures[key] = struct{}{}
	fmt.Fprintf(s.stderr, "Warning: cannot read %s logs: %v\n", source, err)
}

// incompleteError reports whether any log source failed.
func (s *streamedLogSession) incompleteError() error {
	s.failuresMu.Lock()
	defer s.failuresMu.Unlock()
	switch failed := len(s.failedSources); failed {
	case 0:
		return nil
	case 1:
		return errors.New("1 log source failed; output is incomplete")
	default:
		return fmt.Errorf("%d log sources failed; output is incomplete", failed)
	}
}

// logFailureKey identifies an error for de-duplication. AWS API errors carry a
// per-request ID in their text, so they are identified by code and message.
func logFailureKey(err error) string {
	var apiErr interface {
		ErrorCode() string
		ErrorMessage() string
	}
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() + ": " + apiErr.ErrorMessage()
	}
	return err.Error()
}

// cloudWatchStream is one CloudWatch log source of a session.
type cloudWatchStream struct {
	prefix string // names the source in output and warnings
	update func(*cloudwatchlogs.FilterLogEventsInput) error
	// accept keeps only the events it matches. Watch mode then re-reads the
	// last minute on every poll for late events, and replays the whole window
	// when correlationKey changes.
	accept         func(string) bool
	correlationKey func() string
}

// start polls stream in the background. The initial drain waits for the first
// pass of the streams started before it; later ones only add live events.
func (s *streamedLogSession) start(ctx context.Context, cwl cloudWatchLogsAPI, stream cloudWatchStream) {
	firstPassDone := func() {}
	select {
	case <-s.drained:
	default:
		s.collector.wg.Add(1)
		firstPassDone = sync.OnceFunc(s.collector.wg.Done)
	}
	go func() {
		if err := s.streamCloudWatchLogs(ctx, cwl, stream, firstPassDone); err != nil {
			s.logger.Printf("Error streaming %s logs: %v", stream.prefix, err)
		}
	}()
}

func (s *streamedLogSession) startOnce(prefix string, collect func(*logCollector) error) {
	s.collector.wg.Go(func() {
		if err := collect(s.collector); err != nil && !errors.Is(err, context.Canceled) {
			s.reportFailure(prefix, err)
		}
	})
}

// streamCloudWatchLogs reports a failed fetch under the stream's prefix and
// polls again on the next watch interval.
func (s *streamedLogSession) streamCloudWatchLogs(ctx context.Context, cwl cloudWatchLogsAPI, stream cloudWatchStream, firstPassDone func()) error {
	prefix, accept, correlationKey := stream.prefix, stream.accept, stream.correlationKey
	var overlap time.Duration
	if s.opts.Watch && accept != nil {
		overlap = cloudWatchWatchReplayOverlap
	}
	input := &cloudwatchlogs.FilterLogEventsInput{}
	lastCorrelationKey := ""
	if correlationKey != nil {
		lastCorrelationKey = correlationKey()
	}

	for {
		if err := ctx.Err(); err != nil {
			firstPassDone()
			return err
		}
		if correlationKey != nil {
			currentCorrelationKey := correlationKey()
			if currentCorrelationKey != lastCorrelationKey {
				// Replay the original window when a queued Fleet job gains correlation
				// identifiers. The collector suppresses events already shown.
				input.StartTime = nil
				lastCorrelationKey = currentCorrelationKey
			}
		}
		if err := stream.update(input); err != nil {
			s.logger.Printf("[%s]: Cannot stream logs: %v", prefix, err)
		} else {
			s.logger.Printf("[%s]: Streaming logs...", prefix)

			paginator := cloudwatchlogs.NewFilterLogEventsPaginator(cwl, input)
			var lastTimestamp int64
			var fetchErr error

			for paginator.HasMorePages() {
				s.logger.Printf("[%s]: Fetching next page", prefix)
				output, err := paginator.NextPage(ctx)
				if err != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						firstPassDone()
						return ctxErr
					}
					fetchErr = err
					break
				}

				if output.NextToken != nil {
					s.logger.Printf("[%s]: Received %d events (next token: %s)", prefix, len(output.Events), *output.NextToken)
				} else {
					s.logger.Printf("[%s]: Received %d events", prefix, len(output.Events))
				}

				for i, event := range output.Events {
					message := aws.ToString(event.Message)
					if event.Timestamp != nil && *event.Timestamp > lastTimestamp {
						lastTimestamp = *event.Timestamp
					}
					if accept != nil && !accept(message) {
						continue
					}
					s.collector.add(logEvent{
						message:   message,
						prefix:    prefix,
						stream:    aws.ToString(event.LogStreamName),
						timestamp: aws.ToInt64(event.Timestamp),
						eventId:   aws.ToString(event.EventId),
						noColor:   s.opts.NoColor,
					})

					s.logger.Printf("[%s]: %d: Last timestamp: %d", prefix, i, lastTimestamp)
				}
				s.logger.Printf("[%s]: Done fetching page", prefix)
			}

			if fetchErr != nil {
				// Keep the cursor so the next poll retries the pages that failed.
				s.reportFailure(prefix, fetchErr)
			} else if lastTimestamp > 0 {
				nextStartTime := lastTimestamp + 1
				if overlap > 0 {
					nextStartTime = max(lastTimestamp-overlap.Milliseconds(), s.opts.StartTime)
				}
				input.StartTime = aws.Int64(nextStartTime)
			} else {
				lookback := cmp.Or(overlap, time.Second)
				input.StartTime = aws.Int64(max(time.Now().UnixMilli()-lookback.Milliseconds(), s.opts.StartTime))
			}
			s.logger.Printf("[%s]: Updated start time: %d", prefix, aws.ToInt64(input.StartTime))
		}

		s.logger.Printf("[%s]: Done streaming logs", prefix)
		firstPassDone()
		if !s.opts.Watch {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.opts.WatchInterval):
		}
	}

	return nil
}

func (s *streamedLogSession) drainAndWatch(ctx context.Context) error {
	s.collector.wg.Wait()

	s.collector.mu.Lock()
	s.logger.Printf("Draining remaining events")
	sort.Slice(s.collector.events, func(i, j int) bool {
		return s.collector.events[i].timestamp < s.collector.events[j].timestamp
	})
	for _, event := range s.collector.events {
		event.print(s.stdout, s.opts.Format)
	}
	s.collector.pastEventsCollected = true
	close(s.drained)
	s.collector.mu.Unlock()

	if !s.opts.Watch {
		return s.incompleteError()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-s.collector.eventCh:
			event.print(s.stdout, s.opts.Format)
		}
	}
}

func (s *logStreamer) collectConsoleLogs(ctx context.Context, facts *jobFactsProvider, collector *logCollector, opts *LogOptions) error {
	instanceIDs := facts.current().InstanceIDs
	if len(instanceIDs) == 0 {
		// A job without an instance yet has no console output to read.
		s.logger.Printf("Console logs unavailable: %v", facts.instanceUnavailableError())
		return nil
	}

	var errs []error
	for _, instanceID := range instanceIDs {
		input := &ec2.GetConsoleOutputInput{
			InstanceId: aws.String(instanceID),
		}

		result, err := s.ec2.GetConsoleOutput(ctx, input)
		if err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", instanceID, err))
			continue
		}

		if result.Output != nil {
			// Decode base64 encoded console output
			decodedOutput, err := base64.StdEncoding.DecodeString(*result.Output)
			if err != nil {
				s.logger.Printf("Error decoding base64 console output: %v", err)
				// If base64 decoding fails, use the raw output
				decodedOutput = []byte(*result.Output)
			}

			timestamp := result.Timestamp.UnixMilli()
			// Split console output into lines and add them as log events
			lines := strings.Split(string(decodedOutput), "\n")
			for i, line := range lines {
				if strings.TrimSpace(line) == "" {
					continue
				}
				eventId := fmt.Sprintf("console-%s-%d", instanceID, i)

				collector.add(logEvent{
					message:   line,
					prefix:    "console",
					stream:    "console",
					timestamp: timestamp,
					eventId:   eventId,
					noColor:   opts.NoColor,
				})
			}
		}
	}

	return errors.Join(errs...)
}

func applyLogTimeBounds(input *cloudwatchlogs.FilterLogEventsInput, opts *LogOptions) {
	if input.StartTime == nil {
		input.StartTime = aws.Int64(opts.StartTime)
	}
	if opts.EndTime > 0 {
		input.EndTime = aws.Int64(opts.EndTime)
	}
}
