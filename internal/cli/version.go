package cli

import (
	"fmt"
	"os"

	"roc/internal/version"

	"github.com/spf13/cobra"
)

func NewVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Display the version of roc",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(os.Stdout, "%s\n", version.Version)
		},
	}

	return cmd
}

