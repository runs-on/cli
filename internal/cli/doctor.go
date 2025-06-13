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
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
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
	cfn        *cloudformation.Client
	apprunner  *apprunner.Client
	cwl        *cloudwatchlogs.Client
	stackName  string
	httpClient *http.Client
	result     *DoctorResult
	outputs    map[string]string // Cache stack outputs
	workDir    string            // Temporary workspace directory
}

func NewStackDoctor(config *RunsOnConfig) *StackDoctor {
	return &StackDoctor{
		cfg:       config.AWSConfig,
		cfn:       cloudformation.NewFromConfig(config.AWSConfig),
		apprunner: apprunner.NewFromConfig(config.AWSConfig),
		cwl:       cloudwatchlogs.NewFromConfig(config.AWSConfig),
		stackName: config.StackName,
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
	d.printCheckResult("", "❌", "")
	return err
}

func (d *StackDoctor) loadStackOutputs(ctx context.Context) error {
	if d.outputs != nil {
		return nil // Already loaded
	}

	out, err := d.cfn.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
		StackName: &d.stackName,
	})
	if err != nil {
		return err
	}
	if len(out.Stacks) == 0 {
		return fmt.Errorf("stack %s not found", d.stackName)
	}

	d.outputs = make(map[string]string)
	for _, output := range out.Stacks[0].Outputs {
		d.outputs[*output.OutputKey] = *output.OutputValue
	}
	return nil
}

func (d *StackDoctor) checkStackHealth(ctx context.Context) error {
	fmt.Print("Checking CloudFormation stack health...")

	// Get stack status from the same API call
	out, err := d.cfn.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{
		StackName: &d.stackName,
	})
	if err != nil {
		return d.failCheck("CloudFormation stack health", "Failed to describe stack", err)
	}

	stack := out.Stacks[0]
	status := string(stack.StackStatus)

	if strings.Contains(status, "COMPLETE") && !strings.Contains(status, "ROLLBACK") {
		d.addCheck("CloudFormation stack health", "✅", fmt.Sprintf("Status: %s", status), nil)
		d.printCheckResult("", "✅", fmt.Sprintf("status: %s", status))
		return nil
	} else {
		d.addCheck("CloudFormation stack health", "❌", fmt.Sprintf("Status: %s", status), nil)
		d.printCheckResult("", "❌", fmt.Sprintf("status: %s", status))
		return fmt.Errorf("stack is in unhealthy state: %s", status)
	}
}

func (d *StackDoctor) checkAppRunnerService(ctx context.Context) error {
	fmt.Print("Checking AppRunner service...")

	serviceArn, ok := d.outputs["RunsOnServiceArn"]
	if !ok {
		err := fmt.Errorf("RunsOnServiceArn not found in stack outputs")
		return d.failCheck("AppRunner service running", "Service ARN not found", err)
	}

	expectedTag := d.outputs["RunsOnAppTag"]

	out, err := d.apprunner.DescribeService(ctx, &apprunner.DescribeServiceInput{
		ServiceArn: &serviceArn,
	})
	if err != nil {
		return d.failCheck("AppRunner service running", "Failed to describe service", err)
	}

	service := out.Service
	status := string(service.Status)

	if status == "RUNNING" {
		// Extract image tag from the service configuration
		imageUri := *service.SourceConfiguration.ImageRepository.ImageIdentifier
		parts := strings.Split(imageUri, ":")
		var actualTag string
		if len(parts) > 1 {
			actualTag = parts[len(parts)-1]
		}

		if actualTag == expectedTag {
			d.addCheck("AppRunner service running", "✅", fmt.Sprintf("Version: %s", actualTag), nil)
			d.printCheckResult("", "✅", fmt.Sprintf("version: %s", actualTag))
		} else {
			d.addCheck("AppRunner service running", "⚠️", fmt.Sprintf("Version mismatch - running: %s, expected: %s", actualTag, expectedTag), nil)
			d.printCheckResult("", "⚠️", fmt.Sprintf("version mismatch - running: %s, expected: %s", actualTag, expectedTag))
		}
		return nil
	} else {
		d.addCheck("AppRunner service running", "❌", fmt.Sprintf("Status: %s", status), nil)
		d.printCheckResult("", "❌", fmt.Sprintf("status: %s", status))
		return fmt.Errorf("service is not running: %s", status)
	}
}

