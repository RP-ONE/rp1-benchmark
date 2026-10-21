"""Setup failure cases: wrong chain, divergent heads, exited children,
concurrent reuse, changed topology, and cancellation must prevent a run.
"""
import json
import os
from pathlib import Path
import sys
import tempfile
import threading
import unittest
from unittest import mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
import benchmark


class Endpoint:
    def __init__(self, identity, height=9, app_hash="A" * 64):
        self.identity, self.height, self.app_hash = identity, height, app_hash
        self.chain = "fixture-1"
        self.calls = []
        endpoint = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_POST(self):
                request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                endpoint.calls.append(request["method"])
                if request["method"] == "status":
                    result = {
                        "node_info": {"id": endpoint.identity, "network": endpoint.chain},
                        "sync_info": {"latest_block_height": str(endpoint.height), "catching_up": False},
                        "consensus_running": True,
                        "consensus_metrics": {"connected_peers": 1},
                    }
                else:
                    result = {
                        "block_id": {"hash": "B" * 64},
                        "header": {"chain_id": endpoint.chain, "height": str(endpoint.height),
                                   "app_hash": endpoint.app_hash, "time": "2026-10-15T07:00:00Z",
                                   "time_source": "retained_block_data"},
                    }
                body = json.dumps({"result": result}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = "http://127.0.0.1:" + str(self.server.server_port)

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()


class SetupChecks(unittest.TestCase):
    def setUp(self):
        self.first = Endpoint("1111111111111111")
        self.second = Endpoint("2222222222222222")
        self.addCleanup(self.first.close)
        self.addCleanup(self.second.close)
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)

    def test_legacy_profile_runs_exact_batched_workload(self):
        captured = []
        def run(args, state, artifact):
            captured.append((args.envelopes, args.operations_per_envelope, args.concurrency, args.duration,
                             args.poll, args.finality_timeout))
        argv = ["benchmark.py", "private", "--profile", "legacy-6137", "--state-dir", str(self.root)]
        with mock.patch.object(sys, "argv", argv), mock.patch.object(benchmark.platform, "system", return_value="Linux"), \
                mock.patch.object(benchmark.platform, "machine", return_value="x86_64"), \
                mock.patch.object(benchmark, "run_private", side_effect=run):
            self.assertEqual(benchmark.main(), 0)
        self.assertEqual(captured, [(100, 6137, 100, "120s", "50ms", "120s")])

    def test_quickstart_then_legacy_profile_preserves_both_networks(self):
        visited = []
        def run(args, state, artifact):
            visited.append((state, args.concurrency))
            marker = state / "funded-account-count.txt"
            if marker.exists():
                self.assertEqual(marker.read_text(), str(args.concurrency))
            marker.write_text(str(args.concurrency))
        with mock.patch.object(benchmark, "ROOT", self.root), \
                mock.patch.object(benchmark.platform, "system", return_value="Linux"), \
                mock.patch.object(benchmark.platform, "machine", return_value="x86_64"), \
                mock.patch.object(benchmark, "run_private", side_effect=run):
            for extra in ([], ["--profile", "legacy-6137"], []):
                with mock.patch.object(sys, "argv", ["benchmark.py", "private", *extra]):
                    self.assertEqual(benchmark.main(), 0)
        self.assertEqual([accounts for _, accounts in visited], [20, 100, 20])
        self.assertEqual(visited[0][0], visited[2][0])
        self.assertNotEqual(visited[0][0], visited[1][0])
        for directory, accounts in visited[:2]:
            self.assertEqual((directory / "funded-account-count.txt").read_text(), str(accounts))

    def test_legacy_profile_allows_explicit_cadence_and_timeout(self):
        captured = []
        def run(args, state, artifact):
            captured.append((args.poll, args.finality_timeout))
        argv = ["benchmark.py", "private", "--profile", "legacy-6137", "--poll", "5ms",
                "--finality-timeout", "45s", "--state-dir", str(self.root)]
        with mock.patch.object(sys, "argv", argv), mock.patch.object(benchmark.platform, "system", return_value="Linux"), \
                mock.patch.object(benchmark.platform, "machine", return_value="x86_64"), \
                mock.patch.object(benchmark, "run_private", side_effect=run):
            self.assertEqual(benchmark.main(), 0)
        self.assertEqual(captured, [("5ms", "45s")])

    def test_run_benchmark_forwards_finality_timeout_and_poll(self):
        args = type("Args", (), {
            "chain_id": "fixture-1", "duration": "20s", "concurrency": 2,
            "operations_per_envelope": 6137, "envelopes": 100, "fee_urp1": 0,
            "gas": 0, "poll": "50ms", "finality_timeout": "45s",
        })()
        artifact = self.root / "result.json"
        artifact.write_text(json.dumps({"successful_envelopes": 100,
                                        "operations_per_second": 123.0,
                                        "finality_p95": 0.4}))
        with mock.patch.object(benchmark.subprocess, "run") as run:
            benchmark.run_benchmark(args, self.root / "rp1-benchmark", self.root / "accounts.json",
                                    "http://rpc", "http://rest", artifact)
        command = run.call_args.args[0]
        self.assertEqual(command[command.index("--poll") + 1], "50ms")
        self.assertEqual(command[command.index("--finality-timeout") + 1], "45s")

    def test_head_convergence_requires_matching_state(self):
        endpoints = [self.first.url, self.second.url]
        self.second.app_hash = "C" * 64
        with self.assertRaisesRegex(RuntimeError, "converg"):
            benchmark.wait_ready(endpoints, "fixture-1", timeout=0.15)
        self.second.app_hash = self.first.app_hash
        evidence = benchmark.wait_ready(endpoints, "fixture-1", timeout=1)
        self.assertEqual(len(evidence), 2)
        self.assertTrue(all("block_header" in item.calls for item in [self.first, self.second]))

    def test_wrong_chain_and_duplicate_nodes_cannot_be_ready(self):
        self.second.chain = "wrong-chain"
        with self.assertRaises(RuntimeError):
            benchmark.wait_ready([self.first.url, self.second.url], "fixture-1", timeout=0.1)
        self.second.chain = self.first.chain
        self.second.identity = self.first.identity
        with self.assertRaisesRegex(RuntimeError, "duplicate"):
            benchmark.wait_ready([self.first.url, self.second.url], "fixture-1", timeout=0.1)

    def test_exited_daemon_does_not_wait_for_network_timeout(self):
        with benchmark.ChildProcess([sys.executable, "-c", "raise SystemExit(7)"], self.root / "daemon.log") as process:
            process.process.wait(timeout=2)
            with self.assertRaisesRegex(RuntimeError, "exited"):
                benchmark.wait_ready([self.first.url], "fixture-1", timeout=20, child=process)

    def test_cancellation_stops_process_group(self):
        with benchmark.ChildProcess([sys.executable, "-c", "import time; time.sleep(60)"], self.root / "daemon.log") as child:
            pid = child.process.pid
        self.assertIsNotNone(child.process.returncode)
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)

    def test_session_lock_and_topology_mismatch_preserve_wallets(self):
        expected = {"mode": "private", "chain_id": "fixture-1", "validators": 4}
        with benchmark.Session(self.root, expected):
            wallet = self.root / "wallet.json"
            wallet.write_text("local-secret")
            with self.assertRaisesRegex(RuntimeError, "already"):
                with benchmark.Session(self.root, expected):
                    self.fail("second owner acquired live session")
        with self.assertRaisesRegex(RuntimeError, "configuration"):
            with benchmark.Session(self.root, dict(expected, validators=3)):
                self.fail("changed topology reused funded homes")
        self.assertEqual(wallet.read_text(), "local-secret")

    def test_joined_testnet_refuses_daemon_without_state_sync(self):
        with self.assertRaisesRegex(RuntimeError, "state-sync-capable"):
            benchmark.require_join_capability(Path("/tmp/rp1d"), "rp1d testnet join help")

    def test_joined_fee_comes_from_active_node_metadata(self):
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                body = b'{"minimum_gas_price":"0.000001urp1"}'
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        args = type("Args", (), {"fee_urp1": None, "gas": 0, "operations_per_envelope": 100})()
        benchmark.resolve_fee(args, f"http://127.0.0.1:{server.server_port}")
        self.assertEqual(args.fee_urp1, 2)

    def test_joined_observer_preserves_home_and_verifies_rest_replication(self):
        home = self.root / "observer"
        (home / "config").mkdir(parents=True)
        (home / "config" / "app.toml").write_text(
            'minimum-gas-prices = "0urp1"\n\n[api]\nenable = false\naddress = "tcp://127.0.0.1:1317"\n'
        )
        (home / "config" / "state-sync.json").write_text("preserved")
        benchmark.enable_observer_api(home, "127.0.0.1:1317")
        self.assertEqual((home / "config" / "state-sync.json").read_text(), "preserved")
        app_config = (home / "config" / "app.toml").read_text()
        self.assertIn("enable = true", app_config)
        self.assertIn('address = "tcp://127.0.0.1:1317"', app_config)

        tx_hash = "A" * 64
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                body = json.dumps({"tx_response": {"txhash": tx_hash, "code": 0, "height": "9"}}).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        result = self.root / "result.json"
        result.write_text(json.dumps({"attempts": [{"hash": tx_hash, "signed_tx_base64": "c2lnbmVk", "outcome": "committed", "code": 0, "height": 9}]}))
        benchmark.verify_observer_replication(f"http://127.0.0.1:{server.server_port}", result)


if __name__ == "__main__":
    unittest.main()
