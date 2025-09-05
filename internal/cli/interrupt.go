package cli

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/fis"
	"github.com/aws/aws-sdk-go-v2/service/fis/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/spf13/cobra"
)

const (
	trustPolicy = `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Principal": {
					"Service": [
					  ["fis.amazonaws.com"]
					]
				},
				"Action": "sts:AssumeRole"
			}
		]
	}`
	rolePolicy = `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Sid": "AllowFISExperimentRoleSpotInstanceActions",
				"Effect": "Allow",
				"Action": [
					"ec2:SendSpotInstanceInterruptions"
				],
				"Resource": "arn:aws:ec2:*:*:instance/*"
			}
		]
	}`
	spotITNAction  = "aws:ec2:send-spot-instance-interruptions"
	fisRoleName    = "aws-fis-itn"
	fisTargetLimit = 5
)

func NewInterruptCmd(stack *Stack) *cobra.Command {
	var debug bool
	var wait bool
	var delay time.Duration
	var clean bool
	var skipChecks bool

	cmd := &cobra.Command{
		Use:           "interrupt JOB_ID|JOB_URL",
		Short:         "Trigger a spot interruption on the instance running a specific job",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := stack.getStackOutputs(cmd)
			if err != nil {
				return err
			}

			jobID := extractJobID(args[0])
			ctx := cmd.Context()

			logger := log.New(io.Discard, "", 0)
			if debug {
				logger.SetOutput(cmd.OutOrStderr())
			}

			s3Client := s3.NewFromConfig(config.AWSConfig)

			// Get instance ID from S3
			key := fmt.Sprintf("runs-on/db/jobs/%s/instance-id", jobID)
			var instanceID string

			for {
				out, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
					Bucket: &config.BucketConfig,
					Key:    &key,
				})
				if err != nil {
					if !wait {
						return fmt.Errorf("instance ID not found for job %s. Use -w to wait for instance", jobID)
					}
					logger.Printf("Waiting for instance ID for job %s...\n", jobID)
					time.Sleep(5 * time.Second)
					continue
				}
				defer out.Body.Close()

				data, err := io.ReadAll(out.Body)
				if err != nil {
					return err
				}
				instanceID = string(data)
				break
			}

			fmt.Printf("Found instance %s for job %s\n", instanceID, jobID)

			// Log region for debugging
			region := config.AWSConfig.Region
			logger.Printf("Using AWS region: %s\n", region)

			// Test basic AWS connectivity
			logger.Printf("Testing basic AWS connectivity...\n")
			stsClient := sts.NewFromConfig(config.AWSConfig)
			identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return fmt.Errorf("basic AWS connectivity test failed: %w\n\nThis could indicate:\n1. AWS credentials are not configured properly\n2. Network connectivity issues\n3. DNS resolution problems\n4. Regional service issues", err)
			}
			logger.Printf("✓ AWS connectivity verified (Account: %s)\n", *identity.Account)

			if !skipChecks {
				// Pre-flight checks for required services and permissions
				logger.Printf("Performing pre-flight checks...\n")

				// Check FIS service access
				fisClient := fis.NewFromConfig(config.AWSConfig)
				logger.Printf("Testing FIS service access...\n")
				_, err = fisClient.ListExperimentTemplates(ctx, &fis.ListExperimentTemplatesInput{})
				if err != nil {
					if strings.Contains(err.Error(), "ResolveEndpointV2") {
						return fmt.Errorf("AWS FIS service endpoint resolution failed in region %s.\n\nThis could indicate:\n1. FIS service is not available in this region\n2. Network/VPC restrictions preventing FIS access\n3. Service endpoint configuration issues\n\nTry using --skip-checks to bypass pre-flight validation, or contact AWS support.\n\nError: %v", region, err)
					}
					if strings.Contains(err.Error(), "AccessDenied") {
						return fmt.Errorf("insufficient permissions for AWS FIS in region %s.\n\nRequired permissions:\n- fis:ListExperimentTemplates\n- fis:CreateExperimentTemplate\n- fis:StartExperiment\n- fis:GetExperiment\n- fis:DeleteExperimentTemplate\n- ec2:DescribeInstances\n- iam:CreateRole\n- iam:PutRolePolicy\n- sts:GetCallerIdentity\n\nError: %v", region, err)
					}
					logger.Printf("FIS pre-flight check warning: %v\n", err)
				} else {
					logger.Printf("✓ FIS service access verified\n")
				}
			} else {
				logger.Printf("Skipping pre-flight checks as requested\n")
			}

			if !skipChecks {
				// Check EC2 instance details
				ec2Client := ec2.NewFromConfig(config.AWSConfig)
				logger.Printf("Verifying instance %s details...\n", instanceID)

				instanceResp, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
					InstanceIds: []string{instanceID},
				})
				if err != nil {
					return fmt.Errorf("failed to describe instance %s: %w\n\nTry using --skip-checks to bypass instance validation", instanceID, err)
				}

				if len(instanceResp.Reservations) == 0 || len(instanceResp.Reservations[0].Instances) == 0 {
					return fmt.Errorf("instance %s not found", instanceID)
				}

				instance := instanceResp.Reservations[0].Instances[0]
				logger.Printf("Instance lifecycle: %v, state: %v\n", instance.InstanceLifecycle, instance.State.Name)

				if instance.InstanceLifecycle != "spot" {
					return fmt.Errorf("instance %s is not a spot instance (lifecycle: %v). Spot interruptions can only be triggered on spot instances", instanceID, instance.InstanceLifecycle)
				}

				if instance.State.Name != "running" {
					return fmt.Errorf("instance %s is not running (state: %v). Instance must be running to trigger spot interruption", instanceID, instance.State.Name)
				}

				logger.Printf("✓ Instance %s is a running spot instance\n", instanceID)
			} else {
				logger.Printf("Skipping instance validation (assuming %s is a running spot instance)\n", instanceID)
			}

			// Create AWS clients
			fisClient := fis.NewFromConfig(config.AWSConfig)
			iamClient := iam.NewFromConfig(config.AWSConfig)

			// Trigger spot interruption
			fmt.Printf("Triggering spot interruption on instance %s with %v delay in region %s...\n", instanceID, delay, region)

			experiment, err := createSpotInterruption(ctx, fisClient, iamClient, stsClient, []string{instanceID}, delay, region, logger)
			if err != nil {
				return fmt.Errorf("failed to trigger spot interruption in region %s: %w\n\nTroubleshooting:\n1. Ensure AWS FIS is available in your region\n2. Check IAM permissions for FIS, EC2, and IAM services\n3. Verify the instance %s exists and is a spot instance", region, err, instanceID)
			}

			fmt.Printf("Started FIS experiment: %s\n", *experiment.Id)

			// Monitor experiment
			if err := monitorExperiment(ctx, fisClient, experiment, delay, clean, logger); err != nil {
				return fmt.Errorf("error monitoring experiment: %w", err)
			}

			fmt.Printf("Spot interruption completed for instance %s\n", instanceID)
			return nil
		},
	}

	cmd.Flags().BoolVar(&debug, "debug", false, "Enable debug output")
	cmd.Flags().BoolVarP(&wait, "wait", "w", false, "Wait for instance ID if not found")
	cmd.Flags().DurationVar(&delay, "delay", 5*time.Second, "Delay before interruption (e.g., 2m, 30s)")
	cmd.Flags().BoolVar(&clean, "clean", true, "Clean up FIS experiment after completion")
	cmd.Flags().BoolVar(&skipChecks, "skip-checks", false, "Skip pre-flight checks (use with caution)")

	return cmd
}

