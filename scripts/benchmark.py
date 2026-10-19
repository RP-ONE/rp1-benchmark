#!/usr/bin/env python3
"""Set up a binary-only private validator network or fund a shared-testnet run."""
from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from decimal import Decimal, ROUND_CEILING
import fcntl
import json
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import subprocess
import sys
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlparse
from urllib.request import Request, urlopen

from release import ARCHIVE_SHA256, VERSION, install_release

ROOT = Path(__file__).resolve().parents[1]
BOOTSTRAP_URL = "https://testnet.chain.rp.one/bootstrap.json"
PUBLIC_RPC = "https://testnet.rpc.rp.one"
PUBLIC_REST = "https://testnet.rest.rp.one"
FAUCET_URL = "https://app.rp.one/api/faucet"


def write_json(path, value):
    temporary = path.with_suffix(path.suffix + ".tmp")
    with temporary.open("w", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")
    temporary.replace(path)


class Session:
    def __init__(self, directory, config):
        self.directory, self.config, self.lock = directory, config, None

    def __enter__(self):
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        if self.directory.is_symlink() or self.directory.stat().st_mode & 0o077:
            raise RuntimeError(f"state directory must be private (mode 0700): {self.directory}")
        self.lock = (self.directory / "session.lock").open("a")
        try:
            try:
                fcntl.flock(self.lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                raise RuntimeError("this benchmark session is already running") from error
            path = self.directory / "session.json"
            if path.exists() and json.loads(path.read_text()) != self.config:
                raise RuntimeError("state configuration differs; select a new --state-dir to preserve existing homes and keys")
            write_json(path, self.config)
        except BaseException:
            self.lock.close()
            self.lock = None
            raise
        return self

    def __exit__(self, *_):
        if self.lock:
            self.lock.close()


class ChildProcess:
    def __init__(self, command, log, env=None):
        self.command, self.log, self.process, self.stream = command, log, None, None
        self.env = env

    def __enter__(self):
        self.stream = self.log.open("a")
        try:
            self.process = subprocess.Popen(self.command, stdin=subprocess.DEVNULL,
                                            stdout=self.stream, stderr=subprocess.STDOUT,
                                            start_new_session=True, env=self.env)
        except BaseException:
            self.stream.close()
            raise
        return self

    def check(self):
        if self.process.poll() is not None:
            raise RuntimeError(f"validator process exited ({self.process.returncode}); see {self.log}")

    def __exit__(self, *_):
        if self.process:
            try:
                os.killpg(self.process.pid, signal.SIGINT)
                try:
                    self.process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    pass
                os.killpg(self.process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            self.process.wait()
        if self.stream:
            self.stream.close()


def http_json(endpoint, request=None, timeout=5):
    parsed = urlparse(endpoint)
    if parsed.scheme not in ("http", "https") or not parsed.hostname or parsed.username or parsed.query or parsed.fragment:
        raise RuntimeError("endpoint must be an HTTP(S) URL without credentials, query, or fragment")
    data = None if request is None else json.dumps(request).encode()
    headers = {"Content-Type": "application/json", "User-Agent": "rp1-benchmark-setup/1"}
    try:
        with urlopen(Request(endpoint, data=data, headers=headers), timeout=timeout) as response:
            body = response.read(16 * 1024 * 1024 + 1)
            if len(body) > 16 * 1024 * 1024:
                raise RuntimeError("endpoint response exceeds size limit")
            return json.loads(body)
    except (HTTPError, URLError, OSError, ValueError) as error:
        raise RuntimeError(f"{endpoint}: {error}") from error


def rpc(endpoint, method):
    result = http_json(endpoint, {"jsonrpc": "2.0", "id": 1, "method": method, "params": {}})
    if result.get("error"):
        raise RuntimeError(f"{endpoint} {method}: {result['error']}")
    if not isinstance(result.get("result"), dict):
        raise RuntimeError(f"{endpoint} {method}: missing result")
    return result["result"]


def capture_node(endpoint, chain_id, peers):
    status = rpc(endpoint, "status")
    identity = status.get("node_info", {}).get("id", "")
    if (status.get("node_info", {}).get("network") != chain_id
            or status.get("consensus_running") is not True
            or status.get("sync_info", {}).get("catching_up") is not False
            or int(status.get("sync_info", {}).get("latest_block_height", "0")) <= 0
            or not re.fullmatch(r"[0-9a-fA-F]{16}", identity)
            or int(status.get("consensus_metrics", {}).get("connected_peers", 0)) < peers):
        raise RuntimeError(f"{endpoint}: chain, consensus, height, identity, or peer readiness failed")
    head = rpc(endpoint, "block_header")
    header = head.get("header", {})
    if (header.get("chain_id") != chain_id or int(header.get("height", "0")) <= 0
            or header.get("time_source") != "retained_block_data"
            or not re.fullmatch(r"[0-9A-Fa-f]{64}", head.get("block_id", {}).get("hash", ""))
            or not re.fullmatch(r"[0-9A-Fa-f]{64}", header.get("app_hash", ""))):
        raise RuntimeError(f"{endpoint}: complete retained header unavailable")
    block_time = datetime.fromisoformat(header["time"].replace("Z", "+00:00"))
    if block_time.year <= 1 or block_time.tzinfo is None:
        raise RuntimeError(f"{endpoint}: retained block time unavailable")
    return {"rpc": endpoint, "node_id": identity.lower(), "header": head}


def wait_ready(endpoints, chain_id, timeout, child=None):
    deadline, last_error = time.monotonic() + timeout, "waiting for convergence"
    with ThreadPoolExecutor(max_workers=len(endpoints)) as pool:
        while time.monotonic() < deadline:
            if child:
                child.check()
            try:
                futures = [pool.submit(capture_node, endpoint, chain_id, min(1, len(endpoints) - 1)) for endpoint in endpoints]
                nodes = [future.result() for future in futures]
                if len({node["node_id"] for node in nodes}) != len(nodes):
                    raise RuntimeError("duplicate validator node identity")
                reference = nodes[0]["header"]
                if all(node["header"] == reference for node in nodes):
                    return nodes
                last_error = "waiting for validator headers to converge at one height/hash/app-hash/time"
            except (RuntimeError, KeyError, TypeError, ValueError) as error:
                last_error = str(error)
            time.sleep(min(0.2, max(0, deadline - time.monotonic())))
    raise RuntimeError(f"validator readiness/convergence timed out: {last_error}")


def build_clients(directory):
    if not shutil.which("go"):
        raise RuntimeError("Go 1.24+ is required; install Go, then rerun the same command")
    directory.mkdir(exist_ok=True)
    for name in ("rp1-benchmark", "rp1-accounts"):
        subprocess.run(["go", "build", "-o", str(directory / name), f"./cmd/{name}"], cwd=ROOT, check=True)


def run_benchmark(args, binary, seed_path, rpc_url, rest_url, artifact):
    command = [str(binary), "--rpc", rpc_url, "--rest", rest_url, "--chain-id", args.chain_id,
               "--seed-accounts-file", str(seed_path), "--duration", args.duration,
               "--concurrency", str(args.concurrency), "--operations-per-envelope", str(args.operations_per_envelope),
               "--fee-urp1", str(args.fee_urp1), "--gas", str(args.gas), "--poll", args.poll,
               "--output", str(artifact)]
    print(f"Running benchmark: {args.concurrency} workers, {args.operations_per_envelope} operations/envelope, {args.duration}", flush=True)
    subprocess.run(command, check=True, stdout=subprocess.DEVNULL)
    result = json.loads(artifact.read_text())
    print(f"Committed {result['successful_envelopes']} successful envelopes; "
          f"{result['operations_per_second']:.2f} operations/s; finality P95 {result['finality_p95']}", flush=True)


def local_endpoint(address):
    parsed = urlparse(address.replace("tcp://", "http://"))
    if parsed.hostname not in ("127.0.0.1", "0.0.0.0") or not parsed.port:
        raise RuntimeError("managed network published an unexpected listener")
    return f"http://127.0.0.1:{parsed.port}"


def run_private(args, state, artifact):
    if platform.system() != "Linux" or platform.machine() not in ("x86_64", "amd64"):
        raise RuntimeError("validator release requires a native Linux x86-64 host; use that host for this command")
    daemon = install_release(state / "validator")
    try:
        version = subprocess.check_output([str(daemon), "version"], text=True).strip()
    except subprocess.CalledProcessError as error:
        raise RuntimeError("published validator could not run on this CPU; see validator output") from error
    if version != VERSION:
        raise RuntimeError("installed validator version differs from pinned release")
    build_clients(state / "bin")
    command = [str(daemon), "testnet", "start", "--chain-id", args.chain_id,
               "--output-dir", str(state / "network"), "--v", str(args.validators),
               "--seed-accounts", str(args.concurrency), "--seed-demo-transactions=false", "--print-mnemonic=false",
               "--rpc.address", "tcp://0.0.0.0:26657", "--api.address", "tcp://127.0.0.1:1317",
               "--grpc.address", "127.0.0.1:9090", "--json-rpc.address", "127.0.0.1:8545",
               "--minimum-gas-prices", "0urp1", "--consensus.batch-timeout", "20ms",
               "--consensus.round-timeout", "50ms"]
    base = state / "network" / args.chain_id
    temporary = state / "tmp"
    temporary.mkdir(mode=0o700, exist_ok=True)
    environment = dict(os.environ, TMPDIR=str(temporary))
    print(f"Starting {args.validators} private validators; log: {state / 'validators.log'}", flush=True)
    with ChildProcess(command, state / "validators.log", env=environment) as child:
        deadline = time.monotonic() + args.startup_timeout
        topology_path = base / "managed-dag-topology.json"
        while not topology_path.exists():
            child.check()
            if time.monotonic() >= deadline:
                raise RuntimeError("validator topology initialization timed out; inspect validators.log")
            time.sleep(0.2)
        topology = json.loads(topology_path.read_text())
        nodes = topology["nodes"]
        if len(nodes) != args.validators or topology["config"]["chain_id"] != args.chain_id:
            raise RuntimeError("managed topology does not match the requested validator network")
        endpoints = [local_endpoint(node["RPCLaddr"]) for node in nodes]
        rest_url = local_endpoint(nodes[0]["APIAddress"])
        artifact["validators"] = wait_ready(endpoints, args.chain_id, max(0.1, deadline - time.monotonic()), child)
        artifact["validator_archive_sha256"] = ARCHIVE_SHA256
        seeds = base / "seed-accounts" / "accounts.json"
        print("Private network ready. Fresh benchmark accounts are funded in genesis.", flush=True)
        run_benchmark(args, state / "bin" / "rp1-benchmark", seeds, endpoints[0], rest_url, state / "result.json")


def run_testnet(args, state, artifact):
    # This mode benchmarks existing active validators; genesis-only observer join
    # does not synchronize the existing ledger and must not be automated here.
    bootstrap = http_json(BOOTSTRAP_URL)
    if bootstrap.get("chain_id") != "chain-1" or bootstrap.get("genesis", {}).get("chain_id") != "chain-1":
        raise RuntimeError("public bootstrap chain identity is invalid")
    rpc_url, rest_url = bootstrap.get("rpc_url"), bootstrap.get("rest_url")
    if rpc_url != PUBLIC_RPC or rest_url != PUBLIC_REST:
        raise RuntimeError("bootstrap endpoint change requires a reviewed network configuration update")
    artifact["validators"] = wait_ready([rpc_url], args.chain_id, args.startup_timeout)
    if args.fee_urp1 is None:
        minimum = http_json(rest_url + "/cosmos/base/node/v1beta1/config").get("minimum_gas_price", "")
        match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)urp1", minimum)
        if not match:
            raise RuntimeError("node did not publish a native minimum gas price; specify --fee-urp1 explicitly")
        gas = args.gas or 100000 + args.operations_per_envelope * 15000
        args.fee_urp1 = int((Decimal(match[1]) * gas).to_integral_value(rounding=ROUND_CEILING))
    build_clients(state / "bin")
    accounts = state / "bin" / "rp1-accounts"
    seeds = state / "accounts.json"
    if not seeds.exists():
        subprocess.run([str(accounts), "create", "--chain-id", args.chain_id, "--count", str(args.concurrency), "--output", str(seeds)], check=True)
    subprocess.run([str(accounts), "fund", "--seed-accounts-file", str(seeds), "--rpc", rpc_url,
                    "--rest", rest_url, "--faucet-url", FAUCET_URL, "--output", str(state / "funding.json"),
                    "--timeout", str(args.startup_timeout) + "s"], check=True)
    run_benchmark(args, state / "bin" / "rp1-benchmark", seeds, rpc_url, rest_url, state / "result.json")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("private", "testnet"))
    parser.add_argument("--state-dir", type=Path)
    parser.add_argument("--validators", type=int, default=4, help="private network validators (1..32)")
    parser.add_argument("--concurrency", type=int, default=20)
    parser.add_argument("--operations-per-envelope", type=int, default=100)
    parser.add_argument("--duration", default="20s")
    parser.add_argument("--poll", default="5ms")
    parser.add_argument("--fee-urp1", type=int, help="default: zero privately; node minimum on shared testnet")
    parser.add_argument("--gas", type=int, default=0)
    parser.add_argument("--startup-timeout", type=float, default=180)
    args = parser.parse_args()
    if not 1 <= args.validators <= 32 or not 1 <= args.concurrency <= 4096 or not 1 <= args.operations_per_envelope <= 100000 or args.startup_timeout <= 0 or args.gas < 0 or (args.fee_urp1 is not None and args.fee_urp1 < 0):
        parser.error("invalid validator count, concurrency, batch size, timeout, gas, or fee")
    args.chain_id = "rp1-benchmark-1" if args.mode == "private" else "chain-1"
    if args.mode == "private" and args.fee_urp1 is None:
        args.fee_urp1 = 0
    state = (args.state_dir or ROOT / ".rp1-benchmark" / args.mode).absolute()
    config = {"mode": args.mode, "chain_id": args.chain_id,
              "validators": args.validators if args.mode == "private" else None,
              "accounts": args.concurrency, "validator_version": VERSION if args.mode == "private" else None}
    os.umask(0o077)
    artifact = {"configuration": config, "platform": platform.platform(), "cpu_count": os.cpu_count(),
                "started_at": datetime.now(timezone.utc).isoformat(), "complete": False}
    try:
        with Session(state, config):
            try:
                if args.mode == "private":
                    run_private(args, state, artifact)
                else:
                    run_testnet(args, state, artifact)
                artifact["complete"] = True
            except BaseException as error:
                artifact["run_error"] = str(error) or type(error).__name__
                raise
            finally:
                artifact["finished_at"] = datetime.now(timezone.utc).isoformat()
                write_json(state / "setup.json", artifact)
    except (RuntimeError, OSError, ValueError, subprocess.SubprocessError, KeyboardInterrupt) as error:
        print(f"rp1-benchmark setup: {error}", file=sys.stderr)
        return 1
    print(f"Results: {state / 'result.json'}\nSetup evidence: {state / 'setup.json'}")
    return 0


if __name__ == "__main__":
    def interrupted(*_):
        raise KeyboardInterrupt()

    signal.signal(signal.SIGTERM, interrupted)
    raise SystemExit(main())
