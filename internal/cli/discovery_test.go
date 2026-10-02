package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	secretstypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

type mockStackConfigSecretsClient struct {
	getSecretValue func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	listSecrets    func(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error)
}

func (m *mockStackConfigSecretsClient) GetSecretValue(ctx context.Context, input *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return m.getSecretValue(ctx, input, optFns...)
}

func (m *mockStackConfigSecretsClient) ListSecrets(ctx context.Context, input *secretsmanager.ListSecretsInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
	if m.listSecrets == nil {
		return nil, errors.New("AccessDeniedException: not authorized to perform secretsmanager:ListSecrets")
	}
	return m.listSecrets(ctx, input, optFns...)
}

type mockTaggedResourcesClient struct {
	getResources func(context.Context, *resourcegroupstaggingapi.GetResourcesInput, ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error)
}

func (m *mockTaggedResourcesClient) GetResources(ctx context.Context, input *resourcegroupstaggingapi.GetResourcesInput, optFns ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error) {
	return m.getResources(ctx, input, optFns...)
}

func TestLoadRunsOnConfigFromStackSecret(t *testing.T) {
	t.Parallel()

	client := &mockStackConfigSecretsClient{
		getSecretValue: func(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
			if got := aws.ToString(input.SecretId); got != "/runs-on/runs-on-preview-v3/stack-config" {
				t.Fatalf("unexpected secret ID %q", got)
			}
			secret := `{"WorkflowJobsTable":"workflow-jobs","JobDiagnosticsResolverFunctionName":"runs-on-preview-v3-job-diagnostics-resolver","IngressURL":"example.execute-api.us-east-1.amazonaws.com/prod","ServiceLogGroupName":"/aws/ecs/runs-on-preview-v3/flexd","Ec2InstanceLogGroupArn":"arn:aws:logs:us-east-1:123456789012:log-group:runs-on-preview-v3/ec2/instances:*"}`
			return &secretsmanager.GetSecretValueOutput{SecretString: aws.String(secret)}, nil
		},
	}

	config, err := loadRunsOnConfig(context.Background(), client, "runs-on-preview-v3", aws.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("loadRunsOnConfig returned error: %v", err)
	}
	if config.StackName != "runs-on-preview-v3" {
		t.Fatalf("unexpected stack name %q", config.StackName)
	}
	if config.Product != "flex" {
		t.Fatalf("unexpected product %q", config.Product)
	}
	if config.IngressURL != "https://example.execute-api.us-east-1.amazonaws.com/prod" {
		t.Fatalf("unexpected ingress URL %q", config.IngressURL)
	}
	if config.ServiceLogGroupName != "/aws/ecs/runs-on-preview-v3/flexd" {
		t.Fatalf("unexpected service log group %q", config.ServiceLogGroupName)
	}
	if config.EC2InstanceLogGroupArn != "arn:aws:logs:us-east-1:123456789012:log-group:runs-on-preview-v3/ec2/instances" {
		t.Fatalf("unexpected EC2 log group ARN %q", config.EC2InstanceLogGroupArn)
	}
	if config.JobDiagnosticsResolver != "runs-on-preview-v3-job-diagnostics-resolver" {
		t.Fatalf("unexpected diagnostics resolver %q", config.JobDiagnosticsResolver)
	}
}

