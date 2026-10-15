"""Fetch and verify the public RP1 validator release.

The installer deliberately treats the signed checksum file as the trust root
for the four release assets.  It never runs a downloaded file; the returned
binary is only made executable after the archive and provenance have passed all
checks.
"""

from __future__ import annotations

import hashlib
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


VERSION = "0.22.0"
ARCHIVE_NAME = "rp1d_linux_amd64.tar.gz"
ARCHIVE_SHA256 = "a68ddf9e7f8403d79d169ebf24cca63026ac50b6ef6067815eeca085f247ec56"
RELEASE_MANIFEST_SHA256 = "f31ced8eeb0ab944400af744ae85d422e57f5b4c767c9e4f33f93058fc7a9fdf"
RELEASE_BASE_URL = f"https://github.com/RP-ONE/rp1-releases/releases/download/{VERSION}"
ASSET_DIRNAME = "release-assets"
_ASSET_NAMES = ("LICENSE.txt", "PROVENANCE.json", "THIRD_PARTY_NOTICES.txt", ARCHIVE_NAME, "SHA256SUMS", "SHA256SUMS.bundle")
_MAX_ASSET_BYTES = 512 * 1024 * 1024
_MAX_BINARY_BYTES = 256 * 1024 * 1024
_TIMEOUT_SECONDS = 30


class ReleaseError(RuntimeError):
    """Raised when a release cannot be downloaded or verified."""


def _download(name: str) -> bytes:
    request = Request(f"{RELEASE_BASE_URL}/{name}", headers={"User-Agent": "rp1-benchmark-release/1"})
    try:
        with urlopen(request, timeout=_TIMEOUT_SECONDS) as response:
            length = response.headers.get("Content-Length")
            if length and int(length) > _MAX_ASSET_BYTES:
                raise ReleaseError(f"release asset {name} is too large")
            chunks: list[bytes] = []
            total = 0
            while True:
                chunk = response.read(1024 * 1024)
                if not chunk:
                    break
                total += len(chunk)
                if total > _MAX_ASSET_BYTES:
                    raise ReleaseError(f"release asset {name} exceeded size limit")
                chunks.append(chunk)
            return b"".join(chunks)
    except (HTTPError, URLError, TimeoutError, OSError, ValueError) as exc:
        if isinstance(exc, ReleaseError):
            raise
        raise ReleaseError(f"unable to download release asset {name}: {exc}") from exc


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _parse_manifest(data: bytes) -> dict[str, str]:
    if _sha256(data) != RELEASE_MANIFEST_SHA256:
        raise ReleaseError("SHA256SUMS failed its pinned digest check")
    try:
        lines = data.decode("utf-8").splitlines()
    except UnicodeDecodeError as exc:
        raise ReleaseError("SHA256SUMS is not UTF-8") from exc
    result: dict[str, str] = {}
    for line in lines:
        fields = line.split()
        if len(fields) != 2 or len(fields[0]) != 64 or any(c not in "0123456789abcdefABCDEF" for c in fields[0]):
            raise ReleaseError("SHA256SUMS contains a malformed entry")
        digest, name = fields[0].lower(), fields[1]
        if name in result:
            raise ReleaseError(f"SHA256SUMS contains duplicate entry {name}")
        if name not in ("LICENSE.txt", "PROVENANCE.json", "THIRD_PARTY_NOTICES.txt", ARCHIVE_NAME):
            raise ReleaseError(f"SHA256SUMS contains unexpected entry {name}")
        result[name] = digest
    expected = {"LICENSE.txt", "PROVENANCE.json", "THIRD_PARTY_NOTICES.txt", ARCHIVE_NAME}
    if set(result) != expected:
        raise ReleaseError("SHA256SUMS must contain exactly the four release assets")
    if result[ARCHIVE_NAME] != ARCHIVE_SHA256:
        raise ReleaseError("SHA256SUMS archive digest does not match the pinned release")
    return result


def _verify_provenance(data: bytes) -> None:
    try:
        document = json.loads(data)
        artifact = document["artifact"]
        source = document["source"]
    except (ValueError, TypeError, KeyError) as exc:
        raise ReleaseError("PROVENANCE.json is malformed") from exc
    if not isinstance(artifact, dict) or not isinstance(source, dict):
        raise ReleaseError("PROVENANCE.json has invalid sections")
    expected = {"name": ARCHIVE_NAME, "os": "linux", "architecture": "amd64", "sha256": ARCHIVE_SHA256}
    if any(artifact.get(key) != value for key, value in expected.items()):
        raise ReleaseError("PROVENANCE.json does not describe the pinned Linux amd64 archive")
    if source.get("tag") != VERSION:
        raise ReleaseError("PROVENANCE.json has the wrong release tag")