func (d *StackDoctor) checkEndpointAccessibility(ctx context.Context) error {
	entryPoint, ok := d.outputs["RunsOnEntryPoint"]
	if !ok {
		fmt.Print("Checking AppRunner service endpoint...")
		err := fmt.Errorf("RunsOnEntryPoint not found in stack outputs")
		return d.failCheck("AppRunner service endpoint accessible", "Entry point not found", err)
	}

	// Ensure https:// prefix
	if !strings.HasPrefix(entryPoint, "http://") && !strings.HasPrefix(entryPoint, "https://") {
		entryPoint = "https://" + entryPoint
	}

	fmt.Printf("Checking AppRunner service endpoint (%s)...", entryPoint)

	// Check if endpoint is accessible
	resp, err := d.httpClient.Get(entryPoint)
	if err != nil {
		d.addCheck("AppRunner service endpoint accessible", "❌", fmt.Sprintf("Failed to connect to %s", entryPoint), err)
		d.printCheckResult("", "❌", "")
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

	entryPoint, ok := d.outputs["RunsOnEntryPoint"]
	if !ok {
		err := fmt.Errorf("RunsOnEntryPoint not found in stack outputs")
		return d.failCheck("AppRunner service returns 'Congrats'", "Entry point not found", err)
	}

	// Ensure https:// prefix
	if !strings.HasPrefix(entryPoint, "http://") && !strings.HasPrefix(entryPoint, "https://") {
		entryPoint = "https://" + entryPoint
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

func (d *StackDoctor) fetchLogs(ctx context.Context, since time.Duration) (int, error) {
	fmt.Print("Fetching AppRunner logs...")

	serviceArn, ok := d.outputs["RunsOnServiceArn"]
	if !ok {
		err := fmt.Errorf("RunsOnServiceArn not found in stack outputs")
		return 0, d.failCheck("Logs fetched", "Service ARN not found", err)
	}

	// Convert AppRunner ARN to CloudWatch log group ARN
	logGroupArn := getLogGroupArn(serviceArn)

	// Create logs directory in workspace
	logsDir := filepath.Join(d.workDir, "logs")
	err := os.MkdirAll(logsDir, 0755)
	if err != nil {
		return 0, d.failCheck("Logs fetched", "Failed to create logs directory", err)
	}

	startTime := time.Now().Add(-since)

	input := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupIdentifier: &logGroupArn,
		StartTime:          aws.Int64(startTime.UnixMilli()),
	}

	var totalLines int
	logFile, err := os.Create(filepath.Join(logsDir, "application.log"))
	if err != nil {
		return 0, d.failCheck("Logs fetched", "Failed to create log file", err)
	}
	defer logFile.Close()

	paginator := cloudwatchlogs.NewFilterLogEventsPaginator(d.cwl, input)
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return 0, d.failCheck("Logs fetched", "Failed to fetch logs", err)
		}

		for _, event := range output.Events {
			timestamp := time.UnixMilli(*event.Timestamp).Format("2006-01-02T15:04:05.000Z")
			line := fmt.Sprintf("%s [%s] %s\n", timestamp, *event.LogStreamName, *event.Message)
			logFile.WriteString(line)
			totalLines++
		}
	}

	d.addCheck("Logs fetched", "✅", fmt.Sprintf("Fetched %d lines since %s", totalLines, since), nil)
	d.printCheckResult("", "✅", fmt.Sprintf("%d lines since %s", totalLines, since))
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

	// Add logs directory from workspace
	logsDir := filepath.Join(d.workDir, "logs")
	err = addDirectoryToZip(zipWriter, logsDir)
	if err != nil {
		return "", fmt.Errorf("failed to add logs directory to zip: %w", err)
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

	// Load stack outputs once at the beginning
	if err := d.loadStackOutputs(ctx); err != nil {
		return fmt.Errorf("failed to load stack outputs: %w", err)
	}

	// Run all checks
	d.checkStackHealth(ctx)
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

func NewDoctorCmd() *cobra.Command {
	var since string

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose RunsOn stack health and export troubleshooting information",
		Long: `Diagnose RunsOn stack health and export troubleshooting information.

This command performs comprehensive health checks on your RunsOn CloudFormation stack:
- Verifies CloudFormation stack status
- Checks AppRunner service health and version
- Tests endpoint accessibility  
- Validates service configuration
- Fetches application logs

Results are exported as a timestamped ZIP file containing checks.json and logs.

The stack name can be overridden using the RUNS_ON_STACK_NAME environment variable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := getStackOutputs(cmd)
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
