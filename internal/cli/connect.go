package cli

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/spf13/cobra"
)

func NewConnectCmd() *cobra.Command {
	var debug bool
	var watch bool

	cmd := &cobra.Command{
		Use:           "connect JOB_ID|JOB_URL",
		Short:         "Connect to the instance running a specific job via SSM",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := getStackOutputs(cmd)
			if err != nil {
				return err
			}

			jobID := extractJobID(args[0])
			ctx := cmd.Context()

			logger := log.New(io.Discard, "", 0)
			if debug {
				logger.SetOutput(cmd.OutOrStderr())
			}

			s3Client := s3.NewFromConfig(config.AWSConfig)
			ssmClient := ssm.NewFromConfig(config.AWSConfig)

			// Get instance ID from S3
			key := fmt.Sprintf("runs-on/db/jobs/%s/instance-id", jobID)
			var instanceID string

			for {
				out, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
					Bucket: &config.BucketConfig,
					Key:    &key,
				})
				if err != nil {
					if !watch {
						return fmt.Errorf("instance ID not found for job %s", jobID)
					}
					logger.Printf("Waiting for instance ID for job %s...\n", jobID)
					time.Sleep(5 * time.Second)
					continue
				}
				defer out.Body.Close()

				data, err := io.ReadAll(out.Body)
				if err != nil {
					return err
				}
				instanceID = string(data)
				break
			}

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

			fmt.Printf("Connecting to instance %s...\n", instanceID)

			// Create session input for plugin
			region := config.AWSConfig.Region

			// Start session-manager-plugin
			awsPath, err := exec.LookPath("aws")
			if err != nil {
				return fmt.Errorf("aws CLI not found: %w", err)
			}

			// Check if SSM plugin is installed
			cmdSsm := exec.Command(awsPath, "ssm", "start-session", "help")
			if err := cmdSsm.Run(); err != nil {
				return fmt.Errorf("AWS Session Manager plugin not installed. Please install from https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html")
			}

			// Determine shell command based on platform type
			shellCmd := "cd /home/runner && bash"
			if describeOutput.InstanceInformationList[0].PlatformType == "Windows" {
				// will still work even if directory does not exist (defaults to C:\Windows\system32)
				shellCmd = "cd C:\\actions-runner; powershell"
			}

			saveCmd := exec.Command("stty", "-g")
			saveCmd.Stdin = os.Stdin
			savedState, err := saveCmd.Output()
			if err != nil {
				return fmt.Errorf("failed to save terminal state: %w", err)
			}
			
			restoreTerminal := func() {
				restoreCmd := exec.Command("stty", string(savedState))
				restoreCmd.Stdin = os.Stdin
				restoreCmd.Run()
				exec.Command("stty", "sane").Run()
				exec.Command("stty", "echo").Run()
			}
			
			defer restoreTerminal()

			cmd := exec.Command(awsPath, 
				"ssm", "start-session",
				"--target", instanceID,
				"--region", region,
				"--document-name", "AWS-StartInteractiveCommand",
				"--parameters", fmt.Sprintf("command='%s'", shellCmd),
			)

			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
			
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("failed to start session: %w", err)
			}

			done := make(chan error, 1)
			go func() {
				done <- cmd.Wait()
			}()

			select {
			case <-sigChan:
				fmt.Fprintln(os.Stderr, "\nReceived interrupt, terminating connection...")
				if err := cmd.Process.Kill(); err != nil {
					return fmt.Errorf("failed to kill process: %w", err)
				}
				return nil
			case err := <-done:
				if err != nil {
					if exitErr, ok := err.(*exec.ExitError); ok {
						status := exitErr.ExitCode()
						if status == 130 || status == 255 {
							fmt.Fprintln(os.Stderr, "\nInstance connection closed.")
							return nil
						}
					}
					return fmt.Errorf("session ended with error: %w", err)
				}
				fmt.Fprintln(os.Stderr, "\nSession completed successfully.")
				return nil
			}
		},
	}

	cmd.Flags().BoolVar(&debug, "debug", false, "Enable debug output")
	cmd.Flags().BoolVar(&watch, "watch", false, "Wait for instance ID if not found")
	return cmd
}
