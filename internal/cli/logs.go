package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

type StackOutputs struct {
	ServiceLogGroupName    string
	EC2InstanceLogGroupArn string
}

type LogOptions struct {
	Watch         bool
	WatchInterval time.Duration
	StartTime     int64
	Format        string
	NoColor       bool
}

type cloudWatchLogsAPI interface {
	FilterLogEvents(ctx context.Context, params *cloudwatchlogs.FilterLogEventsInput, optFns ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error)
}

type ec2ConsoleAPI interface {
	GetConsoleOutput(ctx context.Context, params *ec2.GetConsoleOutputInput, optFns ...func(*ec2.Options)) (*ec2.GetConsoleOutputOutput, error)
}

type jobLogStreamer struct {
	cwl     cloudWatchLogsAPI
	ec2     ec2ConsoleAPI
	outputs *StackOutputs
	logger  *log.Logger
}

func newJobLogStreamer(config *RunsOnConfig) *jobLogStreamer {
	logger := log.New(io.Discard, "", 0)
	return &jobLogStreamer{
		cwl: cloudwatchlogs.NewFromConfig(config.AWSConfig),
		ec2: ec2.NewFromConfig(config.AWSConfig),
		outputs: &StackOutputs{
			ServiceLogGroupName:    config.ServiceLogGroupName,
			EC2InstanceLogGroupArn: config.EC2InstanceLogGroupArn,
		},
		logger: logger,
	}
}

type applicationLogStreamer struct {
	cwl     cloudWatchLogsAPI
	outputs *StackOutputs
	logger  *log.Logger
}

func newApplicationLogStreamer(config *RunsOnConfig) *applicationLogStreamer {
	logger := log.New(io.Discard, "", 0)
	return &applicationLogStreamer{
		cwl: cloudwatchlogs.NewFromConfig(config.AWSConfig),
		outputs: &StackOutputs{
			ServiceLogGroupName: config.ServiceLogGroupName,
		},
		logger: logger,
	}
}

func (o *StackOutputs) applicationLogGroupIdentifier() (string, error) {
	if o == nil {
		return "", fmt.Errorf("application log group not found")
	}
	if o.ServiceLogGroupName != "" {
		return o.ServiceLogGroupName, nil
	}
	return "", fmt.Errorf("application log group not found")
}

func (s *jobLogStreamer) Stream(ctx context.Context, jobID string, facts *workflowJobFactsProvider, includeTypes []string, opts *LogOptions) error {
	s.ensureLogger()
	if facts == nil {
		return fmt.Errorf("workflow job facts provider is required")
	}
	s.logger.Printf("Fetching logs for job ID: %s (include types: %v)", jobID, includeTypes)

	facts.refresh(ctx)
	refreshCtx, cancelRefresh := context.WithCancel(ctx)
	defer cancelRefresh()
	facts.startRefresh(refreshCtx)

	session := newStreamedLogSession(opts, s.logger)
	session.startCloudWatchStream(ctx, "instance", s.cwl, s.updateInstanceLogInput(jobID, facts, opts))
	if includeLogType(includeTypes, "console") {
		session.startOnce("console", func(collector *logCollector) error {
			return s.collectConsoleLogs(ctx, jobID, facts, collector, opts)
		})
	}
	session.startCloudWatchStream(ctx, "application", s.cwl, s.updateJobApplicationLogInput(jobID, facts, includeTypes, opts))

	return session.drainAndWatch(ctx)
}

func (s *applicationLogStreamer) Stream(ctx context.Context, opts *LogOptions) error {
	s.ensureLogger()
	session := newStreamedLogSession(opts, s.logger)
	session.startCloudWatchStream(ctx, "application", s.cwl, s.updateAllApplicationLogInput(opts))
	return session.drainAndWatch(ctx)
}

func (s *jobLogStreamer) ensureLogger() {
	if s.logger == nil {
		s.logger = log.New(io.Discard, "", 0)
	}
}

func (s *applicationLogStreamer) ensureLogger() {
	if s.logger == nil {
		s.logger = log.New(io.Discard, "", 0)
	}
}

