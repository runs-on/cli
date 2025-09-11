package cli

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/spf13/cobra"
)

type Stack struct {
	cfg aws.Config
}

func (s *Stack) getStackOutputs(cmd *cobra.Command) (*RunsOnConfig, error) {
	stackName := cmd.Flag("stack").Value.String()
	cfg := s.cfg

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
