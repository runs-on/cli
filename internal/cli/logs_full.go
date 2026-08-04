package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cloudtrailtypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	fullLogWindowBefore       = 5 * time.Minute
	fullLogWindowAfter        = 10 * time.Minute
	fullLogDefaultJobDuration = 30 * time.Minute
)

type fullLogManifest struct {
	StackName          string              `json:"stack_name,omitempty"`
	Region             string              `json:"region,omitempty"`
	JobID              int64               `json:"job_id"`
	RunID              int64               `json:"run_id,omitempty"`
	JobStart           time.Time           `json:"job_start"`
	JobEnd             time.Time           `json:"job_end"`
	JobStartSource     string              `json:"job_start_source,omitempty"`
	JobEndSource       string              `json:"job_end_source,omitempty"`
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
	cwl         cloudWatchLogsAPI
	resolver    *jobDiagnosticsResolver
	ec2         ec2ConsoleAPI
	cloudtrail  cloudTrailLookupAPI
	s3          metricsS3API
	outputs     *StackOutputs
	cacheBucket string
	stackName   string
	product     string
	region      string
}

type cloudTrailLookupAPI interface {
	LookupEvents(ctx context.Context, params *cloudtrail.LookupEventsInput, optFns ...func(*cloudtrail.Options)) (*cloudtrail.LookupEventsOutput, error)
}

type metricsS3API interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

func newFullLogExporter(config *RunsOnConfig) *fullLogExporter {
	return &fullLogExporter{
		cwl:         cloudwatchlogs.NewFromConfig(config.AWSConfig),
		resolver:    newJobDiagnosticsResolver(config),
		ec2:         ec2.NewFromConfig(config.AWSConfig),
		cloudtrail:  cloudtrail.NewFromConfig(config.AWSConfig),
		s3:          s3.NewFromConfig(config.AWSConfig),
		cacheBucket: config.CacheBucket,
		stackName:   config.StackName,
		product:     config.Product,
		region:      config.AWSConfig.Region,
		outputs: &StackOutputs{
			ServiceLogGroupName:    config.ServiceLogGroupName,
			EC2InstanceLogGroupArn: config.EC2InstanceLogGroupArn,
		},
	}
}

