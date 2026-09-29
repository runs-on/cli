package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

// lintJSONReport decodes `--format json` output: a single file's report, or
// (via Files) the all-files report. It is declared independently of the
// production structs so a renamed output field fails the tests.
type lintJSONReport struct {
	Path        string `json:"path"`
	Valid       bool   `json:"valid"`
	Diagnostics []struct {
		Message  string `json:"message"`
		Severity string `json:"severity"`
	} `json:"diagnostics"`
	Files []lintJSONReport `json:"files"`
}

// lintSARIFReport decodes the `--format sarif` fields the tests assert on.
type lintSARIFReport struct {
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

func decodeLintOutput[T any](t *testing.T, output string) T {
	t.Helper()

	var report T
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("Failed to parse output %q: %v", output, err)
	}
	return report
}

func TestLintFile_ValidFile(t *testing.T) {
	tmpDir := t.TempDir()
	validFile := writeRunsOnConfig(t, tmpDir, validLintYAML)

	var out bytes.Buffer
	err := lintFile(context.Background(), &out, validFile, "text")
	output := out.String()

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

	var out bytes.Buffer
	err := lintFile(context.Background(), &out, invalidFile, "text")
	output := out.String()

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
	err := lintFile(ctx, io.Discard, "/nonexistent/file.yml", "text")

	if err == nil {
		t.Error("Expected error for nonexistent file")
	}

	if !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("Expected validation error, got: %v", err)
	}
}

func TestLintStdin_ValidInput(t *testing.T) {
	var out bytes.Buffer
	err := lintStdin(context.Background(), strings.NewReader(validLintYAML), &out, "text")
	output := out.String()

	if err != nil {
		t.Errorf("lintStdin returned error for valid input: %v", err)
	}

	if !strings.Contains(output, "Configuration file '<stdin>' is valid!") {
		t.Errorf("Expected valid stdin output, got: %s", output)
	}
}

func TestLintStdin_InvalidInput(t *testing.T) {
	var out bytes.Buffer
	err := lintStdin(context.Background(), strings.NewReader(invalidLintYAML), &out, "text")
	output := out.String()

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

	var out bytes.Buffer
	cmd := NewLintCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--format", "json", invalidFile})
	err := cmd.Execute()
	output := out.String()

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error from Cobra command execution, got: %v", err)
	}

	result := decodeLintOutput[lintJSONReport](t, output)
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
	t.Chdir(t.TempDir())

	var out bytes.Buffer
	err := lintAllFiles(context.Background(), &out, io.Discard, "text")
	output := out.String()

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

	t.Chdir(tmpDir)
	var out bytes.Buffer
	err := lintAllFiles(context.Background(), &out, io.Discard, "text")
	output := out.String()

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

func TestLintAllFiles_SkipsGitAndNodeModules(t *testing.T) {
	tmpDir := t.TempDir()
	for dir, content := range map[string]string{
		".github":                            validLintYAML,
		".git":                               invalidLintYAML,
		filepath.Join("node_modules", "pkg"): invalidLintYAML,
	} {
		path := filepath.Join(tmpDir, dir)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatalf("Failed to create %s: %v", path, err)
		}
		writeRunsOnConfig(t, path, content)
	}

	t.Chdir(tmpDir)
	var out bytes.Buffer
	err := lintAllFiles(context.Background(), &out, io.Discard, "json")
	output := out.String()
	if err != nil {
		t.Fatalf("lintAllFiles returned error: %v", err)
	}

	result := decodeLintOutput[lintJSONReport](t, output)
	if len(result.Files) != 1 || result.Files[0].Path != filepath.Join(".github", "runs-on.yml") {
		t.Fatalf("Expected only .github/runs-on.yml to be linted, got %+v", result.Files)
	}
}

