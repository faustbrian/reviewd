#!/usr/bin/env python3
"""Package an immutable reviewd commit without shared caches or release writes."""

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import platform
import re
import subprocess
import sys
import tarfile
import tempfile


TARGETS = ("darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64")


def run(command, cwd, env, binary=False):
    result = subprocess.run(command, cwd=cwd, env=env, capture_output=True,
                            text=not binary)
    if result.returncode:
        detail = result.stderr.decode(errors="replace") if binary else result.stderr
        raise RuntimeError(detail.strip() or "Build command failed")
    return result.stdout


def encode(value):
    return (json.dumps(value, sort_keys=True, indent=2) + "\n").encode()


def export_source(source, revision, temporary, env):
    git = ["git", "-c", "core.attributesFile=/dev/null"]
    commit = run(git + ["rev-parse", "--verify", "--end-of-options",
                        revision + "^{commit}"], source, env).strip()
    # A fresh bare repository excludes local info/attributes, config, hooks and
    # dirty files from the archive. Only the selected immutable object is fetched.
    objects = temporary / "objects.git"
    run(git + ["init", "--bare", "--template=", str(objects)], temporary, env)
    run(git + ["-c", "protocol.file.allow=always", "-C", str(objects), "fetch",
               "--quiet", "--no-tags", "--no-recurse-submodules", "--depth=1",
               str(source), commit], temporary, env)
    tree = run(git + ["-C", str(objects), "rev-parse", commit + "^{tree}"], temporary, env).strip()
    archive = run(git + ["-C", str(objects), "archive", "--format=tar", commit],
                  temporary, env, binary=True)
    exported = temporary / "source"
    exported.mkdir()
    with tarfile.open(fileobj=io.BytesIO(archive)) as bundle:
        for member in bundle:
            name = PurePosixPath(member.name)
            if name.is_absolute() or ".." in name.parts or not (member.isdir() or member.isfile()):
                raise RuntimeError("Source archive contains an unsupported path or link")
            path = exported.joinpath(*name.parts)
            if member.isdir():
                path.mkdir(parents=True, exist_ok=True)
            else:
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(bundle.extractfile(member).read())
                path.chmod(0o755 if member.mode & 0o111 else 0o644)
    return exported, commit, tree


def json_values(raw):
    decoder = json.JSONDecoder()
    while raw.strip():
        value, end = decoder.raw_decode(raw.lstrip())
        raw = raw.lstrip()[end:]
        yield value


def modules_and_notices(source, env, targets):
    used = set()
    for target in targets:
        current = env.copy()
        current.update(GOOS=target.split("/")[0], GOARCH=target.split("/")[1])
        raw = run(["go", "list", "-deps", "-json", "-mod=readonly", "./cmd/reviewd", "./cmd/reviewd-command-host"], source, current)
        guest = current.copy()
        guest["GOOS"] = "linux"
        raw += "\n" + run(["go", "list", "-deps", "-json", "-mod=readonly",
                            "./cmd/reviewd-command-host"], source, guest)
        for package in json_values(raw):
            module = package.get("Module", {})
            if module and not module.get("Main"):
                used.add(module["Path"])
    raw = run(["go", "list", "-m", "-json", "all"], source, env)
    modules = []
    for module in json_values(raw):
        if module["Path"] in used:
            if module.get("Replace"):
                raise RuntimeError("Distribution requires immutable module versions, not replacements")
            modules.append(module)
    notices = []
    goroot = Path(run(["go", "env", "GOROOT"], source, env).strip())
    license_path = goroot / "LICENSE"
    if not license_path.is_file() and goroot.name == "libexec":
        license_path = goroot.parent / "LICENSE"
    if not license_path.is_file():
        raise RuntimeError("Go toolchain license is missing; use a complete Go installation")
    notices.append("Go toolchain\n============\n" + license_path.read_text())
    graph = []
    for module in sorted(modules, key=lambda item: item["Path"]):
        directory = Path(module["Dir"])
        files = sorted(path for path in directory.iterdir() if path.is_file()
                       and path.name.upper().startswith(("LICENSE", "COPYING", "NOTICE", "PATENTS")))
        if not files:
            raise RuntimeError("Dependency license is unavailable: " + module["Path"])
        graph.append({key.lower(): module[key] for key in ("Path", "Version", "Sum")})
        for path in files:
            notices.append(module["Path"] + "@" + module["Version"] + "/" + path.name
                           + "\n" + "=" * 40 + "\n" + path.read_text())
    return graph, ("\n\n".join(notices) + "\n").encode()


