# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`roc` is the RunsOn CLI - a command-line tool for managing and troubleshooting RunsOn installations on AWS. It provides functionality to connect to GitHub Actions runners via AWS SSM, fetch logs, and diagnose stack health.

## Key Commands

### Build and Development
- `make build` - Build the CLI binary to `dist/roc`
- `make install` - Build and install to `/usr/local/bin/roc`
- `go build -o dist/roc .` - Direct Go build command

### Release Process
- `make bump TAG=vX.Y.Z` - Update version references in README
- `make tag TAG=vX.Y.Z` - Create and push git tag
- `make release TAG=vX.Y.Z` - Full release process (tag + GitHub release)

## Architecture

### Entry Point
- `main.go` - Initializes AWS config with retry logic and launches the CLI

### Command Structure (Cobra-based)
The CLI uses the Cobra framework with commands organized under `internal/cli/`:
- `root.go` - Root command setup and AWS stack configuration retrieval
- `connect.go` - SSM connection to GitHub Actions runner instances
- `logs.go` - Log fetching from CloudWatch (server and instance logs)
- `lint.go` - Configuration file validation and linting (runs-on.yml)
- `doctor.go` - Stack health diagnostics and troubleshooting
- `stack.go` - Parent command for stack management operations
- `interrupt.go` - Trigger spot instance interruptions for testing

### AWS Integration
The CLI heavily integrates with AWS services:
- Resource Groups Tagging API - Resource discovery
- SSM - Instance connections
- CloudWatch Logs - Log streaming
- AppRunner - Service health checks
- S3 - Configuration storage

### Configuration
- Stack name via `--stack` flag or `RUNS_ON_STACK_NAME` or `RUNS_ON_STACK` environment variable (default: "runs-on")
- AWS credentials via standard AWS SDK configuration (profiles, environment variables, IAM roles)

### Key Patterns
1. Commands discover resources via Resource Groups Tagging API to get resource ARNs
2. Context propagation for AWS config through command execution
3. Structured error handling with context-aware messages
4. Real-time log streaming with optional watch mode
5. Health check aggregation for diagnostic reports
6. Configuration validation uses external `github.com/runs-on/config` module with CUE schema
7. Lint command supports recursive file search, stdin input, and multiple output formats (text, json, sarif)

## Dependencies
- Go 1.24.2
- AWS SDK v2 for all AWS service interactions
- Cobra for CLI framework
- Google go-github for GitHub API integration
- `github.com/runs-on/config` - Configuration schema validation (CUE-based)

## Important Notes
- **Always update the README.md** when making changes to command flags, functionality, or usage patterns. The README serves as the primary user documentation and should stay in sync with the actual implementation.