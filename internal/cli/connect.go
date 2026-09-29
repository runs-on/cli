package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/spf13/cobra"
)

func NewConnectCmd(stack *Stack) *cobra.Command {
	var watch bool

	cmd := &cobra.Command{
		Use:           "connect JOB_URL",
		Short:         "Connect to the instance running a specific job via SSM",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			job, err := parseGitHubJobURL(args[0])
			if err != nil {
				return err
			}
			// Check local prerequisites before any lookup, so --watch does not
			// wait for a job only to fail here.
			awsPath, err := exec.LookPath("aws")
			if err != nil {
				return fmt.Errorf("aws CLI not found: %w", err)
			}
			// `aws ssm start-session help` succeeds without the plugin, so look
			// for the plugin binary the aws CLI runs.
			if _, err := exec.LookPath("session-manager-plugin"); err != nil {
				return errors.New("AWS Session Manager plugin not installed. Please install from https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html")
			}
			config, err := stack.discoverResources(cmd)
			if err != nil {
				return err
			}
			if err := config.validateJobLookup(); err != nil {
				return err
			}

			ctx := cmd.Context()
			ssmClient := ssm.NewFromConfig(config.AWSConfig)
			facts, err := lookupWorkflowJobFacts(ctx, config, job, watch, debugLogger(cmd))
			if err != nil {
				return err
			}
			instanceID := facts.CurrentInstanceID

			// Check if instance is running and get platform type
			describeInput := &ssm.DescribeInstanceInformationInput{
				Filters: []types.InstanceInformationStringFilter{
					{
						Key:    aws.String("InstanceIds"),
						Values: []string{instanceID},
					},
				},
			}
			describeOutput, err := ssmClient.DescribeInstanceInformation(ctx, describeInput)
			if err != nil {
				return fmt.Errorf("failed to check instance status: %w", err)
			}
			if len(describeOutput.InstanceInformationList) == 0 {
				return fmt.Errorf("instance %s is not running or not registered with SSM", instanceID)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Connecting to instance %s...\n", instanceID)

			// Determine shell command based on platform type
			shellCmd := "cd /home/runner && sudo -s bash"
			if describeOutput.InstanceInformationList[0].PlatformType == "Windows" {
				// will still work even if directory does not exist (defaults to C:\Windows\system32)
				shellCmd = "cd C:\\actions-runner; powershell"
			}

			return startSession(awsPath, []string{
				"ssm", "start-session",
				"--target", instanceID,
				"--region", config.AWSConfig.Region,
				"--document-name", "AWS-StartInteractiveCommand",
				"--parameters", fmt.Sprintf("command='%s'", shellCmd),
			})
		},
	}

	cmd.Flags().BoolVar(&watch, "watch", false, "Wait for instance ID if not found")
	return cmd
}

// startSession hands the terminal to `aws <args>`. On Unix it replaces roc
// with the aws process. syscall.Exec is unsupported on Windows, so there roc
// runs aws as a child attached to the console and waits for it; the child is
// not tied to a context, so Ctrl-C reaches the session instead of killing it.
func startSession(awsPath string, args []string) error {
	if runtime.GOOS != "windows" {
		if err := syscall.Exec(awsPath, append([]string{"aws"}, args...), os.Environ()); err != nil {
			return fmt.Errorf("failed to run aws ssm start-session: %w", err)
		}
		return nil
	}
	// The console sends Ctrl-C to roc as well as to the session. Without a
	// registered handler, Windows ends roc and leaves the session orphaned.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	session := exec.Command(awsPath, args...)
	session.Stdin, session.Stdout, session.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := session.Run(); err != nil {
		return fmt.Errorf("aws ssm start-session failed: %w", err)
	}
	return nil
}
