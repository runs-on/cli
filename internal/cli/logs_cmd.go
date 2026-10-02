package cli

import (
	"fmt"
	"io"
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

const noColorFlagUsage = "Disable color output for streamed logs (also off when stdout is not a terminal or NO_COLOR is set)"

// logColorDisabled reports whether streamed logs print without ANSI color:
// when --no-color is passed, when NO_COLOR is non-empty (https://no-color.org),
// or when stdout is not a terminal.
func logColorDisabled(noColorFlag bool, noColorEnv string, stdoutIsTerminal bool) bool {
	return noColorFlag || noColorEnv != "" || !stdoutIsTerminal
}

// streamedLogColorDisabled treats a stdout that is not a file as no terminal.
func streamedLogColorDisabled(stdout io.Writer, noColorFlag bool) bool {
	stdoutIsTerminal := false
	if file, ok := stdout.(*os.File); ok {
		info, err := file.Stat()
		stdoutIsTerminal = err == nil && info.Mode()&os.ModeCharDevice != 0
	}
	return logColorDisabled(noColorFlag, os.Getenv("NO_COLOR"), stdoutIsTerminal)
}

func NewLogsCmd(stack *Stack) *cobra.Command {
	var (
		watchDuration string
		full          bool
		noColor       bool
		format        string
		includeFlags  []string
	)

	cmd := &cobra.Command{
		Use:   "logs JOB_URL",
		Short: "Fetch RunsOn and instance logs for a specific GitHub Actions job URL. Use --include to specify log types (run, console)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			job, err := parseGitHubJobURL(args[0])
			if err != nil {
				return err
			}

			watch, watchInterval, err := parseLogWatch(watchDuration)
			if err != nil {
				return err
			}
			if full && watch {
				return fmt.Errorf("--full cannot be used with --watch")
			}

			config, err := stack.discoverResources(cmd)
			if err != nil {
				return err
			}
			if err := config.validateJobLogs(); err != nil {
				return err
			}

			if full {
				exporter := newFullLogExporter(config)
				zipPath, fullErr := exporter.Export(ctx, job)
				if zipPath != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Full log archive exported to: %s\n", zipPath)
				}
				return fullErr
			}

			streamer := newLogStreamer(cmd, config)

			logOptions := &LogOptions{
				Watch:         watch,
				WatchInterval: watchInterval,
				StartTime:     time.Now().Add(-2 * time.Hour).UnixMilli(),
				Format:        format,
				NoColor:       streamedLogColorDisabled(cmd.OutOrStdout(), noColor),
			}

			facts := newJobFactsProvider(config, job, streamer.logger)
			return streamer.StreamJob(ctx, facts, includeFlags, logOptions)
		},
	}

	cmd.Flags().StringVarP(&watchDuration, "watch", "w", "", "Watch for new logs with optional interval (e.g. --watch 2s)")
	cmd.Flags().Lookup("watch").NoOptDefVal = "5s"
	cmd.Flags().BoolVar(&full, "full", false, "Export full diagnostic archive for the job")
	cmd.Flags().StringVarP(&format, "format", "f", "long", "Output format: long (default) or short")
	cmd.Flags().BoolVar(&noColor, "no-color", false, noColorFlagUsage)
	cmd.Flags().StringSliceVar(&includeFlags, "include", []string{}, "Include additional log types: 'run' (all logs from entire run), 'console' (EC2 instance console logs)")

	return cmd
}

func NewStackLogsCmd(stack *Stack) *cobra.Command {
	var (
		watchDuration string
		since         string
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
			config, err := stack.discoverResources(cmd)
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

			logOptions := &LogOptions{
				Watch:         watch,
				WatchInterval: watchInterval,
				StartTime:     startTime.UnixMilli(),
				Format:        format,
				NoColor:       streamedLogColorDisabled(cmd.OutOrStdout(), noColor),
			}

			return newLogStreamer(cmd, config).StreamStack(ctx, logOptions)
		},
	}

	cmd.Flags().StringVarP(&watchDuration, "watch", "w", "", "Watch for new logs with optional interval (e.g. --watch 2s)")
	cmd.Flags().Lookup("watch").NoOptDefVal = "5s"
	cmd.Flags().StringVarP(&since, "since", "s", "2h", "Show logs since duration (e.g. 30m, 2h)")
	cmd.Flags().StringVarP(&format, "format", "f", "long", "Output format: long (default) or short")
	cmd.Flags().BoolVar(&noColor, "no-color", false, noColorFlagUsage)

	return cmd
}