func createSpotInterruption(ctx context.Context, fisClient *fis.Client, iamClient *iam.Client, stsClient *sts.Client, instanceIDs []string, delay time.Duration, region string, logger *log.Logger) (*types.Experiment, error) {
	// Get account ID
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to get account ID: %w", err)
	}
	accountID := *identity.Account

	// Create or get FIS role
	roleARN, err := getOrCreateFISRole(ctx, iamClient, accountID, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create FIS role: %w", err)
	}

	// Create experiment template
	template := &fis.CreateExperimentTemplateInput{
		Actions:        map[string]types.CreateExperimentTemplateActionInput{},
		Targets:        map[string]types.CreateExperimentTemplateTargetInput{},
		StopConditions: []types.CreateExperimentTemplateStopConditionInput{{Source: aws.String("none")}},
		RoleArn:        roleARN,
		Description:    aws.String(fmt.Sprintf("trigger spot ITN for instances %v", instanceIDs)),
	}

	// Batch instances and create actions/targets
	for j, batch := range batchInstances(instanceIDs, fisTargetLimit) {
		key := fmt.Sprintf("itn%d", j)
		template.Actions[key] = types.CreateExperimentTemplateActionInput{
			ActionId: aws.String(spotITNAction),
			Parameters: map[string]string{
				// durationBeforeInterruption is the time before the instance is terminated, so we add 2 minutes
				// so that a user can configure the notification delay rather than the termination delay.
				"durationBeforeInterruption": fmt.Sprintf("PT%dS", int((time.Minute*2 + delay).Seconds())),
			},
			Targets: map[string]string{"SpotInstances": key},
		}
		template.Targets[key] = types.CreateExperimentTemplateTargetInput{
			ResourceType:  aws.String("aws:ec2:spot-instance"),
			SelectionMode: aws.String("ALL"),
			ResourceArns:  instanceIDsToARNs(batch, region, accountID),
		}
	}

	logger.Printf("Creating experiment template with role: %s\n", *roleARN)
	experimentTemplate, err := fisClient.CreateExperimentTemplate(ctx, template)
	if err != nil {
		return nil, fmt.Errorf("failed to create experiment template: %w", err)
	}

	logger.Printf("Starting experiment with template: %s\n", *experimentTemplate.ExperimentTemplate.Id)
	experiment, err := fisClient.StartExperiment(ctx, &fis.StartExperimentInput{
		ExperimentTemplateId: experimentTemplate.ExperimentTemplate.Id,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start experiment: %w", err)
	}

	return experiment.Experiment, nil
}

