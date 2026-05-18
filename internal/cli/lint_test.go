package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runs-on/config/pkg/validate"
)

const validLintYAML = `
runners:
  test-runner:
    cpu: [2]
    ram: [16]
    family: [c7a]

pools:
  test-pool:
    runner: test-runner
    schedule:
      - name: default
        hot: 1
        stopped: 2
`

const invalidLintYAML = `
runners:
  invalid-runner:
    cpu: "not-a-number"
    spot: "invalid-spot-value"
    family: []

pools:
  invalid-pool:
    runner: ""
    schedule:
      - name: test
        hot: -1
        stopped: -2
`

func writeRunsOnConfig(t *testing.T, dir, content string) string {
	t.Helper()

	path := filepath.Join(dir, "runs-on.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write %s: %v", path, err)
	}
	return path
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to capture stdout: %v", err)
	}
	os.Stdout = w

	fnErr := fn()

	if err := w.Close(); err != nil {
		os.Stdout = oldStdout
		t.Fatalf("Failed to close stdout writer: %v", err)
	}
	os.Stdout = oldStdout

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("Failed to read stdout: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Failed to close stdout reader: %v", err)
	}

	return buf.String(), fnErr
}

func withStdin(t *testing.T, input string, fn func() error) error {
	t.Helper()

	stdinFile := filepath.Join(t.TempDir(), "stdin.yml")
	if err := os.WriteFile(stdinFile, []byte(input), 0644); err != nil {
		t.Fatalf("Failed to write stdin fixture: %v", err)
	}

	f, err := os.Open(stdinFile)
	if err != nil {
		t.Fatalf("Failed to open stdin fixture: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Fatalf("Failed to close stdin fixture: %v", err)
		}
	}()

	oldStdin := os.Stdin
	os.Stdin = f
	defer func() {
		os.Stdin = oldStdin
	}()

	return fn()
}

func withWorkingDir(t *testing.T, dir string, fn func()) {
	t.Helper()

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get working directory: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Failed to change working directory: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("Failed to restore working directory: %v", err)
		}
	}()

	fn()
}

func TestLintFile_ValidFile(t *testing.T) {
	tmpDir := t.TempDir()
	validFile := writeRunsOnConfig(t, tmpDir, validLintYAML)

	output, err := captureStdout(t, func() error {
		return lintFile(context.Background(), validFile, "text")
	})

	if err != nil {
		t.Errorf("lintFile returned error for valid file: %v", err)
	}

	if !strings.Contains(output, "is valid!") {
		t.Errorf("Expected valid output, got: %s", output)
	}
}

func TestLintFile_InvalidFile(t *testing.T) {
	tmpDir := t.TempDir()
	invalidFile := writeRunsOnConfig(t, tmpDir, invalidLintYAML)

	output, err := captureStdout(t, func() error {
		return lintFile(context.Background(), invalidFile, "text")
	})

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	if !strings.Contains(output, "has ") || !strings.Contains(output, "error(s)") {
		t.Errorf("Expected error output, got: %s", output)
	}
	if !strings.Contains(output, "Please fix the errors above") {
		t.Errorf("Expected fix guidance in output, got: %s", output)
	}
}

func TestLintFile_NonexistentFile(t *testing.T) {
	ctx := context.Background()
	err := lintFile(ctx, "/nonexistent/file.yml", "text")

	if err == nil {
		t.Error("Expected error for nonexistent file")
	}

	if !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("Expected validation error, got: %v", err)
	}
}

func TestLintStdin_ValidInput(t *testing.T) {
	output, err := captureStdout(t, func() error {
		return withStdin(t, validLintYAML, func() error {
			return lintStdin(context.Background(), "text")
		})
	})

	if err != nil {
		t.Errorf("lintStdin returned error for valid input: %v", err)
	}

	if !strings.Contains(output, "Configuration file '<stdin>' is valid!") {
		t.Errorf("Expected valid stdin output, got: %s", output)
	}
}

func TestLintStdin_InvalidInput(t *testing.T) {
	output, err := captureStdout(t, func() error {
		return withStdin(t, invalidLintYAML, func() error {
			return lintStdin(context.Background(), "text")
		})
	})

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	if !strings.Contains(output, "Configuration file '<stdin>' has") {
		t.Errorf("Expected invalid stdin output, got: %s", output)
	}
}

