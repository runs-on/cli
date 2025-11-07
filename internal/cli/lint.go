package cli

import (
	"context"
	"encoding/json"
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

			if stdin {
				return lintStdin(ctx, format)
			}

			if len(args) > 0 {
				// Validate single file
				return lintFile(ctx, args[0], format)
			}

			// Find and validate all runs-on.yml files
			return lintAllFiles(ctx, format)
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
				var errors []validate.Diagnostic
				var warnings []validate.Diagnostic
				for _, diag := range result.Diagnostics {
					if diag.Severity == validate.SeverityError {
						errors = append(errors, diag)
					} else if diag.Severity == validate.SeverityWarning {
						warnings = append(warnings, diag)
					}
				}
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
		os.Exit(1)
		return nil
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
	type jsonDiagnostic struct {
		Path     string `json:"path"`
		Line     int    `json:"line,omitempty"`
		Column   int    `json:"column,omitempty"`
		Message  string `json:"message"`
		Severity string `json:"severity"`
	}

	type jsonFileResult struct {
		Path        string           `json:"path"`
		Valid       bool             `json:"valid"`
		Diagnostics []jsonDiagnostic `json:"diagnostics"`
	}

	type jsonOutput struct {
		Valid bool             `json:"valid"`
		Files []jsonFileResult `json:"files"`
	}

	allValid := true
	jsonResults := make([]jsonFileResult, len(results))
	for i, result := range results {
		if !result.Valid {
			allValid = false
		}

		diags := make([]jsonDiagnostic, len(result.Diagnostics))
		for j, diag := range result.Diagnostics {
			diags[j] = jsonDiagnostic{
				Path:     diag.Path,
				Line:     diag.Line,
				Column:   diag.Column,
				Message:  diag.Message,
				Severity: string(diag.Severity),
			}
		}

		jsonResults[i] = jsonFileResult{
			Path:        result.Path,
			Valid:       result.Valid,
			Diagnostics: diags,
		}
	}

	output := jsonOutput{
		Valid: allValid,
		Files: jsonResults,
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		return fmt.Errorf("failed to encode JSON: %w", err)
	}

	if !allValid {
		os.Exit(1)
	}

	return nil
}

