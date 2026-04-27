package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/spf13/cobra"
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

type LogFetcher struct {
	cwl          *cloudwatchlogs.Client
	jobs         workflowJobsAPI
	ec2          *ec2.Client
	outputs      *StackOutputs
	jobsTable    string
	instanceID   string
	runID        int64
	jobID        string
	logger       *log.Logger
	collector    *logCollector
	includeTypes []string
}

func NewLogFetcher(config *RunsOnConfig) *LogFetcher {
	logger := log.New(io.Discard, "", 0)
	return &LogFetcher{
		cwl:       cloudwatchlogs.NewFromConfig(config.AWSConfig),
		jobs:      dynamodb.NewFromConfig(config.AWSConfig),
		ec2:       ec2.NewFromConfig(config.AWSConfig),
		jobsTable: config.WorkflowJobsTable,
		outputs: &StackOutputs{
			ServiceLogGroupName:    config.ServiceLogGroupName,
			EC2InstanceLogGroupArn: config.EC2InstanceLogGroupArn,
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

func (f *LogFetcher) Init(ctx context.Context, jobID string, includeTypes []string) error {
	f.jobID = jobID
	f.includeTypes = includeTypes
	f.logger.Printf("Fetching logs for job ID: %s (include types: %v)", jobID, includeTypes)

	if err := f.refreshJobLookup(ctx); err != nil {
		return err
	}

	// Refresh the job lookup so reschedules follow the latest instance.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := f.refreshJobLookup(ctx); err != nil {
					f.logger.Printf("Error refreshing job lookup: %v", err)
				}
				if f.instanceID != "" {
					f.logger.Printf("Instance ID for job %s: %s", f.jobID, f.instanceID)
				}
			}
		}
	}()

	return nil
}

func (f *LogFetcher) hasIncludeType(includeType string) bool {
	return slices.Contains(f.includeTypes, includeType)
}

func (f *LogFetcher) refreshJobLookup(ctx context.Context) error {
	jobLookup, err := findJobLookup(ctx, f.jobs, f.jobsTable, f.jobID)
	if err != nil {
		f.logger.Printf("Error discovering workflow job lookup: %v", err)
		f.instanceID = ""
		f.runID = 0
		return nil
	}
	if jobLookup == nil {
		f.instanceID = ""
		f.runID = 0
		return nil
	}
	f.instanceID = jobLookup.InstanceID
	f.runID = jobLookup.RunID
	return nil
}

func (f *LogFetcher) applicationFilterPattern() (string, error) {
	if f.hasIncludeType("run") {
		if f.runID == 0 {
			return "", fmt.Errorf("workflow run ID for job %s not available yet", f.jobID)
		}
		return fmt.Sprintf(`{ ( $.run_id = "%d" ) }`, f.runID), nil
	}
	if f.instanceID == "" {
		return fmt.Sprintf(`{ ( $.job_id = "%s" ) }`, f.jobID), nil
	}
	return fmt.Sprintf(`{ ( $.job_id = "%s" ) || ( $.message = "*%s*" ) }`, f.jobID, f.instanceID), nil
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
	done                chan struct{}
	wg                  sync.WaitGroup
	pastEventsCollected bool
	seenEvents          map[string]struct{}
}

