"""Release assembly and publication through a local, non-network gh fixture."""
import hashlib
import json
import os
import shutil
from pathlib import Path
import subprocess
import sys
import tarfile
import unittest

import test_package

SCRIPT = Path(__file__).with_name("release.py")


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.fixture = test_package.PackageTests(methodName="runTest")
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.root = self.fixture.root
        self.packages = self.root / "packages"
        result = subprocess.run([sys.executable, str(SCRIPT.with_name("package.py")),
                                 "--source", str(self.fixture.repo), "--output", str(self.packages),
                                 "--version", "v1.0.0"], env=self.fixture.env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.tree = self.fixture.git("rev-parse", "HEAD^{tree}").strip()
        self.output = self.root / "release"
        self.version = "v1.0.0"
        self.log = self.root / "gh.jsonl"
        fake = self.fixture.tools / "gh"
        fake.write_text("#!" + sys.executable + "\n" + '''
import json, os, sys
with open(os.environ["FIXTURE_GH_LOG"], "a") as log: log.write(json.dumps(sys.argv[1:])+"\\n")
if sys.argv[1] == "api": print(os.environ["FIXTURE_REVISION"])
elif os.environ.get("FIXTURE_UPLOAD_FAIL") == "1": sys.exit(17)
''')
        fake.chmod(0o755)
        self.env = self.fixture.env.copy()
        self.env.update(FIXTURE_GH_LOG=str(self.log), FIXTURE_REVISION=self.fixture.revision)

    def release(self, *extra):
        directories = self.packages if isinstance(self.packages, list) else [self.packages]
        return subprocess.run([sys.executable, str(SCRIPT), "--packages", *map(str, directories),
                               "--version", self.version, "--revision", self.fixture.revision,
                               "--tree", self.tree, "--output", str(self.output), *extra],
                              env=self.env, capture_output=True, text=True)

    def test_four_assets_are_bound_and_publish_uses_existing_tag(self):
        # Hosted artifact download produces four independent one-target dirs.
        manifest = json.loads((self.packages / "manifest.json").read_text())
        directories = []
        for row in manifest["artifacts"]:
            directory = self.root / row["target"].replace("/", "-")
            directory.mkdir()
            part = dict(manifest, artifacts=[row])
            (directory / "manifest.json").write_text(json.dumps(part))
            shutil.copyfile(self.packages / row["file"], directory / row["file"])
            directories.append(directory)
        self.packages = directories
        result = self.release("--publish")
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest = json.loads((self.output / "manifest.json").read_text())
        self.assertEqual({a["target"] for a in manifest["artifacts"]},
                         {"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"})
        self.assertEqual(len(manifest["artifacts"]), 4)
        for row in manifest["artifacts"]:
            archive = self.output / row["file"]
            self.assertEqual(hashlib.sha256(archive.read_bytes()).hexdigest(), row["sha256"])
            with tarfile.open(archive) as bundle:
                build = json.load(bundle.extractfile("BUILD.json"))
                self.assertEqual(build["revision"], self.fixture.revision)
                self.assertEqual(build["tree"], self.tree)
                self.assertEqual(build["version"], "v1.0.0")
                self.assertEqual(build["binary_sha256"], hashlib.sha256(bundle.extractfile("reviewd").read()).hexdigest())
        calls = [json.loads(line) for line in self.log.read_text().splitlines()]
        create = calls[-1]
        self.assertEqual(create[:3], ["release", "create", "v1.0.0"])
        self.assertIn("--verify-tag", create)
        self.assertIn("faustbrian/reviewd", create)
        self.assertEqual(sum(arg.endswith(".tar.gz") for arg in create), 4)
        self.assertIn(str(self.output / "manifest.json"), create)
        self.assertIn(str(self.output / "SHA256SUMS"), create)
        for line in (self.output / "SHA256SUMS").read_text().splitlines():
            digest, name = line.split("  ")
            self.assertEqual(digest, hashlib.sha256((self.output / name).read_bytes()).hexdigest())
        again = self.release()
        self.assertNotEqual(again.returncode, 0)

    def test_missing_target_and_corrupt_archive_never_publish(self):
        for scenario in ("missing", "hash", "version"):
            with self.subTest(scenario=scenario):
                manifest_path = self.packages / "manifest.json"
                original = manifest_path.read_bytes()
                manifest = json.loads(original)
                if scenario == "missing": manifest["artifacts"].pop()
                elif scenario == "hash": manifest["artifacts"][0]["sha256"] = "0"*64
                else: manifest["version"] = "v2.0.0"
                manifest_path.write_text(json.dumps(manifest))
                result = self.release("--publish")
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.log.exists())
                self.assertFalse(self.output.exists())
                manifest_path.write_bytes(original)

    def test_upload_failure_and_moved_tag_are_not_success(self):
        self.env["FIXTURE_REVISION"] = "0"*40
        result = self.release("--publish")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('"create"', self.log.read_text())
        # Use a fresh output; never overwrite the reviewable failed attempt.
        self.output = self.root / "retry"
        self.env["FIXTURE_REVISION"] = self.fixture.revision
        self.env["FIXTURE_UPLOAD_FAIL"] = "1"
        result = self.release("--publish")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("Published", result.stdout)

    def test_tag_versions_are_validated_before_publication(self):
        for index, tag in enumerate(("v1.0.0-.", "v1.0.0-alpha..1", "v1.0.0-01", "v1.0.0-rc.1")):
            with self.subTest(tag=tag):
                self.version = tag
                self.packages = self.root / ("tag-input-"+str(index))
                self.output = self.root / ("tag-output-"+str(index))
                self.log.unlink(missing_ok=True)
                built = subprocess.run([sys.executable, str(SCRIPT.with_name("package.py")),
                                        "--source", str(self.fixture.repo), "--output", str(self.packages),
                                        "--version", tag], env=self.fixture.env, capture_output=True, text=True)
                self.assertEqual(built.returncode, 0, built.stderr)
                result = self.release("--publish")
                if tag == "v1.0.0-rc.1":
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn('"--prerelease"', self.log.read_text())
                else:
                    self.assertNotEqual(result.returncode, 0, "malformed version was published: "+tag)
                    self.assertFalse(self.log.exists(), "malformed version reached GitHub")
