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
	"github.com/aws/aws-sdk-go-v2/service/apprunner"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
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

type StackDoctor struct {
	cfg        aws.Config
	apprunner  *apprunner.Client
	cwl        *cloudwatchlogs.Client
	config     *RunsOnConfig // Discovered resources
	httpClient *http.Client
	result     *DoctorResult
	workDir    string // Temporary workspace directory
}

func NewStackDoctor(config *RunsOnConfig) *StackDoctor {
	return &StackDoctor{
		cfg:       config.AWSConfig,
		apprunner: apprunner.NewFromConfig(config.AWSConfig),
		cwl:       cloudwatchlogs.NewFromConfig(config.AWSConfig),
		config:    config,
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

func (d *StackDoctor) printCheckResult(message, status, details string) {
	if details != "" {
		fmt.Printf(" %s (%s)\n", status, details)
	} else {
		fmt.Printf(" %s\n", status)
	}
}

func (d *StackDoctor) failCheck(name, message string, err error) error {
	d.addCheck(name, "❌", message, err)
	d.printCheckResult("", "❌", message)
	return err
}

// getServiceURL gets the AppRunner service URL by calling DescribeService
func (d *StackDoctor) getServiceURL(ctx context.Context) (string, error) {
	serviceArn := d.config.AppRunnerServiceArn
	if serviceArn == "" {
		return "", fmt.Errorf("AppRunner service ARN not found")
	}

	out, err := d.apprunner.DescribeService(ctx, &apprunner.DescribeServiceInput{
		ServiceArn: &serviceArn,
	})
	if err != nil {
		return "", err
	}

	if out.Service != nil && out.Service.ServiceUrl != nil {
		url := *out.Service.ServiceUrl
		if !strings.HasPrefix(url, "https://") {
			url = "https://" + url
		}
		return url, nil
	}
	return "", fmt.Errorf("service URL not available")
}

func (d *StackDoctor) checkAppRunnerService(ctx context.Context) error {
	serviceArn := d.config.AppRunnerServiceArn
	if serviceArn == "" {
		fmt.Print("Checking AppRunner service...")
		err := fmt.Errorf("AppRunner service ARN not found in discovered resources")
		return d.failCheck("AppRunner service running", "Service ARN not found", err)
	}

	// Extract service name from ARN for console URL
	// ARN format: arn:aws:apprunner:region:account:service/service-name/service-id
	parts := strings.Split(serviceArn, "/")
	var serviceName string
	if len(parts) >= 2 {
		serviceName = parts[1]
	}

	region := d.cfg.Region
	appRunnerURL := fmt.Sprintf("https://console.aws.amazon.com/apprunner/home?region=%s#/services/%s", region, serviceName)
	fmt.Printf("Checking AppRunner service (%s)...", appRunnerURL)

	out, err := d.apprunner.DescribeService(ctx, &apprunner.DescribeServiceInput{
		ServiceArn: &serviceArn,
	})
	if err != nil {
		return d.failCheck("AppRunner service running", "Failed to describe service", err)
	}

	service := out.Service
	status := string(service.Status)

	if status == "RUNNING" {
		d.addCheck("AppRunner service running", "✅", fmt.Sprintf("Status: %s", status), nil)
		d.printCheckResult("", "✅", fmt.Sprintf("status: %s", status))
		return nil
	} else {
		d.addCheck("AppRunner service running", "❌", fmt.Sprintf("Status: %s", status), nil)
		d.printCheckResult("", "❌", fmt.Sprintf("status: %s", status))
		return fmt.Errorf("service is not running: %s", status)
	}
}

func (d *StackDoctor) checkEndpointAccessibility(ctx context.Context) error {
	entryPoint, err := d.getServiceURL(ctx)
	if err != nil {
		fmt.Print("Checking AppRunner service endpoint...")
		return d.failCheck("AppRunner service endpoint accessible", "Failed to get service URL", err)
	}

	fmt.Printf("Checking AppRunner service endpoint (%s)...", entryPoint)

	// Check if endpoint is accessible
	resp, err := d.httpClient.Get(entryPoint)
	if err != nil {
		d.addCheck("AppRunner service endpoint accessible", "❌", fmt.Sprintf("Failed to connect to %s", entryPoint), err)
		d.printCheckResult("", "❌", "failed to connect")
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		d.addCheck("AppRunner service endpoint accessible", "✅", entryPoint, nil)
		d.printCheckResult("", "✅", "")
	} else {
		d.addCheck("AppRunner service endpoint accessible", "❌", fmt.Sprintf("HTTP %d from %s", resp.StatusCode, entryPoint), nil)
		d.printCheckResult("", "❌", fmt.Sprintf("HTTP %d", resp.StatusCode))
		return fmt.Errorf("endpoint returned HTTP %d", resp.StatusCode)
	}

	return nil
}

func (d *StackDoctor) checkCongratsResponse(ctx context.Context) error {
	fmt.Print("Checking for 'Congrats' response...")

	entryPoint, err := d.getServiceURL(ctx)
	if err != nil {
		return d.failCheck("AppRunner service returns 'Congrats'", "Failed to get service URL", err)
	}

	resp, err := d.httpClient.Get(entryPoint)
	if err != nil {
		return d.failCheck("AppRunner service returns 'Congrats'", "Failed to connect", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return d.failCheck("AppRunner service returns 'Congrats'", "Failed to read response", err)
	}

	bodyStr := string(body)
	if strings.Contains(bodyStr, "Congrats") {
		d.addCheck("AppRunner service returns 'Congrats'", "✅", "Response contains 'Congrats'", nil)
		d.printCheckResult("", "✅", "")
		return nil
	} else {
		d.addCheck("AppRunner service returns 'Congrats'", "❌", "Response does not contain 'Congrats'", nil)
		d.printCheckResult("", "❌", "AppRunner service not configured yet")
		return fmt.Errorf("response does not contain 'Congrats'")
	}
}

func (d *StackDoctor) fetchLogsFromGroup(ctx context.Context, serviceArn, logGroupType string, since time.Duration) (int, error) {
	// Convert AppRunner ARN to CloudWatch log group ARN
	logGroupArn := getLogGroupArn(serviceArn, logGroupType)

	logsDir := filepath.Join(d.workDir, "logs")

	startTime := time.Now().Add(-since)

	input := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupIdentifier: &logGroupArn,
		StartTime:          aws.Int64(startTime.UnixMilli()),
	}

	var totalLines int
	logFile, err := os.Create(filepath.Join(logsDir, fmt.Sprintf("%s.log", logGroupType)))
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
			logFile.WriteString(line)
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

	serviceArn := d.config.AppRunnerServiceArn
	if serviceArn == "" {
		// Skip logs fetching for failed stacks - this is expected
		d.addCheck("Logs fetched", "⏭️", "Skipped - service not available", nil)
		return 0, nil
	}

	// Fetch application logs
	fmt.Printf("Fetching AppRunner application logs (since %s)...", since)
	appLines, err := d.fetchLogsFromGroup(ctx, serviceArn, "application", since)
	if err != nil {
		return 0, d.failCheck("Application logs fetched", "Failed to fetch application logs", err)
	}
	d.addCheck("Application logs fetched", "✅", fmt.Sprintf("%d lines", appLines), nil)
	d.printCheckResult("", "✅", fmt.Sprintf("%d lines", appLines))

	// Fetch service logs (always from last 14 days)
	fmt.Print("Fetching AppRunner service logs (since 14 days)...")
	serviceLines, err := d.fetchLogsFromGroup(ctx, serviceArn, "service", 14*24*time.Hour)
	if err != nil {
		return 0, d.failCheck("Service logs fetched", "Failed to fetch service logs", err)
	}
	d.addCheck("Service logs fetched", "✅", fmt.Sprintf("%d lines", serviceLines), nil)
	d.printCheckResult("", "✅", fmt.Sprintf("%d lines", serviceLines))

	totalLines := appLines + serviceLines
	return totalLines, nil
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

func addDirectoryToZip(zipWriter *zip.Writer, dirname string) error {
	return filepath.Walk(dirname, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = path

		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			return err
		}

		_, err = io.Copy(writer, file)
		return err
	})
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

	// Run all checks
	d.checkAppRunnerService(ctx)
	d.checkEndpointAccessibility(ctx)
	d.checkCongratsResponse(ctx)
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
- Checks AppRunner service health
- Tests endpoint accessibility
- Validates service configuration
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