func newLogCollector() *logCollector {
	return &logCollector{
		pastEventsCollected: false,
		events:              make([]logEvent, 0),
		mu:                  sync.Mutex{},
		eventCh:             make(chan logEvent, 100),
		done:                make(chan struct{}),
		wg:                  sync.WaitGroup{},
		seenEvents:          make(map[string]struct{}),
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

func (f *LogFetcher) streamLogs(ctx context.Context, prefix string, updateInput func(*cloudwatchlogs.FilterLogEventsInput) error, opts *LogOptions) error {
	collector := f.collector

	input := &cloudwatchlogs.FilterLogEventsInput{}

	pastEventsCollected := false

	for {
		if err := updateInput(input); err != nil {
			f.logger.Printf("[%s]: Cannot stream logs: %v", prefix, err)
		} else {
			f.logger.Printf("[%s]: Streaming logs...", prefix)

			paginator := cloudwatchlogs.NewFilterLogEventsPaginator(f.cwl, input)
			var lastTimestamp int64

			for paginator.HasMorePages() {
				f.logger.Printf("[%s]: Fetching next page", prefix)
				output, err := paginator.NextPage(ctx)
				if err != nil {
					f.logger.Printf("[%s]: Error fetching logs: %v", prefix, err)
					return fmt.Errorf("error fetching logs: %w", err)
				}

				if output.NextToken != nil {
					f.logger.Printf("[%s]: Received %d events (next token: %s)", prefix, len(output.Events), *output.NextToken)
				} else {
					f.logger.Printf("[%s]: Received %d events", prefix, len(output.Events))
				}

				for i, event := range output.Events {
					// f.logger.Printf("[%s]: Received log event: %+v", prefix, event)
					collector.add(logEvent{
						message:   *event.Message,
						prefix:    prefix,
						stream:    *event.LogStreamName,
						timestamp: *event.Timestamp,
						eventId:   *event.EventId,
						noColor:   opts.NoColor,
					})

					if event.Timestamp != nil && *event.Timestamp > lastTimestamp {
						lastTimestamp = *event.Timestamp
					}
					f.logger.Printf("[%s]: %d: Last timestamp: %d", prefix, i, lastTimestamp)
				}
				f.logger.Printf("[%s]: Done fetching page", prefix)
			}

			// Update start time for next poll
			if lastTimestamp > 0 {
				input.StartTime = aws.Int64(lastTimestamp + 1)
			} else {
				input.StartTime = aws.Int64(time.Now().UnixMilli() - 1000)
			}
			f.logger.Printf("[%s]: Updated start time: %d", prefix, *input.StartTime)
		}

		f.logger.Printf("[%s]: Done streaming logs", prefix)
		if !pastEventsCollected {
			pastEventsCollected = true
			collector.wg.Done()
		}
		if !opts.Watch {
			break
		}
		time.Sleep(opts.WatchInterval)
	}

	return nil
}

func (f *LogFetcher) streamInstanceLogs(ctx context.Context, opts *LogOptions) error {
	updateInput := func(input *cloudwatchlogs.FilterLogEventsInput) error {
		input.LogGroupIdentifier = &f.outputs.EC2InstanceLogGroupArn
		input.FilterPattern = aws.String("")
		if input.StartTime == nil {
			input.StartTime = aws.Int64(opts.StartTime)
		}
		if f.instanceID == "" {
			return fmt.Errorf("instance ID for job %s not available yet", f.jobID)
		}

		input.LogStreamNamePrefix = aws.String(fmt.Sprintf("%s/", f.instanceID))
		f.logger.Printf("Streaming instance logs with arn: %s, prefix: %s", *input.LogGroupIdentifier, *input.LogStreamNamePrefix)
		return nil
	}

	return f.streamLogs(ctx, "instance", updateInput, opts)
}

func (f *LogFetcher) streamConsoleLogs(ctx context.Context, opts *LogOptions) error {
	if f.instanceID == "" {
		return fmt.Errorf("instance ID for job %s not available yet", f.jobID)
	}

	// Fetch console output from EC2 instance
	input := &ec2.GetConsoleOutputInput{
		InstanceId: aws.String(f.instanceID),
	}

	result, err := f.ec2.GetConsoleOutput(ctx, input)
	if err != nil {
		return fmt.Errorf("error fetching console logs: %w", err)
	}

	if result.Output != nil {
		// Decode base64 encoded console output
		decodedOutput, err := base64.StdEncoding.DecodeString(*result.Output)
		if err != nil {
			f.logger.Printf("Error decoding base64 console output: %v", err)
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
			eventId := fmt.Sprintf("console-%s-%d", f.instanceID, i)

			f.collector.add(logEvent{
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

func (f *LogFetcher) streamApplicationLogs(ctx context.Context, opts *LogOptions, updateInput func(*cloudwatchlogs.FilterLogEventsInput) error) error {
	logGroupArn, err := f.outputs.applicationLogGroupIdentifier()
	if err != nil {
		return err
	}

	return f.streamLogs(ctx, "application", func(input *cloudwatchlogs.FilterLogEventsInput) error {
		input.LogGroupIdentifier = &logGroupArn
		return updateInput(input)
	}, opts)
}

func (f *LogFetcher) FetchLogs(ctx context.Context, opts *LogOptions) error {
	f.collector = newLogCollector()
	collector := f.collector

	collector.wg.Add(1)
	go func() {
		if err := f.streamInstanceLogs(ctx, opts); err != nil {
			f.logger.Printf("Error streaming instance logs: %v", err)
		}
	}()

	// Add console logs if requested
	if f.hasIncludeType("console") {
		collector.wg.Go(func() {
			if err := f.streamConsoleLogs(ctx, opts); err != nil {
				f.logger.Printf("Error streaming console logs: %v", err)
			}
		})
	}

	collector.wg.Add(1)
	go func() {
		if err := f.streamApplicationLogs(ctx, opts, func(input *cloudwatchlogs.FilterLogEventsInput) error {
			if input.StartTime == nil {
				input.StartTime = aws.Int64(opts.StartTime)
			}
			filterPattern, err := f.applicationFilterPattern()
			if err != nil {
				return err
			}
			input.FilterPattern = aws.String(filterPattern)
			f.logger.Printf("Filter pattern: %s", *input.FilterPattern)
			return nil
		}); err != nil {
			f.logger.Printf("Error streaming application logs: %v", err)
		}
	}()

	// Wait for initial events to be collected
	collector.wg.Wait()

	collector.mu.Lock()
	f.logger.Printf("Draining remaining events")
	sort.Slice(collector.events, func(i, j int) bool {
		return collector.events[i].timestamp < collector.events[j].timestamp
	})
	format := opts.Format
	if format == "" {
		format = "long"
	}
	for _, event := range collector.events {
		event.print(format)
	}
	collector.pastEventsCollected = true
	collector.mu.Unlock()

	if !opts.Watch {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-collector.eventCh:
			event.print(format)
		case <-time.After(10 * time.Second):
			if !opts.Watch {
				return nil
			}
		}
	}
}

func (f *LogFetcher) FetchAllApplicationLogs(ctx context.Context, opts *LogOptions) error {
	f.collector = newLogCollector()
	collector := f.collector

	collector.wg.Add(1)
	go func() {
		if err := f.streamApplicationLogs(ctx, opts, func(input *cloudwatchlogs.FilterLogEventsInput) error {
			// No filtering pattern for all logs
			input.FilterPattern = aws.String("")
			// Set start time from options
			if input.StartTime == nil {
				input.StartTime = aws.Int64(opts.StartTime)
			}
			return nil
		}); err != nil {
			f.logger.Printf("Error streaming application logs: %v", err)
		}
	}()

	// Wait for initial events to be collected
	collector.wg.Wait()

	collector.mu.Lock()
	f.logger.Printf("Draining remaining events")
	sort.Slice(collector.events, func(i, j int) bool {
		return collector.events[i].timestamp < collector.events[j].timestamp
	})
	format := opts.Format
	if format == "" {
		format = "long"
	}
	for _, event := range collector.events {
		event.print(format)
	}
	collector.pastEventsCollected = true
	collector.mu.Unlock()

	if !opts.Watch {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-collector.eventCh:
			event.print(format)
		case <-time.After(10 * time.Second):
			if !opts.Watch {
				return nil
			}
		}
	}
}

func extractJobID(input string) string {
	url, err := url.Parse(input)
	if err == nil && url.Scheme == "https" {
		// Extract job ID from URLs like:
		//
		// - https://github.com/runs-on/runs-on/actions/runs/12312372848/job/34368864490
		// - https://github.com/runs-on/runs-on/actions/runs/12312372848/job/34368864490?pr=123
		parts := strings.Split(url.Path, "/")
		if len(parts) > 1 {
			return parts[len(parts)-1]
		}
	}
	return input
}

func NewLogsCmd(stack *Stack) *cobra.Command {
	var (
		watchDuration string
		since         string
		debug         bool
		noColor       bool
		format        string
		includeFlags  []string
	)

	cmd := &cobra.Command{
		Use:   "logs JOB_ID|JOB_URL|RUN_ID",
		Short: "Fetch RunsOn and instance logs for a specific job ID. Use --include to specify log types (run, console)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := stack.getStackOutputs(cmd)
			if err != nil {
				return err
			}
			if err := config.validateJobLogs(); err != nil {
				return err
			}

			ctx := cmd.Context()

			startTime := time.Now().Add(-2 * time.Hour)
			if since != "" {
				duration, err := time.ParseDuration(since)
				if err != nil {
					return fmt.Errorf("invalid --since value: %w", err)
				}
				startTime = time.Now().Add(-duration)
			}

			fetcher := NewLogFetcher(config)
			if debug {
				fetcher.logger.SetOutput(os.Stderr)
			}

			watchInterval := 5 * time.Second
			watch := watchDuration != ""
			if watch && watchDuration != "true" {
				duration, err := time.ParseDuration(watchDuration)
				if err != nil {
					return fmt.Errorf("invalid --watch value: %w", err)
				}
				watchInterval = duration
			}

			logOptions := &LogOptions{
				Watch:         watch,
				WatchInterval: watchInterval,
				StartTime:     startTime.UnixMilli(),
				Format:        format,
				NoColor:       noColor,
			}

			// If no include flags specified, default to job logs only
			if len(includeFlags) == 0 {
				includeFlags = []string{}
			}

			jobID := extractJobID(args[0])
			if err := fetcher.Init(ctx, jobID, includeFlags); err != nil {
				return err
			}
			return fetcher.FetchLogs(ctx, logOptions)
		},
	}

	cmd.Flags().StringVarP(&watchDuration, "watch", "w", "", "Watch for new logs with optional interval (e.g. --watch 2s)")
	cmd.Flags().Lookup("watch").NoOptDefVal = "5s"
	cmd.Flags().StringVarP(&since, "since", "s", "2h", "Show logs since duration (e.g. 30m, 2h)")
	cmd.Flags().BoolVarP(&debug, "debug", "d", false, "Enable debug output")
	cmd.Flags().StringVarP(&format, "format", "f", "long", "Output format: long (default) or short")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable color output")
	cmd.Flags().StringSliceVar(&includeFlags, "include", []string{}, "Include additional log types: 'run' (all logs from entire run), 'console' (EC2 instance console logs)")

	return cmd
}

func NewStackLogsCmd(stack *Stack) *cobra.Command {
	var (
		watchDuration string
		since         string
		debug         bool
		noColor       bool
		format        string
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Stream all RunsOn application logs from CloudWatch",
		Long: `Stream all RunsOn application logs from the CloudWatch log group.

This command streams all application logs from the RunsOn service, not filtered
by specific jobs. Use this to monitor overall service activity and troubleshoot
system-wide issues.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := stack.getStackOutputs(cmd)
			if err != nil {
				return err
			}
			if err := config.validateStackLogs(); err != nil {
				return err
			}

			ctx := cmd.Context()

			startTime := time.Now().Add(-2 * time.Hour)
			if since != "" {
				duration, err := time.ParseDuration(since)
				if err != nil {
					return fmt.Errorf("invalid --since value: %w", err)
				}
				startTime = time.Now().Add(-duration)
			}

			fetcher := NewLogFetcher(config)
			if debug {
				fetcher.logger.SetOutput(os.Stderr)
			}

			watchInterval := 5 * time.Second
			watch := watchDuration != ""
			if watch && watchDuration != "true" {
				duration, err := time.ParseDuration(watchDuration)
				if err != nil {
					return fmt.Errorf("invalid --watch value: %w", err)
				}
				watchInterval = duration
			}

			logOptions := &LogOptions{
				Watch:         watch,
				WatchInterval: watchInterval,
				StartTime:     startTime.UnixMilli(),
				Format:        format,
				NoColor:       noColor,
			}

			return fetcher.FetchAllApplicationLogs(ctx, logOptions)
		},
	}

	cmd.Flags().StringVarP(&watchDuration, "watch", "w", "", "Watch for new logs with optional interval (e.g. --watch 2s)")
	cmd.Flags().Lookup("watch").NoOptDefVal = "5s"
	cmd.Flags().StringVarP(&since, "since", "s", "2h", "Show logs since duration (e.g. 30m, 2h)")
	cmd.Flags().BoolVarP(&debug, "debug", "d", false, "Enable debug output")
	cmd.Flags().StringVarP(&format, "format", "f", "long", "Output format: long (default) or short")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable color output")

	return cmd
}
