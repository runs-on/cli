package cli

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/spf13/cobra"
)

// discoverResources uses RGTA to find all RunsOn resources by stack name tag
func (s *Stack) discoverResources(cmd *cobra.Command) (*RunsOnConfig, error) {
	stackName := cmd.Flag("stack").Value.String()
	ctx := cmd.Context()

	client := resourcegroupstaggingapi.NewFromConfig(s.cfg)

	// Query all resources with runs-on-stack-name tag
	paginator := resourcegroupstaggingapi.NewGetResourcesPaginator(client, &resourcegroupstaggingapi.GetResourcesInput{
		TagFilters: []types.TagFilter{{
			Key:    aws.String("runs-on-stack-name"),
			Values: []string{stackName},
		}},
	})

	config := &RunsOnConfig{
		StackName: stackName,
		AWSConfig: s.cfg,
	}

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to query resources: %w", err)
		}

		for _, resource := range page.ResourceTagMappingList {
			resourceType := getTagValue(resource.Tags, "runs-on-resource")
			switch resourceType {
			case "apprunner-service":
				config.AppRunnerServiceArn = *resource.ResourceARN
			case "config-bucket":
				config.BucketConfig = extractBucketName(*resource.ResourceARN)
			case "ec2-log-group":
				config.EC2LogGroupArn = *resource.ResourceARN
			}
		}
	}

	// Validate that required resources were found
	if err := config.validate(stackName); err != nil {
		return nil, err
	}

	return config, nil
}

// validate checks that required resources were discovered
func (c *RunsOnConfig) validate(stackName string) error {
	var missing []string
	if c.AppRunnerServiceArn == "" {
		missing = append(missing, "AppRunner service (runs-on-resource=apprunner-service)")
	}
	if c.BucketConfig == "" {
		missing = append(missing, "Config bucket (runs-on-resource=config-bucket)")
	}
	if c.EC2LogGroupArn == "" {
		missing = append(missing, "EC2 log group (runs-on-resource=ec2-log-group)")
	}

	if len(missing) > 0 {
		return fmt.Errorf("no resources found for stack %q with required tags. Missing: %s\n"+
			"Ensure resources are tagged with 'runs-on-stack-name=%s' and appropriate 'runs-on-resource' values",
			stackName, strings.Join(missing, ", "), stackName)
	}
	return nil
}

// extractBucketName extracts the bucket name from an S3 ARN
// arn:aws:s3:::bucket-name -> bucket-name
func extractBucketName(arn string) string {
	parts := strings.Split(arn, ":::")
	if len(parts) == 2 {
		return parts[1]
	}
	return ""
}

// getTagValue finds a tag value by key from a list of tags
func getTagValue(tags []types.Tag, key string) string {
	for _, tag := range tags {
		if tag.Key != nil && *tag.Key == key && tag.Value != nil {
			return *tag.Value
		}
	}
	return ""
}
