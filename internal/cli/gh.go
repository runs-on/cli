package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ghAPIRunner fetches GitHub API paths; implemented by the gh CLI and faked
// in tests. Missing resources return an error wrapping errGHNotFound so
// callers can distinguish definitive absence from transient failures.
type ghAPIRunner interface {
	Get(ctx context.Context, host, path string) ([]byte, error)
}

var (
	errGHNotFound = errors.New("not found")
	errGHMissing  = errors.New("gh is not installed")
)

type ghCLIRunner struct{}

func (ghCLIRunner) Get(ctx context.Context, host, path string) ([]byte, error) {
	output, err := ghAPI(ctx, host, path)
	switch {
	case err == nil:
		return output, nil
	case errors.Is(err, errGHMissing):
		return nil, fmt.Errorf("cleanup requires the GitHub CLI; install gh and run `gh auth login` with repository read access")
	case strings.Contains(err.Error(), "HTTP 404"):
		return nil, fmt.Errorf("%s: %w", path, errGHNotFound)
	}
	return nil, fmt.Errorf("GitHub CLI could not fetch %s; run `gh auth login` with repository read access: %s", path, err)
}

// ghAPI runs `gh api path` against host. It fails with errGHMissing when gh is
// not installed, and otherwise with gh's output as the error text.
func ghAPI(ctx context.Context, host, path string) ([]byte, error) {
	args := []string{"api"}
	if host = strings.TrimSpace(host); host != "" && !strings.EqualFold(host, "github.com") {
		args = append(args, "--hostname", host)
	}
	args = append(args, path)
	output, err := exec.CommandContext(ctx, "gh", args...).CombinedOutput()
	if err == nil {
		return output, nil
	}
	if _, lookupErr := exec.LookPath("gh"); lookupErr != nil {
		return nil, errGHMissing
	}
	if message := strings.TrimSpace(string(output)); message != "" {
		return nil, errors.New(message)
	}
	return nil, err
}
