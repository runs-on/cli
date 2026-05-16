package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/apprunner"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi/types"
	"github.com/spf13/cobra"
)

// discoverResources finds RunsOn resources using a 2-tier RGTA strategy
func (s *Stack) discoverResources(cmd *cobra.Command) (*RunsOnConfig, error) {
	stackName := cmd.Flag("stack").Value.String()
	ctx := cmd.Context()

	// Tier 1: Try fixed "runs-on-stack-name" tag (new deployments)
	if config, _ := s.discoverByTag(ctx, "runs-on-stack-name", stackName); config.isComplete() {
		return config, nil
	}

	// Tier 2: Discover tag key from AppRunner service (older stacks)
	// TODO: Remove this fallback once all users have upgraded to stacks with runs-on-stack-name tag
	tagKey, tagErr := s.discoverTagKeyFromAppRunner(ctx, stackName)
	if tagErr == nil && tagKey != "" {
		if config, _ := s.discoverByTag(ctx, tagKey, stackName); config.isComplete() {
			return config, nil
		}
	}

	return nil, fmt.Errorf("could not discover resources for stack %q", stackName)
}

// discoverByTag queries RGTA for resources with the given tag key=value
func (s *Stack) discoverByTag(ctx context.Context, tagKey, stackName string) (*RunsOnConfig, error) {
	client := resourcegroupstaggingapi.NewFromConfig(s.cfg)

	paginator := resourcegroupstaggingapi.NewGetResourcesPaginator(client, &resourcegroupstaggingapi.GetResourcesInput{
		TagFilters: []types.TagFilter{{
			Key:    aws.String(tagKey),
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
			return config, fmt.Errorf("failed to query resources: %w", err)
		}

		for _, resource := range page.ResourceTagMappingList {
			arn := *resource.ResourceARN
			classifyResource(config, arn, resource.Tags, stackName)
		}
	}

	return config, nil
}

// classifyResource determines resource type from runs-on-resource tag (TF) or ARN pattern (CF fallback)
func classifyResource(config *RunsOnConfig, arn string, tags []types.Tag, stackName string) {
	resourceType := getTagValue(tags, "runs-on-resource")

	switch resourceType {
	// TF deployments have runs-on-resource tag
	case "apprunner-service":
		config.AppRunnerServiceArn = arn
	case "config-bucket":
		config.BucketConfig = extractBucketName(arn)
	case "ec2-log-group":
		config.EC2LogGroupArn = arn
	default:
		// CF fallback: detect by ARN pattern
		switch {
		case isAppRunnerService(arn):
			config.AppRunnerServiceArn = arn
		case isS3Bucket(arn) && isConfigBucket(arn, tags):
			config.BucketConfig = extractBucketName(arn)
		case isCloudWatchLogGroup(arn) && isEC2LogGroup(arn, stackName):
			config.EC2LogGroupArn = arn
		}
	}
}

// discoverTagKeyFromAppRunner finds the tag key used for stack identification
// by searching all AppRunner services for one with a tag value matching stackName
func (s *Stack) discoverTagKeyFromAppRunner(ctx context.Context, stackName string) (string, error) {
	arClient := apprunner.NewFromConfig(s.cfg)

	// List all AppRunner services
	paginator := apprunner.NewListServicesPaginator(arClient, &apprunner.ListServicesInput{})

	pageCount := 0
	serviceCount := 0
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to list AppRunner services (page %d): %w", pageCount, err)
		}
		pageCount++
		serviceCount += len(page.ServiceSummaryList)

		// Check each service's tags for a value matching stackName
		for _, svc := range page.ServiceSummaryList {
			tagsResult, err := arClient.ListTagsForResource(ctx, &apprunner.ListTagsForResourceInput{
				ResourceArn: svc.ServiceArn,
			})
			if err != nil {
				continue // Skip services we can't get tags for
			}

			// Find which tag key has value = stackName
			for _, tag := range tagsResult.Tags {
				if tag.Key != nil && tag.Value != nil && *tag.Value == stackName {
					return *tag.Key, nil
				}
			}
		}
	}

	return "", fmt.Errorf("no AppRunner service found with tag value %s (searched %d pages, %d services)", stackName, pageCount, serviceCount)
}

// isComplete checks if all required resources were discovered
func (c *RunsOnConfig) isComplete() bool {
	return c.AppRunnerServiceArn != "" && c.BucketConfig != "" && c.EC2LogGroupArn != ""
}

// ARN pattern detection helpers
func isAppRunnerService(arn string) bool {
	return strings.Contains(arn, ":apprunner:") && strings.Contains(arn, ":service/")
}

func isS3Bucket(arn string) bool {
	return strings.HasPrefix(arn, "arn:aws:s3:::")
}

func isCloudWatchLogGroup(arn string) bool {
	return strings.Contains(arn, ":logs:") && strings.Contains(arn, ":log-group:")
}

// isConfigBucket identifies config bucket by tag or naming convention
func isConfigBucket(arn string, tags []types.Tag) bool {
	// Check for runs-on/purpose=config tag (CF has this)
	for _, tag := range tags {
		if tag.Key != nil && *tag.Key == "runs-on/purpose" &&
			tag.Value != nil && *tag.Value == "config" {
			return true
		}
	}
	// Fall back to naming convention
	bucketName := extractBucketName(arn)
	return strings.Contains(bucketName, "-config")
}

// isEC2LogGroup identifies EC2 log group by naming convention
func isEC2LogGroup(arn string, stackName string) bool {
	// TF naming: {stackName}/ec2/instances
	if strings.Contains(arn, stackName+"/ec2/instances") {
		return true
	}
	// CF naming: {stackName}-EC2InstanceLogGroup-{suffix}
	if strings.Contains(arn, stackName+"-EC2InstanceLogGroup-") {
		return true
	}
	return false
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
