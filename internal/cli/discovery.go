package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	secretstypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/spf13/cobra"
)

type stackConfigSecretAPI interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

type taggedResourcesAPI interface {
	GetResources(ctx context.Context, params *resourcegroupstaggingapi.GetResourcesInput, optFns ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error)
}

type stackConfigSecretValue struct {
	WorkflowJobsTable                  string `json:"WorkflowJobsTable"`
	JobDiagnosticsResolverFunctionName string `json:"JobDiagnosticsResolverFunctionName"`
	IngressURL                         string `json:"IngressURL"`
	ServiceLogGroupName                string `json:"ServiceLogGroupName"`
	EC2InstanceLogGroupArn             string `json:"Ec2InstanceLogGroupArn"`
}

type fleetConfigSecretValue struct {
	Infra struct {
		ClaimTableName                     string `json:"claim_table_name"`
		JobDiagnosticsResolverFunctionName string `json:"job_diagnostics_resolver_function_name"`
		ServiceLogGroupName                string `json:"service_log_group_name"`
		EC2InstanceLogGroup                string `json:"ec2_instance_log_group"`
	} `json:"infra"`
}

func stackConfigSecretID(stackName string) string {
	return fmt.Sprintf("/runs-on/%s/stack-config", strings.TrimSpace(stackName))
}

func fleetConfigSecretID(stackName string) string {
	return fmt.Sprintf("/runs-on/%s/fleet-config", strings.TrimSpace(stackName))
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
		if isSecretNotFound(err) {
			return loadFleetRunsOnConfig(ctx, client, stackName, cfg, err)
		}
		return nil, formatStackConfigSecretLoadError(secretID, cfg, err)
	}
	if output.SecretString == nil || strings.TrimSpace(*output.SecretString) == "" {
		return nil, fmt.Errorf("stack config secret %s is empty", secretID)
	}

	return parseRunsOnConfig(stackName, cfg, *output.SecretString)
}

func formatStackConfigSecretLoadError(secretID string, cfg aws.Config, err error) error {
	if isSecretNotFound(err) {
		message := fmt.Sprintf("the stack config secret %s couldn't be found in AWS Secrets Manager", secretID)
		if region := strings.TrimSpace(cfg.Region); region != "" {
			message += fmt.Sprintf(" region %s", region)
		}
		return fmt.Errorf("%s. Make sure the selected stack name is correct and AWS_REGION points to the stack's AWS region", message)
	}

	return fmt.Errorf("load stack config secret %s: %w", secretID, err)
}

func isSecretNotFound(err error) bool {
	var notFound *secretstypes.ResourceNotFoundException
	return errors.As(err, &notFound)
}

func loadFleetRunsOnConfig(ctx context.Context, client stackConfigSecretAPI, stackName string, cfg aws.Config, stackErr error) (*RunsOnConfig, error) {
	secretID := fleetConfigSecretID(stackName)
	output, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretID),
	})
	if err != nil {
		if isSecretNotFound(err) {
			return nil, formatStackConfigSecretLoadError(stackConfigSecretID(stackName), cfg, stackErr)
		}
		return nil, fmt.Errorf("load fleet config secret %s: %w", secretID, err)
	}
	if output.SecretString == nil || strings.TrimSpace(*output.SecretString) == "" {
		return nil, fmt.Errorf("fleet config secret %s is empty", secretID)
	}
	return parseFleetRunsOnConfig(stackName, cfg, *output.SecretString)
}

func parseRunsOnConfig(stackName string, cfg aws.Config, secretValue string) (*RunsOnConfig, error) {
	var secret stackConfigSecretValue
	if err := json.Unmarshal([]byte(secretValue), &secret); err != nil {
		return nil, fmt.Errorf("parse stack config: %w", err)
	}

	return &RunsOnConfig{
		StackName:              strings.TrimSpace(stackName),
		Product:                "flex",
		IngressURL:             normalizeDoctorServiceURL(secret.IngressURL),
		ServiceLogGroupName:    strings.TrimSpace(secret.ServiceLogGroupName),
		EC2InstanceLogGroupArn: normalizeCloudWatchLogGroupIdentifier(secret.EC2InstanceLogGroupArn),
		WorkflowJobsTable:      strings.TrimSpace(secret.WorkflowJobsTable),
		JobDiagnosticsResolver: strings.TrimSpace(secret.JobDiagnosticsResolverFunctionName),
		AWSConfig:              cfg,
	}, nil
}

func parseFleetRunsOnConfig(stackName string, cfg aws.Config, secretValue string) (*RunsOnConfig, error) {
	var secret fleetConfigSecretValue
	if err := json.Unmarshal([]byte(secretValue), &secret); err != nil {
		return nil, fmt.Errorf("parse fleet config: %w", err)
	}

	return &RunsOnConfig{
		StackName:              strings.TrimSpace(stackName),
		Product:                "fleet",
		ServiceLogGroupName:    strings.TrimSpace(secret.Infra.ServiceLogGroupName),
		EC2InstanceLogGroupArn: normalizeCloudWatchLogGroupIdentifier(secret.Infra.EC2InstanceLogGroup),
		ClaimTableName:         strings.TrimSpace(secret.Infra.ClaimTableName),
		JobDiagnosticsResolver: strings.TrimSpace(secret.Infra.JobDiagnosticsResolverFunctionName),
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
	if c.Product == "fleet" {
		if c.ClaimTableName == "" {
			return fmt.Errorf("fleet claims table not found for stack %q", c.StackName)
		}
		if c.JobDiagnosticsResolver == "" {
			return fmt.Errorf("job diagnostics resolver Lambda not found for stack %q; make sure the roc CLI version matches the deployed RunsOn stack version", c.StackName)
		}
		return nil
	}
	if c.WorkflowJobsTable == "" {
		return fmt.Errorf("workflow jobs table not found for stack %q", c.StackName)
	}
	return nil
}

func (c *RunsOnConfig) validateJobLogs() error {
	if err := c.validateJobLookup(); err != nil {
		return err
	}
	if c.JobDiagnosticsResolver == "" {
		return fmt.Errorf("job diagnostics resolver Lambda not found for stack %q; make sure the roc CLI version matches the deployed RunsOn stack version", c.StackName)
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
