import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
import local_runtime


class LocalRuntimeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.state = self.root / "state"
        self.state.mkdir(mode=0o700)
        self.log = self.root / "docker.jsonl"
        self.docker = self.root / "docker"
        self.docker.write_text(textwrap.dedent(
            """
            #!/usr/bin/env python3
            import json, os, sys, time
            log = os.environ["FAKE_DOCKER_LOG"]
            with open(log, "a", encoding="utf-8") as stream:
                stream.write(json.dumps(sys.argv[1:]) + "\\n")
            if sys.argv[1:2] == ["version"]:
                print("27.0.0")
                raise SystemExit(0)
            if sys.argv[1:2] == ["stop"]:
                pid_path = os.environ.get("FAKE_DOCKER_RUN_PID")
                if pid_path and os.path.exists(pid_path):
                    os.kill(int(open(pid_path, encoding="utf-8").read()), 15)
                raise SystemExit(0)
            if sys.argv[1:2] == ["run"] and os.environ.get("FAKE_DOCKER_SLEEP"):
                with open(os.environ["FAKE_DOCKER_RUN_PID"], "w", encoding="utf-8") as stream:
                    stream.write(str(os.getpid()))
                signal = __import__("signal")
                def stopped(*_):
                    with open(os.environ["FAKE_DOCKER_STOP_MARKER"], "w", encoding="utf-8") as stream:
                        stream.write("stopped")
                    raise SystemExit(143)
                signal.signal(signal.SIGTERM, stopped)
                time.sleep(30)
            if sys.argv[1:2] == ["run"]:
                raise SystemExit(int(os.environ.get("FAKE_DOCKER_RUN_EXIT", "17")))
            raise SystemExit(0)
            """
        ).strip() + "\n")
        self.docker.chmod(self.docker.stat().st_mode | stat.S_IXUSR)
        self.env = dict(os.environ, DOCKER_BIN=str(self.docker), FAKE_DOCKER_LOG=str(self.log))

    def tearDown(self):
        self.temp.cleanup()

    def _calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def test_private_entrypoint_uses_container_without_host_go(self):
        child_env = dict(self.env, FAKE_DOCKER_RUN_EXIT="0")
        result = subprocess.run(
            [sys.executable, "-c",
             f"import sys; sys.path.insert(0, {str(local_runtime.ROOT / 'scripts')!r}); "
             "import benchmark; benchmark.platform.system=lambda: 'Darwin'; "
             "raise SystemExit(benchmark.main())",
             "private", "--state-dir", str(self.state)], env=child_env,
            capture_output=True, text=True, timeout=10,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any(call[0] == "run" for call in self._calls()))

    def test_runs_public_python_command_with_private_state_and_read_only_repo(self):
        with mock.patch.dict(os.environ, self.env, clear=True):
            code = local_runtime.run_local_container(
                ["scripts/benchmark.py", "--state-dir=/host/state", "--duration", "10s"], self.state
            )
        self.assertEqual(code, 17)
        calls = self._calls()
        self.assertEqual(calls[0][:2], ["version", "--format={{.Server.Version}}"])
        command = calls[1]
        self.assertEqual(command[0], "run")
        self.assertIn("--network=bridge", command)
        self.assertNotIn("host", command)
        self.assertIn("--platform=linux/amd64", command)
        self.assertIn("--mount", command)
        self.assertTrue(any("/work/repo,readonly" in value for value in command))
        self.assertTrue(any("/work/state" in value and "readonly" not in value for value in command))
        self.assertIn("golang:1.24-bookworm", command)
        self.assertIn("python3", command)
        self.assertIn("--state-dir=/work/state", command)
        self.assertNotIn("/host/state", command)
        self.assertTrue(any(call[0] == "stop" for call in calls), "a detached container may survive a failed client")

    def test_translates_separate_state_dir_argument(self):
        with mock.patch.dict(os.environ, self.env, clear=True):
            local_runtime.run_local_container(["scripts/benchmark.py", "--state-dir", "/tmp/old"], self.state)
        self.assertIn("--state-dir", self._calls()[1])
        command = self._calls()[1]
        self.assertEqual(command[command.index("--state-dir") + 1], "/work/state")

    def test_appends_state_dir_when_command_does_not_supply_one(self):
        with mock.patch.dict(os.environ, self.env, clear=True):
            local_runtime.run_local_container(["scripts/benchmark.py"], self.state)
        self.assertIn("--state-dir=/work/state", self._calls()[1])

    def test_rejects_symlink_state_before_resolving_it(self):
        target = self.root / "target"
        target.mkdir(mode=0o700)
        link = self.root / "link"
        link.symlink_to(target, target_is_directory=True)
        with mock.patch.dict(os.environ, self.env, clear=True):
            with self.assertRaisesRegex(RuntimeError, "private"):
                local_runtime.run_local_container(["scripts/benchmark.py"], link)

    def test_missing_docker_fails_before_run(self):
        with mock.patch.dict(os.environ, dict(self.env, DOCKER_BIN=str(self.root / "missing")), clear=True):
            with self.assertRaisesRegex(RuntimeError, "Docker"):
                local_runtime.run_local_container(["scripts/benchmark.py"], self.state)
        self.assertFalse(self.log.exists())

    def test_interrupt_stops_named_container(self):
        driver = self.root / "driver.py"
        driver.write_text(textwrap.dedent(
            f"""
            import os, sys
            sys.path.insert(0, {str(Path(__file__).resolve().parents[1] / 'scripts')!r})
            import local_runtime
            raise SystemExit(local_runtime.run_local_container(['scripts/benchmark.py'], {str(self.state)!r}))
            """
        ))
        pid_path = self.root / "run.pid"
        stopped_path = self.root / "run.stopped"
        child_env = dict(self.env, FAKE_DOCKER_SLEEP="1", FAKE_DOCKER_RUN_PID=str(pid_path),
                         FAKE_DOCKER_STOP_MARKER=str(stopped_path))
        child = subprocess.Popen([sys.executable, str(driver)], env=child_env)
        try:
            deadline = time.monotonic() + 5
            while not pid_path.exists() and time.monotonic() < deadline:
                time.sleep(0.02)
            os.kill(child.pid, signal.SIGINT)
            self.assertEqual(child.wait(timeout=5), 130)
        finally:
            if child.poll() is None:
                child.kill()
        calls = self._calls()
        self.assertTrue(any(call[0] == "stop" for call in calls))
        self.assertTrue(stopped_path.exists())

    def test_sigterm_returns_143_and_stops_runner(self):
        driver = self.root / "driver.py"
        driver.write_text(textwrap.dedent(
            f"""
            import sys
            sys.path.insert(0, {str(Path(__file__).resolve().parents[1] / 'scripts')!r})
            import local_runtime
            raise SystemExit(local_runtime.run_local_container(['scripts/benchmark.py'], {str(self.state)!r}))
            """
        ))
        pid_path = self.root / "run.pid"
        stopped_path = self.root / "run.stopped"
        child_env = dict(self.env, FAKE_DOCKER_SLEEP="1", FAKE_DOCKER_RUN_PID=str(pid_path),
                         FAKE_DOCKER_STOP_MARKER=str(stopped_path))
        child = subprocess.Popen([sys.executable, str(driver)], env=child_env)
        try:
            deadline = time.monotonic() + 5
            while not pid_path.exists() and time.monotonic() < deadline:
                time.sleep(0.02)
            os.kill(child.pid, signal.SIGTERM)
            self.assertEqual(child.wait(timeout=5), 143)
        finally:
            if child.poll() is None:
                child.kill()
        self.assertTrue(any(call[0] == "stop" for call in self._calls()))
        self.assertTrue(stopped_path.exists())


if __name__ == "__main__":
    unittest.main()
