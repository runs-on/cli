package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			var err error
			if stdin {
				err = lintStdin(ctx, cmd.InOrStdin(), out, format)
			} else if len(args) > 0 {
				// Validate single file
				err = lintFile(ctx, out, args[0], format)
			} else {
				// Find and validate all runs-on.yml files
				err = lintAllFiles(ctx, out, cmd.ErrOrStderr(), format)
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

func lintStdin(ctx context.Context, in io.Reader, out io.Writer, format string) error {
	diags, err := validate.ValidateReader(ctx, in, "<stdin>")
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	return outputLintResults(out, diags, "<stdin>", format)
}

func lintFile(ctx context.Context, out io.Writer, filePath string, format string) error {
	diags, err := validate.ValidateFile(ctx, filePath)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	return outputLintResults(out, diags, filePath, format)
}

func lintAllFiles(ctx context.Context, out, errOut io.Writer, format string) error {
	var files []string
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Other hidden directories stay in scope: .github/runs-on.yml is
			// the canonical location.
			switch entry.Name() {
			case ".git", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() == "runs-on.yml" {
			files = append(files, path)
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to search for files: %w", err)
	}

	if len(files) == 0 {
		fmt.Fprintln(out, "No runs-on.yml files found")
		return nil
	}

	var allResults []fileResult

	for _, file := range files {
		diags, err := validate.ValidateFile(ctx, file)
		if err != nil {
			fmt.Fprintf(errOut, "Error validating %s: %v\n", file, err)
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
		return outputLintAllText(out, allResults)
	case "json":
		return outputLintAllJSON(out, allResults)
	case "sarif":
		return outputLintAllSARIF(out, allResults)
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

func writeIndentedJSON(out io.Writer, value any, formatName string) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("failed to encode %s: %w", formatName, err)
	}
	return nil
}

func splitDiagnostics(diags []validate.Diagnostic) ([]validate.Diagnostic, []validate.Diagnostic) {
	var errs []validate.Diagnostic
	var warnings []validate.Diagnostic
	for _, diag := range diags {
		switch diag.Severity {
		case validate.SeverityError:
			errs = append(errs, diag)
		case validate.SeverityWarning:
			warnings = append(warnings, diag)
		}
	}
	return errs, warnings
}

// isValidDiagnostics reports whether diags has no errors; warnings are OK.
func isValidDiagnostics(diags []validate.Diagnostic) bool {
	errs, _ := splitDiagnostics(diags)
	return len(errs) == 0
}

// writeDiagnostics prints diags as a numbered list, each line prefixed by indent.
func writeDiagnostics(out io.Writer, indent string, diags []validate.Diagnostic) {
	for i, diag := range diags {
		location := ""
		if diag.Line > 0 {
			location = fmt.Sprintf("[Line %d, Column %d] ", diag.Line, diag.Column)
		}
		fmt.Fprintf(out, "%s%d. %s%s: %s\n", indent, i+1, location, diag.Severity, diag.Message)
	}
}

func outputLintAllText(out io.Writer, results []fileResult) error {
	if !lintResultsValid(results) {
		fmt.Fprintln(out, "\nDetailed errors:")
		for _, result := range results {
			errs, warnings := splitDiagnostics(result.Diagnostics)
			switch {
			case !result.Valid:
				fmt.Fprintf(out, "\n%s:\n", result.Path)
				writeDiagnostics(out, "  ", errs)
				if len(warnings) > 0 {
					fmt.Fprint(out, "\n  Warnings:\n")
					writeDiagnostics(out, "    ", warnings)
				}
			case len(warnings) > 0:
				fmt.Fprintf(out, "⚠️  %s (%d warning(s))\n", result.Path, len(warnings))
			default:
				fmt.Fprintf(out, "✅ %s\n", result.Path)
			}
		}
		return errLintInvalid
	}

	printedHeader := false
	for _, result := range results {
		_, warnings := splitDiagnostics(result.Diagnostics)
		if len(warnings) == 0 {
			continue
		}
		if !printedHeader {
			fmt.Fprintln(out, "\nWarnings:")
			printedHeader = true
		}
		fmt.Fprintf(out, "\n%s:\n", result.Path)
		writeDiagnostics(out, "  ", warnings)
	}
	return nil
}

func outputLintAllJSON(out io.Writer, results []fileResult) error {
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

	if err := writeIndentedJSON(out, output, "JSON"); err != nil {
		return err
	}

	if !allValid {
		return errLintInvalid
	}

	return nil
}

func outputLintAllSARIF(out io.Writer, results []fileResult) error {
	var allResults []sarifResult
	for _, result := range results {
		for _, diag := range result.Diagnostics {
			allResults = append(allResults, lintSARIFResult(diag, result.Path, fmt.Sprintf("%s: %s", result.Path, diag.Message)))
		}
	}

	if err := writeIndentedJSON(out, lintSARIFOutput(allResults), "SARIF"); err != nil {
		return err
	}

	if !lintResultsValid(results) {
		return errLintInvalid
	}

	return nil
}

func outputLintResults(out io.Writer, diags []validate.Diagnostic, sourceName string, format string) error {
	switch format {
	case "text":
		return outputLintText(out, diags, sourceName)
	case "json":
		return outputLintJSON(out, diags)
	case "sarif":
		return outputLintSARIF(out, diags)
	default:
		return fmt.Errorf("invalid format %q (valid: text, json, sarif)", format)
	}
}

func outputLintText(out io.Writer, diags []validate.Diagnostic, sourceName string) error {
	errs, warnings := splitDiagnostics(diags)
	switch {
	case len(errs) > 0:
		fmt.Fprintf(out, "❌ Configuration file '%s' has %d error(s)", sourceName, len(errs))
		if len(warnings) > 0 {
			fmt.Fprintf(out, " and %d warning(s)", len(warnings))
		}
		fmt.Fprint(out, ":\n\n")
		writeDiagnostics(out, "", errs)
		if len(warnings) > 0 {
			fmt.Fprint(out, "\nWarnings:\n")
			writeDiagnostics(out, "  ", warnings)
		}
		fmt.Fprint(out, "\nPlease fix the errors above and run the validation again.\n")
		return errLintInvalid
	case len(warnings) > 0:
		fmt.Fprintf(out, "⚠️  Configuration file '%s' is valid but has %d warning(s):\n\n", sourceName, len(warnings))
		writeDiagnostics(out, "", warnings)
	default:
		fmt.Fprintf(out, "✅ Configuration file '%s' is valid!\n", sourceName)
	}
	return nil
}

func outputLintJSON(out io.Writer, diags []validate.Diagnostic) error {
	output := lintSingleJSONOutput{
		Valid:       isValidDiagnostics(diags),
		Diagnostics: lintJSONDiagnostics(diags),
	}

	if err := writeIndentedJSON(out, output, "JSON"); err != nil {
		return err
	}

	if !output.Valid {
		return errLintInvalid
	}

	return nil
}

func outputLintSARIF(out io.Writer, diags []validate.Diagnostic) error {
	results := make([]sarifResult, len(diags))
	for i, diag := range diags {
		results[i] = lintSARIFResult(diag, diag.Path, diag.Message)
	}

	if err := writeIndentedJSON(out, lintSARIFOutput(results), "SARIF"); err != nil {
		return err
	}

	if !isValidDiagnostics(diags) {
		return errLintInvalid
	}

	return nil
}
