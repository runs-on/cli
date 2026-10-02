package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi"
	"github.com/spf13/cobra"
)

// DoctorCheck statuses as written to checks.json. Terminal output shows them
// as emoji (see doctorStatusSymbol).
const (
	doctorCheckPass = "pass"
	doctorCheckFail = "fail"
	doctorCheckSkip = "skip"
)

type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

type DoctorResult struct {
	Timestamp time.Time     `json:"timestamp"`
	StackName string        `json:"stack_name"`
	Checks    []DoctorCheck `json:"checks"`
}

type doctorReadinessResponse struct {
	AppTag              string `json:"app_tag"`
	GitHubAppConfigured bool   `json:"github_app_configured"`
}

type StackDoctor struct {
	cfg        aws.Config
	cwl        cloudWatchLogsAPI
	ecs        *ecs.Client
	tagging    taggedResourcesAPI
	config     *RunsOnConfig
	httpClient *http.Client
	result     *DoctorResult
	workDir    string
	out        io.Writer
}

func NewStackDoctor(config *RunsOnConfig, out io.Writer) *StackDoctor {
	return &StackDoctor{
		cfg:     config.AWSConfig,
		cwl:     cloudwatchlogs.NewFromConfig(config.AWSConfig),
		ecs:     ecs.NewFromConfig(config.AWSConfig),
		tagging: resourcegroupstaggingapi.NewFromConfig(config.AWSConfig),
		config:  config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		result: &DoctorResult{
			Timestamp: time.Now(),
			StackName: config.StackName,
			Checks:    []DoctorCheck{},
		},
		out: out,
	}
}

func (d *StackDoctor) addCheck(name, status, result string, err error) {
	check := DoctorCheck{
		Name:   name,
		Status: status,
		Result: result,
	}
	if err != nil {
		check.Error = err.Error()
	}
	d.result.Checks = append(d.result.Checks, check)
}

func doctorStatusSymbol(status string) string {
	switch status {
	case doctorCheckPass:
		return "✅"
	case doctorCheckFail:
		return "❌"
	case doctorCheckSkip:
		return "⏭️"
	default:
		return status
	}
}

func (d *StackDoctor) printCheckResult(status, details string) {
	if details != "" {
		fmt.Fprintf(d.out, " %s (%s)\n", doctorStatusSymbol(status), details)
	} else {
		fmt.Fprintf(d.out, " %s\n", doctorStatusSymbol(status))
	}
}

// check records a check and prints the same result text.
func (d *StackDoctor) check(name, status, result string, err error) {
	d.addCheck(name, status, result, err)
	d.printCheckResult(status, result)
}

// failedChecksError names every failed check so the command exits non-zero.
// Skipped checks are not failures.
func (r *DoctorResult) failedChecksError() error {
	var failed []string
	for _, check := range r.Checks {
		if check.Status == doctorCheckFail {
			failed = append(failed, check.Name)
		}
	}
	switch len(failed) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("1 check failed: %s", failed[0])
	default:
		return fmt.Errorf("%d checks failed: %s", len(failed), strings.Join(failed, ", "))
	}
}

func (d *StackDoctor) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return d.httpClient.Do(req)
}

func (d *StackDoctor) getServiceURL() (string, error) {
	entryPoint := strings.TrimSpace(d.config.IngressURL)
	if entryPoint == "" {
		return "", fmt.Errorf("service URL not available")
	}
	return normalizeDoctorServiceURL(entryPoint), nil
}

func normalizeDoctorServiceURL(url string) string {
	url = strings.TrimSpace(url)
	if url != "" && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}
	return url
}

func doctorReadinessURL(serviceURL string) string {
	serviceURL = normalizeDoctorServiceURL(serviceURL)
	if serviceURL == "" {
		return "/readyz"
	}
	return strings.TrimRight(serviceURL, "/") + "/readyz"
}

