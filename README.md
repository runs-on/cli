# roc

RunsOn CLI (`roc`) is a command line tool to manage and troubleshoot your [RunsOn](https://runs-on.com) installation.

Note: the CLI only works with RunsOn >= v2.6.3.

## Installation

You can download the binaries for your platform (Linux, macOS) from the [Releases](https://github.com/runs-on/cli/releases/latest) page.

Example (macOS ARM64):

```
curl "https://github.com/runs-on/cli/releases/download/v0.1.2/roc-v0.1.2-darwin-arm64.tar.gz" -Lo- | tar -xvz
./roc --help
```

Example (Linux AMD64):

```
curl "https://github.com/runs-on/cli/releases/download/v0.1.2/roc-v0.1.2-linux-amd64.tar.gz" -Lo- | tar -xvz
./roc --help
```

## Features

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

## `roc logs`

Fetch RunsOn server and instance logs for a specific job ID or URL.

```
Usage:
  roc logs JOB_ID|JOB_URL [flags]

Flags:
  -d, --debug                 Enable debug output
  -h, --help                  help for logs
  -s, --since string          Show logs since duration (e.g. 30m, 2h) (default "2h")
  -w, --watch string[="5s"]   Watch for new logs with optional interval (e.g. --watch 2s)

Global Flags:
      --stack string   CloudFormation stack name (default "runs-on")
```

Example:

```bash
AWS_PROFILE=runs-on-admin roc logs https://github.com/runs-on/runs-on/actions/runs/12415485296/job/34661958899 --watch
```

## `roc stack doctor`

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

## Contributing

Contributions are welcome! Ideas of future improvements:

* Make the CloudFormation stack create an IAM role for the CLI to use, so that the CLI automatically assumes it when launched with an admin role?
* `roc stack pause|resume` - set RunsOn in maintenance mode (queue incoming jobs, but don't start them), to perform an upgrade.
* `roc stack upgrade` - upgrade RunsOn stack to the latest version.
* `roc stack logs` - fetch RunsOn server logs.
* `roc cache [list|clear]` - list or clear cached data for a specific repository.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
