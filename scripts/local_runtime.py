"""Run the public benchmark command in a disposable local Linux container."""
from __future__ import annotations

import os
from pathlib import Path
import platform
import signal
import subprocess
import uuid


ROOT = Path(__file__).resolve().parents[1]
CONTAINER_IMAGE = "golang:1.24-bookworm"
CONTAINER_PLATFORM = "linux/amd64"
_PROBE_TIMEOUT = 5
_STOP_TIMEOUT = 15


def _docker_binary() -> str:
    return os.environ.get("DOCKER_BIN", "docker")


def _probe_docker(docker: str) -> None:
    try:
        result = subprocess.run(
            [docker, "version", "--format={{.Server.Version}}"],
            check=False,
            capture_output=True,
            text=True,
            timeout=_PROBE_TIMEOUT,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RuntimeError("Docker is unavailable or did not respond within 5 seconds") from error
    if result.returncode != 0:
        detail = (result.stderr or result.stdout).strip()
        raise RuntimeError(f"Docker is unavailable: {detail or 'version probe failed'}")


def _container_args(argv: list[str], state: Path) -> list[str]:
    translated: list[str] = []
    has_state_dir = False
    index = 0
    while index < len(argv):
        value = str(argv[index])
        if value == "--state-dir":
            has_state_dir = True
            translated.append(value)
            index += 1
            if index >= len(argv):
                raise ValueError("--state-dir requires a path")
            translated.append("/work/state")
        elif value.startswith("--state-dir="):
            has_state_dir = True
            translated.append("--state-dir=/work/state")
        else:
            translated.append(value)
        index += 1
    if not has_state_dir:
        translated.append("--state-dir=/work/state")

    state_mount = f"type=bind,src={state},dst=/work/state"
    repo_mount = f"type=bind,src={ROOT},dst=/work/repo,readonly"
    return [
        "run",
        "--rm",
        "--name",
        f"rp1-benchmark-local-{os.getpid()}-{uuid.uuid4().hex[:10]}",
        "--platform=" + CONTAINER_PLATFORM,
        "--network=bridge",
        "--mount",
        repo_mount,
        "--mount",
        state_mount,
        "--workdir",
        "/work/repo",
        "--env",
        "TMPDIR=/work/state/.tmp",
        "--env",
        "GOMODCACHE=/work/state/.cache/go-mod",
        "--env",
        "GOCACHE=/work/state/.cache/go-build",
        "--env",
        "PYTHONUNBUFFERED=1",
        "--env",
        "RP1_BENCHMARK_HOST_PLATFORM=" + platform.platform(),
        "--env",
        "RP1_BENCHMARK_HOST_CPUS=" + str(os.cpu_count()),
        CONTAINER_IMAGE,
        "python3",
        *translated,
    ]


def _stop(docker: str, name: str) -> None:
    try:
        subprocess.run(
            [docker, "stop", "--time", "10", name],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=_STOP_TIMEOUT,
        )
    except (OSError, subprocess.TimeoutExpired):
        pass


def run_local_container(argv: list[str], state: Path | str) -> int:
    """Run ``python3`` plus *argv* in a disposable local Docker container.

    The selected state directory is the only writable host mount. Docker's
    normal bridge network is used so the container can reach the public chain;
    no host ports, Docker socket, credentials, or private source are mounted.
    """
    if not argv:
        raise ValueError("local benchmark command is empty")
    selected_state = Path(state).expanduser()
    if selected_state.is_symlink():
        raise RuntimeError(f"state directory must be private (mode 0700): {selected_state}")
    state_path = selected_state.resolve()
    state_path.mkdir(mode=0o700, parents=True, exist_ok=True)
    if state_path.is_symlink() or state_path.stat().st_mode & 0o077:
        raise RuntimeError(f"state directory must be private (mode 0700): {state_path}")
    for directory in (state_path / ".tmp", state_path / ".cache" / "go-mod", state_path / ".cache" / "go-build"):
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)

    docker = _docker_binary()
    _probe_docker(docker)
    command = _container_args(argv, state_path)
    name = command[command.index("--name") + 1]
    process = None
    stopped = False
    received_signal = None

    def stop_once() -> None:
        nonlocal stopped
        if process is not None and not stopped:
            stopped = True
            _stop(docker, name)

    def interrupted(signum, _frame):
        nonlocal received_signal
        received_signal = signum
        stop_once()
        if process is not None and process.poll() is None:
            process.terminate()
        raise KeyboardInterrupt

    previous = {item: signal.getsignal(item) for item in (signal.SIGINT, signal.SIGTERM)}
    try:
        signal.signal(signal.SIGINT, interrupted)
        signal.signal(signal.SIGTERM, interrupted)
        process = subprocess.Popen([docker, *command])
        try:
            return process.wait()
        except KeyboardInterrupt:
            stop_once()
            if process.poll() is None:
                try:
                    process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            return 128 + (received_signal or signal.SIGINT)
    finally:
        signal.signal(signal.SIGINT, previous[signal.SIGINT])
        signal.signal(signal.SIGTERM, previous[signal.SIGTERM])
        stop_once()
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
