package cli

import (
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"
)

type RunsOnConfig struct {
	StackName           string
	AppRunnerServiceArn string
	EC2LogGroupArn      string
	BucketConfig        string
	AWSConfig           aws.Config
}

func NewRootCmd(stack *Stack) *cobra.Command {
	var noColor bool
	cmd := &cobra.Command{
		Use:   "roc",
		Short: "RunsOn CLI",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Skip for help command
			if cmd.Name() == "help" {
				return nil
			}

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
	cmd.PersistentFlags().BoolVar(&noColor, "no-color", false, "Disable color output")

	cmd.AddCommand(
		NewLogsCmd(stack),
		NewConnectCmd(stack),
		NewInterruptCmd(stack),
		NewStackCmd(stack),
		NewLintCmd(),
	)

	return cmd
}
