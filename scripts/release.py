#!/usr/bin/env python3
"""Assemble four immutable native packages; publish only with --publish."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile

from package import TARGETS, encode

REPOSITORY = "faustbrian/reviewd"


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def assemble(args):
    version = re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?", args.version)
    # SemVer 2.0.0 section 9: identifiers are nonempty, and numeric prerelease
    # identifiers have no leading zeros (https://semver.org/#spec-item-9).
    if version is None or (version[4] is not None and any(
            not part or (part.isdigit() and len(part) > 1 and part.startswith("0"))
            for part in version[4].split("."))):
        raise ValueError("Release tags must be vMAJOR.MINOR.PATCH with valid SemVer prerelease identifiers")
    if any(not re.fullmatch(r"[0-9a-f]{40}", value) for value in (args.revision, args.tree)):
        raise ValueError("Release source requires full immutable commit and tree IDs")
    output = args.output.absolute()
    if os.path.lexists(output):
        raise ValueError("Output already exists; nothing was overwritten")
    common = dict(repository="github.com/" + REPOSITORY, revision=args.revision,
                  tree=args.tree, version=args.version)
    artifacts, sources = [], {}
    for directory in args.packages:
        manifest = json.loads((directory / "manifest.json").read_text())
        if manifest.get("schema_version") != 1 or any(manifest.get(key) != value for key, value in common.items()):
            raise ValueError("Package source/version does not match selected release")
        for row in manifest["artifacts"]:
            target = row["target"]
            expected = "reviewd_" + args.version + "_" + target.replace("/", "_") + ".tar.gz"
            if target not in TARGETS or target in sources or row["file"] != expected:
                raise ValueError("Duplicate, unsupported or incorrectly named target archive")
            archive = directory / expected
            if archive.is_symlink() or digest(archive) != row["sha256"]:
                raise ValueError("Package archive checksum mismatch")
            with tarfile.open(archive) as bundle:
                members = bundle.getmembers()
                if {m.name for m in members} != {"reviewd", "BUILD.json", "README.md", "THIRD_PARTY_NOTICES.txt"} or len(members) != 4 or not all(m.isfile() for m in members):
                    raise ValueError("Unsupported package archive members")
                build = json.load(bundle.extractfile("BUILD.json"))
                binary_hash = hashlib.sha256(bundle.extractfile("reviewd").read()).hexdigest()
                if any(build.get(key) != value for key, value in common.items()) or build.get("target") != target:
                    raise ValueError("Archive BUILD metadata does not match release")
                if binary_hash != row["binary_sha256"] or binary_hash != build.get("binary_sha256") or bundle.getmember("reviewd").mode != 0o755:
                    raise ValueError("Archive native binary checksum/mode mismatch")
            artifacts.append(row)
            sources[target] = archive
    if set(sources) != set(TARGETS):
        raise ValueError("A release requires all four macOS/Linux native targets")
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="reviewd-release-", dir=output.parent) as temporary:
        staged = Path(temporary) / "artifacts"
        staged.mkdir()
        for row in artifacts:
            shutil.copyfile(sources[row["target"]], staged / row["file"])
        manifest = dict(schema_version=1, **common, artifacts=sorted(artifacts, key=lambda row: row["target"]))
        (staged / "manifest.json").write_bytes(encode(manifest))
        (staged / "SHA256SUMS").write_text("".join(digest(path) + "  " + path.name + "\n" for path in sorted(staged.iterdir())))
        os.rename(staged, output)
    return output


def publish(args, output):
    # --verify-tag prevents gh's normal implicit tag creation. Check the resolved
    # remote commit too: --target alone does not change an already existing tag.
    # Contract: `gh release create --help` (existing tags and draft/upload/publish).
    revision = subprocess.check_output(["gh", "api", "repos/" + REPOSITORY + "/commits/" + args.version,
                                        "--jq", ".sha"], text=True).strip()
    if revision != args.revision:
        raise ValueError("Remote tag moved or does not name the packaged commit")
    command = ["gh", "release", "create", args.version, *[str(path) for path in sorted(output.iterdir())],
               "--repo", REPOSITORY, "--verify-tag", "--target", args.revision,
               "--title", args.version, "--generate-notes"]
    if "-" in args.version:
        command.append("--prerelease")
    # gh stages a draft, uploads assets, then publishes. An upload failure is a
    # failed attempt, never a successful release or an invitation to overwrite it.
    subprocess.run(command, check=True)
    print("Published " + args.version)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--packages", type=Path, nargs="+", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--revision", required=True)
    parser.add_argument("--tree", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--publish", action="store_true", help="Create release for an existing remote tag")
    args = parser.parse_args()
    try:
        output = assemble(args)
        if args.publish:
            publish(args, output)
        else:
            print("Assembled release assets in " + str(output) + "; nothing published")
    except (ValueError, OSError, KeyError, tarfile.TarError, subprocess.SubprocessError) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
