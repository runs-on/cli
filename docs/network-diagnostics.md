# Investigating slow public downloads

Collect evidence while the affected runner still exists. `roc logs JOB_URL --full` retrieves retained diagnostics; it cannot reconstruct destination timings or TCP counters that were never recorded. Use the CLI version matching the stack, and record both versions, the job URL, UTC interval, runner family, region/AZ, networking mode, and whether the slow transfer ran on the host or inside BuildKit.

## Capture a small reproducible sample

Copy [`network_probe.py`](../examples/diagnostics/network_probe.py) into the workload repository. It needs Linux, Python 3.9 or newer, and curl 8.4 or newer. The latter supports enforcing the transfer-size cap even when the server did not advertise its response size. No Python packages, AWS permissions, or root access are required.

Invoke it immediately before and after the slow step. With no arguments it only records local interface/TCP counters, load average, and memory availability. With one to three explicitly selected public HTTPS URLs it also performs bounded GET requests and records DNS, TCP, TLS, first-byte and total timings, response status, byte count, and average download rate. Choose an affected package URL and an independent control of comparable size; tiny HTML pages are poor bandwidth controls.

```yaml
- name: Network counters before the build
  run: python3 .github/scripts/network_probe.py > "$RUNNER_TEMP/network-before.jsonl"

# Run the workload here. Capture during the slowdown if possible; a probe
# after recovery cannot explain a transient incident.

- name: Network counters after the build
  if: always()
  run: python3 .github/scripts/network_probe.py > "$RUNNER_TEMP/network-after.jsonl"

- name: Preserve network observations
  if: always()
  uses: actions/upload-artifact@v7
  with:
    name: network-${{ github.job }}-${{ strategy.job-index }}
    path: ${{ runner.temp }}/network-*.jsonl
    retention-days: 3
```

For transfer timings, pass public URLs as arguments, for example `python3 .github/scripts/network_probe.py https://packages.example.org/sample.tar.gz`. Replace that illustrative URL with a real package or control. Each URL is limited to 20 seconds and a 10 MiB range request, with a five-second connection timeout and at most three HTTPS redirects. A server that ignores ranges may trigger curl exit code 63 (size limit); exit code 28 is a timeout. These are observations, not evidence of successful full transfers. The probe preserves failed-transfer records and exits successfully after collecting them so diagnostics do not replace the build's result.

The sample does not print response bodies, headers, curl stderr, full URLs, environment variables, or credentials. It rejects userinfo, query strings, and fragments, ignores `.curlrc`, and only emits selected fields from curl's JSON. Use public unauthenticated URLs, avoid tokens embedded in URL paths, and review artifacts before attaching them to a public issue. Proxy environment settings still affect curl's route; record whether a proxy is in use without publishing its credentials. Raw `roc` archives and verbose logs need a separate review and are not sanitized by this probe.

## Interpret the observations

| Observation                                                           | Next check                                                                                                                    |
| --------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| DNS time rises                                                        | Resolver health and whether only one destination is affected.                                                                 |
| Connection or TLS time rises                                          | Compare destinations and host/container paths; inspect network path and connection setup.                                     |
| First byte is slow but connection setup is quick                      | Remote package service/CDN response, authentication, or server throttling.                                                    |
| Transfer rate falls and retransmissions rise during the same interval | Collect same-instance route/ENA diagnostics before termination; counters suggest loss or retries but do not locate the cause. |
| High CPU load, memory pressure, or local disk pressure                | Compare workload/resource metrics before attributing delay to egress.                                                         |
| Host probes are fast but a BuildKit package step is slow              | Repeat comparable probes inside the build network namespace; host totals cannot attribute traffic to a specific container.    |
| Only the cache endpoint fails immediately with 404                    | Diagnose cache API selection separately from slow public package downloads.                                                   |

Counters are cumulative and can reset with an interface or instance; compare samples from the same runner and interval. They include unrelated concurrent traffic and provide no per-flow attribution. Inspect destination-specific route, ENA allowance and conntrack data through an authorized live session when needed; aggregate network bytes alone establish none of those conditions. Download speed includes startup time and the sample cap, so it is not a maximum-throughput benchmark.

After the job, export `roc logs JOB_URL --full` from an authorized workstation and correlate it with the probe timestamps. Consult the [current CLI contract](https://runs-on.com/docs/observability/cli/) for the available retained artifacts. [runs-on/runs-on#542](https://github.com/runs-on/runs-on/issues/542) describes a transient incident on v2.12.1-rc.4; this procedure does not establish a defect in that release or reproduce the incident on v3. Provider-side alerting and automatic retention of finer network diagnostics remain separate work.

## Validate the example locally

```bash
python3 -m unittest discover -s examples/diagnostics -p 'test_*.py'
python3 examples/diagnostics/network_probe.py
```

The second command only reads local counters. Transfer limits and timing fields are documented in the [curl manual](https://curl.se/docs/manpage.html).
