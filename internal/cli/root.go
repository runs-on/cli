package cli

import (
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/spf13/cobra"
)

type RunsOnConfig struct {
	StackName           string
	AppRunnerServiceArn string
	EC2LogGroupArn      string
	BucketConfig        string
	AWSConfig           aws.Config
}

func getStackOutputs(cmd *cobra.Command) (*RunsOnConfig, error) {
	stackName := cmd.Flag("stack").Value.String()
	cfg := cmd.Context().Value("aws_config").(aws.Config)

	cfn := cloudformation.NewFromConfig(cfg)
	out, err := cfn.DescribeStacks(cmd.Context(), &cloudformation.DescribeStacksInput{
		StackName: &stackName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe stack: %w", err)
	}
	if len(out.Stacks) == 0 {
		return nil, fmt.Errorf("stack %s not found", stackName)
	}

	config := &RunsOnConfig{
		StackName: stackName,
		AWSConfig: cfg,
	}

	for _, output := range out.Stacks[0].Outputs {
		switch *output.OutputKey {
		case "RunsOnServiceArn":
			config.AppRunnerServiceArn = *output.OutputValue
		case "RunsOnEC2InstanceLogGroupArn":
			config.EC2LogGroupArn = *output.OutputValue
		case "RunsOnBucketConfig":
			config.BucketConfig = *output.OutputValue
		}
	}
	return config, nil
}

func NewRootCmd() *cobra.Command {
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

	defaultStack := os.Getenv("RUNS_ON_STACK_NAME")
	if defaultStack == "" {
		defaultStack = "runs-on"
	}

	cmd.PersistentFlags().String("stack", defaultStack, "CloudFormation stack name (can also be set via RUNS_ON_STACK_NAME env var)")

	cmd.AddCommand(
		NewLogsCmd(),
		NewConnectCmd(),
		NewStackCmd(),
	)

	return cmd
}
