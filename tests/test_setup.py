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


if __name__ == "__main__":
    unittest.main()
