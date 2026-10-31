#!/usr/bin/env python3
"""Focused observational Windows follow-up; no timings, profiles, or product changes."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess

BASELINE = "be65b117e00c364e39c08bc0dece34b51fa5de71"
CANDIDATE = "a4d746b83c57374ff5e1b59122db8267dfd4c6d6"
PROBE = Path("internal/contract/config/path_resolution_followup_windows_test.go")


def execute(command, source, env, log):
    result = subprocess.run(command, cwd=source, env=env, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, timeout=1200, check=False)
    log.write_text(result.stdout, encoding="utf-8")
    if result.returncode:
        raise RuntimeError(f"command failed ({result.returncode}): see {log}")
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--before", type=Path, required=True)
    parser.add_argument("--after", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--compatibility-only", action="store_true")
    parser.add_argument("--cross-compile-windows", action="store_true")
    args = parser.parse_args()
    if not args.compatibility_only:
        parser.error("this focused driver requires --compatibility-only; original timed experiment remains immutable")
    if os.name != "nt" and not args.cross_compile_windows:
        parser.error("native Windows runtime required, or explicitly select compile-only --cross-compile-windows")
    sources = {"baseline": args.before.resolve(), "candidate": args.after.resolve()}
    if sources["baseline"] == sources["candidate"]:
        parser.error("isolated exact baseline/candidate sources required")
    root = Path(__file__).resolve().parents[1]
    data = (root / PROBE).read_bytes()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    env = os.environ.copy()
    env.update({"GOMAXPROCS": "2", "GOFLAGS": "-p=2"})
    native = os.name == "nt"
    if not native:
        env.update({"GOOS": "windows", "GOARCH": "amd64", "CGO_ENABLED": "0"})
    metadata = {"platform": platform.platform(), "go_version": subprocess.check_output(["go", "version"], env=env, text=True).strip(),
                "baseline": BASELINE, "candidate": CANDIDATE,
                "probe_sha256": hashlib.sha256(data).hexdigest(), "compatibility_runtime_executed": native,
                "mode": "compatibility-only", "timing_or_cpu_profiles_executed": False,
                "scope": "constructed filesystem states under test-user authority; not observed production incidents or equivalence proof",
                "runner": {key: os.environ.get(key) for key in ("RUNNER_OS", "RUNNER_ARCH", "RUNNER_NAME", "GITHUB_RUN_ID")}}
    (output / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    created, binaries, records = [], [], []
    try:
        for label, source in sources.items():
            top = subprocess.run(["git", "-C", str(source), "rev-parse", "--show-toplevel"], text=True, capture_output=True)
            if top.returncode == 0 and Path(top.stdout.strip()).resolve() == source:
                head = subprocess.check_output(["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()
                if head != (BASELINE if label == "baseline" else CANDIDATE):
                    raise RuntimeError("source head mismatch")
            path = source / PROBE
            if path.exists():
                if path.read_bytes() != data:
                    raise RuntimeError(f"refusing to overwrite different probe {path}")
            else:
                path.write_bytes(data)
                created.append(path)
            binary = output / f"{label}.test.exe"
            binaries.append(binary)
            execute(["go", "test", "-c", "-o", str(binary), "./internal/contract/config"], source, env, output / f"compile-{label}.log")
            if native:
                text = execute([str(binary), "-test.run=^TestConfigHandleBackendFollowup$", "-test.v", "-test.count=1", "-test.timeout=10m"], source, env, output / f"compatibility-followup-{label}.log")
                rows = [json.loads(match.group(1)) for match in re.finditer(r"CONFIG_HANDLE_FOLLOWUP (\{[^\n]+\})", text)]
                if not rows:
                    raise RuntimeError("no follow-up observations emitted")
                records.extend(dict(row, source_revision=label) for row in rows)
        result = {"metadata": metadata, "observations": records}
        (output / "results.json").write_text(json.dumps(result, indent=2) + "\n")
        (output / PROBE.name).write_bytes(data)
        shutil.copyfile(__file__, output / Path(__file__).name)
        print(json.dumps({"metadata": metadata, "observation_count": len(records)}, indent=2), flush=True)
    finally:
        for path in created:
            path.unlink()
        for path in binaries:
            path.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
