package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cloudtrailtypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

const fullLogWindowPadding = time.Hour

type fullLogManifest struct {
	StackName          string              `json:"stack_name,omitempty"`
	Region             string              `json:"region,omitempty"`
	JobID              int64               `json:"job_id"`
	RunID              int64               `json:"run_id,omitempty"`
	WindowStart        time.Time           `json:"window_start"`
	WindowEnd          time.Time           `json:"window_end"`
	AttemptedInstances []string            `json:"attempted_instances"`
	Errors             []fullArtifactError `json:"errors,omitempty"`
}

type fullArtifactError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type fullLogExporter struct {
	cwl        cloudWatchLogsAPI
	jobs       workflowJobsAPI
	ec2        ec2ConsoleAPI
	cloudtrail cloudTrailLookupAPI
	outputs    *StackOutputs
	jobsTable  string
	stackName  string
	region     string
}

type cloudTrailLookupAPI interface {
	LookupEvents(ctx context.Context, params *cloudtrail.LookupEventsInput, optFns ...func(*cloudtrail.Options)) (*cloudtrail.LookupEventsOutput, error)
}

func newFullLogExporter(config *RunsOnConfig) *fullLogExporter {
	return &fullLogExporter{
		cwl:        cloudwatchlogs.NewFromConfig(config.AWSConfig),
		jobs:       dynamodb.NewFromConfig(config.AWSConfig),
		ec2:        ec2.NewFromConfig(config.AWSConfig),
		cloudtrail: cloudtrail.NewFromConfig(config.AWSConfig),
		jobsTable:  config.WorkflowJobsTable,
		stackName:  config.StackName,
		region:     config.AWSConfig.Region,
		outputs: &StackOutputs{
			ServiceLogGroupName:    config.ServiceLogGroupName,
			EC2InstanceLogGroupArn: config.EC2InstanceLogGroupArn,
		},
	}
}

func (f *fullLogExporter) Export(ctx context.Context, jobID string) (string, error) {
	facts, err := findWorkflowJobFacts(ctx, f.jobs, f.jobsTable, jobID)
	if err != nil {
		return "", err
	}
	if facts == nil {
		return "", fmt.Errorf("job %s not found in workflow jobs table", jobID)
	}

	createdAt, err := facts.createdAtOrError()
	if err != nil {
		return "", err
	}
	windowStart := createdAt.Add(-fullLogWindowPadding)
	windowEnd := createdAt.Add(fullLogWindowPadding)
	instanceIDs := facts.AttemptedInstanceIDs
	parsedJobID := strconv.FormatInt(facts.JobID, 10)

	zipPath := fmt.Sprintf("roc-logs-%s-%s.zip", parsedJobID, time.Now().Format("2006-01-02-15-04-05"))

	archive, err := newArchiveWriter(zipPath)
	if err != nil {
		return "", fmt.Errorf("create full log archive: %w", err)
	}
	defer archive.Close()

	artifactErrors := make([]fullArtifactError, 0)
	addArtifactError := func(artifactPath string, err error) {
		if err == nil {
			return
		}
		artifactErrors = append(artifactErrors, fullArtifactError{Path: artifactPath, Error: err.Error()})
		_ = archive.writeJSON(errorPathFor(artifactPath), map[string]string{"error": err.Error()})
	}

	jobRecordPath := fmt.Sprintf("dynamodb/job-%s.ddb.json", parsedJobID)
	if data, err := facts.rawDynamoDBItemJSON(); err != nil {
		addArtifactError(jobRecordPath, err)
	} else if err := archive.writeBytes(jobRecordPath, prettyJSON(data)); err != nil {
		addArtifactError(jobRecordPath, err)
	}

	jobLogsPath := fmt.Sprintf("server/job-%s.jsonl", parsedJobID)
	if err := f.writeCloudWatchMessages(ctx, archive, jobLogsPath, cloudWatchLogRequest{
		LogGroupIdentifier: f.outputs.ServiceLogGroupName,
		FilterPattern:      fullJobFilterPattern(parsedJobID, instanceIDs),
		StartTime:          windowStart,
		EndTime:            windowEnd,
	}); err != nil {
		addArtifactError(jobLogsPath, err)
	}

	runLogsPath := fmt.Sprintf("server/run-%d.jsonl", facts.RunID)
	if facts.RunID == 0 {
		addArtifactError(runLogsPath, fmt.Errorf("workflow run ID for job %s is not available", parsedJobID))
	} else if err := f.writeCloudWatchMessages(ctx, archive, runLogsPath, cloudWatchLogRequest{
		LogGroupIdentifier: f.outputs.ServiceLogGroupName,
		FilterPattern:      runFilterPattern(facts.RunID),
		StartTime:          windowStart,
		EndTime:            windowEnd,
	}); err != nil {
		addArtifactError(runLogsPath, err)
	}

	for _, instanceID := range instanceIDs {
		instanceDir := path.Join("instances", instanceID)
		cloudTrailPath := path.Join(instanceDir, "cloudtrail.json")
		if err := f.writeCloudTrailEvents(ctx, archive, cloudTrailPath, instanceID, windowStart, windowEnd); err != nil {
			addArtifactError(cloudTrailPath, err)
		}

		consolePath := path.Join(instanceDir, "console.log")
		if err := f.writeConsoleLog(ctx, archive, consolePath, instanceID); err != nil {
			addArtifactError(consolePath, err)
		}

		agentPath := path.Join(instanceDir, "agent.jsonl")
		if err := f.writeCloudWatchMessages(ctx, archive, agentPath, cloudWatchLogRequest{
			LogGroupIdentifier:  f.outputs.EC2InstanceLogGroupArn,
			LogStreamNamePrefix: fmt.Sprintf("%s/", instanceID),
			StartTime:           windowStart,
			EndTime:             windowEnd,
		}); err != nil {
			addArtifactError(agentPath, err)
		}
	}

	manifest := fullLogManifest{
		StackName:          f.stackName,
		Region:             f.region,
		JobID:              facts.JobID,
		RunID:              facts.RunID,
		WindowStart:        windowStart.UTC(),
		WindowEnd:          windowEnd.UTC(),
		AttemptedInstances: instanceIDs,
		Errors:             artifactErrors,
	}
	if err := archive.writeJSON("manifest.json", manifest); err != nil {
		return zipPath, fmt.Errorf("write manifest.json: %w", err)
	}
	if err := archive.Close(); err != nil {
		return zipPath, fmt.Errorf("finalize full log archive: %w", err)
	}

	if len(artifactErrors) == 0 {
		return zipPath, nil
	}

	joined := make([]error, 0, len(artifactErrors))
	for _, artifactErr := range artifactErrors {
		joined = append(joined, fmt.Errorf("%s: %s", artifactErr.Path, artifactErr.Error))
	}
	return zipPath, fmt.Errorf("full log archive completed with %d artifact errors: %w", len(artifactErrors), errors.Join(joined...))
}