func (d *StackDoctor) checkService(ctx context.Context) {
	serviceArn, err := discoverTaggedECSServiceARN(ctx, d.tagging, d.config.StackName)
	if err != nil {
		fmt.Fprint(d.out, "Checking service...")
		d.check("Service running", doctorCheckFail, "Service ARN not found", err)
		return
	}

	clusterName, serviceName, ok := parseDoctorECSServiceARN(serviceArn)
	if !ok {
		fmt.Fprint(d.out, "Checking service...")
		d.check("Service running", doctorCheckFail, "Invalid ECS service ARN", fmt.Errorf("parse ecs service ARN %q", serviceArn))
		return
	}

	consoleURL := fmt.Sprintf("https://%s.console.aws.amazon.com/ecs/v2/clusters/%s/services/%s/configuration/overview", d.cfg.Region, clusterName, serviceName)
	fmt.Fprintf(d.out, "Checking service (%s)...", consoleURL)

	output, err := d.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterName),
		Services: []string{serviceName},
	})
	if err != nil {
		d.check("Service running", doctorCheckFail, "Failed to describe service", err)
		return
	}
	if len(output.Failures) > 0 {
		d.check("Service running", doctorCheckFail, "Failed to describe service", fmt.Errorf("%s", aws.ToString(output.Failures[0].Reason)))
		return
	}
	if len(output.Services) == 0 {
		d.check("Service running", doctorCheckFail, "Service not found in response", fmt.Errorf("DescribeServices returned no services"))
		return
	}

	service := output.Services[0]
	status, result := aws.ToString(service.Status), doctorCheckFail
	if strings.EqualFold(status, "ACTIVE") && service.DesiredCount > 0 && service.RunningCount >= service.DesiredCount {
		status, result = "RUNNING", doctorCheckPass
	}
	tasks := fmt.Sprintf("%s (%d/%d tasks)", status, service.RunningCount, service.DesiredCount)
	// checks.json capitalizes "Status"; the terminal does not.
	d.addCheck("Service running", result, "Status: "+tasks, nil)
	d.printCheckResult(result, "status: "+tasks)
}