func getOrCreateFISRole(ctx context.Context, iamClient *iam.Client, accountID string, logger *log.Logger) (*string, error) {
	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, fisRoleName)

	// Try to create the role
	logger.Printf("Creating IAM role: %s\n", fisRoleName)
	out, err := iamClient.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 aws.String(fisRoleName),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
	})

	// If role already exists, return existing ARN
	if err != nil {
		if !strings.Contains(err.Error(), "EntityAlreadyExists") {
			return nil, fmt.Errorf("failed to create role: %w", err)
		}
		logger.Printf("Role %s already exists\n", fisRoleName)
		return &roleARN, nil
	}

	// Attach inline policy to new role
	logger.Printf("Attaching policy to role: %s\n", fisRoleName)
	_, err = iamClient.PutRolePolicy(ctx, &iam.PutRolePolicyInput{
		PolicyName:     aws.String(fmt.Sprintf("%s-policy", fisRoleName)),
		PolicyDocument: aws.String(rolePolicy),
		RoleName:       out.Role.RoleName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to attach policy to role: %w", err)
	}

	return out.Role.Arn, nil
}

func batchInstances(instanceIDs []string, size int) [][]string {
	instanceIDBatches := [][]string{}
	currentBatch := []string{}
	for i, instanceID := range instanceIDs {
		if i%size == 0 && len(currentBatch) > 0 {
			instanceIDBatches = append(instanceIDBatches, currentBatch)
			currentBatch = []string{}
		}
		currentBatch = append(currentBatch, instanceID)
	}
	if len(currentBatch) > 0 {
		instanceIDBatches = append(instanceIDBatches, currentBatch)
	}
	return instanceIDBatches
}

func instanceIDsToARNs(instanceIDs []string, region string, accountID string) []string {
	var arns []string
	for _, instanceID := range instanceIDs {
		arns = append(arns, fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", region, accountID, instanceID))
	}
	return arns
}

func monitorExperiment(ctx context.Context, fisClient *fis.Client, experiment *types.Experiment, delay time.Duration, clean bool, logger *log.Logger) error {
	logger.Printf("✅ Rebalance Recommendation sent\n")

	if clean {
		defer func() {
			logger.Printf("Cleaning up experiment template: %s\n", *experiment.ExperimentTemplateId)
			if _, err := fisClient.DeleteExperimentTemplate(ctx, &fis.DeleteExperimentTemplateInput{
				Id: experiment.ExperimentTemplateId,
			}); err != nil {
				logger.Printf("❌ Error cleaning up FIS Experiment template: %v\n", err)
			}
		}()
	}

	// Wait for experiment delay
	if experiment.StartTime != nil && time.Until(*experiment.StartTime) < delay {
		timeUntilStart := delay - time.Until(*experiment.StartTime)
		logger.Printf("⏳ Interruption will be sent in %d seconds\n", int(timeUntilStart.Seconds()))
		time.Sleep(timeUntilStart)
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			experimentUpdate, err := fisClient.GetExperiment(ctx, &fis.GetExperimentInput{Id: experiment.Id})
			if err != nil {
				return fmt.Errorf("failed to get experiment status: %w", err)
			}

			switch experimentUpdate.Experiment.State.Status {
			case types.ExperimentStatusPending:
				logger.Printf("⏰ Interruption Experiment is pending\n")
			case types.ExperimentStatusInitiating:
				logger.Printf("🔧 Interruption Experiment is initializing\n")
			case types.ExperimentStatusRunning:
				logger.Printf("🚀 Interruption Experiment is running\n")
			case types.ExperimentStatusFailed, types.ExperimentStatusStopped:
				if experimentUpdate.Experiment.State.Reason != nil {
					return fmt.Errorf("experiment failed: %s", *experimentUpdate.Experiment.State.Reason)
				}
				return fmt.Errorf("experiment failed with status: %s", experimentUpdate.Experiment.State.Status)
			case types.ExperimentStatusCompleted:
				logger.Printf("✅ Spot 2-minute Interruption Notification sent\n")
				time.Sleep(2 * time.Minute)
				logger.Printf("✅ Spot Instance Shutdown sent\n")
				return nil
			}
		case <-ctx.Done():
			return fmt.Errorf("monitoring timed out")
		}
	}
}
