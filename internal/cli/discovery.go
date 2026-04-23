package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/spf13/cobra"
)

type stackConfigSecretAPI interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

type taggedResourcesAPI interface {
	GetResources(ctx context.Context, params *resourcegroupstaggingapi.GetResourcesInput, optFns ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error)
}

type stackConfigSecretValue struct {
	WorkflowJobsTable      string `json:"WorkflowJobsTable"`
	IngressURL             string `json:"IngressURL"`
	ServiceLogGroupName    string `json:"ServiceLogGroupName"`
	EC2InstanceLogGroupArn string `json:"Ec2InstanceLogGroupArn"`
}

func stackConfigSecretID(stackName string) string {
	return fmt.Sprintf("/runs-on/%s/stack-config", strings.TrimSpace(stackName))
}

func (s *Stack) discoverResources(cmd *cobra.Command) (*RunsOnConfig, error) {
	stackName := strings.TrimSpace(cmd.Flag("stack").Value.String())
	client := secretsmanager.NewFromConfig(s.cfg)
	return loadRunsOnConfig(cmd.Context(), client, stackName, s.cfg)
}

func loadRunsOnConfig(ctx context.Context, client stackConfigSecretAPI, stackName string, cfg aws.Config) (*RunsOnConfig, error) {
	if client == nil {
		return nil, fmt.Errorf("stack config client is required")
	}
	stackName = strings.TrimSpace(stackName)
	if stackName == "" {
		return nil, fmt.Errorf("stack name is required")
	}

	secretID := stackConfigSecretID(stackName)
	output, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretID),
	})
	if err != nil {
		return nil, fmt.Errorf("load stack config secret %s: %w", secretID, err)
	}
	if output.SecretString == nil || strings.TrimSpace(*output.SecretString) == "" {
		return nil, fmt.Errorf("stack config secret %s is empty", secretID)
	}

	return parseRunsOnConfig(stackName, cfg, *output.SecretString)
}

func parseRunsOnConfig(stackName string, cfg aws.Config, secretValue string) (*RunsOnConfig, error) {
	var secret stackConfigSecretValue
	if err := json.Unmarshal([]byte(secretValue), &secret); err != nil {
		return nil, fmt.Errorf("parse stack config: %w", err)
	}

	return &RunsOnConfig{
		StackName:              strings.TrimSpace(stackName),
		IngressURL:             normalizeDoctorServiceURL(secret.IngressURL),
		ServiceLogGroupName:    strings.TrimSpace(secret.ServiceLogGroupName),
		EC2InstanceLogGroupArn: normalizeCloudWatchLogGroupIdentifier(secret.EC2InstanceLogGroupArn),
		WorkflowJobsTable:      strings.TrimSpace(secret.WorkflowJobsTable),
		AWSConfig:              cfg,
	}, nil
}

func normalizeCloudWatchLogGroupIdentifier(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	identifier = strings.TrimSuffix(identifier, ":*")
	identifier = strings.TrimSuffix(identifier, ":log-stream")
	return identifier
}

func discoverTaggedECSServiceARN(ctx context.Context, client taggedResourcesAPI, stackName string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("tagged resources client is required")
	}
	stackName = strings.TrimSpace(stackName)
	if stackName == "" {
		return "", fmt.Errorf("stack name is required")
	}

	input := &resourcegroupstaggingapi.GetResourcesInput{
		ResourceTypeFilters: []string{"ecs:service"},
		TagFilters: []tagtypes.TagFilter{
			{
				Key:    aws.String("runs-on-stack-name"),
				Values: []string{stackName},
			},
		},
	}

	var matches []string
	for {
		output, err := client.GetResources(ctx, input)
		if err != nil {
			return "", fmt.Errorf("discover ecs service for stack %q: %w", stackName, err)
		}
		for _, resource := range output.ResourceTagMappingList {
			arn := strings.TrimSpace(aws.ToString(resource.ResourceARN))
			if arn != "" {
				matches = append(matches, arn)
			}
		}

		token := strings.TrimSpace(aws.ToString(output.PaginationToken))
		if token == "" {
			break
		}
		input.PaginationToken = aws.String(token)
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("ecs service not found for stack %q", stackName)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("multiple ecs services found for stack %q", stackName)
	}
}

func (c *RunsOnConfig) validateJobLookup() error {
	if c.WorkflowJobsTable == "" {
		return fmt.Errorf("workflow jobs table not found for stack %q", c.StackName)
	}
	return nil
}

func (c *RunsOnConfig) validateJobLogs() error {
	if err := c.validateJobLookup(); err != nil {
		return err
	}
	if c.EC2InstanceLogGroupArn == "" {
		return fmt.Errorf("EC2 instance log group not found for stack %q", c.StackName)
	}
	if c.ServiceLogGroupName == "" {
		return fmt.Errorf("application log group not found for stack %q", c.StackName)
	}
	return nil
}

func (c *RunsOnConfig) validateStackLogs() error {
	if c.ServiceLogGroupName == "" {
		return fmt.Errorf("application log group not found for stack %q", c.StackName)
	}
	return nil
}
