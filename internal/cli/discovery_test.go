package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	tagtypes "github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

type mockStackConfigSecretsClient struct {
	getSecretValue func(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

func (m *mockStackConfigSecretsClient) GetSecretValue(ctx context.Context, input *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	return m.getSecretValue(ctx, input, optFns...)
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
			secret := `{"WorkflowJobsTable":"workflow-jobs","IngressURL":"example.execute-api.us-east-1.amazonaws.com/prod","ServiceLogGroupName":"/aws/ecs/runs-on-preview-v3/flexd","Ec2InstanceLogGroupArn":"arn:aws:logs:us-east-1:123456789012:log-group:runs-on-preview-v3/ec2/instances:*"}`
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
	if config.IngressURL != "https://example.execute-api.us-east-1.amazonaws.com/prod" {
		t.Fatalf("unexpected ingress URL %q", config.IngressURL)
	}
	if config.ServiceLogGroupName != "/aws/ecs/runs-on-preview-v3/flexd" {
		t.Fatalf("unexpected service log group %q", config.ServiceLogGroupName)
	}
	if config.EC2InstanceLogGroupArn != "arn:aws:logs:us-east-1:123456789012:log-group:runs-on-preview-v3/ec2/instances" {
		t.Fatalf("unexpected EC2 log group ARN %q", config.EC2InstanceLogGroupArn)
	}
	if config.WorkflowJobsTable != "workflow-jobs" {
		t.Fatalf("unexpected workflow jobs table %q", config.WorkflowJobsTable)
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