def _verify_assets(assets: dict[str, bytes]) -> None:
    checksums = _parse_manifest(assets["SHA256SUMS"])
    for name, expected in checksums.items():
        if _sha256(assets[name]) != expected:
            raise ReleaseError(f"release asset {name} failed its SHA256SUMS check")
    if not assets["SHA256SUMS.bundle"]:
        raise ReleaseError("SHA256SUMS.bundle is empty")
    try:
        bundle = json.loads(assets["SHA256SUMS.bundle"])
    except (ValueError, UnicodeDecodeError) as exc:
        raise ReleaseError("SHA256SUMS.bundle is not valid JSON") from exc
    if not isinstance(bundle, dict) or not bundle.get("mediaType", "").startswith("application/vnd.dev.sigstore.bundle"):
        raise ReleaseError("SHA256SUMS.bundle is not a Sigstore bundle")
    _verify_provenance(assets["PROVENANCE.json"])


def _read_cached_assets(asset_directory: Path) -> dict[str, bytes] | None:
    """Return a complete cache, or None when no complete cache is present.

    A complete but invalid cache is an error: silently replacing a tampered
    release bundle from the network would make offline operation ambiguous.
    """

    if not asset_directory.exists():
        return None
    if asset_directory.is_symlink() or not asset_directory.is_dir():
        raise ReleaseError("release-assets cache is not a directory")
    paths = {name: asset_directory / name for name in _ASSET_NAMES}
    if not all(path.exists() for path in paths.values()):
        return None
    if any(path.is_symlink() or not path.is_file() for path in paths.values()):
        raise ReleaseError("release-assets cache contains a non-regular file")
    try:
        assets = {name: path.read_bytes() for name, path in paths.items()}
        _verify_assets(assets)
        _extract_binary(assets[ARCHIVE_NAME])
        return assets
    except OSError as exc:
        raise ReleaseError(f"unable to read release-assets cache: {exc}") from exc


def _extract_binary(archive: bytes) -> bytes:
    if len(archive) > _MAX_ASSET_BYTES:
        raise ReleaseError("release archive is too large")
    try:
        with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as tar:
            members = tar.getmembers()
            if len(members) != 1 or members[0].name != "rp1d":
                raise ReleaseError("release archive must contain exactly one rp1d entry")
            member = members[0]
            if not member.isreg() or member.size <= 0 or member.size > _MAX_BINARY_BYTES:
                raise ReleaseError("release archive rp1d entry is not a regular bounded file")
            stream = tar.extractfile(member)
            if stream is None:
                raise ReleaseError("release archive rp1d entry cannot be read")
            binary = stream.read(_MAX_BINARY_BYTES + 1)
            if len(binary) != member.size:
                raise ReleaseError("release archive rp1d entry was truncated")
            return binary
    except (tarfile.TarError, OSError) as exc:
        if isinstance(exc, ReleaseError):
            raise
        raise ReleaseError(f"unable to read release archive: {exc}") from exc


def _atomic_write(path: Path, data: bytes, mode: int | None = None) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        if mode is not None:
            os.chmod(temporary, mode)
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def install_release(directory: Path) -> Path:
    """Install and return the verified ``rp1d`` path under *directory*."""

    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    if not directory.is_dir():
        raise ReleaseError(f"install directory is not a directory: {directory}")

    asset_directory = directory / ASSET_DIRNAME
    assets = _read_cached_assets(asset_directory)
    if assets is None:
        # Fetch and pin the checksum manifest before downloading the archive.
        sums = _download("SHA256SUMS")
        _parse_manifest(sums)
        assets = {"SHA256SUMS": sums}
        for name in _ASSET_NAMES:
            if name != "SHA256SUMS":
                assets[name] = _download(name)
        _verify_assets(assets)
    binary = _extract_binary(assets[ARCHIVE_NAME])

    if asset_directory.is_symlink():
        raise ReleaseError("release-assets cache is a symlink")
    asset_directory.mkdir(exist_ok=True)
    for name, data in assets.items():
        _atomic_write(asset_directory / name, data)
    executable = directory / "rp1d"
    if executable.is_file() and not executable.is_symlink():
        try:
            if _sha256(executable.read_bytes()) == _sha256(binary):
                os.chmod(executable, 0o755)
                return executable
        except OSError:
            pass
    _atomic_write(executable, binary, 0o755)
    return executable