def archive_files(destination, files):
    with destination.open("wb") as raw:
        with gzip.GzipFile(fileobj=raw, filename="", mode="wb", mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as bundle:
                for name, data, mode in sorted(files):
                    member = tarfile.TarInfo(name)
                    member.size, member.mode, member.mtime = len(data), mode, 0
                    member.uid, member.gid = 0, 0
                    member.uname, member.gname = "", ""
                    bundle.addfile(member, io.BytesIO(data))


def package(args):
    output = args.output.absolute()
    if os.path.lexists(output):
        raise RuntimeError("Output already exists; choose a new directory (nothing was overwritten)")
    targets = args.target or list(TARGETS)
    if len(set(targets)) != len(targets):
        raise RuntimeError("Duplicate package target")
    host = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system())
    arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    if args.smoke and (not host or not arch):
        raise RuntimeError("Native smoke is supported only on macOS/Linux amd64/arm64")
    if args.smoke and targets != [host + "/" + arch]:
        raise RuntimeError("--smoke requires exactly the current native target")
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="reviewd-package-", dir=output.parent) as directory:
        temporary = Path(directory)
        env = os.environ.copy()
        env.update(GOCACHE=str(temporary / "cache"), GOMODCACHE=str(temporary / "modules"),
                   GOTMPDIR=str(temporary / "tmp"), GOWORK="off", GOTOOLCHAIN="local", GOENV="off",
                   CGO_ENABLED="0", GOFLAGS="", GOEXPERIMENT="", GOAMD64="v1", GOARM64="v8.0",
                   GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null", GIT_ATTR_NOSYSTEM="1")
        Path(env["GOTMPDIR"]).mkdir()
        source, revision, tree = export_source(args.source.resolve(), args.revision, temporary, env)
        version = args.version or "dev-" + revision[:12]
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+_-]{0,79}", version):
            raise RuntimeError("Version must be a short identifier without whitespace or paths")
        toolchain = run(["go", "version"], source, env).split()[2]
        before = {name: (source / name).read_bytes() for name in ("go.mod", "go.sum")}
        graph, notices = modules_and_notices(source, env, targets)
        staged = temporary / "artifacts"
        staged.mkdir()
        metadata = {"schema_version": 1, "repository": "github.com/faustbrian/reviewd",
                    "revision": revision, "tree": tree, "version": version,
                    "go_version": toolchain, "modules": graph, "artifacts": []}
        for target in targets:
            build_env = env.copy()
            build_env.update(GOOS=target.split("/")[0], GOARCH=target.split("/")[1])
            binary = temporary / "reviewd"
            flags = ["-trimpath", "-buildvcs=false", "-mod=readonly",
                     "-ldflags=-s -w -X main.version=" + version]
            run(["go", "build", *flags, "-o", str(binary), "./cmd/reviewd"], source, build_env)
            if args.smoke:
                actual = run([str(binary), "--version"], source, env).strip()
                if actual != "reviewd version " + version:
                    raise RuntimeError("Packaged binary reports an unexpected version")
                run([str(binary), "--help"], source, env)
                for shell in ("bash", "zsh", "fish"):
                    if not run([str(binary), "completion", shell], source, env).strip():
                        raise RuntimeError("Packaged binary produced empty shell completion")
            data = binary.read_bytes()
            helper_env = build_env.copy()
            helper_env["GOOS"] = "linux"
            helper_binary = temporary / "reviewd-command-host"
            run(["go", "build", *flags, "-o", str(helper_binary), "./cmd/reviewd-command-host"], source, helper_env)
            helper = helper_binary.read_bytes()
            helper_metadata = {"file": "reviewd-command-host", "target": "linux/" + target.split("/")[1],
                               "protocol": 1, "sha256": hashlib.sha256(helper).hexdigest()}
            if args.smoke and host == "linux":
                actual = run([str(helper_binary), "--version"], source, env).strip()
                if actual != "reviewd command host version " + version:
                    raise RuntimeError("Packaged container helper reports an unexpected version")
            build = {key: value for key, value in metadata.items() if key != "artifacts"}
            build.update(target=target, build_flags=flags,
                         binary_sha256=hashlib.sha256(data).hexdigest(), container_helper=helper_metadata)
            name = "reviewd_" + version + "_" + target.replace("/", "_") + ".tar.gz"
            archive_files(staged / name, [("reviewd", data, 0o755), ("reviewd-command-host", helper, 0o755), ("BUILD.json", encode(build), 0o644),
                                          ("README.md", (source / "README.md").read_bytes(), 0o644),
                                          ("THIRD_PARTY_NOTICES.txt", notices, 0o644)])
            metadata["artifacts"].append({"target": target, "file": name,
                "sha256": hashlib.sha256((staged / name).read_bytes()).hexdigest(),
                "binary_sha256": build["binary_sha256"], "container_helper": helper_metadata})
        if any((source / name).read_bytes() != value for name, value in before.items()):
            raise RuntimeError("Build changed immutable module metadata; fix dependencies before distribution")
        (staged / "manifest.json").write_bytes(encode(metadata))
        checksums = [hashlib.sha256(path.read_bytes()).hexdigest() + "  " + path.name
                     for path in sorted(staged.iterdir())]
        (staged / "SHA256SUMS").write_text("\n".join(checksums) + "\n")
        os.rename(staged, output)
    print("Packaged " + revision + " into " + str(output))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--revision", default="HEAD", help="Immutable commit to export; dirty files are excluded")
    parser.add_argument("--output", type=Path, required=True, help="New artifact directory; existing paths are never overwritten")
    parser.add_argument("--target", choices=TARGETS, action="append", help="Repeat for selected targets; default: all four")
    parser.add_argument("--version", help="Explicit binary label; default: dev-<commit>; does not create a release")
    parser.add_argument("--smoke", action="store_true", help="Run native version/help/completion checks")
    args = parser.parse_args()
    try:
        package(args)
    except (RuntimeError, OSError, ValueError, KeyError) as error:
        print(str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