func jobApplicationFilterPattern(jobID string, facts *workflowJobFactsProvider, includeTypes []string) (string, error) {
	if includeLogType(includeTypes, "run") {
		runID := facts.runID()
		if runID == 0 {
			return "", fmt.Errorf("workflow run ID for job %s not available yet", jobID)
		}
		return fmt.Sprintf(`{ ( $.run_id = "%d" ) }`, runID), nil
	}
	instanceID := facts.currentInstanceID()
	if instanceID == "" {
		return fmt.Sprintf(`{ ( $.job_id = "%s" ) }`, jobID), nil
	}
	return fmt.Sprintf(`{ ( $.job_id = "%s" ) || ( $.message = "*%s*" ) }`, jobID, instanceID), nil
}

func includeLogType(includeTypes []string, includeType string) bool {
	return slices.Contains(includeTypes, includeType)
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

func (e *logEvent) print(format string) {
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
		fmt.Printf("%s [%s] %s\n", localTime, e.stream, message)
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
	fmt.Printf("\033[90m%s\033[0m %s[%s]\033[0m %s\n", localTime, color, stream, message)
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
}

func newStreamedLogSession(opts *LogOptions, logger *log.Logger) *streamedLogSession {
	if opts == nil {
		opts = &LogOptions{}
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &streamedLogSession{
		collector: newLogCollector(),
		opts:      opts,
		logger:    logger,
	}
}

func (s *streamedLogSession) startCloudWatchStream(ctx context.Context, prefix string, cwl cloudWatchLogsAPI, updateInput func(*cloudwatchlogs.FilterLogEventsInput) error) {
	s.collector.wg.Add(1)
	go func() {
		if err := s.streamCloudWatchLogs(ctx, prefix, cwl, updateInput); err != nil {
			s.logger.Printf("Error streaming %s logs: %v", prefix, err)
		}
	}()
}

func (s *streamedLogSession) startOnce(prefix string, collect func(*logCollector) error) {
	s.collector.wg.Go(func() {
		if err := collect(s.collector); err != nil {
			s.logger.Printf("Error streaming %s logs: %v", prefix, err)
		}
	})
}

func (s *streamedLogSession) streamCloudWatchLogs(ctx context.Context, prefix string, cwl cloudWatchLogsAPI, updateInput func(*cloudwatchlogs.FilterLogEventsInput) error) error {
	input := &cloudwatchlogs.FilterLogEventsInput{}
	pastEventsCollected := false
	markPastEventsCollected := func() {
		if !pastEventsCollected {
			pastEventsCollected = true
			s.collector.wg.Done()
		}
	}

	for {
		if err := updateInput(input); err != nil {
			s.logger.Printf("[%s]: Cannot stream logs: %v", prefix, err)
		} else {
			s.logger.Printf("[%s]: Streaming logs...", prefix)

			paginator := cloudwatchlogs.NewFilterLogEventsPaginator(cwl, input)
			var lastTimestamp int64

			for paginator.HasMorePages() {
				s.logger.Printf("[%s]: Fetching next page", prefix)
				output, err := paginator.NextPage(ctx)
				if err != nil {
					s.logger.Printf("[%s]: Error fetching logs: %v", prefix, err)
					markPastEventsCollected()
					return fmt.Errorf("error fetching logs: %w", err)
				}

				if output.NextToken != nil {
					s.logger.Printf("[%s]: Received %d events (next token: %s)", prefix, len(output.Events), *output.NextToken)
				} else {
					s.logger.Printf("[%s]: Received %d events", prefix, len(output.Events))
				}

				for i, event := range output.Events {
					s.collector.add(logEvent{
						message:   aws.ToString(event.Message),
						prefix:    prefix,
						stream:    aws.ToString(event.LogStreamName),
						timestamp: aws.ToInt64(event.Timestamp),
						eventId:   aws.ToString(event.EventId),
						noColor:   s.opts.NoColor,
					})

					if event.Timestamp != nil && *event.Timestamp > lastTimestamp {
						lastTimestamp = *event.Timestamp
					}
					s.logger.Printf("[%s]: %d: Last timestamp: %d", prefix, i, lastTimestamp)
				}
				s.logger.Printf("[%s]: Done fetching page", prefix)
			}

			if lastTimestamp > 0 {
				input.StartTime = aws.Int64(lastTimestamp + 1)
			} else {
				input.StartTime = aws.Int64(time.Now().UnixMilli() - 1000)
			}
			s.logger.Printf("[%s]: Updated start time: %d", prefix, *input.StartTime)
		}

		s.logger.Printf("[%s]: Done streaming logs", prefix)
		markPastEventsCollected()
		if !s.opts.Watch {
			break
		}
		time.Sleep(s.opts.WatchInterval)
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
	format := s.opts.Format
	if format == "" {
		format = "long"
	}
	for _, event := range s.collector.events {
		event.print(format)
	}
	s.collector.pastEventsCollected = true
	s.collector.mu.Unlock()

	if !s.opts.Watch {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-s.collector.eventCh:
			event.print(format)
		case <-time.After(10 * time.Second):
			if !s.opts.Watch {
				return nil
			}
		}
	}
}

func (s *jobLogStreamer) updateInstanceLogInput(jobID string, facts *workflowJobFactsProvider, opts *LogOptions) func(*cloudwatchlogs.FilterLogEventsInput) error {
	return func(input *cloudwatchlogs.FilterLogEventsInput) error {
		input.LogGroupIdentifier = &s.outputs.EC2InstanceLogGroupArn
		input.FilterPattern = aws.String("")
		if input.StartTime == nil {
			input.StartTime = aws.Int64(opts.StartTime)
		}
		instanceID := facts.currentInstanceID()
		if instanceID == "" {
			return fmt.Errorf("instance ID for job %s not available yet", jobID)
		}

		input.LogStreamNamePrefix = aws.String(fmt.Sprintf("%s/", instanceID))
		s.logger.Printf("Streaming instance logs with arn: %s, prefix: %s", *input.LogGroupIdentifier, *input.LogStreamNamePrefix)
		return nil
	}
}

func (s *jobLogStreamer) collectConsoleLogs(ctx context.Context, jobID string, facts *workflowJobFactsProvider, collector *logCollector, opts *LogOptions) error {
	instanceID := facts.currentInstanceID()
	if instanceID == "" {
		return fmt.Errorf("instance ID for job %s not available yet", jobID)
	}

	input := &ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
	}

	result, err := s.ec2.GetConsoleOutput(ctx, input)
	if err != nil {
		return fmt.Errorf("error fetching console logs: %w", err)
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

	return nil
}

func (s *jobLogStreamer) updateJobApplicationLogInput(jobID string, facts *workflowJobFactsProvider, includeTypes []string, opts *LogOptions) func(*cloudwatchlogs.FilterLogEventsInput) error {
	return updateApplicationLogInput(s.outputs, func(input *cloudwatchlogs.FilterLogEventsInput) error {
		if input.StartTime == nil {
			input.StartTime = aws.Int64(opts.StartTime)
		}
		filterPattern, err := jobApplicationFilterPattern(jobID, facts, includeTypes)
		if err != nil {
			return err
		}
		input.FilterPattern = aws.String(filterPattern)
		s.logger.Printf("Filter pattern: %s", *input.FilterPattern)
		return nil
	})
}

func (s *applicationLogStreamer) updateAllApplicationLogInput(opts *LogOptions) func(*cloudwatchlogs.FilterLogEventsInput) error {
	return updateApplicationLogInput(s.outputs, func(input *cloudwatchlogs.FilterLogEventsInput) error {
		input.FilterPattern = aws.String("")
		if input.StartTime == nil {
			input.StartTime = aws.Int64(opts.StartTime)
		}
		return nil
	})
}

func updateApplicationLogInput(outputs *StackOutputs, update func(*cloudwatchlogs.FilterLogEventsInput) error) func(*cloudwatchlogs.FilterLogEventsInput) error {
	return func(input *cloudwatchlogs.FilterLogEventsInput) error {
		logGroupArn, err := outputs.applicationLogGroupIdentifier()
		if err != nil {
			return err
		}
		input.LogGroupIdentifier = &logGroupArn
		return update(input)
	}
}