func TestLintCommand_InvalidFileReturnsError(t *testing.T) {
	tmpDir := t.TempDir()
	invalidFile := writeRunsOnConfig(t, tmpDir, invalidLintYAML)

	output, err := captureStdout(t, func() error {
		cmd := NewLintCmd()
		cmd.SetArgs([]string{"--format", "json", invalidFile})
		return cmd.Execute()
	})

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error from Cobra command execution, got: %v", err)
	}

	var result struct {
		Valid       bool `json:"valid"`
		Diagnostics []struct {
			Severity string `json:"severity"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("Failed to parse JSON output: %v", err)
	}
	if result.Valid {
		t.Error("Expected valid=false for invalid command execution")
	}
	if len(result.Diagnostics) == 0 {
		t.Fatal("Expected diagnostics for invalid command execution")
	}
	if result.Diagnostics[0].Severity != "error" {
		t.Errorf("Expected first diagnostic severity error, got %s", result.Diagnostics[0].Severity)
	}
}

func TestLintAllFiles_NoFiles(t *testing.T) {
	tmpDir := t.TempDir()

	var output string
	var err error
	withWorkingDir(t, tmpDir, func() {
		output, err = captureStdout(t, func() error {
			return lintAllFiles(context.Background(), "text")
		})
	})

	if err != nil {
		t.Errorf("lintAllFiles returned error when no files found: %v", err)
	}

	if !strings.Contains(output, "No runs-on.yml files found") {
		t.Errorf("Expected 'No runs-on.yml files found' message, got: %s", output)
	}
}

func TestLintAllFiles_MultipleFiles(t *testing.T) {
	tmpDir := t.TempDir()

	subDir1 := filepath.Join(tmpDir, "dir1")
	subDir2 := filepath.Join(tmpDir, "dir2")
	if err := os.MkdirAll(subDir1, 0755); err != nil {
		t.Fatalf("Failed to create %s: %v", subDir1, err)
	}
	if err := os.MkdirAll(subDir2, 0755); err != nil {
		t.Fatalf("Failed to create %s: %v", subDir2, err)
	}

	writeRunsOnConfig(t, subDir1, validLintYAML)
	writeRunsOnConfig(t, subDir2, invalidLintYAML)

	var output string
	var err error
	withWorkingDir(t, tmpDir, func() {
		output, err = captureStdout(t, func() error {
			return lintAllFiles(context.Background(), "text")
		})
	})

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	if !strings.Contains(output, "dir1/runs-on.yml") || !strings.Contains(output, "dir2/runs-on.yml") {
		t.Errorf("Expected to find both files, got: %s", output)
	}
	if !strings.Contains(output, "Detailed errors:") {
		t.Errorf("Expected detailed errors output, got: %s", output)
	}
}

func TestOutputLintResults_TextFormat(t *testing.T) {
	diags := []validate.Diagnostic{
		{
			Path:     "test.yml",
			Line:     5,
			Column:   10,
			Message:  "Deprecated field",
			Severity: validate.SeverityWarning,
		},
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := outputLintResults(diags, "test.yml", "text")

	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Errorf("outputLintResults returned error: %v", err)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("Failed to read stdout: %v", err)
	}
	output := buf.String()

	// Should contain warning information
	if !strings.Contains(output, "warning") && !strings.Contains(output, "⚠️") {
		t.Errorf("Expected warning output, got: %s", output)
	}
}

func TestOutputLintResults_TextFormatWithErrors(t *testing.T) {
	diags := []validate.Diagnostic{
		{
			Path:     "test.yml",
			Line:     5,
			Column:   10,
			Message:  "Invalid value",
			Severity: validate.SeverityError,
		},
	}

	output, err := captureStdout(t, func() error {
		return outputLintResults(diags, "test.yml", "text")
	})

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}
	if !strings.Contains(output, "Configuration file 'test.yml' has 1 error(s)") {
		t.Errorf("Expected text error summary, got: %s", output)
	}
	if !strings.Contains(output, "error: Invalid value") {
		t.Errorf("Expected diagnostic message, got: %s", output)
	}
}

func TestOutputLintResults_JSONFormat(t *testing.T) {
	diags := []validate.Diagnostic{
		{
			Path:     "test.yml",
			Line:     7,
			Column:   3,
			Message:  "Deprecated field",
			Severity: validate.SeverityWarning,
		},
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := outputLintResults(diags, "test.yml", "json")

	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Errorf("outputLintResults returned error: %v", err)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("Failed to read stdout: %v", err)
	}

	// Parse JSON to verify structure
	var result struct {
		Valid       bool `json:"valid"`
		Diagnostics []struct {
			Path     string `json:"path"`
			Line     int    `json:"line"`
			Column   int    `json:"column"`
			Message  string `json:"message"`
			Severity string `json:"severity"`
		} `json:"diagnostics"`
	}

	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Errorf("Failed to parse JSON output: %v", err)
	}

	// Warnings only should still be valid
	if !result.Valid {
		t.Error("Expected valid=true for warning-only diagnostics")
	}

	if len(result.Diagnostics) != 1 {
		t.Errorf("Expected 1 diagnostic, got %d", len(result.Diagnostics))
	}

	if result.Diagnostics[0].Severity != "warning" {
		t.Errorf("Expected warning severity, got %s", result.Diagnostics[0].Severity)
	}
}

func TestOutputLintResults_JSONFormatWithErrors(t *testing.T) {
	diags := []validate.Diagnostic{
		{
			Path:     "test.yml",
			Line:     7,
			Column:   3,
			Message:  "Invalid value",
			Severity: validate.SeverityError,
		},
	}

	output, err := captureStdout(t, func() error {
		return outputLintResults(diags, "test.yml", "json")
	})

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	var result struct {
		Valid       bool `json:"valid"`
		Diagnostics []struct {
			Path     string `json:"path"`
			Line     int    `json:"line"`
			Column   int    `json:"column"`
			Message  string `json:"message"`
			Severity string `json:"severity"`
		} `json:"diagnostics"`
	}

	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Errorf("Failed to parse JSON output: %v", err)
	}
	if result.Valid {
		t.Error("Expected valid=false for error diagnostics")
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("Expected 1 diagnostic, got %d", len(result.Diagnostics))
	}
	if result.Diagnostics[0].Message != "Invalid value" {
		t.Errorf("Expected diagnostic message to be preserved, got %s", result.Diagnostics[0].Message)
	}
	if result.Diagnostics[0].Severity != "error" {
		t.Errorf("Expected error severity, got %s", result.Diagnostics[0].Severity)
	}
}

func TestOutputLintResults_SARIFFormat(t *testing.T) {
	diags := []validate.Diagnostic{
		{
			Path:     "test.yml",
			Line:     5,
			Column:   10,
			Message:  "Deprecated field",
			Severity: validate.SeverityWarning,
		},
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := outputLintResults(diags, "test.yml", "sarif")

	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Errorf("outputLintResults returned error: %v", err)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("Failed to read stdout: %v", err)
	}

	// Parse SARIF JSON to verify structure
	var result struct {
		Version string `json:"version"`
		Runs    []struct {
			Tool struct {
				Driver struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"driver"`
			} `json:"tool"`
			Results []struct {
				RuleID  string `json:"ruleId"`
				Level   string `json:"level"`
				Message struct {
					Text string `json:"text"`
				} `json:"message"`
				Locations []struct {
					PhysicalLocation struct {
						URI    string `json:"uri"`
						Region struct {
							StartLine   int `json:"startLine"`
							StartColumn int `json:"startColumn"`
						} `json:"region"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}

	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Errorf("Failed to parse SARIF JSON output: %v", err)
	}

	if result.Version != "2.1.0" {
		t.Errorf("Expected SARIF version 2.1.0, got %s", result.Version)
	}

	if len(result.Runs) != 1 {
		t.Errorf("Expected 1 run, got %d", len(result.Runs))
	}

	if len(result.Runs[0].Results) != 1 {
		t.Errorf("Expected 1 result, got %d", len(result.Runs[0].Results))
	}

	if result.Runs[0].Results[0].Level != "warning" {
		t.Errorf("Expected warning level, got %s", result.Runs[0].Results[0].Level)
	}
}

func TestOutputLintResults_SARIFFormatWithErrors(t *testing.T) {
	diags := []validate.Diagnostic{
		{
			Path:     "test.yml",
			Line:     5,
			Column:   10,
			Message:  "Invalid value",
			Severity: validate.SeverityError,
		},
	}

	output, err := captureStdout(t, func() error {
		return outputLintResults(diags, "test.yml", "sarif")
	})

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	var result struct {
		Version string `json:"version"`
		Runs    []struct {
			Results []struct {
				Level   string `json:"level"`
				Message struct {
					Text string `json:"text"`
				} `json:"message"`
				Locations []struct {
					PhysicalLocation struct {
						URI string `json:"uri"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}

	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Errorf("Failed to parse SARIF JSON output: %v", err)
	}
	if len(result.Runs) != 1 {
		t.Fatalf("Expected 1 run, got %d", len(result.Runs))
	}
	if len(result.Runs[0].Results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(result.Runs[0].Results))
	}
	got := result.Runs[0].Results[0]
	if got.Level != "error" {
		t.Errorf("Expected error level, got %s", got.Level)
	}
	if got.Message.Text != "Invalid value" {
		t.Errorf("Expected SARIF message to be preserved, got %s", got.Message.Text)
	}
	if len(got.Locations) != 1 || got.Locations[0].PhysicalLocation.URI != "test.yml" {
		t.Errorf("Expected SARIF location URI to be preserved, got %+v", got.Locations)
	}
}