var (
	lintTestError        = validate.Diagnostic{Line: 5, Column: 10, Message: "Invalid value", Severity: validate.SeverityError}
	lintTestErrorNoLine  = validate.Diagnostic{Message: "Missing runner", Severity: validate.SeverityError}
	lintTestWarning      = validate.Diagnostic{Line: 7, Column: 3, Message: "Deprecated field", Severity: validate.SeverityWarning}
	lintTestWarnNoLine   = validate.Diagnostic{Message: "Unknown key", Severity: validate.SeverityWarning}
	lintTestFixErrorsTip = "\nPlease fix the errors above and run the validation again.\n"
)

func TestOutputLintResults_TextFormat(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		diags   []validate.Diagnostic
		want    string
		wantErr error
	}{
		{
			name:   "valid",
			source: "test.yml",
			want:   "✅ Configuration file 'test.yml' is valid!\n",
		},
		{
			name:   "valid stdin",
			source: "<stdin>",
			diags:  []validate.Diagnostic{},
			want:   "✅ Configuration file '<stdin>' is valid!\n",
		},
		{
			name:   "warnings only",
			source: "test.yml",
			diags:  []validate.Diagnostic{lintTestWarning, lintTestWarnNoLine},
			want: "⚠️  Configuration file 'test.yml' is valid but has 2 warning(s):\n\n" +
				"1. [Line 7, Column 3] warning: Deprecated field\n" +
				"2. warning: Unknown key\n",
		},
		{
			name:   "errors only",
			source: "test.yml",
			diags:  []validate.Diagnostic{lintTestError, lintTestErrorNoLine},
			want: "❌ Configuration file 'test.yml' has 2 error(s):\n\n" +
				"1. [Line 5, Column 10] error: Invalid value\n" +
				"2. error: Missing runner\n" +
				lintTestFixErrorsTip,
			wantErr: errLintInvalid,
		},
		{
			name:   "errors and warnings",
			source: "<stdin>",
			diags:  []validate.Diagnostic{lintTestWarning, lintTestError, lintTestWarnNoLine},
			want: "❌ Configuration file '<stdin>' has 1 error(s) and 2 warning(s):\n\n" +
				"1. [Line 5, Column 10] error: Invalid value\n" +
				"\nWarnings:\n" +
				"  1. [Line 7, Column 3] warning: Deprecated field\n" +
				"  2. warning: Unknown key\n" +
				lintTestFixErrorsTip,
			wantErr: errLintInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := outputLintResults(&out, tt.diags, tt.source, "text")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}
			if out.String() != tt.want {
				t.Errorf("unexpected output\ngot:\n%s\nwant:\n%s", out.String(), tt.want)
			}
		})
	}
}

