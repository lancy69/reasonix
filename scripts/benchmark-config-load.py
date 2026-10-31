#!/usr/bin/env python3
"""Measure identical public config loads without production instrumentation."""

import argparse
import hashlib
import json
import os
import platform
from pathlib import Path
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import time


BASELINE = "5f372e2229826a00b3a368ec1a1da9d1a1782147"
HARNESS = Path("internal/contract/config/path_resolution_benchmark_test.go")
BENCHMARK = re.compile(
    r"^(BenchmarkConfigLoad(Warm|First)/(typical|large-tree|large-config)/(readonly|runtime))-\d+\s+"
    r"(\d+)\s+([\d.]+)\s+ns/op(?:\s|$)"
)


def execute(command, cwd, env, log, timeout=1200):
    started = time.monotonic()
    result = subprocess.run(command, cwd=cwd, env=env, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            timeout=timeout, check=False)
    log.write_text(result.stdout, encoding="utf-8")
    if result.returncode:
        raise RuntimeError(f"command failed ({result.returncode}); see {log}: {command}")
    return result.stdout, time.monotonic() - started


def revision(source, expected):
    top = subprocess.run(["git", "-C", str(source), "rev-parse", "--show-toplevel"],
                         capture_output=True, text=True, check=False)
    actual = None
    if top.returncode == 0 and Path(top.stdout.strip()).resolve() == source:
        actual = subprocess.check_output(
            ["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()
        if actual != expected:
            raise RuntimeError(f"{source}: expected {expected}, found {actual}")
    return {"expected_revision": expected, "git_head": actual,
            "provenance": "git-checkout" if actual else "caller-supplied-immutable-archive"}


def parse_samples(text, label, log, sequence):
    samples = []
    for line in text.splitlines():
        match = BENCHMARK.match(line)
        if match:
            _, phase, profile, mode, iterations, elapsed = match.groups()
            allocated = re.search(r"(\d+)\s+B/op(?:\s|$)", line)
            allocations = re.search(r"(\d+)\s+allocs/op(?:\s|$)", line)
            if not allocated or not allocations:
                raise RuntimeError(f"missing allocation metrics in {log}: {line}")
            samples.append({"label": label, "phase": phase.lower(), "profile": profile,
                            "mode": mode, "iterations": int(iterations),
                            "ns_per_op": float(elapsed), "log": log.name,
                            "bytes_per_op": int(allocated.group(1)),
                            "allocs_per_op": int(allocations.group(1)),
                            "sequence": sequence})
    return samples


def distribution(values):
    quartiles = statistics.quantiles(values, n=4, method="inclusive") if len(values) > 1 else values * 3
    return {"n": len(values), "median_ns": statistics.median(values),
            "mean_ns": statistics.mean(values), "min_ns": min(values), "max_ns": max(values),
            "q1_ns": quartiles[0], "q3_ns": quartiles[2],
            "stdev_ns": statistics.stdev(values) if len(values) > 1 else 0}


def summarize(samples):
    groups = {}
    for sample in samples:
        key = (sample["phase"], sample["profile"], sample["mode"])
        groups.setdefault(key, {}).setdefault(sample["label"], []).append(sample)
    summary = []
    for (phase, profile, mode), values in sorted(groups.items()):
        before_rows, after_rows = values["before"], values["after"]
        before = distribution([row["ns_per_op"] for row in before_rows])
        after = distribution([row["ns_per_op"] for row in after_rows])
        pairs = list(zip(before_rows, after_rows, strict=True))
        summary.append({"phase": phase, "profile": profile, "mode": mode,
                        "before": before, "after": after,
                        "median_delta_ms": (after["median_ns"] - before["median_ns"]) / 1e6,
                        "median_change_percent": 100 * (after["median_ns"] / before["median_ns"] - 1),
                        "paired_faster": sum(right["ns_per_op"] < left["ns_per_op"] for left, right in pairs),
                        "paired_count": len(pairs),
                        "before_allocs_per_op_median": statistics.median(row["allocs_per_op"] for row in before_rows),
                        "after_allocs_per_op_median": statistics.median(row["allocs_per_op"] for row in after_rows),
                        "before_bytes_per_op_median": statistics.median(row["bytes_per_op"] for row in before_rows),
                        "after_bytes_per_op_median": statistics.median(row["bytes_per_op"] for row in after_rows)})
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--before", type=Path, required=True)
    parser.add_argument("--after", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--before-revision", default=BASELINE)
    parser.add_argument("--after-revision", required=True)
    parser.add_argument("--warm-samples", type=int, default=10)
    parser.add_argument("--first-samples", type=int, default=10)
    parser.add_argument("--benchtime", default="1s")
    args = parser.parse_args()
    if min(args.warm_samples, args.first_samples) < 1:
        parser.error("sample counts must be positive")
    sources = {"before": args.before.resolve(), "after": args.after.resolve()}
    if sources["before"] == sources["after"]:
        parser.error("before and after must be isolated source directories")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    payload = Path(__file__).resolve().parents[1] / HARNESS
    harness = payload.read_bytes()
    metadata = {"platform": platform.platform(), "machine": platform.machine(),
                "processor": platform.processor(), "cpu_count": os.cpu_count(),
                "python": sys.version, "harness_sha256": hashlib.sha256(harness).hexdigest(),
                "arguments": vars(args) | {key: str(value) for key, value in vars(args).items() if isinstance(value, Path)},
                "runner": {key: os.environ.get(key) for key in
                           ("RUNNER_OS", "RUNNER_ARCH", "RUNNER_NAME", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT")},
                "definitions": {
                    "warm": "one untimed public load, then repeated public loads; fresh stat/content and per-load resolution",
                    "first": "one public load per fresh test process; fixture preparation and process startup excluded",
                    "filesystem": "same absolute fixture paths before/after; no OS/page-cache purge; NOT disk-cold",
                    "order": "before/after then after/before on successive samples; serial, no competing timed phases",
                    "counts": "Not collected: isolation of config-source resolution from program authority changes the old evalSymlinks hook population. No claim of fewer canonicalization requests; this optimization changes the backend per access, not request reuse.",
                    "exclusions": "fixture setup, compilation, process startup/cleanup, benchmark observer overhead, boot/model/network execution",
                    "profiles": "typical=100 files/depth4/2 providers+2 plugins per source/2 JSON MCP; large-tree=10000/depth12/same config; large-config=10000/depth12/64 providers+128 plugins per source/96 JSON MCP",
                    "workspace_state": "pre-approved through public Roots.ApproveWorkspacePrograms in separate prepare processes; supported api_key_env plus dummy .env keys; complete retained identities and zero diagnostics validated outside timing",
                    "cpu_profiles": "Separate diagnostic processes after all unobserved pairs; profiles include benchmark-process setup and validation. Cumulative sampled CPU percentages overlap and are not blocked wall time.",
                }, "sources": {}}
    env = os.environ.copy()
    env.update({"GOMAXPROCS": "2", "GOFLAGS": "-p=2"})
    metadata["go_version"] = subprocess.check_output(["go", "version"], text=True, env=env).strip()
    metadata["go_env"] = json.loads(subprocess.check_output(
        ["go", "env", "-json", "GOOS", "GOARCH", "GOVERSION", "GOTOOLCHAIN", "CGO_ENABLED"], text=True, env=env))
    (output / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n", encoding="utf-8")
    created = []
    samples, counts, commands = [], [], []
    binaries = {}
    try:
        for label, source in sources.items():
            expected = args.before_revision if label == "before" else args.after_revision
            metadata["sources"][label] = revision(source, expected)
            destination = source / HARNESS
            if destination.exists():
                if destination.read_bytes() != harness:
                    raise RuntimeError(f"refusing to overwrite different existing harness: {destination}")
            else:
                destination.write_bytes(harness)
                created.append(destination)
            binary = output / (label + (".test.exe" if os.name == "nt" else ".test"))
            binaries[label] = binary
            command = ["go", "test", "-c", "-o", str(binary), "./internal/contract/config"]
            _, elapsed = execute(command, source, env, output / f"compile-{label}.log")
            commands.append({"command": command, "label": label, "seconds_outside_measurement": elapsed})
        with tempfile.TemporaryDirectory(prefix="reasonix-config-measure-") as fixtures:
            env["REASONIX_CONFIG_BENCH_ROOT"] = fixtures
            metadata["fixture_root"] = fixtures
            for label in ("before", "after"):
                log = output / f"prepare-{label}.log"
                command = [str(binaries[label]), "-test.run=^TestConfigLoadPrepareFixtures$", "-test.v", "-test.count=1", "-test.timeout=10m"]
                _, elapsed = execute(command, sources[label], env, log)
                commands.append({"command": command, "label": label, "phase": "prepare", "log": log.name,
                                 "seconds_outside_measurement": elapsed})
            sequence = 0
            for index in range(args.warm_samples):
                order = ("before", "after") if index % 2 == 0 else ("after", "before")
                for label in order:
                    log = output / f"warm-{index:02d}-{label}.log"
                    command = [str(binaries[label]), "-test.run=^$", "-test.bench=^BenchmarkConfigLoadWarm$",
                               "-test.benchtime=" + args.benchtime, "-test.count=1", "-test.cpu=2", "-test.timeout=10m"]
                    text, _ = execute(command, sources[label], env, log)
                    rows = parse_samples(text, label, log, sequence)
                    if len(rows) != 6:
                        raise RuntimeError(f"expected six warm measurements in {log}, found {len(rows)}")
                    samples.extend(rows)
                    commands.append({"sequence": sequence, "command": command, "label": label, "log": log.name})
                    sequence += 1
            for index in range(args.first_samples):
                order = ("before", "after") if index % 2 == 0 else ("after", "before")
                for profile in ("typical", "large-tree", "large-config"):
                    for mode in ("readonly", "runtime"):
                        for label in order:
                            log = output / f"first-{index:02d}-{profile}-{mode}-{label}.log"
                            command = [str(binaries[label]), "-test.run=^$",
                                       f"-test.bench=^BenchmarkConfigLoadFirst$/^{profile}$/^{mode}$",
                                       "-test.benchtime=1x", "-test.count=1", "-test.cpu=2", "-test.timeout=10m"]
                            text, _ = execute(command, sources[label], env, log)
                            rows = parse_samples(text, label, log, sequence)
                            if len(rows) != 1 or rows[0]["iterations"] != 1:
                                raise RuntimeError(f"expected exactly one first-load iteration in {log}")
                            samples.extend(rows)
                            commands.append({"sequence": sequence, "command": command, "label": label, "log": log.name})
                            sequence += 1
            for label in ("before", "after"):
                for profile in ("typical", "large-tree"):
                    profile_path = output / f"cpu-{label}-{profile}.pprof"
                    log = output / f"cpu-{label}-{profile}.log"
                    command = [str(binaries[label]), "-test.run=^$",
                               f"-test.bench=^BenchmarkConfigLoadWarm$/^{profile}$/^readonly$",
                               "-test.benchtime=5s", "-test.count=1", "-test.cpu=2",
                               "-test.cpuprofile=" + str(profile_path), "-test.timeout=10m"]
                    execute(command, sources[label], env, log)
                    commands.append({"phase": "cpu-diagnostic", "command": command, "label": label, "log": log.name})
                    for suffix, flags in (("top", []), ("cumulative", ["-cum"])):
                        command = ["go", "tool", "pprof", "-top", "-nodecount=40", *flags, str(profile_path)]
                        execute(command, sources[label], env, output / f"cpu-{label}-{profile}-{suffix}.txt")
            result = {"metadata": metadata, "counts": counts, "samples": samples,
                      "commands": commands, "summary": summarize(samples)}
            (output / "results.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
            (output / "measurement_harness.go").write_bytes(harness)
            shutil.copyfile(Path(__file__), output / "benchmark-config-load.py")
            print(json.dumps(result["summary"], indent=2), flush=True)
    finally:
        for destination in created:
            destination.unlink()
        for binary in binaries.values():
            binary.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