func TestOutputLintResults_InvalidFormat(t *testing.T) {
	diags := []validate.Diagnostic{}

	err := outputLintResults(diags, "test.yml", "invalid")

	if err == nil {
		t.Error("Expected error for invalid format")
	}

	if !strings.Contains(err.Error(), "invalid format") {
		t.Errorf("Expected 'invalid format' error, got: %v", err)
	}
}

func TestIsValidDiagnostics(t *testing.T) {
	tests := []struct {
		name      string
		diags     []validate.Diagnostic
		wantValid bool
	}{
		{
			name:      "empty diagnostics",
			diags:     []validate.Diagnostic{},
			wantValid: true,
		},
		{
			name: "only warnings",
			diags: []validate.Diagnostic{
				{Severity: validate.SeverityWarning, Message: "warning"},
			},
			wantValid: true,
		},
		{
			name: "has errors",
			diags: []validate.Diagnostic{
				{Severity: validate.SeverityError, Message: "error"},
			},
			wantValid: false,
		},
		{
			name: "errors and warnings",
			diags: []validate.Diagnostic{
				{Severity: validate.SeverityError, Message: "error"},
				{Severity: validate.SeverityWarning, Message: "warning"},
			},
			wantValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidDiagnostics(tt.diags)
			if got != tt.wantValid {
				t.Errorf("isValidDiagnostics() = %v, want %v", got, tt.wantValid)
			}
		})
	}
}