func (f *fullLogExporter) Export(ctx context.Context, jobID string) (string, error) {
	request, err := buildJobDiagnosticsRequest(jobID)
	if err != nil {
		return "", err
	}
	diagnostics, err := f.resolveJobDiagnostics(ctx, jobID)
	if err != nil {
		return "", err
	}
	if diagnostics.Local == nil && diagnostics.GitHub.WorkflowJob == nil {
		return "", fmt.Errorf("job %s not found", jobID)
	}
	facts := diagnostics.workflowFacts(parsedJobIDOrZero(extractJobID(jobID)))
	if facts == nil {
		return "", fmt.Errorf("job %s not found", jobID)
	}
	metricsRequest := canonicalMetricsRequest(diagnostics, request)

	window, err := facts.fullLogWindow()
	if err != nil {
		return "", err
	}
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

	if len(diagnostics.raw) > 0 {
		if err := archive.writeBytes("diagnostics/resolver.json", prettyJSON(diagnostics.raw)); err != nil {
			addArtifactError("diagnostics/resolver.json", err)
		}
	}

	ecsLogsPath := "server/ecs.jsonl"
	if err := f.writeCloudWatchMessages(ctx, archive, ecsLogsPath, cloudWatchLogRequest{
		LogGroupIdentifier: f.outputs.ServiceLogGroupName,
		StartTime:          window.Start,
		EndTime:            window.End,
	}); err != nil {
		addArtifactError(ecsLogsPath, err)
	}

	jobLogsPath := fmt.Sprintf("server/job-%s.jsonl", parsedJobID)
	if err := f.writeCloudWatchMessages(ctx, archive, jobLogsPath, cloudWatchLogRequest{
		LogGroupIdentifier: f.outputs.ServiceLogGroupName,
		FilterPattern:      fullJobFilterPattern(parsedJobID, facts.RunID, instanceIDs),
		StartTime:          window.Start,
		EndTime:            window.End,
	}); err != nil {
		addArtifactError(jobLogsPath, err)
	}

	runLogsPath := fmt.Sprintf("server/run-%d.jsonl", facts.RunID)
	if facts.RunID == 0 {
		addArtifactError(runLogsPath, fmt.Errorf("workflow run ID for job %s is not available", parsedJobID))
	} else if err := f.writeCloudWatchMessages(ctx, archive, runLogsPath, cloudWatchLogRequest{
		LogGroupIdentifier: f.outputs.ServiceLogGroupName,
		FilterPattern:      runFilterPattern(facts.RunID),
		StartTime:          window.Start,
		EndTime:            window.End,
	}); err != nil {
		addArtifactError(runLogsPath, err)
	}

	for _, instanceID := range instanceIDs {
		instanceDir := path.Join("instances", instanceID)
		cloudTrailPath := path.Join(instanceDir, "cloudtrail.json")
		if err := f.writeCloudTrailEvents(ctx, archive, cloudTrailPath, instanceID, window.Start, window.End); err != nil {
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
			StartTime:           window.Start,
			EndTime:             window.End,
		}); err != nil {
			addArtifactError(agentPath, err)
		}
	}

	for _, artifactErr := range f.writeMetricsFiles(ctx, archive, metricsRequest, facts.RunID, instanceIDs) {
		addArtifactError(artifactErr.Path, errors.New(artifactErr.Error))
	}

	manifest := fullLogManifest{
		StackName:          f.stackName,
		Region:             f.region,
		JobID:              facts.JobID,
		RunID:              facts.RunID,
		JobStart:           window.JobStart.UTC(),
		JobEnd:             window.JobEnd.UTC(),
		JobStartSource:     window.JobStartSource,
		JobEndSource:       window.JobEndSource,
		WindowStart:        window.Start.UTC(),
		WindowEnd:          window.End.UTC(),
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

func canonicalMetricsRequest(diagnostics *jobDiagnosticsResponse, fallback jobDiagnosticsRequest) jobDiagnosticsRequest {
	if diagnostics == nil {
		return fallback
	}

	var repositoryCandidates []string
	if diagnostics.GitHub.WorkflowJob != nil {
		repositoryCandidates = append(repositoryCandidates, diagnostics.GitHub.WorkflowJob.HTMLURL)
	}
	if diagnostics.GitHub.WorkflowRun != nil {
		repositoryCandidates = append(repositoryCandidates, diagnostics.GitHub.WorkflowRun.HTMLURL)
	}

	var local struct {
		JobHTMLURL     string `json:"job_html_url"`
		OrgName        string `json:"org_name"`
		RepoName       string `json:"repo_name"`
		OwnerName      string `json:"owner_name"`
		RepositoryName string `json:"repository_name"`
	}
	if diagnostics.Local != nil && len(diagnostics.Local.Record) > 0 && json.Unmarshal(diagnostics.Local.Record, &local) == nil {
		repositoryCandidates = append(repositoryCandidates, local.JobHTMLURL)
		if strings.TrimSpace(local.OrgName) != "" && strings.TrimSpace(local.RepoName) != "" {
			fallback.Owner = strings.TrimSpace(local.OrgName)
			fallback.Repo = strings.TrimSpace(local.RepoName)
		}
		if strings.TrimSpace(local.OwnerName) != "" && strings.TrimSpace(local.RepositoryName) != "" {
			fallback.Owner = strings.TrimSpace(local.OwnerName)
			fallback.Repo = strings.TrimSpace(local.RepositoryName)
		}
	}

	for _, candidate := range repositoryCandidates {
		owner, repo, ok := repositoryFromGitHubURL(candidate)
		if !ok {
			continue
		}
		fallback.Owner = owner
		fallback.Repo = repo
		return fallback
	}
	return fallback
}

func repositoryFromGitHubURL(input string) (string, string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(input))
	if err != nil || parsed.Scheme != "https" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (f *fullLogExporter) writeMetricsFiles(ctx context.Context, archive *archiveWriter, request jobDiagnosticsRequest, runID int64, instanceIDs []string) []fullArtifactError {
	if f.s3 == nil || strings.TrimSpace(f.cacheBucket) == "" || strings.TrimSpace(request.Owner) == "" || strings.TrimSpace(request.Repo) == "" || runID == 0 || len(instanceIDs) == 0 {
		return nil
	}

	prefix := fmt.Sprintf("cache/metrics/v1/%s/%s/%d/", request.Owner, request.Repo, runID)
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(f.cacheBucket),
		Prefix: aws.String(prefix),
	}
	paginator := s3.NewListObjectsV2Paginator(f.s3, input)
	keysByInstance := make(map[string]string, len(instanceIDs))
	instances := make(map[string]struct{}, len(instanceIDs))
	for _, instanceID := range instanceIDs {
		instances[instanceID] = struct{}{}
	}

	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return []fullArtifactError{{Path: "instances/metrics.jsonl", Error: fmt.Sprintf("list metrics files: %v", err)}}
		}
		for _, object := range output.Contents {
			key := aws.ToString(object.Key)
			parts := strings.Split(strings.TrimPrefix(key, prefix), "/")
			if len(parts) != 3 || parts[2] != "metrics.jsonl" {
				continue
			}
			instanceID := parts[1]
			if _, ok := instances[instanceID]; !ok {
				continue
			}
			if _, exists := keysByInstance[instanceID]; !exists {
				keysByInstance[instanceID] = key
			}
		}
	}

	var artifactErrors []fullArtifactError
	for _, instanceID := range instanceIDs {
		key, ok := keysByInstance[instanceID]
		if !ok {
			continue
		}
		artifactPath := path.Join("instances", instanceID, "metrics.jsonl")
		output, err := f.s3.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(f.cacheBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			artifactErrors = append(artifactErrors, fullArtifactError{Path: artifactPath, Error: fmt.Sprintf("fetch metrics file: %v", err)})
			continue
		}
		writeErr := archive.writeReader(artifactPath, output.Body)
		closeErr := output.Body.Close()
		if writeErr != nil {
			artifactErrors = append(artifactErrors, fullArtifactError{Path: artifactPath, Error: fmt.Sprintf("write metrics file: %v", writeErr)})
			continue
		}
		if closeErr != nil {
			artifactErrors = append(artifactErrors, fullArtifactError{Path: artifactPath, Error: fmt.Sprintf("close metrics file: %v", closeErr)})
		}
	}
	return artifactErrors
}

