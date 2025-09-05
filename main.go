package main

import (
	"context"
	"fmt"
	"os"

	"roc/internal/cli"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
)

func main() {
	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRetryer(func() aws.Retryer {
			return retry.AddWithMaxAttempts(retry.NewStandard(), 10)
		}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to load SDK config: %v\n", err)
		os.Exit(1)
	}

	if err := cli.NewRootCmd(cli.NewStack(cfg)).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
