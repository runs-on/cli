import argparse
import json
import subprocess
import unittest
from unittest.mock import patch

import network_probe


class NetworkProbeTests(unittest.TestCase):
    def test_rejects_credential_bearing_and_non_https_urls(self):
        for url in (
            "http://example.com/file",
            "https://user:secret@example.com/file",
            "https://example.com/file?token=secret",
            "https://example.com/#secret",
            "https://example.com/\nfile",
            "https://example.com:99999/file",
        ):
            with self.subTest(url=url), self.assertRaises(argparse.ArgumentTypeError):
                network_probe.validate_url(url)

    def test_only_numeric_counter_fields_are_collected(self):
        sample = "Tcp: InSegs RetransSegs Secret\nTcp: 120 4 do-not-emit\n"
        self.assertEqual(
            network_probe.tcp_counters(sample),
            {"Tcp.InSegs": 120, "Tcp.RetransSegs": 4},
        )

    @patch("network_probe.subprocess.run")
    def test_transfer_filters_sensitive_curl_output(self, run):
        run.return_value = subprocess.CompletedProcess(
            [],
            28,
            json.dumps(
                {
                    "http_code": 206,
                    "time_total": 20,
                    "size_download": 1024,
                    "url_effective": "https://example.com/?token=secret",
                    "errormsg": "secret",
                    "proxy_used": "secret",
                    "referer": "secret",
                }
            ),
            "secret",
        )
        result = network_probe.transfer("https://example.com/file", 1)
        self.assertEqual(result["curl_exit_code"], 28)
        self.assertEqual(result["size_download"], 1024)
        self.assertNotIn("secret", json.dumps(result))
        command = run.call_args.args[0]
        self.assertEqual(command[:2], ["curl", "--disable"])
        self.assertIn("--globoff", command)
        self.assertEqual(command[command.index("--max-time") + 1], "20")
        self.assertEqual(command[command.index("--max-filesize") + 1], "10485760")

    @patch(
        "network_probe.subprocess.run",
        side_effect=subprocess.TimeoutExpired(["secret"], 25),
    )
    def test_timeout_does_not_echo_the_command(self, run):
        result = network_probe.transfer("https://example.com/file", 1)
        self.assertEqual(result["error"], "probe-timeout")
        self.assertNotIn("secret", json.dumps(result))


if __name__ == "__main__":
    unittest.main()
