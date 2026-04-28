package cli

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/spf13/cobra"
)

type Stack struct {
	cfg aws.Config
}

// getStackOutputs loads the stable stack metadata that roc needs from the
// standard stack config secret for the selected stack.
func (s *Stack) getStackOutputs(cmd *cobra.Command) (*RunsOnConfig, error) {
	return s.discoverResources(cmd)
}

func NewStack(cfg aws.Config) *Stack {
	return &Stack{cfg: cfg}
}

func NewStackCmd(stack *Stack) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stack",
		Short: "RunsOn stack management commands",
		Long: `Commands for managing and troubleshooting your RunsOn CloudFormation stack.

These commands operate on your deployed RunsOn CloudFormation stack to help you
manage, monitor, and troubleshoot your RunsOn infrastructure.

The stack name can be specified using the --stack flag or by setting the
RUNS_ON_STACK_NAME environment variable (defaults to "runs-on").`,
	}

	cmd.AddCommand(
		NewDoctorCmd(stack),
		NewStackLogsCmd(stack),
	)

	return cmd
}
