package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"roc/internal/version"

	"github.com/runs-on/config/pkg/validate"
	"github.com/spf13/cobra"
)

func NewLintCmd() *cobra.Command {
	var format string
	var stdin bool

	cmd := &cobra.Command{
		Use:   "lint [flags] [file]",
		Short: "Validate runs-on.yml configuration files",
		Long: `Validate and lint runs-on.yml configuration files.

If no file is specified, searches for all runs-on.yml files in the current directory
and subdirectories and validates each one.

This command checks the configuration file for:
- YAML syntax errors
- Schema validation errors
- Invalid field values
- Missing required fields

The validator supports YAML anchors and will automatically expand them during validation.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if stdin && len(args) > 0 {
				return fmt.Errorf("cannot specify both file path and --stdin")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			var err error
			if stdin {
				err = lintStdin(ctx, format)
			} else if len(args) > 0 {
				// Validate single file
				err = lintFile(ctx, args[0], format)
			} else {
				// Find and validate all runs-on.yml files
				err = lintAllFiles(ctx, format)
			}

			if errors.Is(err, errLintInvalid) {
				cmd.SilenceUsage = true
				cmd.SilenceErrors = true
			}
			return err
		},
	}

	cmd.Flags().StringVarP(&format, "format", "f", "text", "Output format: text, json, or sarif")
	cmd.Flags().BoolVar(&stdin, "stdin", false, "Read from stdin instead of file")

	// Enable file path completion for the file argument
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Check if --stdin flag is set
		if cmd.Flags().Changed("stdin") {
			stdinFlag, _ := cmd.Flags().GetBool("stdin")
			if stdinFlag {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
		}

		// If we already have a file argument, don't complete more
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		// Use default file completion (allows files and directories)
		return nil, cobra.ShellCompDirectiveDefault
	}

	return cmd
}

var errLintInvalid = errors.New("lint found configuration errors")

func lintStdin(ctx context.Context, format string) error {
	diags, err := validate.ValidateReader(ctx, os.Stdin, "<stdin>")
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	return outputLintResults(diags, "<stdin>", format)
}

func lintFile(ctx context.Context, filePath string, format string) error {
	diags, err := validate.ValidateFile(ctx, filePath)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	return outputLintResults(diags, filePath, format)
}

func lintAllFiles(ctx context.Context, format string) error {
	var files []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == "runs-on.yml" {
			files = append(files, path)
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to search for files: %w", err)
	}

	if len(files) == 0 {
		fmt.Println("No runs-on.yml files found")
		return nil
	}

	var allResults []fileResult

	for _, file := range files {
		diags, err := validate.ValidateFile(ctx, file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error validating %s: %v\n", file, err)
			allResults = append(allResults, fileResult{
				Path:        file,
				Valid:       false,
				Diagnostics: []validate.Diagnostic{},
			})
			continue
		}

		isValid := isValidDiagnostics(diags)
		allResults = append(allResults, fileResult{
			Path:        file,
			Valid:       isValid,
			Diagnostics: diags,
		})
	}

	switch format {
	case "text":
		return outputLintAllText(allResults)
	case "json":
		return outputLintAllJSON(allResults)
	case "sarif":
		return outputLintAllSARIF(allResults)
	default:
		return fmt.Errorf("invalid format %q (valid: text, json, sarif)", format)
	}
}

type fileResult struct {
	Path        string
	Valid       bool
	Diagnostics []validate.Diagnostic
}

type lintJSONDiagnostic struct {
	Path     string `json:"path"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

type lintJSONFileResult struct {
	Path        string               `json:"path"`
	Valid       bool                 `json:"valid"`
	Diagnostics []lintJSONDiagnostic `json:"diagnostics"`
}

type lintSingleJSONOutput struct {
	Valid       bool                 `json:"valid"`
	Diagnostics []lintJSONDiagnostic `json:"diagnostics"`
}

type lintAllJSONOutput struct {
	Valid bool                 `json:"valid"`
	Files []lintJSONFileResult `json:"files"`
}

type sarifRegion struct {
	StartLine   int `json:"startLine,omitempty"`
	StartColumn int `json:"startColumn,omitempty"`
}

type sarifLocation struct {
	URI    string      `json:"uri"`
	Region sarifRegion `json:"region"`
}

type sarifPhysicalLocation struct {
	PhysicalLocation sarifLocation `json:"physicalLocation"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID    string                  `json:"ruleId"`
	Level     string                  `json:"level"`
	Message   sarifMessage            `json:"message"`
	Locations []sarifPhysicalLocation `json:"locations"`
}

type sarifDriver struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifOutput struct {
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

func lintResultsValid(results []fileResult) bool {
	for _, result := range results {
		if !result.Valid {
			return false
		}
	}
	return true
}

func lintJSONDiagnostics(diags []validate.Diagnostic) []lintJSONDiagnostic {
	jsonDiags := make([]lintJSONDiagnostic, len(diags))
	for i, diag := range diags {
		jsonDiags[i] = lintJSONDiagnostic{
			Path:     diag.Path,
			Line:     diag.Line,
			Column:   diag.Column,
			Message:  diag.Message,
			Severity: string(diag.Severity),
		}
	}
	return jsonDiags
}

func sarifLevel(severity validate.Severity) string {
	if severity == validate.SeverityWarning {
		return "warning"
	}
	return "error"
}

func lintSARIFResult(diag validate.Diagnostic, uri string, message string) sarifResult {
	location := sarifLocation{URI: uri}
	if diag.Line > 0 {
		location.Region.StartLine = diag.Line
		location.Region.StartColumn = diag.Column
	}
	return sarifResult{
		RuleID:  "config-validation",
		Level:   sarifLevel(diag.Severity),
		Message: sarifMessage{Text: message},
		Locations: []sarifPhysicalLocation{
			{PhysicalLocation: location},
		},
	}
}

func lintSARIFOutput(results []sarifResult) sarifOutput {
	return sarifOutput{
		Version: "2.1.0",
		Runs: []sarifRun{
			{
				Tool: sarifTool{
					Driver: sarifDriver{
						Name:    "roc",
						Version: version.String(),
					},
				},
				Results: results,
			},
		},
	}
}

func writeIndentedJSON(value any, formatName string) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("failed to encode %s: %w", formatName, err)
	}
	return nil
}

func splitDiagnostics(diags []validate.Diagnostic) ([]validate.Diagnostic, []validate.Diagnostic) {
	var errors []validate.Diagnostic
	var warnings []validate.Diagnostic
	for _, diag := range diags {
		switch diag.Severity {
		case validate.SeverityError:
			errors = append(errors, diag)
		case validate.SeverityWarning:
			warnings = append(warnings, diag)
		}
	}
	return errors, warnings
}

func outputLintAllText(results []fileResult) error {
	allValid := true
	for _, result := range results {
		if !result.Valid {
			allValid = false
			break
		}
	}

	if !allValid {
		fmt.Println("\nDetailed errors:")
		for _, result := range results {
			if !result.Valid {
				fmt.Printf("\n%s:\n", result.Path)
				errors, warnings := splitDiagnostics(result.Diagnostics)
				for i, diag := range errors {
					fmt.Printf("  %d. ", i+1)
					if diag.Line > 0 {
						fmt.Printf("[Line %d, Column %d] ", diag.Line, diag.Column)
					}
					fmt.Printf("%s: %s\n", diag.Severity, diag.Message)
				}
				if len(warnings) > 0 {
					fmt.Printf("\n  Warnings:\n")
					for i, diag := range warnings {
						fmt.Printf("    %d. ", i+1)
						if diag.Line > 0 {
							fmt.Printf("[Line %d, Column %d] ", diag.Line, diag.Column)
						}
						fmt.Printf("%s: %s\n", diag.Severity, diag.Message)
					}
				}
			} else {
				// File is valid but might have warnings
				var warnings []validate.Diagnostic
				for _, diag := range result.Diagnostics {
					if diag.Severity == validate.SeverityWarning {
						warnings = append(warnings, diag)
					}
				}
				if len(warnings) > 0 {
					fmt.Printf("⚠️  %s (%d warning(s))\n", result.Path, len(warnings))
				} else {
					fmt.Printf("✅ %s\n", result.Path)
				}
			}
		}
		return errLintInvalid
	}

	// All files are valid, but check for warnings
	hasWarnings := false
	for _, result := range results {
		for _, diag := range result.Diagnostics {
			if diag.Severity == validate.SeverityWarning {
				hasWarnings = true
				break
			}
		}
		if hasWarnings {
			break
		}
	}

	if hasWarnings {
		fmt.Println("\nWarnings:")
		for _, result := range results {
			var warnings []validate.Diagnostic
			for _, diag := range result.Diagnostics {
				if diag.Severity == validate.SeverityWarning {
					warnings = append(warnings, diag)
				}
			}
			if len(warnings) > 0 {
				fmt.Printf("\n%s:\n", result.Path)
				for i, diag := range warnings {
					fmt.Printf("  %d. ", i+1)
					if diag.Line > 0 {
						fmt.Printf("[Line %d, Column %d] ", diag.Line, diag.Column)
					}
					fmt.Printf("%s: %s\n", diag.Severity, diag.Message)
				}
			}
		}
	}

	return nil
}

// hasErrors checks if any diagnostics are errors (not warnings)
func hasErrors(diags []validate.Diagnostic) bool {
	for _, diag := range diags {
		if diag.Severity == validate.SeverityError {
			return true
		}
	}
	return false
}

// isValidDiagnostics checks if diagnostics are valid (no errors; warnings are OK)
func isValidDiagnostics(diags []validate.Diagnostic) bool {
	return len(diags) == 0 || !hasErrors(diags)
}

func outputLintAllJSON(results []fileResult) error {
	allValid := lintResultsValid(results)
	jsonResults := make([]lintJSONFileResult, len(results))
	for i, result := range results {
		jsonResults[i] = lintJSONFileResult{
			Path:        result.Path,
			Valid:       result.Valid,
			Diagnostics: lintJSONDiagnostics(result.Diagnostics),
		}
	}

	output := lintAllJSONOutput{
		Valid: allValid,
		Files: jsonResults,
	}

	if err := writeIndentedJSON(output, "JSON"); err != nil {
		return err
	}

	if !allValid {
		return errLintInvalid
	}

	return nil
}

func outputLintAllSARIF(results []fileResult) error {
	var allResults []sarifResult
	for _, result := range results {
		for _, diag := range result.Diagnostics {
			allResults = append(allResults, lintSARIFResult(diag, result.Path, fmt.Sprintf("%s: %s", result.Path, diag.Message)))
		}
	}

	if err := writeIndentedJSON(lintSARIFOutput(allResults), "SARIF"); err != nil {
		return err
	}

	if !lintResultsValid(results) {
		return errLintInvalid
	}

	return nil
}

func outputLintResults(diags []validate.Diagnostic, sourceName string, format string) error {
	switch format {
	case "text":
		return outputLintText(diags, sourceName)
	case "json":
		return outputLintJSON(diags)
	case "sarif":
		return outputLintSARIF(diags)
	default:
		return fmt.Errorf("invalid format %q (valid: text, json, sarif)", format)
	}
}

func outputLintText(diags []validate.Diagnostic, sourceName string) error {
	// Separate errors and warnings
	errors, warnings := splitDiagnostics(diags)

	if len(errors) == 0 && len(warnings) == 0 {
		fmt.Printf("✅ Configuration file '%s' is valid!\n", sourceName)
		return nil
	}

	if len(errors) > 0 {
		fmt.Printf("❌ Configuration file '%s' has %d error(s)", sourceName, len(errors))
		if len(warnings) > 0 {
			fmt.Printf(" and %d warning(s)", len(warnings))
		}
		fmt.Printf(":\n\n")
		for i, diag := range errors {
			fmt.Printf("%d. ", i+1)
			if diag.Line > 0 {
				fmt.Printf("[Line %d, Column %d] ", diag.Line, diag.Column)
			}
			fmt.Printf("%s: %s\n", diag.Severity, diag.Message)
		}
		if len(warnings) > 0 {
			fmt.Printf("\nWarnings:\n")
			for i, diag := range warnings {
				fmt.Printf("  %d. ", i+1)
				if diag.Line > 0 {
					fmt.Printf("[Line %d, Column %d] ", diag.Line, diag.Column)
				}
				fmt.Printf("%s: %s\n", diag.Severity, diag.Message)
			}
		}
		fmt.Printf("\nPlease fix the errors above and run the validation again.\n")
		return errLintInvalid
	}

	// Only warnings, no errors
	fmt.Printf("⚠️  Configuration file '%s' is valid but has %d warning(s):\n\n", sourceName, len(warnings))
	for i, diag := range warnings {
		fmt.Printf("%d. ", i+1)
		if diag.Line > 0 {
			fmt.Printf("[Line %d, Column %d] ", diag.Line, diag.Column)
		}
		fmt.Printf("%s: %s\n", diag.Severity, diag.Message)
	}
	return nil
}

func outputLintJSON(diags []validate.Diagnostic) error {
	output := lintSingleJSONOutput{
		Valid:       isValidDiagnostics(diags),
		Diagnostics: lintJSONDiagnostics(diags),
	}

	if err := writeIndentedJSON(output, "JSON"); err != nil {
		return err
	}

	if !output.Valid {
		return errLintInvalid
	}

	return nil
}

func outputLintSARIF(diags []validate.Diagnostic) error {
	results := make([]sarifResult, len(diags))
	for i, diag := range diags {
		results[i] = lintSARIFResult(diag, diag.Path, diag.Message)
	}

	if err := writeIndentedJSON(lintSARIFOutput(results), "SARIF"); err != nil {
		return err
	}

	if !isValidDiagnostics(diags) {
		return errLintInvalid
	}

	return nil
}
