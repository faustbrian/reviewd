"""Behavioral packaging checks; the fake Go boundary never runs provider code."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("package.py")


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="reviewd-package-test-")
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.repo = self.root / "source"
        self.repo.mkdir()
        self.git("init", "-q", "-b", "main", "--template=")
        (self.repo / "go.mod").write_text("module github.com/faustbrian/reviewd\n\ngo 1.26.0\n")
        (self.repo / "go.sum").write_text("")
        (self.repo / "README.md").write_text("Committed installation instructions.\n")
        (self.repo / "value.txt").write_text("committed payload")
        self.git("add", "go.mod", "go.sum", "README.md", "value.txt")
        self.git("commit", "-qm", "source")
        self.revision = self.git("rev-parse", "HEAD").strip()
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.licenses = self.root / "dependency"
        self.licenses.mkdir()
        (self.licenses / "LICENSE").write_text("Owned dependency license text.\n")
        self.goroot = self.root / "goroot"
        self.goroot.mkdir()
        (self.goroot / "LICENSE").write_text("Owned Go license text.\n")
        fake = self.tools / "go"
        fake.write_text("#!" + sys.executable + "\n" + '''
import json, os, pathlib, sys
args = sys.argv[1:]
with open(os.environ["FIXTURE_LOG"], "a") as log:
    log.write(json.dumps({"args": args, "cwd": str(pathlib.Path.cwd()),
        "cache": os.environ.get("GOCACHE"), "modules": os.environ.get("GOMODCACHE"),
        "temp": os.environ.get("GOTMPDIR")}) + "\\n")
if args == ["version"]:
    print("go version go1.26.0 darwin/arm64")
elif args == ["env", "GOROOT"]:
    print(os.environ["FIXTURE_GOROOT"])
elif args[:3] == ["list", "-deps", "-json"]:
    print(json.dumps({"ImportPath": "example.invalid/library/package",
        "Module": {"Path": "example.invalid/library", "Version": "v1.2.3"}}))
elif args[:3] == ["list", "-m", "-json"]:
    print(json.dumps({"Path": "github.com/faustbrian/reviewd", "Main": True}))
    print(json.dumps({"Path": "example.invalid/library", "Version": "v1.2.3",
        "Sum": "h1:owned-fixture", "Dir": os.environ["FIXTURE_DEPENDENCY"]}))
    print(json.dumps({"Path": "example.invalid/unused", "Version": "v1.0.0"}))
elif args and args[0] == "build":
    if os.environ.get("FIXTURE_BUILD_FAIL") == "1" or (os.environ.get("FIXTURE_HELPER_FAIL") == "1" and args[-1] == "./cmd/reviewd-command-host"):
        print("owned compiler failure", file=sys.stderr)
        sys.exit(1)
    output = pathlib.Path(args[args.index("-o") + 1])
    data = pathlib.Path("value.txt").read_bytes()
    suffix = b":helper" if args[-1] == "./cmd/reviewd-command-host" else b""
    output.write_bytes(b"owned-binary:" + data + b":" + os.environ["GOOS"].encode()
        + b":" + os.environ["GOARCH"].encode() + suffix)
    output.chmod(0o755)
else:
    raise SystemExit("unexpected fake Go command")
''')
        fake.chmod(0o755)
        self.log = self.root / "go-log.jsonl"
        self.env = os.environ.copy()
        self.env.update(PATH=str(self.tools) + os.pathsep + self.env["PATH"],
                        FIXTURE_LOG=str(self.log), FIXTURE_GOROOT=str(self.goroot),
                        FIXTURE_DEPENDENCY=str(self.licenses))

    def git(self, *args):
        env = os.environ.copy()
        env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null",
                   GIT_AUTHOR_NAME="Fixture", GIT_AUTHOR_EMAIL="fixture@example.invalid",
                   GIT_COMMITTER_NAME="Fixture", GIT_COMMITTER_EMAIL="fixture@example.invalid")
        result = subprocess.run(["git", *args], cwd=self.repo, env=env,
                                text=True, capture_output=True, check=True)
        return result.stdout

    def package(self, output, *extra):
        return subprocess.run([sys.executable, str(SCRIPT), "--source", str(self.repo),
                               "--output", str(output), "--target", "linux/amd64",
                               *extra], env=self.env, text=True, capture_output=True)

    def test_packages_immutable_revision_with_checksums_licenses_and_clean_caches(self):
        # Dirty and untracked source must not silently enter a commit-labelled binary.
        (self.repo / "value.txt").write_text("dirty payload")
        (self.repo / "private.txt").write_text("never distribute this")
        # Local Git archive attributes must not alter the selected commit.
        (self.repo / ".git" / "info").mkdir(exist_ok=True)
        (self.repo / ".git" / "info" / "attributes").write_text("value.txt export-ignore\n")
        output = self.root / "output"
        result = self.package(output)
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest = json.loads((output / "manifest.json").read_text())
        self.assertEqual(manifest["revision"], self.revision)
        self.assertEqual(manifest["tree"], self.git("rev-parse", "HEAD^{tree}").strip())
        self.assertEqual(manifest["go_version"], "go1.26.0")
        self.assertEqual([a["target"] for a in manifest["artifacts"]], ["linux/amd64"])
        artifact = manifest["artifacts"][0]
        archive = output / artifact["file"]
        self.assertEqual(hashlib.sha256(archive.read_bytes()).hexdigest(), artifact["sha256"])
        self.assertIn(artifact["sha256"] + "  " + archive.name,
                      (output / "SHA256SUMS").read_text())
        with tarfile.open(archive) as bundle:
            self.assertNotIn("private.txt", bundle.getnames())
            self.assertEqual(bundle.extractfile("reviewd").read(),
                             b"owned-binary:committed payload:linux:amd64")
            self.assertEqual(bundle.getmember("reviewd").mode, 0o755)
            helper = bundle.extractfile("reviewd-command-host").read()
            self.assertEqual(helper,b"owned-binary:committed payload:linux:amd64:helper")
            self.assertEqual(bundle.getmember("reviewd-command-host").mode,0o755)
            helper_metadata=json.loads(bundle.extractfile("BUILD.json").read())["container_helper"]
            self.assertEqual(helper_metadata,{"file":"reviewd-command-host","target":"linux/amd64","protocol":1,"sha256":hashlib.sha256(helper).hexdigest()})
            self.assertEqual(artifact["container_helper"],helper_metadata)
            self.assertTrue(all(m.uid == 0 and m.gid == 0 and m.mtime == 0 for m in bundle.getmembers()))
            licenses = bundle.extractfile("THIRD_PARTY_NOTICES.txt").read().decode()
            self.assertIn("Owned dependency license text.", licenses)
            self.assertIn("Owned Go license text.", licenses)
            self.assertNotIn(str(self.root), licenses)
        records = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertTrue(records)
        for record in records:
            for key in ["cache", "modules", "temp"]:
                self.assertTrue(record[key])
                self.assertFalse(Path(record[key]).exists(), record[key])
        self.assertEqual((self.repo / "value.txt").read_text(), "dirty payload")

    def test_darwin_packages_pair_native_cli_with_linux_guest_helper(self):
        output=self.root/"darwin-pair"
        result=self.package(output,"--target","darwin/arm64")
        self.assertEqual(result.returncode,0,result.stderr)
        metadata=json.loads((output/"manifest.json").read_text())
        artifact=next(item for item in metadata["artifacts"] if item["target"]=="darwin/arm64")
        with tarfile.open(output/artifact["file"]) as bundle:
            self.assertEqual(bundle.extractfile("reviewd").read(),b"owned-binary:committed payload:darwin:arm64")
            helper=bundle.extractfile("reviewd-command-host").read()
            self.assertEqual(helper,b"owned-binary:committed payload:linux:arm64:helper")
            self.assertEqual(artifact["container_helper"]["target"],"linux/arm64")
            self.assertEqual(artifact["container_helper"]["sha256"],hashlib.sha256(helper).hexdigest())

    def test_helper_build_failure_never_publishes_partial_package(self):
        self.env["FIXTURE_HELPER_FAIL"]="1"
        output=self.root/"helper-failure"
        result=self.package(output)
        self.assertNotEqual(result.returncode,0)
        self.assertFalse(output.exists())
        for record in map(json.loads,self.log.read_text().splitlines()):
            for key in ("cache","modules","temp"):
                self.assertFalse(Path(record[key]).exists())

    def test_identical_inputs_produce_identical_archives_and_do_not_overwrite(self):
        first, second = self.root / "first", self.root / "second"
        for output in [first, second]:
            result = self.package(output)
            self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual({p.name: p.read_bytes() for p in first.iterdir()},
                         {p.name: p.read_bytes() for p in second.iterdir()})
        before = {p.name: p.read_bytes() for p in first.iterdir()}
        result = self.package(first)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(before, {p.name: p.read_bytes() for p in first.iterdir()})

    def test_build_failure_and_unsupported_target_leave_no_artifacts(self):
        output = self.root / "failed"
        self.env["FIXTURE_BUILD_FAIL"] = "1"
        result = self.package(output)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("owned compiler failure", result.stderr)
        self.assertFalse(output.exists() and any(output.iterdir()))
        self.env.pop("FIXTURE_BUILD_FAIL")
        result = self.package(output, "--target", "windows/amd64")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("windows/amd64", result.stderr)
        self.assertFalse(output.exists() and any(output.iterdir()))

    def test_explicit_older_revision_does_not_change_the_checkout(self):
        original = self.revision
        (self.repo / "value.txt").write_text("later commit")
        self.git("add", "value.txt")
        self.git("commit", "-qm", "later")
        current = self.git("rev-parse", "HEAD")
        output = self.root / "older"
        result = self.package(output, "--revision", original)
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest = json.loads((output / "manifest.json").read_text())
        self.assertEqual(manifest["revision"], original)
        with tarfile.open(output / manifest["artifacts"][0]["file"]) as bundle:
            self.assertIn(b"committed payload", bundle.extractfile("reviewd").read())
        self.assertEqual(self.git("rev-parse", "HEAD"), current)
        self.assertEqual((self.repo / "value.txt").read_text(), "later commit")

    def test_relocated_toolchain_retains_go_license(self):
        # Homebrew keeps LICENSE next to libexec rather than inside GOROOT.
        relocated = self.goroot / "libexec"
        relocated.mkdir()
        self.env["FIXTURE_GOROOT"] = str(relocated)
        result = self.package(self.root / "relocated")
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
