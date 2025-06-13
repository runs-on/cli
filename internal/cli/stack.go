package cli

import (
	"github.com/spf13/cobra"
)

func NewStackCmd() *cobra.Command {
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
		NewDoctorCmd(),
	)

	return cmd
}
