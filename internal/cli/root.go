package cli

import (
	"io"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"
)

type RunsOnConfig struct {
	StackName              string
	Product                runsOnProduct
	IngressURL             string
	ServiceLogGroupName    string
	EC2InstanceLogGroupArn string
	JobDiagnosticsResolver string
	CacheBucket            string
	AWSConfig              aws.Config
}

func NewRootCmd(stack *Stack) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "roc",
		Short: "RunsOn CLI",
		// main prints the returned error once.
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Flags and arguments are valid by now, so a failure is not a
			// usage problem.
			cmd.SilenceUsage = true
			return nil
		},
	}

	defaultStack := "runs-on"
	for _, envVar := range []string{"RUNS_ON_STACK_NAME", "RUNS_ON_STACK"} {
		if stackName, ok := os.LookupEnv(envVar); ok {
			defaultStack = stackName
			break
		}
	}

	cmd.PersistentFlags().String("stack", defaultStack, "CloudFormation stack name")
	cmd.PersistentFlags().BoolP("debug", "d", false, "Enable debug output")

	cmd.AddCommand(
		NewLogsCmd(stack),
		NewConnectCmd(stack),
		NewInterruptCmd(stack),
		NewCleanupCmd(stack),
		NewStackCmd(stack),
		NewLintCmd(),
		NewVersionCmd(),
	)

	return cmd
}

// debugLogger returns the logger for --debug output, which goes to stderr.
func debugLogger(cmd *cobra.Command) *log.Logger {
	out := io.Discard
	if debug, _ := cmd.Flags().GetBool("debug"); debug {
		out = cmd.ErrOrStderr()
	}
	return log.New(out, "", 0)
}
