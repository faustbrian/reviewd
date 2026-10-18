#!/usr/bin/env python3
"""Run Go quality checks and macOS/Linux target builds with disposable caches."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path,
                        help="Optional report outside the tracked source tree")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    results = []
    with tempfile.TemporaryDirectory(prefix="reviewd-check-") as directory:
        env = os.environ.copy()
        env.update(GOCACHE=str(Path(directory)/"build"),
                   GOMODCACHE=str(Path(directory)/"modules"),
                   GOTMPDIR=str(Path(directory)/"tmp"), GOWORK="off")
        Path(env["GOTMPDIR"]).mkdir()
        commands = [["go", "test", "-race", "./...", "-count=1"],
                    ["go", "vet", "./..."]]
        for command in commands:
            print("Running:", " ".join(command), flush=True)
            completed = subprocess.run(command, cwd=root, env=env)
            results.append({"command": command, "exit_code": completed.returncode})
            if completed.returncode:
                break
        if all(result["exit_code"] == 0 for result in results):
            for target in ("darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64"):
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
