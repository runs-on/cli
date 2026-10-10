#!/usr/bin/env python3
"""Collect bounded Linux egress diagnostics without recording request secrets."""

import argparse
import datetime
import json
import os
import subprocess
from pathlib import Path
from urllib.parse import urlsplit

MAX_BYTES = 10 * 1024 * 1024
MAX_SECONDS = 20
TIMING_FIELDS = (
    "http_code",
    "http_version",
    "num_redirects",
    "size_download",
    "speed_download",
    "time_namelookup",
    "time_connect",
    "time_appconnect",
    "time_starttransfer",
    "time_total",
)
TCP_FIELDS = {
    "RetransSegs",
    "InErrs",
    "OutRsts",
    "InSegs",
    "OutSegs",
    "TCPSynRetrans",
    "TCPTimeouts",
    "TCPFastRetrans",
    "TCPLostRetransmit",
}
INTERFACE_FIELDS = (
    "rx_bytes",
    "tx_bytes",
    "rx_packets",
    "tx_packets",
    "rx_errors",
    "tx_errors",
    "rx_dropped",
    "tx_dropped",
)


def timestamp():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def read_text(path):
    try:
        return Path(path).read_text()
    except OSError:
        return ""


def tcp_counters(text):
    result = {}
    lines = text.splitlines()
    for header, values in zip(lines[::2], lines[1::2]):
        names, counts = header.split(), values.split()
        if not names or not counts or names[0] != counts[0]:
            continue
        for name, value in zip(names[1:], counts[1:]):
            if name in TCP_FIELDS and value.isdigit():
                result[names[0].rstrip(":") + "." + name] = int(value)
    return result


def system_sample(phase):
    interfaces = {}
    for interface in sorted(Path("/sys/class/net").glob("*")):
        counters = {}
        for field in INTERFACE_FIELDS:
            value = read_text(interface / "statistics" / field).strip()
            if value.isdigit():
                counters[field] = int(value)
        interfaces[interface.name] = counters
    memory = {}
    for line in read_text("/proc/meminfo").splitlines():
        name, _, value = line.partition(":")
        if name in {"MemTotal", "MemAvailable", "SwapTotal", "SwapFree"}:
            memory[name] = value.strip()
    return {
        "kind": "system",
        "phase": phase,
        "timestamp": timestamp(),
        "interfaces": interfaces,
        "memory": memory,
        "load_average": read_text("/proc/loadavg").split()[:3],
        "tcp": tcp_counters(read_text("/proc/net/snmp"))
        | tcp_counters(read_text("/proc/net/netstat")),
    }


def validate_url(value):
    try:
        parsed = urlsplit(value)
        if (
            parsed.scheme != "https"
            or not parsed.hostname
            or parsed.username is not None
            or parsed.password is not None
            or parsed.query
            or parsed.fragment
            or any(ord(char) < 33 or ord(char) == 127 for char in value)
        ):
            raise ValueError
        _ = (
            parsed.port
        )  # Validate malformed or out-of-range ports before invoking curl.
    except ValueError:
        raise argparse.ArgumentTypeError(
            "use an HTTPS URL without credentials, query, fragment, or whitespace"
        ) from None
    return value


def transfer(url, index):
    result = {
        "kind": "transfer",
        "timestamp": timestamp(),
        "endpoint_index": index,
        "hostname": urlsplit(url).hostname,
        "max_bytes": MAX_BYTES,
        "max_seconds": MAX_SECONDS,
    }
    try:
        process = subprocess.run(
            [
                "curl",
                "--disable",
                "--silent",
                "--globoff",
                "--proto",
                "=https",
                "--proto-redir",
                "=https",
                "--location",
                "--max-redirs",
                "3",
                "--connect-timeout",
                "5",
                "--max-time",
                str(MAX_SECONDS),
                "--range",
                f"0-{MAX_BYTES - 1}",
                "--max-filesize",
                str(MAX_BYTES),
                "--output",
                os.devnull,
                "--write-out",
                "%{json}",
                "--url",
                url,
            ],
            capture_output=True,
            text=True,
            timeout=MAX_SECONDS + 5,
            check=False,
        )
        result["curl_exit_code"] = process.returncode
        # curl's complete JSON and stderr can contain URLs, credentials, and
        # proxy details. Emit only explicitly selected timing and size fields.
        data = json.loads(process.stdout)
        result.update({key: data[key] for key in TIMING_FIELDS if key in data})
    except FileNotFoundError:
        result["error"] = "curl-not-found"
    except subprocess.TimeoutExpired:
        result["error"] = "probe-timeout"
    except (ValueError, TypeError):
        result["error"] = "invalid-curl-json"
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "urls",
        nargs="*",
        type=validate_url,
        help="up to three public package or control URLs",
    )
    args = parser.parse_args()
    if len(args.urls) > 3:
        parser.error("at most three URLs are allowed per probe")
    print(json.dumps(system_sample("before")), flush=True)
    for index, url in enumerate(args.urls, start=1):
        print(json.dumps(transfer(url, index)), flush=True)
    print(json.dumps(system_sample("after")), flush=True)


if __name__ == "__main__":
    main()
