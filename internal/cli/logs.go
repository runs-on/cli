package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/go-github/v66/github"
	"github.com/spf13/cobra"
)

type StackOutputs struct {
	AppRunnerServiceArn    string
	EC2InstanceLogGroupArn string
	BucketConfig           string
}

type LogFetcher struct {
	cfg         aws.Config
	cwl         *cloudwatchlogs.Client
	s3          *s3.Client
	cfn         *cloudformation.Client
	stackName   string
	outputs     *StackOutputs
	instanceID  string
	jobID       string
	workflowJob *github.WorkflowJob
	logger      *log.Logger
}

func NewLogFetcher(config *RunsOnConfig) *LogFetcher {
	logger := log.New(io.Discard, "", 0)
	return &LogFetcher{
		cfg:       config.AWSConfig,
		cwl:       cloudwatchlogs.NewFromConfig(config.AWSConfig),
		s3:        s3.NewFromConfig(config.AWSConfig),
		cfn:       cloudformation.NewFromConfig(config.AWSConfig),
		stackName: config.StackName,
		outputs: &StackOutputs{
			AppRunnerServiceArn:    config.AppRunnerServiceArn,
			EC2InstanceLogGroupArn: config.EC2LogGroupArn,
			BucketConfig:           config.BucketConfig,
		},
		logger: logger,
	}
}

func (f *LogFetcher) Init(ctx context.Context, jobID string) error {
	f.jobID = jobID
	f.logger.Printf("Fetching logs for job ID: %s", jobID)

	if err := f.refreshWorkflowJobDetails(ctx); err != nil {
		return err
	}
	if err := f.refreshInstanceID(ctx); err != nil {
		return err
	}

	// Start goroutine to refresh instance ID every 5s
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if f.workflowJob == nil {
					if err := f.refreshWorkflowJobDetails(ctx); err != nil {
						f.logger.Printf("Error refreshing workflow job details: %v", err)
					}
				}
				// keep refreshing instance ID, because a job might get rescheduled on another instance
				if err := f.refreshInstanceID(ctx); err != nil {
					f.logger.Printf("Error refreshing instance ID: %v", err)
				}
				if f.instanceID != "" {
					f.logger.Printf("Instance ID for job %s: %s", f.jobID, f.instanceID)
				}
			}
		}
	}()

	return nil
}

func (f *LogFetcher) refreshInstanceID(ctx context.Context) error {
	instanceKey := fmt.Sprintf("runs-on/db/jobs/%s/instance-id", f.jobID)
	instanceOut, err := f.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &f.outputs.BucketConfig,
		Key:    &instanceKey,
	})
	if err != nil {
		f.logger.Printf("Error fetching instance ID from S3: %v", err)
		f.instanceID = ""
		return nil
	}
	defer instanceOut.Body.Close()

	instanceData, err := io.ReadAll(instanceOut.Body)
	if err != nil {
		return err
	}
	f.instanceID = string(instanceData)
	return nil
}

func (f *LogFetcher) refreshWorkflowJobDetails(ctx context.Context) error {
	jobDetailsKey := fmt.Sprintf("runs-on/db/jobs/%s/webhooks/queued", f.jobID)
	payload, err := f.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &f.outputs.BucketConfig,
		Key:    &jobDetailsKey,
	})
	if err != nil {
		f.logger.Printf("Error fetching workflow job details from S3: %v", err)
		return nil
	}
	defer payload.Body.Close()

	body, err := io.ReadAll(payload.Body)
	if err != nil {
		return err
	}
	workflowJob := &github.WorkflowJob{}
	err = json.Unmarshal(body, workflowJob)
	if err != nil {
		return err
	}

	f.workflowJob = workflowJob
	// Process jobDetailsData as needed
	f.logger.Printf("Fetched workflow job details: %s", *workflowJob.Name)
	return nil
}

type logEvent struct {
	message   string
	prefix    string
	stream    string
	timestamp int64
	eventId   string
}

func (e *logEvent) print() {
	localTime := time.UnixMilli(e.timestamp).Local().Format("15:04:05.000")
	color := "\033[34m" // blue for instance
	stream := e.stream
	if e.prefix == "application" {
		color = "\033[33m" // yellow for application
		stream = e.prefix
	}
	fmt.Printf("\033[90m%s\033[0m %s[%s]\033[0m %s\n", localTime, color, stream, e.message)
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

type contextKey string

const (
	collectorKey contextKey = "collector"
)

func (f *LogFetcher) streamLogs(ctx context.Context, watch bool, watchInterval time.Duration, prefix string, updateInput func(*cloudwatchlogs.FilterLogEventsInput) error) error {
	collector := ctx.Value(collectorKey).(*logCollector)

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
		if !watch {
			break
		}
		time.Sleep(watchInterval)
	}

	return nil
}

