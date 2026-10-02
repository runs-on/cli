package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type jobFactsProvider struct {
	product   runsOnProduct
	stackName string
	resolver  *jobDiagnosticsResolver
	github    localGitHubWorkflowJobFetcher
	job       parsedGitHubJobURL
	logger    *log.Logger

	mu          sync.RWMutex
	diagnostics *jobDiagnosticsResponse // the latest resolver response
	facts       *workflowJobFacts       // derived from diagnostics
}

func newJobFactsProvider(config *RunsOnConfig, job parsedGitHubJobURL, logger *log.Logger) *jobFactsProvider {
	return &jobFactsProvider{
		product:   config.Product,
		stackName: strings.TrimSpace(config.StackName),
		resolver:  newJobDiagnosticsResolver(config),
		github:    ghCLIWorkflowJobFetcher{},
		job:       job,
		logger:    logger,
	}
}

// refresh asks the resolver about the job again. After a failed refresh the
// job counts as not found, but its log lines still match the run, URL and
// scaleset job of the latest response.
func (p *jobFactsProvider) refresh(ctx context.Context) error {
	err := p.find(ctx)
	if err != nil {
		p.logger.Printf("Error discovering job facts: %v", err)
		p.mu.Lock()
		if last := p.facts; last != nil {
			p.facts = &workflowJobFacts{RunID: last.RunID, JobURL: last.JobURL, ScalesetJobID: last.ScalesetJobID}
		}
		p.mu.Unlock()
	}
	return err
}

func (p *jobFactsProvider) find(ctx context.Context) error {
	request := buildJobDiagnosticsRequest(p.job)
	// Job state changes surface in the local record, so repeat polls reuse the
	// GitHub details from an earlier response and let the resolver skip GitHub.
	cached := p.currentDiagnostics()
	if cached != nil && cached.GitHub.WorkflowJob != nil {
		request.GitHubCached = true
		request.RunnerName = cached.GitHub.WorkflowJob.RunnerName
	}
	response, err := p.resolver.resolveRequest(ctx, request)
	if err != nil {
		return err
	}
	if request.GitHubCached {
		response.reuseGitHub(cached.GitHub)
	}
	response.logDebug(p.logger)
	if p.shouldUseLocalGitHubFallback(response) {
		p.enrichWithLocalGitHub(ctx, response)
	}
	facts := response.jobFacts(p.job)
	p.mu.Lock()
	p.diagnostics, p.facts = response, facts
	p.mu.Unlock()
	return response.validateStackProduct(p.product, p.stackName, p.job.JobID)
}

func (p *jobFactsProvider) shouldUseLocalGitHubFallback(response *jobDiagnosticsResponse) bool {
	if p.github == nil || parseProduct(response.Product) != productFleet {
		return false
	}
	if response.GitHub.WorkflowJob != nil {
		return false
	}
	if response.Request.WorkflowJobID == 0 || response.Request.Owner == "" || response.Request.Repo == "" {
		return false
	}
	return response.Local == nil || response.Status == "ambiguous"
}

func (p *jobFactsProvider) enrichWithLocalGitHub(ctx context.Context, response *jobDiagnosticsResponse) {
	response.Diagnostics = append(response.Diagnostics, jobDiagnosticsDiagnostic{
		Level:   "info",
		Code:    "local_gh_workflow_job_fetch_attempt",
		Message: "resolver could not resolve the Fleet workflow job; trying local gh CLI",
	})
	p.logger.Printf("Job diagnostics info local_gh_workflow_job_fetch_attempt: resolver could not resolve the Fleet workflow job; trying local gh CLI")
	workflowJob, err := p.github.FetchWorkflowJob(ctx, response.Request)
	if err != nil {
		response.Diagnostics = append(response.Diagnostics, jobDiagnosticsDiagnostic{
			Level:   "warn",
			Code:    "local_gh_workflow_job_fetch_failed",
			Message: err.Error(),
		})
		p.logger.Printf("Job diagnostics warn local_gh_workflow_job_fetch_failed: %s", err.Error())
		return
	}
	response.GitHub.WorkflowJob = workflowJob
	response.Status = "partial"
	response.Diagnostics = append(response.Diagnostics, jobDiagnosticsDiagnostic{
		Level:   "info",
		Code:    "local_gh_workflow_job_fetched",
		Message: "workflow job details fetched with local gh CLI fallback",
	})
	p.logger.Printf("Job diagnostics info local_gh_workflow_job_fetched: workflow job details fetched with local gh CLI fallback")
}

