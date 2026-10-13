#!/usr/bin/env python3
"""Run bounded Go checks without retaining shared caches or build artifacts."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--live-codex", action="store_true",
                        help="Use existing authorized Codex auth for the live fixture")
    parser.add_argument("--codex-capabilities", action="store_true",
                        help="Use installed Codex with a credential-free local provider")
    parser.add_argument("--install-codex", action="store_true",
                        help="Install tested Codex into disposable storage for the fixture")
    parser.add_argument("--report", type=Path,
                        help="Optional report path outside the tracked source tree")
    args = parser.parse_args()
    if args.install_codex and not args.codex_capabilities:
        parser.error("--install-codex requires --codex-capabilities")
    root = Path(__file__).resolve().parents[1]
    commands = [["go", "test", "./...", "-count=1"],
                ["go", "test", "-race", "./...", "-count=1"],
                ["go", "vet", "./..."]]
    if args.live_codex:
        commands.append(["go", "test", "./internal/codex", "-run",
                         "TestLiveCodex", "-count=1", "-v"])
    if args.codex_capabilities:
        commands.append(["go", "test", "./internal/codex", "-run",
                         "TestCodexCapabilityBoundary", "-count=1", "-v"])
    results = []
    with tempfile.TemporaryDirectory(prefix="reviewd-check-") as directory:
        env = os.environ.copy()
        env.update(GOCACHE=str(Path(directory)/"build"),
                   GOMODCACHE=str(Path(directory)/"modules"),
                   GOTMPDIR=str(Path(directory)/"tmp"), GOWORK="off")
        Path(env["GOTMPDIR"]).mkdir()
        if args.install_codex:
            command = ["npm", "install", "--prefix", str(Path(directory)/"codex"),
                       "--cache", str(Path(directory)/"npm-cache"),
                       "--ignore-scripts", "--no-audit", "--no-fund",
                       "@openai/codex@0.162.1"]
            print("Installing disposable Codex 0.162.1", flush=True)
            completed = subprocess.run(command, cwd=root, env=env)
            if completed.returncode:
                return completed.returncode
            env["PATH"] = str(Path(directory)/"codex"/"node_modules"/".bin")+os.pathsep+env["PATH"]
        for command in commands:
            current = env.copy()
            if "TestLiveCodex" in command:
                current["REVIEWD_LIVE_CODEX"] = "1"
            else:
                current.pop("REVIEWD_LIVE_CODEX", None)
            if "TestCodexCapabilityBoundary" in command:
                current["REVIEWD_CODEX_CAPABILITIES"] = "1"
            else:
                current.pop("REVIEWD_CODEX_CAPABILITIES", None)
            print("Running:", " ".join(command), flush=True)
            completed = subprocess.run(command, cwd=root, env=current)
            results.append({"command": command, "exit_code": completed.returncode})
            if completed.returncode:
                break
        if all(result["exit_code"] == 0 for result in results):
            for target in ["darwin/arm64", "darwin/amd64", "linux/amd64",
                           "linux/arm64"]:
                build_env = env.copy()
                build_env.update(GOOS=target.split("/")[0],
                                 GOARCH=target.split("/")[1], CGO_ENABLED="0")
                command = ["go", "build", "./..."]
                print("Building:", target, flush=True)
                completed = subprocess.run(command, cwd=root, env=build_env)
                results.append({"command": command, "target": target,
                                "exit_code": completed.returncode})
                if completed.returncode:
                    break
    if args.report:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps({"checks": results}, indent=2)+"\n")
    return 0 if results and all(result["exit_code"] == 0 for result in results) else 1


if __name__ == "__main__":
    sys.exit(main())
