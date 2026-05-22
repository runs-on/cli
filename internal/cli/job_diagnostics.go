package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

type jobDiagnosticsLambdaAPI interface {
	Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

type jobDiagnosticsResolver struct {
	client       jobDiagnosticsLambdaAPI
	functionName string
}

type localGitHubWorkflowJobFetcher interface {
	FetchWorkflowJob(ctx context.Context, request jobDiagnosticsRequest) (*jobDiagnosticsWorkflowJob, error)
}

type ghCLIWorkflowJobFetcher struct{}

type jobDiagnosticsRequest struct {
	JobURL                  string `json:"job_url,omitempty"`
	GitHubHost              string `json:"github_host,omitempty"`
	Owner                   string `json:"owner,omitempty"`
	Repo                    string `json:"repo,omitempty"`
	WorkflowRunID           int64  `json:"workflow_run_id,omitempty"`
	WorkflowJobID           int64  `json:"workflow_job_id,omitempty"`
	IncludeDeliveryMetadata bool   `json:"include_delivery_metadata"`
}

type jobDiagnosticsResponse struct {
	Status      string                     `json:"status"`
	Product     string                     `json:"product"`
	Request     jobDiagnosticsRequest      `json:"request"`
	GitHub      jobDiagnosticsGitHub       `json:"github"`
	Local       *jobDiagnosticsLocal       `json:"local"`
	Fleet       *jobDiagnosticsFleet       `json:"fleet,omitempty"`
	Diagnostics []jobDiagnosticsDiagnostic `json:"diagnostics,omitempty"`
	raw         []byte
}

type jobDiagnosticsGitHub struct {
	WorkflowJob *jobDiagnosticsWorkflowJob `json:"workflow_job"`
	WorkflowRun *jobDiagnosticsWorkflowRun `json:"workflow_run"`
	Deliveries  []map[string]any           `json:"deliveries,omitempty"`
}

type jobDiagnosticsWorkflowJob struct {
	ID              int64    `json:"id"`
	RunID           int64    `json:"run_id"`
	RunAttempt      int64    `json:"run_attempt,omitempty"`
	Name            string   `json:"name,omitempty"`
	Status          string   `json:"status,omitempty"`
	Conclusion      string   `json:"conclusion,omitempty"`
	RunnerName      string   `json:"runner_name,omitempty"`
	RunnerID        int64    `json:"runner_id,omitempty"`
	RunnerGroupName string   `json:"runner_group_name,omitempty"`
	WorkflowName    string   `json:"workflow_name,omitempty"`
	HeadBranch      string   `json:"head_branch,omitempty"`
	HeadSHA         string   `json:"head_sha,omitempty"`
	HTMLURL         string   `json:"html_url,omitempty"`
	CreatedAt       string   `json:"created_at,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
	CompletedAt     string   `json:"completed_at,omitempty"`
	Labels          []string `json:"labels,omitempty"`
}

type jobDiagnosticsWorkflowRun struct {
	ID           int64  `json:"id"`
	RunNumber    int64  `json:"run_number,omitempty"`
	RunAttempt   int64  `json:"run_attempt,omitempty"`
	Name         string `json:"name,omitempty"`
	Path         string `json:"path,omitempty"`
	Event        string `json:"event,omitempty"`
	Status       string `json:"status,omitempty"`
	Conclusion   string `json:"conclusion,omitempty"`
	HeadBranch   string `json:"head_branch,omitempty"`
	HeadSHA      string `json:"head_sha,omitempty"`
	HTMLURL      string `json:"html_url,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	RunStartedAt string `json:"run_started_at,omitempty"`
}

type jobDiagnosticsLocal struct {
	Source          string          `json:"source,omitempty"`
	WorkflowJobID   int64           `json:"workflow_job_id,omitempty"`
	WorkflowRunID   int64           `json:"workflow_run_id,omitempty"`
	ScalesetJobID   string          `json:"scaleset_job_id,omitempty"`
	RunnerName      string          `json:"runner_name,omitempty"`
	InstanceIDs     []string        `json:"instance_ids,omitempty"`
	Status          string          `json:"status,omitempty"`
	SchedulingState string          `json:"scheduling_state,omitempty"`
	CreatedAt       string          `json:"created_at,omitempty"`
	Record          json.RawMessage `json:"record,omitempty"`
}

type jobDiagnosticsFleet struct {
	ClaimCount      int    `json:"claim_count,omitempty"`
	ExactMatchCount int    `json:"exact_match_count,omitempty"`
	MatchBasis      string `json:"match_basis,omitempty"`
	InstallationID  int64  `json:"installation_id,omitempty"`
}

type jobDiagnosticsDiagnostic struct {
	Level   string `json:"level,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func newJobDiagnosticsResolver(config *RunsOnConfig) *jobDiagnosticsResolver {
	return &jobDiagnosticsResolver{
		client:       lambda.NewFromConfig(config.AWSConfig),
		functionName: strings.TrimSpace(config.JobDiagnosticsResolver),
	}
}

func (r *jobDiagnosticsResolver) Resolve(ctx context.Context, jobRef string) (*jobDiagnosticsResponse, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("job diagnostics resolver client is required")
	}
	if r.functionName == "" {
		return nil, fmt.Errorf("job diagnostics resolver Lambda is not configured")
	}
	request, err := buildJobDiagnosticsRequest(jobRef)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal job diagnostics request: %w", err)
	}

	output, err := r.client.Invoke(ctx, &lambda.InvokeInput{
		FunctionName:   aws.String(r.functionName),
		InvocationType: lambdatypes.InvocationTypeRequestResponse,
		Payload:        payload,
	})
	if err != nil {
		return nil, fmt.Errorf("invoke job diagnostics resolver %s: %w", r.functionName, err)
	}
	if output.FunctionError != nil && strings.TrimSpace(*output.FunctionError) != "" {
		return nil, fmt.Errorf("job diagnostics resolver %s failed: %s", r.functionName, strings.TrimSpace(string(output.Payload)))
	}

	var response jobDiagnosticsResponse
	if err := json.Unmarshal(output.Payload, &response); err != nil {
		return nil, fmt.Errorf("decode job diagnostics resolver response: %w", err)
	}
	response.raw = append([]byte(nil), output.Payload...)
	return &response, nil
}

func buildJobDiagnosticsRequest(jobRef string) (jobDiagnosticsRequest, error) {
	parsed, err := requireGitHubJobURL(jobRef)
	if err != nil {
		return jobDiagnosticsRequest{}, err
	}
	request := jobDiagnosticsRequest{
		JobURL:                  strings.TrimSpace(jobRef),
		GitHubHost:              parsed.Host,
		Owner:                   parsed.Owner,
		Repo:                    parsed.Repo,
		WorkflowRunID:           parsed.RunID,
		WorkflowJobID:           parsed.JobID,
		IncludeDeliveryMetadata: true,
	}
	return request, nil
}

func requireGitHubJobURL(input string) (parsedGitHubJobURL, error) {
	if parsed, ok := parseGitHubJobURL(input); ok {
		return parsed, nil
	}
	return parsedGitHubJobURL{}, fmt.Errorf("GitHub Actions job URL is required (expected https://<github-host>/<owner>/<repo>/actions/runs/<run_id>/job/<job_id>)")
}

type parsedGitHubJobURL struct {
	Host  string
	Owner string
	Repo  string
	RunID int64
	JobID int64
}

func parseGitHubJobURL(input string) (parsedGitHubJobURL, bool) {
	parsed, err := url.Parse(strings.TrimSpace(input))
	if err != nil || parsed.Scheme != "https" {
		return parsedGitHubJobURL{}, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 7 || parts[2] != "actions" || parts[3] != "runs" || parts[5] != "job" {
		return parsedGitHubJobURL{}, false
	}
	runID, runErr := strconv.ParseInt(parts[4], 10, 64)
	jobID, jobErr := strconv.ParseInt(parts[6], 10, 64)
	if runErr != nil || jobErr != nil {
		return parsedGitHubJobURL{}, false
	}
	return parsedGitHubJobURL{
		Host:  parsed.Host,
		Owner: parts[0],
		Repo:  parts[1],
		RunID: runID,
		JobID: jobID,
	}, true
}

func (ghCLIWorkflowJobFetcher) FetchWorkflowJob(ctx context.Context, request jobDiagnosticsRequest) (*jobDiagnosticsWorkflowJob, error) {
	if strings.TrimSpace(request.Owner) == "" || strings.TrimSpace(request.Repo) == "" || request.WorkflowJobID == 0 {
		return nil, fmt.Errorf("GitHub job URL is required for local gh fallback")
	}
	args := []string{"api"}
	host := strings.TrimSpace(request.GitHubHost)
	if host != "" && !strings.EqualFold(host, "github.com") {
		args = append(args, "--hostname", host)
	}
	args = append(args, fmt.Sprintf("repos/%s/%s/actions/jobs/%d", request.Owner, request.Repo, request.WorkflowJobID))
	output, err := exec.CommandContext(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		if _, lookupErr := exec.LookPath("gh"); lookupErr != nil {
			return nil, fmt.Errorf("GitHub CLI fallback requires gh; install GitHub CLI and run `gh auth login` with repository Actions read access")
		}
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("GitHub CLI could not fetch workflow job; run `gh auth login` with repository Actions read access: %s", message)
	}
	var job jobDiagnosticsWorkflowJob
	if err := json.Unmarshal(output, &job); err != nil {
		return nil, fmt.Errorf("decode GitHub CLI workflow job response: %w", err)
	}
	if job.ID == 0 {
		job.ID = request.WorkflowJobID
	}
	if job.RunID == 0 {
		job.RunID = request.WorkflowRunID
	}
	return &job, nil
}

func (r *jobDiagnosticsResponse) workflowFacts(jobID int64) *workflowJobFacts {
	if r == nil {
		return nil
	}
	if r.Local == nil && r.GitHub.WorkflowJob == nil {
		return nil
	}
	runID := int64(0)
	if r.GitHub.WorkflowJob != nil && r.GitHub.WorkflowJob.RunID != 0 {
		runID = r.GitHub.WorkflowJob.RunID
	} else if r.GitHub.WorkflowRun != nil {
		runID = r.GitHub.WorkflowRun.ID
	} else if r.Local != nil {
		runID = r.Local.WorkflowRunID
	}
	if runID == 0 {
		runID = r.Request.WorkflowRunID
	}

	status := ""
	schedulingState := ""
	runnerName := ""
	var instanceIDs []string
	var createdAt time.Time
	createdAtSource := ""
	if r.Local != nil {
		status = r.Local.Status
		schedulingState = r.Local.SchedulingState
		runnerName = r.Local.RunnerName
		instanceIDs = append(instanceIDs, r.Local.InstanceIDs...)
		createdAt, createdAtSource = parseDiagnosticsTime(r.Local.CreatedAt, "local.created_at")
	}
	if r.GitHub.WorkflowJob != nil {
		if status == "" {
			status = r.GitHub.WorkflowJob.Status
		}
		if runnerName == "" {
			runnerName = r.GitHub.WorkflowJob.RunnerName
		}
		if r.GitHub.WorkflowJob.ID != 0 {
			jobID = r.GitHub.WorkflowJob.ID
		}
		if createdAt.IsZero() {
			createdAt, createdAtSource = parseDiagnosticsTime(r.GitHub.WorkflowJob.CreatedAt, "github.workflow_job.created_at")
		}
	}
	instanceIDs = append(instanceIDs, parseRunnerNameInstanceID(runnerName))
	instanceIDs = dedupeStrings(instanceIDs)
	currentInstanceID := ""
	if len(instanceIDs) > 0 {
		currentInstanceID = instanceIDs[0]
	}
	if r.Local != nil && len(r.Local.InstanceIDs) > 0 {
		currentInstanceID = strings.TrimSpace(r.Local.InstanceIDs[0])
	}
	if strings.EqualFold(r.Product, "fleet") {
		if instanceID := fleetClaimCurrentInstanceID(r.Local); instanceID != "" {
			currentInstanceID = instanceID
		}
	}

	return &workflowJobFacts{
		JobID:                jobID,
		RunID:                runID,
		Status:               status,
		SchedulingState:      schedulingState,
		CurrentInstanceID:    currentInstanceID,
		AttemptedInstanceIDs: instanceIDs,
		CreatedAt:            createdAt,
		CreatedAtSource:      createdAtSource,
		rawJSON:              r.localRecordJSON(),
	}
}

type fleetClaimCurrentRecord struct {
	InstanceID           string   `json:"instance_id"`
	DesiredRunnerName    string   `json:"desired_runner_name"`
	AttemptedInstanceIDs []string `json:"attempted_instance_ids"`
}

func fleetClaimCurrentInstanceID(local *jobDiagnosticsLocal) string {
	if local == nil {
		return ""
	}

	var record fleetClaimCurrentRecord
	if len(local.Record) > 0 {
		_ = json.Unmarshal(local.Record, &record)
	}
	if instanceID := strings.TrimSpace(record.InstanceID); instanceID != "" {
		return instanceID
	}
	if instanceID := parseRunnerNameInstanceID(record.DesiredRunnerName); instanceID != "" {
		return instanceID
	}
	for i := len(record.AttemptedInstanceIDs) - 1; i >= 0; i-- {
		if instanceID := strings.TrimSpace(record.AttemptedInstanceIDs[i]); instanceID != "" {
			return instanceID
		}
	}
	if instanceID := parseRunnerNameInstanceID(local.RunnerName); instanceID != "" {
		return instanceID
	}
	for i := len(local.InstanceIDs) - 1; i >= 0; i-- {
		if instanceID := strings.TrimSpace(local.InstanceIDs[i]); instanceID != "" {
			return instanceID
		}
	}
	return ""
}

func (r *jobDiagnosticsResponse) localRecordJSON() []byte {
	if r == nil || r.Local == nil || len(r.Local.Record) == 0 {
		return nil
	}
	return append([]byte(nil), r.Local.Record...)
}

func (r *jobDiagnosticsResponse) writeSummary(w io.Writer) {
	if r == nil || w == nil {
		return
	}
	instanceIDs := []string{}
	runnerName := ""
	source := "(none)"
	if r.Local != nil {
		instanceIDs = r.Local.InstanceIDs
		runnerName = r.Local.RunnerName
		source = displayValue(r.Local.Source)
	}
	if r.GitHub.WorkflowJob != nil {
		if runnerName == "" {
			runnerName = r.GitHub.WorkflowJob.RunnerName
		}
		instanceIDs = append(instanceIDs, parseRunnerNameInstanceID(r.GitHub.WorkflowJob.RunnerName))
	}
	fmt.Fprintf(w, "Job diagnostics: product=%s status=%s local=%s runner=%s instance_ids=%s\n",
		displayValue(r.Product),
		displayValue(r.Status),
		source,
		displayValue(runnerName),
		displayList(instanceIDs),
	)
	if r.Fleet != nil {
		fmt.Fprintf(w, "Fleet diagnostics: claims=%d match_basis=%s exact_matches=%d\n",
			r.Fleet.ClaimCount,
			displayValue(r.Fleet.MatchBasis),
			r.Fleet.ExactMatchCount,
		)
	}
	for _, diagnostic := range r.compactDiagnostics() {
		fmt.Fprintf(w, "Job diagnostics %s: %s\n", displayValue(diagnostic.Code), displayValue(diagnostic.Message))
	}
}

func (r *jobDiagnosticsResponse) logDebug(logger debugLogger) {
	if r == nil || logger == nil {
		return
	}
	logger.Printf("Job diagnostics resolver status: product=%s status=%s", displayValue(r.Product), displayValue(r.Status))
	for _, diagnostic := range r.Diagnostics {
		logger.Printf("Job diagnostics %s %s: %s", displayValue(diagnostic.Level), displayValue(diagnostic.Code), displayValue(diagnostic.Message))
	}
	if len(r.raw) > 0 {
		logger.Printf("Job diagnostics resolver response: %s", string(r.raw))
	}
}

func (r *jobDiagnosticsResponse) compactDiagnostics() []jobDiagnosticsDiagnostic {
	if r == nil {
		return nil
	}
	codes := map[string]struct{}{
		"github_workflow_job_fetch_not_accessible": {},
		"github_workflow_run_fetch_not_accessible": {},
		"fleet_claim_ambiguous":                    {},
		"fleet_claim_missing_for_runner":           {},
		"local_gh_workflow_job_fetch_attempt":      {},
		"local_gh_workflow_job_fetch_failed":       {},
		"local_gh_workflow_job_fetched":            {},
	}
	var diagnostics []jobDiagnosticsDiagnostic
	for _, diagnostic := range r.Diagnostics {
		if _, ok := codes[strings.TrimSpace(diagnostic.Code)]; !ok {
			continue
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	return diagnostics
}

type debugLogger interface {
	Printf(format string, v ...any)
}

func parseDiagnosticsTime(value string, source string) (time.Time, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, ""
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, ""
	}
	return parsed.UTC(), source
}

func dedupeStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func displayList(values []string) string {
	values = dedupeStrings(values)
	if len(values) == 0 {
		return "(none)"
	}
	return strings.Join(values, ",")
}