// current returns the facts of the latest refresh, nil before the first.
func (p *jobFactsProvider) current() *workflowJobFacts {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.facts
}

func (p *jobFactsProvider) currentDiagnostics() *jobDiagnosticsResponse {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.diagnostics
}

func (p *jobFactsProvider) lookupTarget() string {
	return "job diagnostics resolver Lambda " + displayValue(p.resolver.functionName)
}

func displayValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "(not configured)"
	}
	return value
}

func (p *jobFactsProvider) logLookupSnapshot() {
	p.logger.Printf("Stack product detected: %s", cmp.Or(p.product, productFlex))
	p.logger.Printf("Job lookup target: %s", p.lookupTarget())

	jobID, facts := p.job.JobID, p.current()
	switch {
	case !facts.Found:
		p.logger.Printf("Job %d not found in %s", jobID, p.lookupTarget())
	case p.product == productFleet:
		p.logger.Printf("Job %d found in %s: workflow_run_id=%d state=%s current_instance_id=%s attempted_instance_ids=%s",
			jobID, p.lookupTarget(), facts.RunID, displayValue(facts.Status), displayValue(facts.CurrentInstanceID), displayList(facts.InstanceIDs))
	default:
		p.logger.Printf("Job %d found in %s: run_id=%d status=%s scheduling_state=%s current_instance_id=%s attempted_instance_ids=%s",
			jobID, p.lookupTarget(), facts.RunID, displayValue(facts.Status), displayValue(facts.SchedulingState), displayValue(facts.CurrentInstanceID), displayList(facts.InstanceIDs))
	}
}

// instanceUnavailableError explains why the job has no current instance.
func (p *jobFactsProvider) instanceUnavailableError() error {
	facts := p.current()
	if !facts.Found {
		return fmt.Errorf("job %d not found in %s", p.job.JobID, p.lookupTarget())
	}
	parts := []string{fmt.Sprintf("instance ID for job %d not available yet job_found_in=%q", p.job.JobID, p.lookupTarget())}
	if facts.Status != "" {
		parts = append(parts, "status="+facts.Status)
	}
	if facts.SchedulingState != "" {
		parts = append(parts, "scheduling_state="+facts.SchedulingState)
	}
	return errors.New(strings.Join(parts, " "))
}

func lookupWorkflowJobFacts(ctx context.Context, config *RunsOnConfig, job parsedGitHubJobURL, watch bool, logger *log.Logger) (*workflowJobFacts, error) {
	return waitForJobFacts(ctx, newJobFactsProvider(config, job, logger), watch, 5*time.Second)
}

// waitForJobFacts refreshes the job's facts until it has a current instance,
// or once when not watching.
func waitForJobFacts(ctx context.Context, provider *jobFactsProvider, watch bool, interval time.Duration) (*workflowJobFacts, error) {
	for {
		if err := provider.refresh(ctx); err != nil {
			return nil, err
		}
		if facts := provider.current(); facts.CurrentInstanceID != "" {
			return facts, nil
		}
		if !watch {
			return nil, provider.instanceUnavailableError()
		}
		provider.logger.Printf("Waiting for instance ID for job %d...\n", provider.job.JobID)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// refreshUntilCompleted refreshes job facts on every tick, then calls
// refreshed, until the local record reports completion. GitHub can report it
// first, but only the local record's completion guarantees it lists every
// instance the job used.
func (p *jobFactsProvider) refreshUntilCompleted(ctx context.Context, ticks <-chan time.Time, refreshed func()) {
	for {
		if diagnostics := p.currentDiagnostics(); diagnostics != nil && diagnostics.Local != nil && strings.TrimSpace(diagnostics.Local.CompletedAt) != "" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if err := p.refresh(ctx); err != nil {
				p.logger.Printf("Error refreshing job facts: %v", err)
			}
			refreshed()
		}
	}
}

func parseRunnerNameInstanceID(runnerName string) string {
	parts := strings.Split(strings.TrimSpace(runnerName), "--")
	if len(parts) < 2 {
		return ""
	}
	instanceID := strings.TrimSpace(parts[1])
	if strings.HasPrefix(instanceID, "i-") {
		return instanceID
	}
	return ""
}