func TestLoadRunsOnConfigFromFleetSecret(t *testing.T) {
	t.Parallel()

	notFound := &secretstypes.ResourceNotFoundException{
		Message: aws.String("Secrets Manager can't find the specified secret."),
	}
	var calls []string
	client := &mockStackConfigSecretsClient{
		getSecretValue: func(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
			secretID := aws.ToString(input.SecretId)
			calls = append(calls, secretID)
			switch secretID {
			case "/runs-on/runs-on-preview-v3/stack-config":
				return nil, notFound
			case "/runs-on/runs-on-preview-v3/fleet-config":
				secret := `{"infra":{"claim_table_name":"runs-on-preview-v3-fleet-claims","job_diagnostics_resolver_function_name":"runs-on-preview-v3-job-diagnostics-resolver","service_log_group_name":"/aws/ecs/runs-on-preview-v3/fleetd","ec2_instance_log_group":"arn:aws:logs:us-east-1:123456789012:log-group:runs-on-preview-v3/ec2/instances:*"}}`
				return &secretsmanager.GetSecretValueOutput{SecretString: aws.String(secret)}, nil
			default:
				t.Fatalf("unexpected secret ID %q", secretID)
				return nil, nil
			}
		},
	}

	config, err := loadRunsOnConfig(context.Background(), client, "runs-on-preview-v3", aws.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatalf("loadRunsOnConfig returned error: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("secret calls = %v, want stack then fleet", calls)
	}
	if config.Product != "fleet" {
		t.Fatalf("unexpected product %q", config.Product)
	}
	if config.ServiceLogGroupName != "/aws/ecs/runs-on-preview-v3/fleetd" {
		t.Fatalf("service log group = %q", config.ServiceLogGroupName)
	}
	if config.EC2InstanceLogGroupArn != "arn:aws:logs:us-east-1:123456789012:log-group:runs-on-preview-v3/ec2/instances" {
		t.Fatalf("EC2 log group = %q", config.EC2InstanceLogGroupArn)
	}
	if config.JobDiagnosticsResolver != "runs-on-preview-v3-job-diagnostics-resolver" {
		t.Fatalf("diagnostics resolver = %q", config.JobDiagnosticsResolver)
	}
	if err := config.validateStackLogs(); err != nil {
		t.Fatalf("Fleet stack logs validation returned error: %v", err)
	}
}

// Commands check the stack config before any AWS read; the log streams rely
// on these checks.
func TestRunsOnConfigValidation(t *testing.T) {
	t.Parallel()

	const (
		noResolver = "CLI version matches the deployed RunsOn stack version"
		noAppLogs  = `application log group not found for stack "runs-on-preview-v3"`
	)
	tests := []struct {
		name     string
		validate func(*RunsOnConfig) error
		config   RunsOnConfig
		want     string
	}{
		{name: "flex job lookup without resolver", validate: (*RunsOnConfig).validateJobLookup, config: RunsOnConfig{Product: productFlex}, want: noResolver},
		{name: "fleet job lookup without resolver", validate: (*RunsOnConfig).validateJobLookup, config: RunsOnConfig{Product: productFleet}, want: noResolver},
		{name: "job logs without application log group", validate: (*RunsOnConfig).validateJobLogs, config: RunsOnConfig{JobDiagnosticsResolver: "resolver", EC2InstanceLogGroupArn: "arn:ec2"}, want: noAppLogs},
		{name: "stack logs without application log group", validate: (*RunsOnConfig).validateStackLogs, want: noAppLogs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.config.StackName = "runs-on-preview-v3"
			if err := tt.validate(&tt.config); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadRunsOnConfigRejectsEmptySecret(t *testing.T) {
	t.Parallel()

	client := &mockStackConfigSecretsClient{
		getSecretValue: func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
			return &secretsmanager.GetSecretValueOutput{}, nil
		},
	}

	_, err := loadRunsOnConfig(context.Background(), client, "runs-on-preview-v3", aws.Config{})
	if err == nil || err.Error() != "stack config secret /runs-on/runs-on-preview-v3/stack-config is empty" {
		t.Fatalf("expected empty secret error, got %v", err)
	}
}

func TestLoadRunsOnConfigExplainsMissingStack(t *testing.T) {
	t.Parallel()

	const header = `RunsOn stack "run-on-fleet-ue1" not found in AWS Secrets Manager region us-east-1 (no /runs-on/run-on-fleet-ue1/stack-config or /runs-on/run-on-fleet-ue1/fleet-config secret).`
	const footer = "Make sure the selected stack name is correct and AWS_REGION points to the stack's AWS region."
	listing := func(names ...string) func(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
		return func(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error) {
			output := &secretsmanager.ListSecretsOutput{}
			for _, name := range names {
				output.SecretList = append(output.SecretList, secretstypes.SecretListEntry{Name: aws.String(name)})
			}
			return output, nil
		}
	}

	for _, tc := range []struct {
		name        string
		listSecrets func(context.Context, *secretsmanager.ListSecretsInput, ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error)
		hint        string
	}{
		{
			name: "lists Flex and Fleet stacks",
			listSecrets: listing(
				"/runs-on/runs-on-fleet-ue1/fleet-config",
				"/runs-on/runs-on-fleet-ue1/github-app",
				"/RUNS-ON/other/stack-config",
				"/runs-on/runs-on/stack-config",
			),
			hint: "Stacks found there: runs-on, runs-on-fleet-ue1\n",
		},
		{
			name:        "names the only stack",
			listSecrets: listing("/runs-on/runs-on-fleet-ue1/fleet-config", "/runs-on/runs-on-fleet-ue1/github-app"),
			hint:        "Did you mean --stack runs-on-fleet-ue1?\n",
		},
		{
			name:        "says when the region has no stacks",
			listSecrets: listing("/runs-on/shared/github-app"),
			hint:        "No RunsOn stacks were found there.\n",
		},
		{
			name: "omits the hint when listing is denied",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := &mockStackConfigSecretsClient{
				getSecretValue: func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
					return nil, &secretstypes.ResourceNotFoundException{Message: aws.String("Secrets Manager can't find the specified secret.")}
				},
				listSecrets: tc.listSecrets,
			}

			_, err := loadRunsOnConfig(context.Background(), client, "run-on-fleet-ue1", aws.Config{Region: "us-east-1"})
			if err == nil {
				t.Fatal("expected missing stack error")
			}
			want := header + "\n" + tc.hint + footer
			if err.Error() != want {
				t.Fatalf("unexpected error:\nwant: %s\n got: %s", want, err.Error())
			}
		})
	}
}

func TestNormalizeCloudWatchLogGroupIdentifierStripsWildcards(t *testing.T) {
	t.Parallel()

	if got := normalizeCloudWatchLogGroupIdentifier("arn:aws:logs:us-east-1:123456789012:log-group:runs-on-preview-v3/ec2/instances:*"); got != "arn:aws:logs:us-east-1:123456789012:log-group:runs-on-preview-v3/ec2/instances" {
		t.Fatalf("unexpected normalized log group identifier %q", got)
	}
}

func TestParseRunsOnConfigRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	_, err := parseRunsOnConfig("runs-on-preview-v3", aws.Config{}, "{")
	if err == nil {
		t.Fatal("expected parseRunsOnConfig to fail")
	}
}

