# roc

RunsOn CLI (`roc`) is a command line tool to manage and troubleshoot your [RunsOn](https://runs-on.com) installation.

Note: the CLI only works with RunsOn >= v2.6.3.

## Installation

You can download the binaries for your platform (Linux, macOS) from the [Releases](https://github.com/runs-on/cli/releases/latest) page.

Example (macOS ARM64):

```
curl "https://github.com/runs-on/cli/releases/download/v0.1.1/roc-v0.1.1-darwin-arm64.tar.gz" -Lo- | tar -xvz
./roc --help
```

Example (Linux AMD64):

```
curl "https://github.com/runs-on/cli/releases/download/v0.1.1/roc-v0.1.1-linux-amd64.tar.gz" -Lo- | tar -xvz
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
roc connect https://github.com/runs-on/runs-on/actions/runs/12415485296/job/34661958899
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
roc logs https://github.com/runs-on/runs-on/actions/runs/12415485296/job/34661958899 --watch
```

## Contributing

Contributions are welcome! Ideas of future improvements:

* Make the CloudFormation stack create an IAM role for the CLI to use, so that the CLI automatically assumes it when launched with an admin role?
* `roc stack doctor` - check RunsOn stack and make sure everything is healthy (AppRunner endpoint health check, GitHub App webhook deliveries, etc.).
* `roc stack pause|resume` - set RunsOn in maintenance mode (queue incoming jobs, but don't start them), to perform an upgrade.
* `roc stack upgrade` - upgrade RunsOn stack to the latest version.
* `roc stack logs` - fetch RunsOn server logs.
* `roc cache [list|clear]` - list or clear cached data for a specific repository.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