type fullLogWindow struct {
	JobStart       time.Time
	JobEnd         time.Time
	JobStartSource string
	JobEndSource   string
	Start          time.Time
	End            time.Time
}

func (f *workflowJobFacts) fullLogWindow() (fullLogWindow, error) {
	createdAt, err := f.createdAtOrError()
	if err != nil {
		return fullLogWindow{}, err
	}

	jobEnd := f.CompletedAt
	jobEndSource := f.CompletedAtSource
	if jobEnd.IsZero() {
		jobEnd = createdAt.Add(fullLogDefaultJobDuration)
		jobEndSource = strings.TrimSpace(f.CreatedAtSource)
		if jobEndSource == "" {
			jobEndSource = "created_at"
		}
		jobEndSource += "+30m"
	}

	return fullLogWindow{
		JobStart:       createdAt.UTC(),
		JobEnd:         jobEnd.UTC(),
		JobStartSource: f.CreatedAtSource,
		JobEndSource:   jobEndSource,
		Start:          createdAt.Add(-fullLogWindowBefore).UTC(),
		End:            jobEnd.Add(fullLogWindowAfter).UTC(),
	}, nil
}

func (f *fullLogExporter) resolveJobDiagnostics(ctx context.Context, jobID string) (*jobDiagnosticsResponse, error) {
	if f.resolver == nil {
		return nil, fmt.Errorf("job diagnostics resolver is required")
	}
	diagnostics, err := f.resolver.Resolve(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if err := diagnostics.validateStackProduct(f.product, f.stackName, parsedJobIDOrZero(extractJobID(jobID))); err != nil {
		return nil, err
	}
	return diagnostics, nil
}

func fullJobFilterPattern(jobID string, runID int64, instanceIDs []string) string {
	terms := []string{
		fmt.Sprintf(`( $.job_id = "%s" )`, jobID),
		fmt.Sprintf(`( $.workflow_job_id = "%s" )`, jobID),
	}
	if runID != 0 {
		terms = append(terms, fmt.Sprintf(`( $.run_id = "%d" )`, runID))
	}
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