func TestHasErrors(t *testing.T) {
	tests := []struct {
		name    string
		diags   []validate.Diagnostic
		wantHas bool
	}{
		{
			name:    "empty diagnostics",
			diags:   []validate.Diagnostic{},
			wantHas: false,
		},
		{
			name: "only warnings",
			diags: []validate.Diagnostic{
				{Severity: validate.SeverityWarning, Message: "warning"},
			},
			wantHas: false,
		},
		{
			name: "has errors",
			diags: []validate.Diagnostic{
				{Severity: validate.SeverityError, Message: "error"},
			},
			wantHas: true,
		},
		{
			name: "errors and warnings",
			diags: []validate.Diagnostic{
				{Severity: validate.SeverityError, Message: "error"},
				{Severity: validate.SeverityWarning, Message: "warning"},
			},
			wantHas: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasErrors(tt.diags)
			if got != tt.wantHas {
				t.Errorf("hasErrors() = %v, want %v", got, tt.wantHas)
			}
		})
	}
}

func TestOutputLintAllJSON(t *testing.T) {
	results := []fileResult{
		{
			Path:  "file1.yml",
			Valid: true,
			Diagnostics: []validate.Diagnostic{
				{Severity: validate.SeverityWarning, Message: "warning"},
			},
		},
		{
			Path:        "file2.yml",
			Valid:       true,
			Diagnostics: []validate.Diagnostic{},
		},
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := outputLintAllJSON(results)

	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Errorf("outputLintAllJSON returned error: %v", err)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("Failed to read stdout: %v", err)
	}

	var result struct {
		Valid bool `json:"valid"`
		Files []struct {
			Path        string `json:"path"`
			Valid       bool   `json:"valid"`
			Diagnostics []struct {
				Severity string `json:"severity"`
				Message  string `json:"message"`
			} `json:"diagnostics"`
		} `json:"files"`
	}

	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Errorf("Failed to parse JSON output: %v", err)
	}

	if !result.Valid {
		t.Error("Expected valid=true when all files are valid")
	}

	if len(result.Files) != 2 {
		t.Errorf("Expected 2 files, got %d", len(result.Files))
	}

	if result.Files[0].Valid != true {
		t.Error("Expected file1 to be valid")
	}

	if result.Files[1].Valid != true {
		t.Error("Expected file2 to be valid")
	}
}