func outputLintAllSARIF(results []fileResult) error {
	type sarifLocation struct {
		URI    string `json:"uri"`
		Region struct {
			StartLine   int `json:"startLine,omitempty"`
			StartColumn int `json:"startColumn,omitempty"`
		} `json:"region,omitempty"`
	}

	type sarifResult struct {
		RuleID  string `json:"ruleId"`
		Level   string `json:"level"`
		Message struct {
			Text string `json:"text"`
		} `json:"message"`
		Locations []struct {
			PhysicalLocation sarifLocation `json:"physicalLocation"`
		} `json:"locations"`
	}

	type sarifRun struct {
		Tool struct {
			Driver struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"driver"`
		} `json:"tool"`
		Results []sarifResult `json:"results"`
	}

	type sarifOutput struct {
		Version string     `json:"version"`
		Runs    []sarifRun `json:"runs"`
	}

	var allResults []sarifResult
	for _, result := range results {
		for _, diag := range result.Diagnostics {
			level := "error"
			if diag.Severity == validate.SeverityWarning {
				level = "warning"
			}

			sarifDiag := sarifResult{
				RuleID: "config-validation",
				Level:  level,
			}
			sarifDiag.Message.Text = fmt.Sprintf("%s: %s", result.Path, diag.Message)

			loc := sarifLocation{
				URI: result.Path,
			}
			if diag.Line > 0 {
				loc.Region.StartLine = diag.Line
				loc.Region.StartColumn = diag.Column
			}

			sarifDiag.Locations = []struct {
				PhysicalLocation sarifLocation `json:"physicalLocation"`
			}{
				{PhysicalLocation: loc},
			}

			allResults = append(allResults, sarifDiag)
		}
	}

	output := sarifOutput{
		Version: "2.1.0",
		Runs: []sarifRun{
			{
				Tool: struct {
					Driver struct {
						Name    string `json:"name"`
						Version string `json:"version"`
					} `json:"driver"`
				}{
					Driver: struct {
						Name    string `json:"name"`
						Version string `json:"version"`
					}{
						Name:    "roc",
						Version: version.Version,
					},
				},
				Results: allResults,
			},
		},
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		return fmt.Errorf("failed to encode SARIF: %w", err)
	}

	allValid := true
	for _, result := range results {
		if !result.Valid {
			allValid = false
			break
		}
	}

	if !allValid {
		os.Exit(1)
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
	var errors []validate.Diagnostic
	var warnings []validate.Diagnostic
	for _, diag := range diags {
		if diag.Severity == validate.SeverityError {
			errors = append(errors, diag)
		} else if diag.Severity == validate.SeverityWarning {
			warnings = append(warnings, diag)
		}
	}

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
		os.Exit(1)
		return nil
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
	type jsonDiagnostic struct {
		Path     string `json:"path"`
		Line     int    `json:"line,omitempty"`
		Column   int    `json:"column,omitempty"`
		Message  string `json:"message"`
		Severity string `json:"severity"`
	}

	type jsonOutput struct {
		Valid       bool             `json:"valid"`
		Diagnostics []jsonDiagnostic `json:"diagnostics"`
	}

	output := jsonOutput{
		Valid:       isValidDiagnostics(diags),
		Diagnostics: make([]jsonDiagnostic, len(diags)),
	}

	for i, diag := range diags {
		output.Diagnostics[i] = jsonDiagnostic{
			Path:     diag.Path,
			Line:     diag.Line,
			Column:   diag.Column,
			Message:  diag.Message,
			Severity: string(diag.Severity),
		}
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		return fmt.Errorf("failed to encode JSON: %w", err)
	}

	if !output.Valid {
		os.Exit(1)
	}

	return nil
}

func outputLintSARIF(diags []validate.Diagnostic) error {
	type sarifLocation struct {
		URI    string `json:"uri"`
		Region struct {
			StartLine   int `json:"startLine,omitempty"`
			StartColumn int `json:"startColumn,omitempty"`
		} `json:"region,omitempty"`
	}

	type sarifResult struct {
		RuleID  string `json:"ruleId"`
		Level   string `json:"level"`
		Message struct {
			Text string `json:"text"`
		} `json:"message"`
		Locations []struct {
			PhysicalLocation sarifLocation `json:"physicalLocation"`
		} `json:"locations"`
	}

	type sarifRun struct {
		Tool struct {
			Driver struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"driver"`
		} `json:"tool"`
		Results []sarifResult `json:"results"`
	}

	type sarifOutput struct {
		Version string     `json:"version"`
		Runs    []sarifRun `json:"runs"`
	}

	results := make([]sarifResult, len(diags))
	for i, diag := range diags {
		level := "error"
		if diag.Severity == validate.SeverityWarning {
			level = "warning"
		}

		result := sarifResult{
			RuleID: "config-validation",
			Level:  level,
		}
		result.Message.Text = diag.Message

		loc := sarifLocation{
			URI: diag.Path,
		}
		if diag.Line > 0 {
			loc.Region.StartLine = diag.Line
			loc.Region.StartColumn = diag.Column
		}

		result.Locations = []struct {
			PhysicalLocation sarifLocation `json:"physicalLocation"`
		}{
			{PhysicalLocation: loc},
		}

		results[i] = result
	}

	output := sarifOutput{
		Version: "2.1.0",
		Runs: []sarifRun{
			{
				Tool: struct {
					Driver struct {
						Name    string `json:"name"`
						Version string `json:"version"`
					} `json:"driver"`
				}{
					Driver: struct {
						Name    string `json:"name"`
						Version string `json:"version"`
					}{
						Name:    "roc",
						Version: version.Version,
					},
				},
				Results: results,
			},
		},
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		return fmt.Errorf("failed to encode SARIF: %w", err)
	}

	if !isValidDiagnostics(diags) {
		os.Exit(1)
	}

	return nil
}