func TestDiscoverTaggedECSServiceARN(t *testing.T) {
	t.Parallel()

	client := &mockTaggedResourcesClient{
		getResources: func(_ context.Context, input *resourcegroupstaggingapi.GetResourcesInput, _ ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error) {
			if len(input.ResourceTypeFilters) != 1 || input.ResourceTypeFilters[0] != "ecs:service" {
				t.Fatalf("unexpected resource type filters: %v", input.ResourceTypeFilters)
			}
			if got := aws.ToString(input.TagFilters[0].Key); got != "runs-on-stack-name" {
				t.Fatalf("unexpected tag filter key %q", got)
			}
			if len(input.TagFilters[0].Values) != 1 || input.TagFilters[0].Values[0] != "runs-on-preview-v3" {
				t.Fatalf("unexpected tag filter values: %v", input.TagFilters[0].Values)
			}
			return &resourcegroupstaggingapi.GetResourcesOutput{
				ResourceTagMappingList: []tagtypes.ResourceTagMapping{
					{ResourceARN: aws.String("arn:aws:ecs:us-east-1:123456789012:service/runs-on-preview-v3/flexd")},
				},
			}, nil
		},
	}

	serviceARN, err := discoverTaggedECSServiceARN(context.Background(), client, "runs-on-preview-v3")
	if err != nil {
		t.Fatalf("discoverTaggedECSServiceARN returned error: %v", err)
	}
	if serviceARN != "arn:aws:ecs:us-east-1:123456789012:service/runs-on-preview-v3/flexd" {
		t.Fatalf("unexpected service ARN %q", serviceARN)
	}
}

func TestDiscoverTaggedECSServiceARNRequiresSingleMatch(t *testing.T) {
	t.Parallel()

	client := &mockTaggedResourcesClient{
		getResources: func(context.Context, *resourcegroupstaggingapi.GetResourcesInput, ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error) {
			return &resourcegroupstaggingapi.GetResourcesOutput{
				ResourceTagMappingList: []tagtypes.ResourceTagMapping{
					{ResourceARN: aws.String("arn:aws:ecs:us-east-1:123456789012:service/runs-on-preview-v3/flexd")},
					{ResourceARN: aws.String("arn:aws:ecs:us-east-1:123456789012:service/runs-on-preview-v3/flexd-canary")},
				},
			}, nil
		},
	}

	_, err := discoverTaggedECSServiceARN(context.Background(), client, "runs-on-preview-v3")
	if err == nil || err.Error() != `multiple ecs services found for stack "runs-on-preview-v3"` {
		t.Fatalf("expected multiple service error, got %v", err)
	}
}

func TestDiscoverTaggedECSServiceARNPropagatesLookupErrors(t *testing.T) {
	t.Parallel()

	client := &mockTaggedResourcesClient{
		getResources: func(context.Context, *resourcegroupstaggingapi.GetResourcesInput, ...func(*resourcegroupstaggingapi.Options)) (*resourcegroupstaggingapi.GetResourcesOutput, error) {
			return nil, errors.New("boom")
		},
	}

	_, err := discoverTaggedECSServiceARN(context.Background(), client, "runs-on-preview-v3")
	if err == nil || err.Error() != `discover ecs service for stack "runs-on-preview-v3": boom` {
		t.Fatalf("unexpected error %v", err)
	}
}