func parseDoctorECSServiceARN(arn string) (string, string, bool) {
	parts := strings.SplitN(strings.TrimSpace(arn), ":service/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	resourceParts := strings.Split(strings.Trim(parts[1], "/"), "/")
	if len(resourceParts) < 2 {
		return "", "", false
	}
	return resourceParts[len(resourceParts)-2], resourceParts[len(resourceParts)-1], true
}

func (d *StackDoctor) checkEndpointAccessibility(ctx context.Context) {
	entryPoint, err := d.getServiceURL()
	if err != nil {
		fmt.Fprint(d.out, "Checking service endpoint...")
		d.check("Service endpoint accessible", doctorCheckFail, "Failed to get service URL", err)
		return
	}

	fmt.Fprintf(d.out, "Checking service endpoint (%s)...", entryPoint)

	// checks.json names the endpoint; the terminal already printed it.
	resp, err := d.get(ctx, entryPoint)
	if err != nil {
		d.addCheck("Service endpoint accessible", doctorCheckFail, fmt.Sprintf("Failed to connect to %s", entryPoint), err)
		d.printCheckResult(doctorCheckFail, "failed to connect")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		d.addCheck("Service endpoint accessible", doctorCheckFail, fmt.Sprintf("HTTP %d from %s", resp.StatusCode, entryPoint), nil)
		d.printCheckResult(doctorCheckFail, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return
	}
	d.addCheck("Service endpoint accessible", doctorCheckPass, entryPoint, nil)
	d.printCheckResult(doctorCheckPass, "")
}

func (d *StackDoctor) checkReadiness(ctx context.Context) {
	fmt.Fprint(d.out, "Checking service readiness...")

	serviceURL, err := d.getServiceURL()
	if err != nil {
		d.check("Service readiness", doctorCheckFail, "Failed to get service URL", err)
		return
	}

	resp, err := d.get(ctx, doctorReadinessURL(serviceURL))
	if err != nil {
		d.check("Service readiness", doctorCheckFail, "Failed to connect", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		d.check("Service readiness", doctorCheckFail, "Failed to read response", err)
		return
	}

	var readiness doctorReadinessResponse
	if err := json.Unmarshal(body, &readiness); err != nil {
		d.check("Service readiness", doctorCheckFail, "Failed to parse readiness response", err)
		return
	}

	switch {
	case resp.StatusCode != http.StatusOK:
		d.check("Service readiness", doctorCheckFail, fmt.Sprintf("HTTP %d", resp.StatusCode), nil)
	case !readiness.GitHubAppConfigured:
		d.check("Service readiness", doctorCheckFail, "GitHub app is not configured", nil)
	default:
		d.check("Service readiness", doctorCheckPass, fmt.Sprintf("app_tag: %s", readiness.AppTag), nil)
	}
}

func (d *StackDoctor) checkHTTPHealth(ctx context.Context) {
	if d.config.Product == productFleet {
		fmt.Fprint(d.out, "Checking service endpoint...")
		d.check("Service endpoint accessible", doctorCheckSkip, "Skipped - Fleet does not expose a public service endpoint", nil)
		fmt.Fprint(d.out, "Checking service readiness...")
		d.check("Service readiness", doctorCheckSkip, "Skipped - Fleet does not expose a public readiness endpoint", nil)
		return
	}

	d.checkEndpointAccessibility(ctx)
	d.checkReadiness(ctx)
}

func (d *StackDoctor) fetchLogsFromGroup(ctx context.Context, logGroupIdentifier, outputName string, since time.Duration) (int, error) {
	logsDir := filepath.Join(d.workDir, "logs")

	startTime := time.Now().Add(-since)

	input := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupIdentifier: aws.String(logGroupIdentifier),
		StartTime:          aws.Int64(startTime.UnixMilli()),
	}

	var totalLines int
	logFile, err := os.Create(filepath.Join(logsDir, fmt.Sprintf("%s.log", outputName)))
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	paginator := cloudwatchlogs.NewFilterLogEventsPaginator(d.cwl, input)
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return 0, err
		}

		for _, event := range output.Events {
			timestamp := time.UnixMilli(*event.Timestamp).Format("2006-01-02T15:04:05.000Z")
			line := fmt.Sprintf("%s [%s] %s\n", timestamp, *event.LogStreamName, *event.Message)
			if _, err := logFile.WriteString(line); err != nil {
				return 0, err
			}
			totalLines++
		}
	}

	return totalLines, nil
}

func (d *StackDoctor) fetchLogs(ctx context.Context, since time.Duration) {
	// Always create the logs directory, even if we can't fetch logs.
	// createZipFile reports when it is missing.
	if err := os.MkdirAll(filepath.Join(d.workDir, "logs"), 0755); err != nil {
		return
	}

	serviceLogGroup := strings.TrimSpace(d.config.ServiceLogGroupName)
	if serviceLogGroup == "" {
		// Skip logs fetching for failed stacks or incomplete discoveries.
		d.addCheck("Logs fetched", doctorCheckSkip, "Skipped - service not available", nil)
		return
	}

	fmt.Fprintf(d.out, "Fetching application logs (since %s)...", since)
	appLines, err := d.fetchLogsFromGroup(ctx, serviceLogGroup, "application", since)
	if err != nil {
		d.check("Application logs fetched", doctorCheckFail, "Failed to fetch application logs", err)
		return
	}
	d.check("Application logs fetched", doctorCheckPass, fmt.Sprintf("%d lines", appLines), nil)
	d.addCheck("Service logs fetched", doctorCheckSkip, "Skipped - ECS stacks use the application log group", nil)
}