func TestOutputLintAllJSON_InvalidResults(t *testing.T) {
	results := []fileResult{
		{
			Path:  "file1.yml",
			Valid: false,
			Diagnostics: []validate.Diagnostic{
				{Path: "file1.yml", Severity: validate.SeverityError, Message: "error"},
			},
		},
	}

	output, err := captureStdout(t, func() error {
		return outputLintAllJSON(results)
	})

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	var result struct {
		Valid bool `json:"valid"`
		Files []struct {
			Path        string `json:"path"`
			Valid       bool   `json:"valid"`
			Diagnostics []struct {
				Path     string `json:"path"`
				Severity string `json:"severity"`
				Message  string `json:"message"`
			} `json:"diagnostics"`
		} `json:"files"`
	}

	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Errorf("Failed to parse JSON output: %v", err)
	}
	if result.Valid {
		t.Error("Expected valid=false when a file is invalid")
	}
	if len(result.Files) != 1 {
		t.Fatalf("Expected 1 file, got %d", len(result.Files))
	}
	if result.Files[0].Valid {
		t.Error("Expected file1 to be invalid")
	}
	if result.Files[0].Diagnostics[0].Message != "error" {
		t.Errorf("Expected diagnostic message to be preserved, got %s", result.Files[0].Diagnostics[0].Message)
	}
}

func TestOutputLintAllSARIF(t *testing.T) {
	results := []fileResult{
		{
			Path:  "file1.yml",
			Valid: true,
			Diagnostics: []validate.Diagnostic{
				{
					Path:     "file1.yml",
					Line:     5,
					Column:   10,
					Message:  "Warning message",
					Severity: validate.SeverityWarning,
				},
			},
		},
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := outputLintAllSARIF(results)

	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Errorf("outputLintAllSARIF returned error: %v", err)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("Failed to read stdout: %v", err)
	}

	var result struct {
		Version string `json:"version"`
		Runs    []struct {
			Results []struct {
				Level   string `json:"level"`
				Message struct {
					Text string `json:"text"`
				} `json:"message"`
			} `json:"results"`
		} `json:"runs"`
	}

	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Errorf("Failed to parse SARIF JSON output: %v", err)
	}

	if len(result.Runs) != 1 {
		t.Errorf("Expected 1 run, got %d", len(result.Runs))
	}

	if len(result.Runs[0].Results) != 1 {
		t.Errorf("Expected 1 result, got %d", len(result.Runs[0].Results))
	}

	if result.Runs[0].Results[0].Level != "warning" {
		t.Errorf("Expected warning level, got %s", result.Runs[0].Results[0].Level)
	}
}

func TestOutputLintAllSARIF_InvalidResults(t *testing.T) {
	results := []fileResult{
		{
			Path:  "file1.yml",
			Valid: false,
			Diagnostics: []validate.Diagnostic{
				{
					Path:     "file1.yml",
					Line:     5,
					Column:   10,
					Message:  "Error message",
					Severity: validate.SeverityError,
				},
			},
		},
	}

	output, err := captureStdout(t, func() error {
		return outputLintAllSARIF(results)
	})

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	var result struct {
		Runs []struct {
			Results []struct {
				Level   string `json:"level"`
				Message struct {
					Text string `json:"text"`
				} `json:"message"`
			} `json:"results"`
		} `json:"runs"`
	}

	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Errorf("Failed to parse SARIF JSON output: %v", err)
	}
	if len(result.Runs) != 1 {
		t.Fatalf("Expected 1 run, got %d", len(result.Runs))
	}
	if len(result.Runs[0].Results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(result.Runs[0].Results))
	}
	if result.Runs[0].Results[0].Level != "error" {
		t.Errorf("Expected error level, got %s", result.Runs[0].Results[0].Level)
	}
	if result.Runs[0].Results[0].Message.Text != "file1.yml: Error message" {
		t.Errorf("Expected SARIF message to be preserved, got %s", result.Runs[0].Results[0].Message.Text)
	}
}

func TestLintTestDataFile(t *testing.T) {
	// Test that validates the test/runs-on.yml file
	testFile := filepath.Join("..", "..", "test", "runs-on.yml")

	// Check if file exists
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Skipf("Test data file %s does not exist", testFile)
		return
	}

	ctx := context.Background()
	diags, err := validate.ValidateFile(ctx, testFile)
	if err != nil {
		t.Fatalf("Failed to validate test file: %v", err)
	}

	// Log diagnostics for debugging
	for _, d := range diags {
		if d.Severity == validate.SeverityError {
			t.Errorf("Validation error in %s:%d:%d: %s", d.Path, d.Line, d.Column, d.Message)
		} else {
			t.Logf("Validation warning in %s:%d:%d: %s", d.Path, d.Line, d.Column, d.Message)
		}
	}

	// Test should pass even with warnings, but fail on errors
	hasErrors := hasErrors(diags)
	if hasErrors {
		t.Error("Test data file has validation errors")
	}
}