func TestOutputLintAllText(t *testing.T) {
	tests := []struct {
		name    string
		results []fileResult
		want    string
		wantErr error
	}{
		{
			name:    "all valid without warnings prints nothing",
			results: []fileResult{{Path: "a.yml", Valid: true}},
		},
		{
			name: "all valid with warnings",
			results: []fileResult{
				{Path: "a.yml", Valid: true, Diagnostics: []validate.Diagnostic{lintTestWarning}},
				{Path: "b.yml", Valid: true},
				{Path: "c.yml", Valid: true, Diagnostics: []validate.Diagnostic{lintTestWarnNoLine, lintTestWarning}},
			},
			want: "\nWarnings:\n" +
				"\na.yml:\n" +
				"  1. [Line 7, Column 3] warning: Deprecated field\n" +
				"\nc.yml:\n" +
				"  1. warning: Unknown key\n" +
				"  2. [Line 7, Column 3] warning: Deprecated field\n",
		},
		{
			name: "invalid files",
			results: []fileResult{
				{Path: "a.yml", Valid: false, Diagnostics: []validate.Diagnostic{lintTestWarning, lintTestError, lintTestErrorNoLine}},
				{Path: "b.yml", Valid: true, Diagnostics: []validate.Diagnostic{lintTestWarning}},
				{Path: "c.yml", Valid: true},
				// A file the validator could not read has no diagnostics.
				{Path: "d.yml", Valid: false, Diagnostics: []validate.Diagnostic{}},
			},
			want: "\nDetailed errors:\n" +
				"\na.yml:\n" +
				"  1. [Line 5, Column 10] error: Invalid value\n" +
				"  2. error: Missing runner\n" +
				"\n  Warnings:\n" +
				"    1. [Line 7, Column 3] warning: Deprecated field\n" +
				"⚠️  b.yml (1 warning(s))\n" +
				"✅ c.yml\n" +
				"\nd.yml:\n",
			wantErr: errLintInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := outputLintAllText(&out, tt.results)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}
			if out.String() != tt.want {
				t.Errorf("unexpected output\ngot:\n%s\nwant:\n%s", out.String(), tt.want)
			}
		})
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

	var out bytes.Buffer
	err := outputLintResults(&out, diags, "test.yml", "json")
	output := out.String()
	if err != nil {
		t.Errorf("outputLintResults returned error: %v", err)
	}

	result := decodeLintOutput[lintJSONReport](t, output)
	// Warnings only should still be valid
	if !result.Valid {
		t.Error("Expected valid=true for warning-only diagnostics")
	}
	if len(result.Diagnostics) != 1 {
		t.Fatalf("Expected 1 diagnostic, got %d", len(result.Diagnostics))
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

	var out bytes.Buffer
	err := outputLintResults(&out, diags, "test.yml", "json")
	output := out.String()

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	result := decodeLintOutput[lintJSONReport](t, output)
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

	var out bytes.Buffer
	err := outputLintResults(&out, diags, "test.yml", "sarif")
	output := out.String()
	if err != nil {
		t.Errorf("outputLintResults returned error: %v", err)
	}

	result := decodeLintOutput[lintSARIFReport](t, output)
	if result.Version != "2.1.0" {
		t.Errorf("Expected SARIF version 2.1.0, got %s", result.Version)
	}
	if len(result.Runs) != 1 {
		t.Fatalf("Expected 1 run, got %d", len(result.Runs))
	}
	if len(result.Runs[0].Results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(result.Runs[0].Results))
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

	var out bytes.Buffer
	err := outputLintResults(&out, diags, "test.yml", "sarif")
	output := out.String()

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	result := decodeLintOutput[lintSARIFReport](t, output)
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

	err := outputLintResults(io.Discard, diags, "test.yml", "invalid")

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

	var out bytes.Buffer
	err := outputLintAllJSON(&out, results)
	output := out.String()
	if err != nil {
		t.Errorf("outputLintAllJSON returned error: %v", err)
	}

	result := decodeLintOutput[lintJSONReport](t, output)
	if !result.Valid {
		t.Error("Expected valid=true when all files are valid")
	}
	if len(result.Files) != 2 {
		t.Fatalf("Expected 2 files, got %d", len(result.Files))
	}
	if !result.Files[0].Valid {
		t.Error("Expected file1 to be valid")
	}
	if !result.Files[1].Valid {
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

	var out bytes.Buffer
	err := outputLintAllJSON(&out, results)
	output := out.String()

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	result := decodeLintOutput[lintJSONReport](t, output)
	if result.Valid {
		t.Error("Expected valid=false when a file is invalid")
	}
	if len(result.Files) != 1 {
		t.Fatalf("Expected 1 file, got %d", len(result.Files))
	}
	if result.Files[0].Valid {
		t.Error("Expected file1 to be invalid")
	}
	if len(result.Files[0].Diagnostics) != 1 || result.Files[0].Diagnostics[0].Message != "error" {
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

	var out bytes.Buffer
	err := outputLintAllSARIF(&out, results)
	output := out.String()
	if err != nil {
		t.Errorf("outputLintAllSARIF returned error: %v", err)
	}

	result := decodeLintOutput[lintSARIFReport](t, output)
	if len(result.Runs) != 1 {
		t.Fatalf("Expected 1 run, got %d", len(result.Runs))
	}
	if len(result.Runs[0].Results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(result.Runs[0].Results))
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

	var out bytes.Buffer
	err := outputLintAllSARIF(&out, results)
	output := out.String()

	if !errors.Is(err, errLintInvalid) {
		t.Fatalf("Expected lint invalid error, got: %v", err)
	}

	result := decodeLintOutput[lintSARIFReport](t, output)
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