func (f *LogFetcher) streamInstanceLogs(ctx context.Context, watch bool, watchInterval time.Duration) error {
	updateInput := func(input *cloudwatchlogs.FilterLogEventsInput) error {
		input.LogGroupIdentifier = &f.outputs.EC2InstanceLogGroupArn
		input.FilterPattern = aws.String("")

		if f.workflowJob != nil {
			// only set start time if it's not already set
			if input.StartTime == nil {
				input.StartTime = aws.Int64(f.workflowJob.CreatedAt.UnixMilli() - 10000)
			}
		} else {
			return fmt.Errorf("workflow job queued event not found yet for job %s", f.jobID)
		}
		if f.instanceID == "" {
			return fmt.Errorf("instance ID for job %s not available yet", f.jobID)
		}

		input.LogStreamNamePrefix = aws.String(fmt.Sprintf("%s/", f.instanceID))
		f.logger.Printf("Streaming instance logs with arn: %s, prefix: %s", *input.LogGroupIdentifier, *input.LogStreamNamePrefix)
		return nil
	}

	return f.streamLogs(ctx, watch, watchInterval, "instance", updateInput)
}

func (f *LogFetcher) FetchLogs(ctx context.Context, watch bool, watchInterval time.Duration, startTime int64) error {
	collector := newLogCollector()
	ctx = context.WithValue(ctx, collectorKey, collector)

	collector.wg.Add(1)
	go func() {
		if err := f.streamInstanceLogs(ctx, watch, watchInterval); err != nil {
			f.logger.Printf("Error streaming instance logs: %v", err)
		}
	}()

	collector.wg.Add(1)
	go func() {
		logGroupArn := getLogGroupArn(f.outputs.AppRunnerServiceArn)
		if err := f.streamLogs(ctx, watch, watchInterval, "application", func(input *cloudwatchlogs.FilterLogEventsInput) error {
			input.LogGroupIdentifier = &logGroupArn
			filterPatterns := []string{}
			if f.workflowJob != nil {
				filterPatterns = append(filterPatterns, fmt.Sprintf("( $.job_id = \"%d\" )", *f.workflowJob.ID))
				// only set start time if it's not already set
				if input.StartTime == nil {
					input.StartTime = aws.Int64(f.workflowJob.CreatedAt.UnixMilli() - 10000)
				}
			} else {
				return fmt.Errorf("workflow job queued event not found yet for job %s", f.jobID)
			}
			// also grep for messages mentioning the instance ID
			if f.instanceID != "" {
				filterPatterns = append(filterPatterns, fmt.Sprintf(`( $.message = "%s" )`, f.instanceID))
			}
			input.FilterPattern = aws.String(fmt.Sprintf("{ %s }", strings.Join(filterPatterns, " || ")))
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
	for _, event := range collector.events {
		event.print()
	}
	collector.pastEventsCollected = true
	collector.mu.Unlock()

	if !watch {
		return nil
	}

	// Watch for new events
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-collector.eventCh:
			event.print()
		case <-time.After(10 * time.Second):
			if !watch {
				return nil
			}
		}
	}
}

func getLogGroupArn(arn string) string {
	return fmt.Sprintf("%s/application", strings.Replace(strings.Replace(arn, "apprunner", "logs", 1), ":service", ":log-group:/aws/apprunner", 1))
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

func NewLogsCmd() *cobra.Command {
	var (
		watchDuration string
		since         string
		debug         bool
	)

	cmd := &cobra.Command{
		Use:   "logs JOB_ID|JOB_URL",
		Short: "Fetch RunsOn and instance logs for a specific job ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := getStackOutputs(cmd)
			if err != nil {
				return err
			}

			jobID := extractJobID(args[0])
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

			if err := fetcher.Init(ctx, jobID); err != nil {
				return err
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

			return fetcher.FetchLogs(ctx, watch, watchInterval, startTime.UnixMilli())
		},
	}

	cmd.Flags().StringVarP(&watchDuration, "watch", "w", "", "Watch for new logs with optional interval (e.g. --watch 2s)")
	cmd.Flags().Lookup("watch").NoOptDefVal = "5s"
	cmd.Flags().StringVarP(&since, "since", "s", "2h", "Show logs since duration (e.g. 30m, 2h)")
	cmd.Flags().BoolVarP(&debug, "debug", "d", false, "Enable debug output")
	return cmd
}
