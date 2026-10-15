import gzip
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from scripts import release


class _Response:
    def __init__(self, data: bytes, fail: bool = False):
        self._stream = io.BytesIO(data)
        self._fail = fail
        self.headers = {"Content-Length": str(len(data))}

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False

    def read(self, size=-1):
        if self._fail:
            raise OSError("connection interrupted")
        return self._stream.read(size)


def _archive(entries):
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w:gz") as tar:
        for entry in entries:
            name, kind, data = entry
            if kind == "file":
                info = tarfile.TarInfo(name)
                info.size = len(data)
                info.mode = 0o755
                tar.addfile(info, io.BytesIO(data))
            elif kind == "symlink":
                info = tarfile.TarInfo(name)
                info.type = tarfile.SYMTYPE
                info.linkname = "rp1d"
                tar.addfile(info)
            else:
                info = tarfile.TarInfo(name)
                info.type = tarfile.DIRTYPE
                tar.addfile(info)
    return stream.getvalue()


class ReleaseInstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.original = {
            key: getattr(release, key)
            for key in ("RELEASE_BASE_URL", "RELEASE_MANIFEST_SHA256", "ARCHIVE_SHA256")
        }
        self.binary = b"rp1d test binary\n"
        self.archive = _archive([("rp1d", "file", self.binary)])
        self.assets = {
            "LICENSE.txt": b"Apache License\n",
            "PROVENANCE.json": b"",
            "THIRD_PARTY_NOTICES.txt": b"notices\n",
            release.ARCHIVE_NAME: self.archive,
            "SHA256SUMS": b"",
            "SHA256SUMS.bundle": json.dumps({"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json"}).encode(),
        }
        self.assets["PROVENANCE.json"] = json.dumps({
            "artifact": {"name": release.ARCHIVE_NAME, "os": "linux", "architecture": "amd64", "sha256": hashlib.sha256(self.archive).hexdigest()},
            "source": {"tag": release.VERSION},
        }).encode()
        lines = []
        for name in ("LICENSE.txt", "PROVENANCE.json", "THIRD_PARTY_NOTICES.txt", release.ARCHIVE_NAME):
            lines.append(f"{hashlib.sha256(self.assets[name]).hexdigest()}  {name}")
        self.assets["SHA256SUMS"] = "\n".join(lines).encode() + b"\n"
        release.RELEASE_BASE_URL = "https://fixture.invalid/0.22.0"
        release.RELEASE_MANIFEST_SHA256 = hashlib.sha256(self.assets["SHA256SUMS"]).hexdigest()
        release.ARCHIVE_SHA256 = hashlib.sha256(self.archive).hexdigest()

    def tearDown(self):
        for key, value in self.original.items():
            setattr(release, key, value)
        self.temp.cleanup()

    def _run(self, assets=None, failing=None):
        assets = assets or self.assets

        def open_url(_request, timeout):
            self.assertEqual(timeout, release._TIMEOUT_SECONDS)
            name = _request.full_url.rsplit("/", 1)[-1]
            if name == failing:
                return _Response(b"", fail=True)
            return _Response(assets[name])

        with patch.object(release, "urlopen", side_effect=open_url):
            return release.install_release(Path(self.temp.name))

    def test_install_verifies_all_assets_and_repairs_corrupt_cache(self):
        executable = self._run()
        self.assertEqual(executable.read_bytes(), self.binary)
        self.assertEqual(executable.stat().st_mode & 0o777, 0o755)
        (executable).write_bytes(b"corrupt")
        repaired = self._run()
        self.assertEqual(repaired, executable)
        self.assertEqual(repaired.read_bytes(), self.binary)
        self.assertEqual((Path(self.temp.name) / release.ASSET_DIRNAME / "PROVENANCE.json").read_bytes(), self.assets["PROVENANCE.json"])

    def test_complete_cache_is_used_when_network_is_down(self):
        executable = self._run()
        with patch.object(release, "urlopen", side_effect=AssertionError("network must not be used")):
            self.assertEqual(release.install_release(Path(self.temp.name)), executable)

    def test_tampered_complete_cache_fails_without_network(self):
        self._run()
        manifest = Path(self.temp.name) / release.ASSET_DIRNAME / "SHA256SUMS"
        manifest.write_bytes(manifest.read_bytes().replace(b"LICENSE.txt", b"BROKEN.txt"))
        with patch.object(release, "urlopen", side_effect=AssertionError("network must not be used")):
            with self.assertRaises(release.ReleaseError):
                release.install_release(Path(self.temp.name))

    def test_pinned_manifest_mismatch_is_rejected_before_install(self):
        release.RELEASE_MANIFEST_SHA256 = "0" * 64
        with self.assertRaisesRegex(release.ReleaseError, "pinned digest"):
            self._run()
        self.assertFalse((Path(self.temp.name) / "rp1d").exists())

    def test_corrupt_archive_digest_is_rejected(self):
        assets = dict(self.assets)
        assets[release.ARCHIVE_NAME] = b"not a gzip archive"
        with self.assertRaises(release.ReleaseError):
            self._run(assets)
        self.assertFalse((Path(self.temp.name) / "rp1d").exists())

    def test_archive_rejects_traversal_nonregular_and_multiple_entries(self):
        for entries in (
            [("../rp1d", "file", self.binary)],
            [("rp1d", "symlink", b"")],
            [("rp1d", "file", self.binary), ("extra", "file", b"x")],
        ):
            archive = _archive(entries)
            assets = dict(self.assets)
            assets[release.ARCHIVE_NAME] = archive
            assets["SHA256SUMS"] = "\n".join(
                f"{hashlib.sha256(assets[name]).hexdigest()}  {name}"
                for name in ("LICENSE.txt", "PROVENANCE.json", "THIRD_PARTY_NOTICES.txt", release.ARCHIVE_NAME)
            ).encode() + b"\n"
            release.RELEASE_MANIFEST_SHA256 = hashlib.sha256(assets["SHA256SUMS"]).hexdigest()
            release.ARCHIVE_SHA256 = hashlib.sha256(archive).hexdigest()
            provenance = json.loads(assets["PROVENANCE.json"])
            provenance["artifact"]["sha256"] = release.ARCHIVE_SHA256
            assets["PROVENANCE.json"] = json.dumps(provenance).encode()
            assets["SHA256SUMS"] = assets["SHA256SUMS"].replace(self.assets["PROVENANCE.json"], assets["PROVENANCE.json"])
            assets["SHA256SUMS"] = b"\n".join(
                f"{hashlib.sha256(assets[name]).hexdigest()}  {name}".encode()
                for name in ("LICENSE.txt", "PROVENANCE.json", "THIRD_PARTY_NOTICES.txt", release.ARCHIVE_NAME)
            ) + b"\n"
            release.RELEASE_MANIFEST_SHA256 = hashlib.sha256(assets["SHA256SUMS"]).hexdigest()
            with self.assertRaises(release.ReleaseError):
                self._run(assets)

    def test_provenance_mismatch_and_interrupted_download_never_publish(self):
        assets = dict(self.assets)
        provenance = json.loads(assets["PROVENANCE.json"])
        provenance["source"]["tag"] = "0.21.0"
        assets["PROVENANCE.json"] = json.dumps(provenance).encode()
        assets["SHA256SUMS"] = b"\n".join(
            f"{hashlib.sha256(assets[name]).hexdigest()}  {name}".encode()
            for name in ("LICENSE.txt", "PROVENANCE.json", "THIRD_PARTY_NOTICES.txt", release.ARCHIVE_NAME)
        ) + b"\n"
        release.RELEASE_MANIFEST_SHA256 = hashlib.sha256(assets["SHA256SUMS"]).hexdigest()
        with self.assertRaises(release.ReleaseError):
            self._run(assets)
        with self.assertRaises(release.ReleaseError):
            self._run(failing="rp1d_linux_amd64.tar.gz")
        self.assertFalse((Path(self.temp.name) / "rp1d").exists())


if __name__ == "__main__":
    unittest.main()
