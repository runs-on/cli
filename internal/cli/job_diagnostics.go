package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"slices"
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
	// GitHubCached marks a repeat poll from a caller that already holds the
	// GitHub workflow job and run, so the resolver can skip GitHub. RunnerName
	// is the cached job's runner, which Fleet still needs to match claims.
	GitHubCached bool   `json:"github_cached,omitempty"`
	RunnerName   string `json:"runner_name,omitempty"`
}

type jobDiagnosticsResponse struct {
	Status      string                     `json:"status"`
	Product     string                     `json:"product"`
	StackName   string                     `json:"stack_name,omitempty"`
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
	CompletedAt     string          `json:"completed_at,omitempty"`
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

func (r *jobDiagnosticsResolver) resolveRequest(ctx context.Context, request jobDiagnosticsRequest) (*jobDiagnosticsResponse, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("job diagnostics resolver client is required")
	}
	if r.functionName == "" {
		return nil, fmt.Errorf("job diagnostics resolver Lambda is not configured")
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

func buildJobDiagnosticsRequest(job parsedGitHubJobURL) jobDiagnosticsRequest {
	return jobDiagnosticsRequest{
		JobURL:        job.URL,
		GitHubHost:    job.Host,
		Owner:         job.Owner,
		Repo:          job.Repo,
		WorkflowRunID: job.RunID,
		WorkflowJobID: job.JobID,
	}
}

type parsedGitHubJobURL struct {
	URL   string // the trimmed input, sent to the resolver as job_url
	Host  string
	Owner string
	Repo  string
	RunID int64
	JobID int64
}

var errNotGitHubJobURL = errors.New("GitHub Actions job URL is required (expected https://<github-host>/<owner>/<repo>/actions/runs/<run_id>/job/<job_id>)")

func parseGitHubJobURL(input string) (parsedGitHubJobURL, error) {
	input = strings.TrimSpace(input)
	parsed, err := url.Parse(input)
	if err != nil || parsed.Scheme != "https" {
		return parsedGitHubJobURL{}, errNotGitHubJobURL
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 7 || parts[2] != "actions" || parts[3] != "runs" || parts[5] != "job" {
		return parsedGitHubJobURL{}, errNotGitHubJobURL
	}
	runID, runErr := strconv.ParseInt(parts[4], 10, 64)
	jobID, jobErr := strconv.ParseInt(parts[6], 10, 64)
	if runErr != nil || jobErr != nil {
		return parsedGitHubJobURL{}, errNotGitHubJobURL
	}
	return parsedGitHubJobURL{
		URL:   input,
		Host:  parsed.Host,
		Owner: parts[0],
		Repo:  parts[1],
		RunID: runID,
		JobID: jobID,
	}, nil
}

func (ghCLIWorkflowJobFetcher) FetchWorkflowJob(ctx context.Context, request jobDiagnosticsRequest) (*jobDiagnosticsWorkflowJob, error) {
	if strings.TrimSpace(request.Owner) == "" || strings.TrimSpace(request.Repo) == "" || request.WorkflowJobID == 0 {
		return nil, fmt.Errorf("GitHub job URL is required for local gh fallback")
	}
	output, err := ghAPI(ctx, request.GitHubHost, fmt.Sprintf("repos/%s/%s/actions/jobs/%d", request.Owner, request.Repo, request.WorkflowJobID))
	if errors.Is(err, errGHMissing) {
		return nil, fmt.Errorf("GitHub CLI fallback requires gh; install GitHub CLI and run `gh auth login` with repository Actions read access")
	}
	if err != nil {
		return nil, fmt.Errorf("GitHub CLI could not fetch workflow job; run `gh auth login` with repository Actions read access: %s", err)
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

// workflowJobFacts is what the job commands read from one resolver response.
type workflowJobFacts struct {
	Found             bool // the response has a local record or a GitHub workflow job
	RunID             int64
	JobURL            string
	ScalesetJobID     string
	Status            string
	SchedulingState   string
	CurrentInstanceID string
	InstanceIDs       []string // every instance the job used, the current one included
	CreatedAt         time.Time
	CreatedAtSource   string
	CompletedAt       time.Time
	CompletedAtSource string
}

// jobFacts derives the job's facts from the response. Every fallback between
// GitHub, the local record and the request lives here.
func (r *jobDiagnosticsResponse) jobFacts(job parsedGitHubJobURL) *workflowJobFacts {
	local, github := r.Local, r.GitHub.WorkflowJob
	facts := &workflowJobFacts{Found: local != nil || github != nil}
	switch {
	case github != nil && github.RunID != 0:
		facts.RunID = github.RunID
	case r.GitHub.WorkflowRun != nil:
		facts.RunID = r.GitHub.WorkflowRun.ID
	case local != nil:
		facts.RunID = local.WorkflowRunID
	}
	facts.RunID = cmp.Or(facts.RunID, r.Request.WorkflowRunID)

	var runnerName, htmlURL string
	var localInstanceIDs []string
	if local != nil {
		facts.ScalesetJobID = strings.TrimSpace(local.ScalesetJobID)
		facts.Status = local.Status
		facts.SchedulingState = local.SchedulingState
		runnerName = local.RunnerName
		localInstanceIDs = local.InstanceIDs
		facts.CreatedAt, facts.CreatedAtSource = parseDiagnosticsTime(local.CreatedAt, "local.created_at")
		facts.CompletedAt, facts.CompletedAtSource = parseDiagnosticsTime(local.CompletedAt, "local.completed_at")
	}
	if github != nil {
		facts.Status = cmp.Or(facts.Status, github.Status)
		runnerName = cmp.Or(runnerName, github.RunnerName)
		htmlURL = github.HTMLURL
		if t, source := parseDiagnosticsTime(github.CreatedAt, "github.workflow_job.created_at"); !t.IsZero() {
			facts.CreatedAt, facts.CreatedAtSource = t, source
		}
		if t, source := parseDiagnosticsTime(github.CompletedAt, "github.workflow_job.completed_at"); !t.IsZero() {
			facts.CompletedAt, facts.CompletedAtSource = t, source
		}
	}
	facts.JobURL = cmp.Or(strings.TrimSpace(htmlURL), strings.TrimSpace(r.Request.JobURL), job.URL)

	facts.InstanceIDs = dedupeStrings(slices.Concat(localInstanceIDs, []string{parseRunnerNameInstanceID(runnerName)}))
	if len(facts.InstanceIDs) > 0 {
		facts.CurrentInstanceID = facts.InstanceIDs[0]
	}
	if parseProduct(r.Product) == productFleet {
		if instanceID := fleetClaimCurrentInstanceID(local); instanceID != "" {
			facts.CurrentInstanceID = instanceID
			facts.InstanceIDs = dedupeStrings(append(facts.InstanceIDs, instanceID))
		}
	}
	return facts
}

func (r *jobDiagnosticsResponse) validateStackProduct(stackProduct runsOnProduct, stackName string, jobID int64) error {
	if err := r.validateStackName(stackName, jobID); err != nil {
		return err
	}
	jobProduct := r.classifiedJobProduct()
	if stackProduct == "" || jobProduct == "" || stackProduct == jobProduct {
		return nil
	}
	if jobID == 0 {
		jobID = r.Request.WorkflowJobID
	}
	return fmt.Errorf("job %d is a %s job, but stack %q is a %s stack", jobID, jobProduct.display(), strings.TrimSpace(stackName), stackProduct.display())
}

func (r *jobDiagnosticsResponse) validateStackName(stackName string, jobID int64) error {
	expected := strings.TrimSpace(stackName)
	actual := strings.TrimSpace(r.StackName)
	if expected == "" || actual == "" || expected == actual {
		return nil
	}
	if jobID == 0 {
		jobID = r.Request.WorkflowJobID
	}
	return fmt.Errorf("job %d diagnostics resolved stack %q, but CLI selected stack %q", jobID, actual, expected)
}

func (r *jobDiagnosticsResponse) classifiedJobProduct() runsOnProduct {
	if r == nil {
		return ""
	}
	if r.Local != nil {
		switch strings.TrimSpace(r.Local.Source) {
		case "flex_workflow_jobs":
			return productFlex
		case "fleet_claims":
			return productFleet
		}
	}
	return classifyJobProductFromLabels(r.GitHub.WorkflowJob)
}

func classifyJobProductFromLabels(job *jobDiagnosticsWorkflowJob) runsOnProduct {
	if job == nil {
		return ""
	}
	sawRunsOn := false
	for _, label := range job.Labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		labelRunsOn := false
		labelFleet := false
		for _, part := range strings.FieldsFunc(label, func(r rune) bool {
			return r == '/' || r == ','
		}) {
			key, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			key = strings.TrimSpace(key)
			if strings.HasPrefix(key, "runs-on") {
				sawRunsOn = true
				labelRunsOn = true
			}
			if key == "fleet" {
				labelFleet = true
			}
		}
		if labelRunsOn && labelFleet {
			return productFleet
		}
	}
	if sawRunsOn {
		return productFlex
	}
	return ""
}

// runsOnProduct is the RunsOn product a stack or job belongs to.
type runsOnProduct string

const (
	productFlex  runsOnProduct = "flex"
	productFleet runsOnProduct = "fleet"
)

// parseProduct reads a product name case-insensitively; anything else is "".
func parseProduct(name string) runsOnProduct {
	switch product := runsOnProduct(strings.ToLower(strings.TrimSpace(name))); product {
	case productFlex, productFleet:
		return product
	}
	return ""
}

func (p runsOnProduct) display() string {
	switch p {
	case productFlex:
		return "Flex"
	case productFleet:
		return "Fleet"
	}
	return string(p)
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

// reuseGitHub fills in the GitHub details that a cached poll skipped. The
// resolver fetches a Fleet job again until GitHub names its runner, so a cached
// Fleet job without one is not reused: that would hide a failed fetch and keep
// the local gh fallback from asking again.
func (r *jobDiagnosticsResponse) reuseGitHub(cached jobDiagnosticsGitHub) {
	pendingFleetJob := parseProduct(r.Product) == productFleet && cached.WorkflowJob != nil && cached.WorkflowJob.RunnerName == ""
	if r.GitHub.WorkflowJob == nil && !pendingFleetJob {
		r.GitHub.WorkflowJob = cached.WorkflowJob
	}
	if r.GitHub.WorkflowRun == nil {
		r.GitHub.WorkflowRun = cached.WorkflowRun
	}
}

// writeSummary describes the resolver response itself, so it lists the GitHub
// runner's instance even when the local record names another runner.
func (r *jobDiagnosticsResponse) writeSummary(w io.Writer) {
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
	fmt.Fprintf(w, "Job diagnostics: product=%s stack=%s status=%s local=%s runner=%s instance_ids=%s\n",
		displayValue(r.Product),
		displayValue(r.StackName),
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

func (r *jobDiagnosticsResponse) logDebug(logger *log.Logger) {
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
