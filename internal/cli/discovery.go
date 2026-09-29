package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	ListSecrets(ctx context.Context, params *secretsmanager.ListSecretsInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error)
}

type taggedResourcesAPI interface {
	GetResources(ctx context.Context, params *resourcegroupstaggingapi.GetResourcesInput, optFns ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error)
}

type stackConfigSecretValue struct {
	JobDiagnosticsResolverFunctionName string `json:"JobDiagnosticsResolverFunctionName"`
	IngressURL                         string `json:"IngressURL"`
	ServiceLogGroupName                string `json:"ServiceLogGroupName"`
	EC2InstanceLogGroupArn             string `json:"Ec2InstanceLogGroupArn"`
	BucketCache                        string `json:"BucketCache"`
}

type fleetConfigSecretValue struct {
	Infra struct {
		JobDiagnosticsResolverFunctionName string `json:"job_diagnostics_resolver_function_name"`
		ServiceLogGroupName                string `json:"service_log_group_name"`
		EC2InstanceLogGroup                string `json:"ec2_instance_log_group"`
		BucketCache                        string `json:"bucket_cache"`
	} `json:"infra"`
}

func stackConfigSecretID(stackName string) string {
	return fmt.Sprintf("/runs-on/%s/stack-config", strings.TrimSpace(stackName))
}

func fleetConfigSecretID(stackName string) string {
	return fmt.Sprintf("/runs-on/%s/fleet-config", strings.TrimSpace(stackName))
}

// discoverResources loads the stable stack metadata that roc needs from the
// standard stack config secret for the selected stack.
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
			return loadFleetRunsOnConfig(ctx, client, stackName, cfg)
		}
		return nil, fmt.Errorf("load stack config secret %s: %w", secretID, err)
	}
	if output.SecretString == nil || strings.TrimSpace(*output.SecretString) == "" {
		return nil, fmt.Errorf("stack config secret %s is empty", secretID)
	}

	return parseRunsOnConfig(stackName, cfg, *output.SecretString)
}

// formatMissingStackError explains that neither the Flex nor the Fleet config
// secret exists, and lists the stacks that do, so a mistyped --stack or a wrong
// region is obvious.
func formatMissingStackError(ctx context.Context, client stackConfigSecretAPI, stackName string, cfg aws.Config) error {
	where := "AWS Secrets Manager"
	if region := strings.TrimSpace(cfg.Region); region != "" {
		where += " region " + region
	}
	lines := []string{fmt.Sprintf("RunsOn stack %q not found in %s (no %s or %s secret).",
		stackName, where, stackConfigSecretID(stackName), fleetConfigSecretID(stackName))}

	// Listing is a hint only: callers without secretsmanager:ListSecrets still
	// get the rest of the message.
	if stacks, err := listRunsOnStacks(ctx, client); err == nil {
		switch len(stacks) {
		case 0:
			lines = append(lines, "No RunsOn stacks were found there.")
		case 1:
			lines = append(lines, fmt.Sprintf("Did you mean --stack %s?", stacks[0]))
		default:
			lines = append(lines, "Stacks found there: "+strings.Join(stacks, ", "))
		}
	}
	lines = append(lines, "Make sure the selected stack name is correct and AWS_REGION points to the stack's AWS region.")
	return errors.New(strings.Join(lines, "\n"))
}

// listRunsOnStacks returns the sorted names of the stacks that have a Flex or
// Fleet config secret in the client's region.
func listRunsOnStacks(ctx context.Context, client stackConfigSecretAPI) ([]string, error) {
	const prefix = "/runs-on/"
	paginator := secretsmanager.NewListSecretsPaginator(client, &secretsmanager.ListSecretsInput{
		Filters: []secretstypes.Filter{{
			Key:    secretstypes.FilterNameStringTypeName,
			Values: []string{prefix},
		}},
	})

	var stacks []string
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, secret := range page.SecretList {
			// The name filter is a case-insensitive prefix match.
			rest, ok := strings.CutPrefix(aws.ToString(secret.Name), prefix)
			if !ok {
				continue
			}
			stack, kind, ok := strings.Cut(rest, "/")
			if ok && stack != "" && (kind == "stack-config" || kind == "fleet-config") {
				stacks = append(stacks, stack)
			}
		}
	}
	slices.Sort(stacks)
	return slices.Compact(stacks), nil
}

func isSecretNotFound(err error) bool {
	var notFound *secretstypes.ResourceNotFoundException
	return errors.As(err, &notFound)
}

func loadFleetRunsOnConfig(ctx context.Context, client stackConfigSecretAPI, stackName string, cfg aws.Config) (*RunsOnConfig, error) {
	secretID := fleetConfigSecretID(stackName)
	output, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretID),
	})
	if err != nil {
		if isSecretNotFound(err) {
			return nil, formatMissingStackError(ctx, client, stackName, cfg)
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
		Product:                productFlex,
		IngressURL:             normalizeDoctorServiceURL(secret.IngressURL),
		ServiceLogGroupName:    strings.TrimSpace(secret.ServiceLogGroupName),
		EC2InstanceLogGroupArn: normalizeCloudWatchLogGroupIdentifier(secret.EC2InstanceLogGroupArn),
		JobDiagnosticsResolver: strings.TrimSpace(secret.JobDiagnosticsResolverFunctionName),
		CacheBucket:            strings.TrimSpace(secret.BucketCache),
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
		Product:                productFleet,
		ServiceLogGroupName:    strings.TrimSpace(secret.Infra.ServiceLogGroupName),
		EC2InstanceLogGroupArn: normalizeCloudWatchLogGroupIdentifier(secret.Infra.EC2InstanceLogGroup),
		JobDiagnosticsResolver: strings.TrimSpace(secret.Infra.JobDiagnosticsResolverFunctionName),
		CacheBucket:            strings.TrimSpace(secret.Infra.BucketCache),
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

// Job lookups go through the stack's resolver Lambda, which ships with the
// tables it reads, so roc never depends on their item layout.
func (c *RunsOnConfig) validateJobLookup() error {
	if c.JobDiagnosticsResolver == "" {
		return fmt.Errorf("job diagnostics resolver Lambda not found for stack %q; make sure the roc CLI version matches the deployed RunsOn stack version", c.StackName)
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