func (d *StackDoctor) saveResults() error {
	// Save checks.json in workspace
	checksData, err := json.MarshalIndent(d.result, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal results: %w", err)
	}

	checksPath := filepath.Join(d.workDir, "checks.json")
	err = os.WriteFile(checksPath, checksData, 0644)
	if err != nil {
		return fmt.Errorf("failed to write checks.json: %w", err)
	}

	return nil
}
func (d *StackDoctor) createZipFile() (string, error) {
	timestamp := time.Now().Format("2006-01-02-15-04-05")
	zipFileName := fmt.Sprintf("roc-doctor-%s.zip", timestamp)

	archive, err := newArchiveWriter(zipFileName)
	if err != nil {
		return "", fmt.Errorf("failed to create zip file: %w", err)
	}
	defer archive.Close()

	// Add checks.json from workspace
	checksPath := filepath.Join(d.workDir, "checks.json")
	err = archive.writeFile("checks.json", checksPath)
	if err != nil {
		return "", fmt.Errorf("failed to add checks.json to zip: %w", err)
	}

	// Add log files directly to zip
	logsDir := filepath.Join(d.workDir, "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return "", fmt.Errorf("failed to read logs directory: %w", err)
	}

	// Add log files if any exist
	for _, entry := range entries {
		if !entry.IsDir() {
			logPath := filepath.Join(logsDir, entry.Name())
			err = archive.writeFile(filepath.Join("logs", entry.Name()), logPath)
			if err != nil {
				return "", fmt.Errorf("failed to add log file %s to zip: %w", entry.Name(), err)
			}
		}
	}

	if err := archive.Close(); err != nil {
		return "", fmt.Errorf("failed to finalize zip file: %w", err)
	}
	return zipFileName, nil
}

func (d *StackDoctor) cleanup() {
	if d.workDir != "" {
		os.RemoveAll(d.workDir)
	}
}

func (d *StackDoctor) Run(ctx context.Context, since time.Duration) error {
	// Create temporary workspace directory
	var err error
	d.workDir, err = os.MkdirTemp("", "roc-doctor-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary workspace: %w", err)
	}
	defer d.cleanup()

	// Run all checks, but continue on failures so doctor can export partial
	// results. The failures are reported once the ZIP is written.
	d.checkService(ctx)
	d.checkHTTPHealth(ctx)
	d.fetchLogs(ctx, since)

	// Save results
	err = d.saveResults()
	if err != nil {
		return fmt.Errorf("failed to save results: %w", err)
	}

	// Create zip file
	zipFileName, err := d.createZipFile()
	if err != nil {
		return fmt.Errorf("failed to create zip file: %w", err)
	}

	// Get absolute path for output
	absPath, err := filepath.Abs(zipFileName)
	if err != nil {
		absPath = zipFileName
	}

	fmt.Fprintf(d.out, "\nFull results exported to: %s\n", absPath)

	return d.result.failedChecksError()
}

func NewDoctorCmd(stack *Stack) *cobra.Command {
	var since string

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose RunsOn stack health and export troubleshooting information",
		Long: `Diagnose RunsOn stack health and export troubleshooting information.

This command performs comprehensive health checks on your RunsOn stack:
- Checks ECS service health
- Tests endpoint accessibility for Flex stacks
- Validates service readiness for Flex stacks
- Fetches application logs

Results are exported as a timestamped ZIP file containing checks.json and logs.
The command exits non-zero when any check fails, after exporting the ZIP file.

The stack name can be overridden using the RUNS_ON_STACK_NAME or RUNS_ON_STACK environment variable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := stack.discoverResources(cmd)
			if err != nil {
				return err
			}

			// Parse since duration
			duration, err := time.ParseDuration(since)
			if err != nil {
				return fmt.Errorf("invalid --since value: %w", err)
			}

			doctor := NewStackDoctor(config, cmd.OutOrStdout())
			return doctor.Run(cmd.Context(), duration)
		},
	}

	cmd.Flags().StringVar(&since, "since", "24h", "Fetch logs since duration (e.g. 30m, 2h, 24h)")

	return cmd
}
