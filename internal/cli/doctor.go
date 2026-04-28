package cli

import (
	"archive/zip"
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
	cwl        *cloudwatchlogs.Client
	ecs        *ecs.Client
	tagging    *resourcegroupstaggingapi.Client
	config     *RunsOnConfig
	httpClient *http.Client
	result     *DoctorResult
	workDir    string
	serviceARN string
}

func NewStackDoctor(config *RunsOnConfig) *StackDoctor {
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

func (d *StackDoctor) printCheckResult(status, details string) {
	if details != "" {
		fmt.Printf(" %s (%s)\n", status, details)
	} else {
		fmt.Printf(" %s\n", status)
	}
}

func (d *StackDoctor) failCheck(name, message string, err error) error {
	d.addCheck(name, "❌", message, err)
	d.printCheckResult("❌", message)
	return err
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

func (d *StackDoctor) discoverServiceARN(ctx context.Context) (string, error) {
	if d.serviceARN != "" {
		return d.serviceARN, nil
	}

	serviceARN, err := discoverTaggedECSServiceARN(ctx, d.tagging, d.config.StackName)
	if err != nil {
		return "", err
	}
	d.serviceARN = serviceARN
	return serviceARN, nil
}

func (d *StackDoctor) checkService(ctx context.Context) error {
	return d.checkECSService(ctx, "Service running")
}

func (d *StackDoctor) checkECSService(ctx context.Context, checkName string) error {
	serviceArn, err := d.discoverServiceARN(ctx)
	if err != nil {
		fmt.Print("Checking service...")
		return d.failCheck(checkName, "Service ARN not found", err)
	}

	clusterName, serviceName, ok := parseDoctorECSServiceARN(serviceArn)
	if !ok {
		fmt.Print("Checking service...")
		return d.failCheck(checkName, "Invalid ECS service ARN", fmt.Errorf("parse ecs service ARN %q", serviceArn))
	}

	consoleURL := fmt.Sprintf("https://%s.console.aws.amazon.com/ecs/v2/clusters/%s/services/%s/configuration/overview", d.cfg.Region, clusterName, serviceName)
	fmt.Printf("Checking service (%s)...", consoleURL)

	output, err := d.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  aws.String(clusterName),
		Services: []string{serviceName},
	})
	if err != nil {
		return d.failCheck(checkName, "Failed to describe service", err)
	}
	if len(output.Failures) > 0 {
		return d.failCheck(checkName, "Failed to describe service", fmt.Errorf("%s", aws.ToString(output.Failures[0].Reason)))
	}
	if len(output.Services) == 0 {
		return d.failCheck(checkName, "Service not found in response", fmt.Errorf("DescribeServices returned no services"))
	}

	service := output.Services[0]
	status := aws.ToString(service.Status)
	if strings.EqualFold(status, "ACTIVE") && service.DesiredCount > 0 && service.RunningCount >= service.DesiredCount {
		d.addCheck(checkName, "✅", fmt.Sprintf("Status: RUNNING (%d/%d tasks)", service.RunningCount, service.DesiredCount), nil)
		d.printCheckResult("✅", fmt.Sprintf("status: RUNNING (%d/%d tasks)", service.RunningCount, service.DesiredCount))
		return nil
	}

	d.addCheck(checkName, "❌", fmt.Sprintf("Status: %s (%d/%d tasks)", status, service.RunningCount, service.DesiredCount), nil)
	d.printCheckResult("❌", fmt.Sprintf("status: %s (%d/%d tasks)", status, service.RunningCount, service.DesiredCount))
	return fmt.Errorf("service is not healthy: %s (%d/%d tasks)", status, service.RunningCount, service.DesiredCount)
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

func (d *StackDoctor) checkEndpointAccessibility() error {
	entryPoint, err := d.getServiceURL()
	if err != nil {
		fmt.Print("Checking service endpoint...")
		return d.failCheck("Service endpoint accessible", "Failed to get service URL", err)
	}

	fmt.Printf("Checking service endpoint (%s)...", entryPoint)

	// Check if endpoint is accessible
	resp, err := d.httpClient.Get(entryPoint)
	if err != nil {
		d.addCheck("Service endpoint accessible", "❌", fmt.Sprintf("Failed to connect to %s", entryPoint), err)
		d.printCheckResult("❌", "failed to connect")
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		d.addCheck("Service endpoint accessible", "✅", entryPoint, nil)
		d.printCheckResult("✅", "")
	} else {
		d.addCheck("Service endpoint accessible", "❌", fmt.Sprintf("HTTP %d from %s", resp.StatusCode, entryPoint), nil)
		d.printCheckResult("❌", fmt.Sprintf("HTTP %d", resp.StatusCode))
		return fmt.Errorf("endpoint returned HTTP %d", resp.StatusCode)
	}

	return nil
}

func (d *StackDoctor) checkReadiness() error {
	fmt.Print("Checking service readiness...")

	serviceURL, err := d.getServiceURL()
	if err != nil {
		return d.failCheck("Service readiness", "Failed to get service URL", err)
	}

	resp, err := d.httpClient.Get(doctorReadinessURL(serviceURL))
	if err != nil {
		return d.failCheck("Service readiness", "Failed to connect", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return d.failCheck("Service readiness", "Failed to read response", err)
	}

	var readiness doctorReadinessResponse
	if err := json.Unmarshal(body, &readiness); err != nil {
		return d.failCheck("Service readiness", "Failed to parse readiness response", err)
	}

	if resp.StatusCode != http.StatusOK {
		d.addCheck("Service readiness", "❌", fmt.Sprintf("HTTP %d", resp.StatusCode), nil)
		d.printCheckResult("❌", fmt.Sprintf("HTTP %d", resp.StatusCode))
		return fmt.Errorf("readiness endpoint returned HTTP %d", resp.StatusCode)
	}
	if !readiness.GitHubAppConfigured {
		d.addCheck("Service readiness", "❌", "GitHub app is not configured", nil)
		d.printCheckResult("❌", "GitHub app is not configured")
		return fmt.Errorf("github app is not configured")
	}

	d.addCheck("Service readiness", "✅", fmt.Sprintf("app_tag: %s", readiness.AppTag), nil)
	d.printCheckResult("✅", fmt.Sprintf("app_tag: %s", readiness.AppTag))
	return nil
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

func (d *StackDoctor) fetchLogs(ctx context.Context, since time.Duration) (int, error) {
	// Always create logs directory structure, even if we can't fetch logs
	logsDir := filepath.Join(d.workDir, "logs")
	err := os.MkdirAll(logsDir, 0755)
	if err != nil {
		return 0, fmt.Errorf("failed to create logs directory: %w", err)
	}

	serviceLogGroup := strings.TrimSpace(d.config.ServiceLogGroupName)
	if serviceLogGroup == "" {
		// Skip logs fetching for failed stacks or incomplete discoveries.
		d.addCheck("Logs fetched", "⏭️", "Skipped - service not available", nil)
		return 0, nil
	}

	fmt.Printf("Fetching application logs (since %s)...", since)
	appLines, err := d.fetchLogsFromGroup(ctx, serviceLogGroup, "application", since)
	if err != nil {
		return 0, d.failCheck("Application logs fetched", "Failed to fetch application logs", err)
	}
	d.addCheck("Application logs fetched", "✅", fmt.Sprintf("%d lines", appLines), nil)
	d.printCheckResult("✅", fmt.Sprintf("%d lines", appLines))

	d.addCheck("Service logs fetched", "⏭️", "Skipped - ECS stacks use the application log group", nil)
	return appLines, nil
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

	zipFile, err := os.Create(zipFileName)
	if err != nil {
		return "", fmt.Errorf("failed to create zip file: %w", err)
	}
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	// Add checks.json from workspace
	checksPath := filepath.Join(d.workDir, "checks.json")
	err = addFileToZipWithPath(zipWriter, checksPath, "checks.json")
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
			err = addFileToZipWithPath(zipWriter, logPath, filepath.Join("logs", entry.Name()))
			if err != nil {
				return "", fmt.Errorf("failed to add log file %s to zip: %w", entry.Name(), err)
			}
		}
	}

	return zipFileName, nil
}

func addFileToZipWithPath(zipWriter *zip.Writer, filePath, zipPath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = zipPath

	writer, err := zipWriter.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = io.Copy(writer, file)
	return err
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

	// Run all checks, but continue on failures so doctor can export partial results.
	_ = d.checkService(ctx)
	_ = d.checkEndpointAccessibility()
	_ = d.checkReadiness()
	_, _ = d.fetchLogs(ctx, since)

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

	fmt.Printf("\nFull results exported to: %s\n", absPath)

	return nil
}

func NewDoctorCmd(stack *Stack) *cobra.Command {
	var since string

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose RunsOn stack health and export troubleshooting information",
		Long: `Diagnose RunsOn stack health and export troubleshooting information.

This command performs comprehensive health checks on your RunsOn stack:
- Checks ECS service health
- Tests endpoint accessibility
- Validates service readiness
- Fetches application logs

Results are exported as a timestamped ZIP file containing checks.json and logs.

The stack name can be overridden using the RUNS_ON_STACK_NAME or RUNS_ON_STACK environment variable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := stack.getStackOutputs(cmd)
			if err != nil {
				return err
			}

			// Parse since duration
			duration, err := time.ParseDuration(since)
			if err != nil {
				return fmt.Errorf("invalid --since value: %w", err)
			}

			doctor := NewStackDoctor(config)
			return doctor.Run(cmd.Context(), duration)
		},
	}

	cmd.Flags().StringVar(&since, "since", "24h", "Fetch logs since duration (e.g. 30m, 2h, 24h)")

	return cmd
}
