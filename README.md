# roc

RunsOn CLI (`roc`) is a command line tool to manage and troubleshoot your [RunsOn](https://runs-on.com) installation.

Note: the CLI only works with RunsOn >= v2.6.3.

## Table of Contents

### Core Commands
- [`roc connect`](#roc-connect) - Connect to GitHub Actions runner instances via SSM
- [`roc logs`](#roc-logs) - Fetch RunsOn server and instance logs for specific jobs
- [`roc interrupt`](#roc-interrupt) - Trigger spot interruptions for testing
- [`roc lint`](#roc-lint) - Validate and lint runs-on configuration files

### Stack Management
- [`roc stack doctor`](#roc-stack-doctor) - Diagnose RunsOn stack health and export troubleshooting info
- [`roc stack logs`](#roc-stack-logs) - Stream all RunsOn application logs from CloudWatch

### Other
- [Installation](#installation) - Download and install the CLI
- [Contributing](#contributing) - Ideas for future improvements
- [License](#license) - Project license information

## Installation

You can download the binaries for your platform (Linux, macOS) from the [Releases](https://github.com/runs-on/cli/releases/latest) page.

Example (macOS ARM64):

```
curl -Lo ./roc https://github.com/runs-on/cli/releases/download/v0.1.8/roc_0.1.8_darwin_arm64
chmod a+x ./roc
./roc --help
```

Example (Linux AMD64):

```
curl -Lo ./roc https://github.com/runs-on/cli/releases/download/v0.1.8/roc_0.1.8_linux_amd64
chmod a+x ./roc
./roc --help
```

## Core Commands

### `roc connect`

Connect to the instance running a specific job via SSM, by just pasting the GitHub Actions job URL or ID.

This feature requires the [AWS Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html) to be installed on your local machine.

```
Usage:
  roc connect JOB_ID|JOB_URL [flags]

Flags:
      --debug   Enable debug output
  -h, --help    help for connect
      --watch   Wait for instance ID if not found

Global Flags:
      --stack string   CloudFormation stack name (default "runs-on")
```

Example:

```bash
AWS_PROFILE=runs-on-admin roc connect https://github.com/runs-on/runs-on/actions/runs/12415485296/job/34661958899
```

### `roc logs`

Fetch RunsOn server and instance logs for a specific job ID or URL. Use the `--include` flag to specify additional log types.

```
Usage:
  roc logs JOB_ID|JOB_URL [flags]

Flags:
  -d, --debug                 Enable debug output
  -f, --format string         Output format: long (default) or short (default "long")  
  -h, --help                  help for logs
      --include strings       Include additional log types: 'run' (all logs from entire run), 'console' (EC2 instance console logs)
      --no-color              Disable color output
  -s, --since string          Show logs since duration (e.g. 30m, 2h) (default "2h")
  -w, --watch string[="5s"]   Watch for new logs with optional interval (e.g. --watch 2s)

Global Flags:
      --stack string   CloudFormation stack name (default "runs-on")
```

Examples:

```bash
# Fetch logs for a specific job (default behavior)
AWS_PROFILE=runs-on-admin roc logs https://github.com/runs-on/runs-on/actions/runs/12415485296/job/34661958899 --watch

# Fetch all application logs for a run (all jobs in the run)  
AWS_PROFILE=runs-on-admin roc logs https://github.com/runs-on/runs-on/actions/runs/12415485296/job/34661958899 --include=run --watch

# Fetch EC2 instance console logs
AWS_PROFILE=runs-on-admin roc logs 34661958899 --include=console

# Fetch both run logs and console logs
AWS_PROFILE=runs-on-admin roc logs 34661958899 --include=run,console --watch
```

### `roc interrupt`

Trigger a spot interruption on the instance running a specific job, simulating a spot instance interruption for testing purposes.

This command uses AWS Fault Injection Simulator (FIS) to send a spot interruption notification to the running instance.

```
Usage:
  roc interrupt JOB_ID|JOB_URL [flags]

Flags:
      --debug            Enable debug output
      --delay duration   Delay before interruption (e.g., 2m, 30s) (default 5s)
  -h, --help             help for interrupt
  -w, --wait             Wait for instance ID if not found

Global Flags:
      --stack string   CloudFormation stack name (default "runs-on")
```

**Requirements:**
- The target instance must be a running spot instance
- AWS FIS service must be available in your region

**How it works:**
1. Validates the instance is a running spot instance
2. Creates an IAM role for FIS if it doesn't exist
3. Creates and starts a FIS experiment to send the interruption
4. Monitors the experiment progress
5. Automatically cleans up the experiment template when complete

Example:

```bash
AWS_PROFILE=runs-on-admin roc interrupt https://github.com/runs-on/runs-on/actions/runs/12415485296/job/34661958899
```

```bash
# Wait for instance if job hasn't started yet
AWS_PROFILE=runs-on-admin roc interrupt 34661958899 --wait

# Custom delay before interruption (default is 5 seconds)
AWS_PROFILE=runs-on-admin roc interrupt 34661958899 --delay 30s
```

### `roc lint`

Validate and lint runs-on.yml configuration files. This command validates your configuration files against the RunsOn schema, checking for syntax errors, invalid values, missing required fields, and schema violations.

When no file path is provided, the command recursively searches for all `runs-on.yml` files in the current directory and subdirectories.

```
Usage:
  roc lint [flags] [file]

Flags:
      --format string   Output format: text, json, or sarif (default "text")
      --stdin          Read configuration from stdin
  -h, --help           help for lint

Global Flags:
      --stack string   CloudFormation stack name (default "runs-on")
```

**What it validates:**
- YAML syntax errors
- Schema validation for all top-level fields (`_extends`, `runners`, `images`, `pools`, `admins`)
- Required fields and valid value types
- Pool configuration (name pattern, schedule values, runner references)
- Runner specifications (CPU, RAM, family, spot values, etc.)
- Image specifications (AMI IDs, platform, architecture, etc.)
- Custom fields are allowed (e.g., `x-defaults` for YAML anchors)

**Output formats:**
- `text` (default): Human-readable output with file status and diagnostics
- `json`: Structured JSON output for CI/CD integration
- `sarif`: SARIF format for GitHub Code Scanning and other tools

Examples:

```bash
# Lint a specific configuration file
roc lint .github/runs-on.yml

# Lint all runs-on.yml files recursively (no arguments)
roc lint

# Lint from stdin
cat runs-on.yml | roc lint --stdin

# Lint with JSON output for CI/CD pipelines
roc lint config/runs-on.yml --format json

# Lint with SARIF output for GitHub Code Scanning
roc lint .github/runs-on.yml --format sarif
```

**Integration with CI/CD:**

The command exits with a non-zero status code when validation errors are found:

```bash
# Exit code 0 for valid config, 1 for invalid
if roc lint runs-on.yml --format json > validation-report.json; then
  echo "Configuration is valid!"
else
  echo "Configuration validation failed. See validation-report.json for details."
  exit 1
fi
```

## Stack Management

### `roc stack doctor`

Diagnose RunsOn stack health and export troubleshooting information.

This command performs comprehensive health checks on your RunsOn CloudFormation stack:
- Verifies CloudFormation stack status
- Checks AppRunner service health and version
- Tests endpoint accessibility  
- Validates service configuration
- Fetches application logs

Results are exported as a timestamped ZIP file containing checks.json and logs.

```
Usage:
  roc stack doctor [flags]

Flags:
  -h, --help           help for doctor
      --since string   Fetch logs since duration (e.g. 30m, 2h, 24h) (default "24h")

Global Flags:
      --stack string   CloudFormation stack name (default "runs-on")
```

Example:

```bash
AWS_PROFILE=runs-on-admin roc stack doctor --since 2h
```

Output:

```
Checking CloudFormation stack health (https://console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/stackinfo?stackId=runs-on-test)... ✅ (status: UPDATE_COMPLETE)
Checking AppRunner service (https://console.aws.amazon.com/apprunner/home?region=us-east-1#/services/RunsOnService-4rHCauYu4m23)... ✅ (version: v2.8.4)
Checking AppRunner service endpoint (https://wxrwksit5a.us-east-1.awsapprunner.com)... ✅
Checking for 'Congrats' response... ✅
Fetching AppRunner application logs (since 24h0m0s)... ✅ (5419 lines)
Fetching AppRunner service logs (since 14 days)... ✅ (13 lines)

Full results exported to: /Users/crohr/dev/runs-on/cli/roc-doctor-2025-06-20-12-40-29.zip
```

### `roc stack logs`

Stream all RunsOn application logs from CloudWatch log streams.

This command streams all application logs from the RunsOn service, not filtered by specific jobs. Use this to monitor overall service activity and troubleshoot system-wide issues.

```
Usage:
  roc stack logs [flags]

Flags:
  -d, --debug                 Enable debug output
  -f, --format string         Output format: long (default) or short (default "long")
  -h, --help                  help for logs
      --no-color              Disable color output
  -s, --since string          Show logs since duration (e.g. 30m, 2h) (default "2h")
  -w, --watch string[="5s"]   Watch for new logs with optional interval (e.g. --watch 2s)

Global Flags:
      --stack string   CloudFormation stack name (default "runs-on")
```

Examples:

```bash
# Stream last 2 hours of application logs (default)
AWS_PROFILE=runs-on-admin roc stack logs

# Stream last 24 hours of logs
AWS_PROFILE=runs-on-admin roc stack logs --since 24h

# Stream logs with watch mode (refreshes every 5 seconds)
AWS_PROFILE=runs-on-admin roc stack logs --watch

# Stream logs with custom watch interval
AWS_PROFILE=runs-on-admin roc stack logs --watch 10s

# Stream logs in short format without color
AWS_PROFILE=runs-on-admin roc stack logs --format short --no-color
```

## Contributing

Contributions are welcome! Ideas of future improvements:

* `roc stack pause|resume` - set RunsOn in maintenance mode (queue incoming jobs, but don't start them), to perform an upgrade.
* `roc stack upgrade` - upgrade RunsOn stack to the latest version.
* `roc cache [list|clear]` - list or clear cached data for a specific repository.
* `roc validate path/to/runs-on.yml` - validate the RunsOn repository configuration file.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
