package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func parseLogWatch(watchDuration string) (bool, time.Duration, error) {
	watchInterval := 5 * time.Second
	watch := watchDuration != ""
	if watch && watchDuration != "true" {
		duration, err := time.ParseDuration(watchDuration)
		if err != nil {
			return false, 0, fmt.Errorf("invalid --watch value: %w", err)
		}
		watchInterval = duration
	}
	return watch, watchInterval, nil
}

func NewLogsCmd(stack *Stack) *cobra.Command {
	var (
		watchDuration string
		debug         bool
		full          bool
		noColor       bool
		format        string
		includeFlags  []string
	)

	cmd := &cobra.Command{
		Use:   "logs JOB_ID|JOB_URL|RUN_ID",
		Short: "Fetch RunsOn and instance logs for a specific job ID. Use --include to specify log types (run, console)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			watch, watchInterval, err := parseLogWatch(watchDuration)
			if err != nil {
				return err
			}
			if full && watch {
				return fmt.Errorf("--full cannot be used with --watch")
			}

			config, err := stack.getStackOutputs(cmd)
			if err != nil {
				return err
			}
			if err := config.validateJobLogs(); err != nil {
				return err
			}

			jobID := extractJobID(args[0])
			if full {
				exporter := newFullLogExporter(config)
				zipPath, fullErr := exporter.Export(ctx, jobID)
				if zipPath != "" {
					fmt.Printf("Full log archive exported to: %s\n", zipPath)
				}
				return fullErr
			}

			streamer := newJobLogStreamer(config)
			if debug {
				streamer.logger.SetOutput(os.Stderr)
			}

			logOptions := &LogOptions{
				Watch:         watch,
				WatchInterval: watchInterval,
				StartTime:     time.Now().Add(-2 * time.Hour).UnixMilli(),
				Format:        format,
				NoColor:       noColor,
			}

			facts := newWorkflowJobFactsProvider(config, jobID, streamer.logger)
			return streamer.Stream(ctx, jobID, facts, includeFlags, logOptions)
		},
	}

	cmd.Flags().StringVarP(&watchDuration, "watch", "w", "", "Watch for new logs with optional interval (e.g. --watch 2s)")
	cmd.Flags().Lookup("watch").NoOptDefVal = "5s"
	cmd.Flags().BoolVarP(&debug, "debug", "d", false, "Enable debug output")
	cmd.Flags().BoolVar(&full, "full", false, "Export full diagnostic archive for the job")
	cmd.Flags().StringVarP(&format, "format", "f", "long", "Output format: long (default) or short")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable color output for streamed logs")
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

			watch, watchInterval, err := parseLogWatch(watchDuration)
			if err != nil {
				return err
			}

			streamer := newApplicationLogStreamer(config)
			if debug {
				streamer.logger.SetOutput(os.Stderr)
			}

			logOptions := &LogOptions{
				Watch:         watch,
				WatchInterval: watchInterval,
				StartTime:     startTime.UnixMilli(),
				Format:        format,
				NoColor:       noColor,
			}

			return streamer.Stream(ctx, logOptions)
		},
	}

	cmd.Flags().StringVarP(&watchDuration, "watch", "w", "", "Watch for new logs with optional interval (e.g. --watch 2s)")
	cmd.Flags().Lookup("watch").NoOptDefVal = "5s"
	cmd.Flags().StringVarP(&since, "since", "s", "2h", "Show logs since duration (e.g. 30m, 2h)")
	cmd.Flags().BoolVarP(&debug, "debug", "d", false, "Enable debug output")
	cmd.Flags().StringVarP(&format, "format", "f", "long", "Output format: long (default) or short")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable color output for streamed logs")

	return cmd
}
