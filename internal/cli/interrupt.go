package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/fis"
	"github.com/aws/aws-sdk-go-v2/service/fis/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"
)

const (
	trustPolicy = `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Effect": "Allow",
				"Principal": {
					"Service": "fis.amazonaws.com"
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
	// spotNoticeLead is how long EC2 waits between a spot interruption
	// notice and the interruption itself.
	spotNoticeLead         = 2 * time.Minute
	experimentPollInterval = 5 * time.Second
	templateCleanupTimeout = 10 * time.Second
)

// interruptFISAPI is the part of the FIS client that runs the experiment.
type interruptFISAPI interface {
	CreateExperimentTemplate(ctx context.Context, params *fis.CreateExperimentTemplateInput, optFns ...func(*fis.Options)) (*fis.CreateExperimentTemplateOutput, error)
	StartExperiment(ctx context.Context, params *fis.StartExperimentInput, optFns ...func(*fis.Options)) (*fis.StartExperimentOutput, error)
	GetExperiment(ctx context.Context, params *fis.GetExperimentInput, optFns ...func(*fis.Options)) (*fis.GetExperimentOutput, error)
	DeleteExperimentTemplate(ctx context.Context, params *fis.DeleteExperimentTemplateInput, optFns ...func(*fis.Options)) (*fis.DeleteExperimentTemplateOutput, error)
}

// fisRoleAPI is the part of the IAM client that finds or creates the role FIS
// assumes to interrupt the instance.
type fisRoleAPI interface {
	GetRole(ctx context.Context, params *iam.GetRoleInput, optFns ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	CreateRole(ctx context.Context, params *iam.CreateRoleInput, optFns ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	PutRolePolicy(ctx context.Context, params *iam.PutRolePolicyInput, optFns ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
}

func NewInterruptCmd(stack *Stack) *cobra.Command {
	var wait bool
	var delay time.Duration

	cmd := &cobra.Command{
		Use:           "interrupt JOB_URL",
		Short:         "Trigger a spot interruption on the instance running a specific job",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			job, err := parseGitHubJobURL(args[0])
			if err != nil {
				return err
			}
			config, err := stack.discoverResources(cmd)
			if err != nil {
				return err
			}
			if err := config.validateJobLookup(); err != nil {
				return err
			}

			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			logger := debugLogger(cmd)

			ec2Client := ec2.NewFromConfig(config.AWSConfig)
			facts, err := lookupWorkflowJobFacts(ctx, config, job, wait, logger)
			if err != nil {
				if !wait {
					return fmt.Errorf("%w. Use -w to wait for instance", err)
				}
				return err
			}
			instanceID := facts.CurrentInstanceID

			fmt.Fprintf(out, "Found instance %s for job %d\n", instanceID, job.JobID)

			// Log region for debugging
			region := config.AWSConfig.Region
			logger.Printf("Using AWS region: %s\n", region)

			// Test basic AWS connectivity
			logger.Printf("Testing basic AWS connectivity...\n")
			identity, err := sts.NewFromConfig(config.AWSConfig).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return fmt.Errorf("basic AWS connectivity test failed: %w\n\nThis could indicate:\n1. AWS credentials are not configured properly\n2. Network connectivity issues\n3. DNS resolution problems\n4. Regional service issues", err)
			}
			accountID := aws.ToString(identity.Account)
			logger.Printf("✓ AWS connectivity verified (Account: %s)\n", accountID)

			// Pre-flight checks for required services and permissions
			logger.Printf("Performing pre-flight checks...\n")

			// Check FIS service access
			fisClient := fis.NewFromConfig(config.AWSConfig)
			logger.Printf("Testing FIS service access...\n")
			_, err = fisClient.ListExperimentTemplates(ctx, &fis.ListExperimentTemplatesInput{})
			if err != nil {
				if isEndpointResolutionError(err) {
					return fmt.Errorf("AWS FIS service endpoint resolution failed in region %s.\n\nThis could indicate:\n1. FIS service is not available in this region\n2. Network/VPC restrictions preventing FIS access\n3. Service endpoint configuration issues\n\nContact AWS support or try a different region.\n\nError: %v", region, err)
				}
				if isAccessDenied(err) {
					return fmt.Errorf("insufficient permissions for AWS FIS in region %s.\n\n%s\nError: %v", region, interruptPermissionsHelp(), err)
				}
				logger.Printf("FIS pre-flight check warning: %v\n", err)
			} else {
				logger.Printf("✓ FIS service access verified\n")
			}

			// Check EC2 instance details
			logger.Printf("Verifying instance %s details...\n", instanceID)

			instanceResp, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
				InstanceIds: []string{instanceID},
			})
			if err != nil {
				return fmt.Errorf("failed to describe instance %s: %w", instanceID, err)
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

			// From here on roc creates FIS resources, so the first Ctrl-C (or
			// SIGTERM) stops watching and cleans up instead of ending roc; a
			// second one ends roc at once.
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			context.AfterFunc(ctx, stop)

			// Trigger spot interruption
			fmt.Fprintf(out, "Triggering spot interruption on instance %s with %v delay in region %s...\n", instanceID, delay, region)

			experiment, err := createSpotInterruption(ctx, fisClient, iam.NewFromConfig(config.AWSConfig), accountID, []string{instanceID}, delay, region, logger)
			if err != nil {
				if ctx.Err() != nil {
					return errors.New("stopped before the FIS experiment started; nothing was started")
				}
				return fmt.Errorf("failed to trigger spot interruption in region %s: %w\n\nTroubleshooting:\n1. Ensure AWS FIS is available in your region\n2. Check IAM permissions for FIS, EC2, and IAM services\n3. Verify the instance %s exists and is a spot instance", region, err, instanceID)
			}

			experimentID := aws.ToString(experiment.Id)
			fmt.Fprintf(out, "Started FIS experiment: %s\n", experimentID)

			if shutdownAt, err := monitorExperiment(ctx, fisClient, experiment, delay, out, logger); err != nil {
				if ctx.Err() == nil {
					return fmt.Errorf("error monitoring experiment: %w", err)
				}
				if shutdownAt.IsZero() {
					return fmt.Errorf("stopped watching FIS experiment %s; the spot interruption may already be in flight.\n"+
						"To stop the experiment, run:\n"+
						"  aws fis stop-experiment --id %s --region %s\n"+
						"Once the interruption notice has been sent, stopping the experiment does not undo it",
						experimentID, experimentID, region)
				}
				return fmt.Errorf("stopped watching FIS experiment %s after it sent the spot interruption notice; EC2 interrupts instance %s at about %s",
					experimentID, instanceID, shutdownAt.Format(time.TimeOnly))
			}

			fmt.Fprintf(out, "Spot interruption completed for instance %s\n", instanceID)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&wait, "wait", "w", false, "Wait for instance ID if not found")
	cmd.Flags().DurationVar(&delay, "delay", 5*time.Second, "Delay before interruption (e.g., 2m, 30s)")

	return cmd
}

// interruptPermissionsHelp lists the permissions for the calls roc interrupt
// makes once it has found the job's instance.
func interruptPermissionsHelp() string {
	return fmt.Sprintf(`Required permissions:
- fis:ListExperimentTemplates, fis:CreateExperimentTemplate, fis:StartExperiment,
  fis:GetExperiment and fis:DeleteExperimentTemplate
- ec2:DescribeInstances
- iam:GetRole and iam:PassRole on the %[1]s role
- iam:CreateRole and iam:PutRolePolicy, only the first time, while the %[1]s
  role does not exist yet
`, fisRoleName)
}

// isEndpointResolutionError reports whether the SDK could not resolve the
// service endpoint for the configured region.
func isEndpointResolutionError(err error) bool {
	// The SDK wraps these failures in untyped errors, so only the text identifies them.
	return strings.Contains(err.Error(), "failed to resolve service endpoint")
}

func isAccessDenied(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "AccessDenied", "AccessDeniedException":
		return true
	}
	return false
}

func createSpotInterruption(ctx context.Context, fisClient interruptFISAPI, iamClient fisRoleAPI, accountID string, instanceIDs []string, delay time.Duration, region string, logger *log.Logger) (*types.Experiment, error) {
	// Create or get FIS role
	roleARN, err := getOrCreateFISRole(ctx, iamClient, accountID, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create FIS role: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
				"durationBeforeInterruption": fmt.Sprintf("PT%dS", int((spotNoticeLead + delay).Seconds())),
			},
			Targets: map[string]string{"SpotInstances": key},
		}
		template.Targets[key] = types.CreateExperimentTemplateTargetInput{
			ResourceType:  aws.String("aws:ec2:spot-instance"),
			SelectionMode: aws.String("ALL"),
			ResourceArns:  instanceIDsToARNs(batch, region, accountID),
		}
	}

	// FIS may act on a request roc stopped waiting for, so these two run to
	// completion even if ctx is cancelled meanwhile: roc then knows which
	// template to delete and which experiment to report.
	uncancelled := context.WithoutCancel(ctx)

	logger.Printf("Creating experiment template with role: %s\n", *roleARN)
	experimentTemplate, err := fisClient.CreateExperimentTemplate(uncancelled, template)
	if err != nil {
		return nil, fmt.Errorf("failed to create experiment template: %w", err)
	}
	templateID := experimentTemplate.ExperimentTemplate.Id
	if err := ctx.Err(); err != nil {
		deleteExperimentTemplate(ctx, fisClient, templateID, logger)
		return nil, err
	}

	logger.Printf("Starting experiment with template: %s\n", aws.ToString(templateID))
	experiment, err := fisClient.StartExperiment(uncancelled, &fis.StartExperimentInput{
		ExperimentTemplateId: templateID,
	})
	if err != nil {
		deleteExperimentTemplate(ctx, fisClient, templateID, logger)
		return nil, fmt.Errorf("failed to start experiment: %w", err)
	}

	return experiment.Experiment, nil
}

func getOrCreateFISRole(ctx context.Context, iamClient fisRoleAPI, accountID string, logger *log.Logger) (*string, error) {
	roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s", accountID, fisRoleName)

	// Prefer a plain lookup: callers running under a restricted principal
	// (for example the CI role) may hold iam:GetRole/iam:PassRole on this
	// role without being allowed to create IAM roles at all.
	if _, err := iamClient.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(fisRoleName)}); err == nil {
		logger.Printf("Role %s already exists\n", fisRoleName)
		return &roleARN, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Creating the role and attaching its policy run to completion even if
	// ctx is cancelled meanwhile: later runs find the role with GetRole and
	// would never attach a policy a Ctrl-C skipped.
	uncancelled := context.WithoutCancel(ctx)

	// Try to create the role
	logger.Printf("Creating IAM role: %s\n", fisRoleName)
	out, err := iamClient.CreateRole(uncancelled, &iam.CreateRoleInput{
		RoleName:                 aws.String(fisRoleName),
		AssumeRolePolicyDocument: aws.String(trustPolicy),
	})

	// If role already exists, return existing ARN
	if err != nil {
		var exists *iamtypes.EntityAlreadyExistsException
		if !errors.As(err, &exists) {
			return nil, fmt.Errorf("failed to create role: %w", err)
		}
		logger.Printf("Role %s already exists\n", fisRoleName)
		return &roleARN, nil
	}

	// Attach inline policy to new role
	logger.Printf("Attaching policy to role: %s\n", fisRoleName)
	_, err = iamClient.PutRolePolicy(uncancelled, &iam.PutRolePolicyInput{
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

// monitorExperiment prints the experiment's progress to out until EC2 is due
// to interrupt the instance, and deletes the experiment template either way.
// Once FIS has sent the interruption notice, it returns about when EC2
// interrupts the instance. It returns ctx.Err() as soon as ctx is cancelled.
func monitorExperiment(ctx context.Context, fisClient interruptFISAPI, experiment *types.Experiment, delay time.Duration, out io.Writer, logger *log.Logger) (time.Time, error) {
	defer deleteExperimentTemplate(ctx, fisClient, experiment.ExperimentTemplateId, logger)

	reported := experimentStatus(experiment)
	reportExperimentStatus(out, reported)
	if delay > 0 {
		fmt.Fprintf(out, "Interruption notice due in %s\n", delay)
	}

	// FIS sends the notice delay after the experiment starts. Poll only from
	// then on, so a Completed status is not taken for a notice not sent yet.
	noticeDue := time.Now().Add(delay)
	if experiment.StartTime != nil {
		noticeDue = experiment.StartTime.Add(delay)
	}
	if err := sleepContext(ctx, time.Until(noticeDue)); err != nil {
		return time.Time{}, err
	}

	ticker := time.NewTicker(experimentPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-ticker.C:
		}

		experimentUpdate, err := fisClient.GetExperiment(ctx, &fis.GetExperimentInput{Id: experiment.Id})
		if err != nil {
			if ctx.Err() != nil {
				return time.Time{}, ctx.Err()
			}
			return time.Time{}, fmt.Errorf("failed to get experiment status: %w", err)
		}

		status := experimentStatus(experimentUpdate.Experiment)
		switch status {
		case types.ExperimentStatusFailed, types.ExperimentStatusStopped, types.ExperimentStatusCancelled:
			if reason := experimentUpdate.Experiment.State.Reason; reason != nil {
				return time.Time{}, fmt.Errorf("experiment %s: %s", status, *reason)
			}
			return time.Time{}, fmt.Errorf("experiment ended with status: %s", status)
		case types.ExperimentStatusCompleted:
			shutdownAt := time.Now().Add(spotNoticeLead)
			fmt.Fprintln(out, "Spot interruption notice sent")
			fmt.Fprintf(out, "Waiting %s for the instance shutdown\n", spotNoticeLead)
			if err := sleepContext(ctx, spotNoticeLead); err != nil {
				return shutdownAt, err
			}
			fmt.Fprintln(out, "Spot instance shutdown sent")
			return shutdownAt, nil
		}
		if status != reported {
			reportExperimentStatus(out, status)
			reported = status
		}
	}
}

func experimentStatus(experiment *types.Experiment) types.ExperimentStatus {
	if experiment == nil || experiment.State == nil {
		return ""
	}
	return experiment.State.Status
}

func reportExperimentStatus(out io.Writer, status types.ExperimentStatus) {
	switch status {
	case types.ExperimentStatusPending, types.ExperimentStatusInitiating, types.ExperimentStatusRunning:
		fmt.Fprintf(out, "FIS experiment %s\n", status)
	}
}

// deleteExperimentTemplate removes the one-off template even after ctx is
// cancelled, so an interrupted run does not leave it behind.
func deleteExperimentTemplate(ctx context.Context, fisClient interruptFISAPI, templateID *string, logger *log.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), templateCleanupTimeout)
	defer cancel()

	logger.Printf("Cleaning up experiment template: %s\n", aws.ToString(templateID))
	if _, err := fisClient.DeleteExperimentTemplate(ctx, &fis.DeleteExperimentTemplateInput{Id: templateID}); err != nil {
		logger.Printf("Error cleaning up FIS experiment template: %v\n", err)
	}
}

// sleepContext waits for d, returning ctx.Err() early if ctx is cancelled.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