func fullJobFilterPattern(jobID string, instanceIDs []string) string {
	terms := []string{fmt.Sprintf(`( $.job_id = "%s" )`, jobID)}
	for _, instanceID := range instanceIDs {
		terms = append(terms, fmt.Sprintf(`( $.message = "*%s*" )`, instanceID))
	}
	return fmt.Sprintf("{ %s }", strings.Join(terms, " || "))
}

func runFilterPattern(runID int64) string {
	return fmt.Sprintf(`{ ( $.run_id = "%d" ) }`, runID)
}

type cloudWatchLogRequest struct {
	LogGroupIdentifier  string
	LogStreamNamePrefix string
	FilterPattern       string
	StartTime           time.Time
	EndTime             time.Time
}

func (f *fullLogExporter) writeCloudWatchMessages(ctx context.Context, archive *archiveWriter, path string, request cloudWatchLogRequest) error {
	if strings.TrimSpace(request.LogGroupIdentifier) == "" {
		return fmt.Errorf("CloudWatch log group is not configured")
	}

	input := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupIdentifier: aws.String(request.LogGroupIdentifier),
		StartTime:          aws.Int64(request.StartTime.UnixMilli()),
		EndTime:            aws.Int64(request.EndTime.UnixMilli()),
	}
	if request.FilterPattern != "" {
		input.FilterPattern = aws.String(request.FilterPattern)
	}
	if request.LogStreamNamePrefix != "" {
		input.LogStreamNamePrefix = aws.String(request.LogStreamNamePrefix)
	}

	paginator := cloudwatchlogs.NewFilterLogEventsPaginator(f.cwl, input)
	var buf bytes.Buffer
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("fetch CloudWatch logs: %w", err)
		}
		for _, event := range output.Events {
			message := aws.ToString(event.Message)
			if strings.TrimSpace(message) == "" {
				continue
			}
			buf.WriteString(message)
			if !strings.HasSuffix(message, "\n") {
				buf.WriteByte('\n')
			}
		}
	}

	return archive.writeBytes(path, buf.Bytes())
}

func (f *fullLogExporter) writeCloudTrailEvents(ctx context.Context, archive *archiveWriter, path, instanceID string, start, end time.Time) error {
	if f.cloudtrail == nil {
		return fmt.Errorf("CloudTrail client is not configured")
	}

	input := &cloudtrail.LookupEventsInput{
		StartTime: aws.Time(start),
		EndTime:   aws.Time(end),
		LookupAttributes: []cloudtrailtypes.LookupAttribute{
			{
				AttributeKey:   cloudtrailtypes.LookupAttributeKeyResourceName,
				AttributeValue: aws.String(instanceID),
			},
		},
	}

	paginator := cloudtrail.NewLookupEventsPaginator(f.cloudtrail, input)
	var events []cloudtrailtypes.Event
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("fetch CloudTrail events: %w", err)
		}
		events = append(events, output.Events...)
	}

	return archive.writeJSON(path, events)
}

func (f *fullLogExporter) writeConsoleLog(ctx context.Context, archive *archiveWriter, path, instanceID string) error {
	output, err := f.ec2.GetConsoleOutput(ctx, &ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
		Latest:     aws.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("fetch console log: %w", err)
	}
	if output.Output == nil {
		return archive.writeBytes(path, nil)
	}

	decoded, err := base64.StdEncoding.DecodeString(*output.Output)
	if err != nil {
		decoded = []byte(*output.Output)
	}
	return archive.writeBytes(path, decoded)
}

func errorPathFor(path string) string {
	ext := filepath.Ext(path)
	if ext == "" {
		return path + ".error.json"
	}
	return strings.TrimSuffix(path, ext) + ".error.json"
}

func prettyJSON(data []byte) []byte {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return data
	}
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return data
	}
	return append(pretty, '\n')
}
