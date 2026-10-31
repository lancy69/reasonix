#!/usr/bin/env python3
"""Static-fixture causal investigation only; never a production cache proposal."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import statistics
import subprocess
import sys
import tempfile

BASELINE = "be65b117e00c364e39c08bc0dece34b51fa5de71"
CANDIDATE = "a4d746b83c57374ff5e1b59122db8267dfd4c6d6"
FIXTURE_SHA = "ca9dbe8eb2aa91899b133a88b53a2b279ba336304036f53e1247a85698326185"
FIXTURE = Path("internal/contract/config/path_resolution_benchmark_test.go")
MECHANISM = Path("internal/contract/config/path_resolution_mechanism_test.go")
COMPATIBILITY = Path("internal/contract/config/path_resolution_mechanism_windows_test.go")
PATTERN = re.compile(r"^BenchmarkConfigMechanism(Warm|First)/([^/ ]+)/(readonly|runtime)-\d+\s+(\d+)\s+([\d.]+)\s+ns/op")
MICRO = re.compile(r"^BenchmarkConfigPathOperation/([^/ ]+)/(user|project)/([^/ ]+)-\d+\s+(\d+)\s+([\d.]+)\s+ns/op")


def execute(command, cwd, env, log, timeout=1800):
    result = subprocess.run(command, cwd=cwd, env=env, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, timeout=timeout, check=False)
    log.write_text(result.stdout, encoding="utf-8")
    if result.returncode:
        raise RuntimeError(f"exit={result.returncode} see {log}: {command}")
    return result.stdout


def stats(values):
    q = statistics.quantiles(values, n=4, method="inclusive") if len(values) > 1 else values * 3
    return {"n": len(values), "median_ns": statistics.median(values), "mean_ns": statistics.mean(values),
            "min_ns": min(values), "max_ns": max(values), "q1_ns": q[0], "q3_ns": q[2]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--before", required=True, type=Path)
    parser.add_argument("--after", required=True, type=Path)
    parser.add_argument("--fixture-harness", type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--samples", type=int, default=10)
    parser.add_argument("--benchtime", default="1s")
    parser.add_argument("--profile-seconds", default="5s")
    parser.add_argument("--micro-samples", type=int, default=5)
    parser.add_argument("--micro-benchtime", default="200ms")
    parser.add_argument("--include-large-config", action="store_true")
    parser.add_argument("--include-handle-control", action="store_true")
    parser.add_argument("--cross-compile-windows", action="store_true")
    args = parser.parse_args()
    if min(args.samples, args.micro_samples) < 1:
        parser.error("sample counts must be positive")
    root = Path(__file__).resolve().parents[1]
    fixture = (args.fixture_harness or (root / FIXTURE)).resolve()
    fixture_bytes = fixture.read_bytes()
    if hashlib.sha256(fixture_bytes).hexdigest() != FIXTURE_SHA:
        raise RuntimeError("admitted fixture harness changed")
    payload = {FIXTURE: fixture_bytes, MECHANISM: (root / MECHANISM).read_bytes()}
    if (root / COMPATIBILITY).exists():
        payload[COMPATIBILITY] = (root / COMPATIBILITY).read_bytes()
    sources = {"baseline": args.before.resolve(), "candidate": args.after.resolve()}
    if sources["baseline"] == sources["candidate"]:
        parser.error("isolated baseline/candidate source directories required")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    env = os.environ.copy()
    env.update({"GOMAXPROCS": "2", "GOFLAGS": "-p=2"})
    env.pop("REASONIX_CONFIG_CPU_PROFILE", None)
    profiles = ["typical", "large-tree"] + (["large-config"] if args.include_large_config else [])
    variants = [("stock", "baseline", "stock"), ("backend-memo", "baseline", "backend-memo"),
                ("full-a4", "candidate", "stock")]
    if os.name == "nt" or args.include_handle_control:
        variants.append(("handle-backend", "baseline", "handle-backend"))
    metadata = {"platform": platform.platform(), "machine": platform.machine(), "cpu_count": os.cpu_count(),
                "python": sys.version, "baseline": BASELINE, "candidate": CANDIDATE,
                "go_version": subprocess.check_output(["go", "version"], env=env, text=True).strip(),
                "go_env": json.loads(subprocess.check_output(["go", "env", "-json", "GOOS", "GOARCH", "GOVERSION", "CGO_ENABLED", "GOTOOLCHAIN"], env=env, text=True)),
                "runner": {key: os.environ.get(key) for key in ("RUNNER_OS", "RUNNER_ARCH", "RUNNER_NAME", "GITHUB_RUN_ID")},
                "hashes": {str(path): hashlib.sha256(data).hexdigest() for path, data in payload.items()},
                "samples_per_cell": args.samples, "profiles": profiles,
                "warning": "BACKEND_MEMO IS UNSAFE AS A PRODUCTION FIX; unchanged static fixtures only; no security equivalence is established",
                "definitions": {
                    "stock": "exact baseline with unchanged callers/guards/stat/absolute reads and original filepath.EvalSymlinks",
                    "backend-memo": "exact baseline, replacing only existing evalSymlinks backend hook for one public call; fresh memo each load, filepath.EvalSymlinks on misses",
                    "handle-backend": "exact baseline, existing fileutil.ResolveExistingPath at every backend access; original filepath backend on helper errors; no memo; non-Windows helper is identical filepath.EvalSymlinks",
                    "full-a4": "exact published candidate production implementation; no experimental backend replacement",
                    "fixture": "unchanged admitted ca9d harness; real public approvals prepared in separate processes; all retained identities and zero diagnostics asserted outside timers",
                    "timing": "warm1s or one load per fresh first-process; setup/startup/cleanup excluded; same physical paths; no disk-cold claim; no duration observer during timing",
                    "order": "cyclic order and its reverse on successive samples; paired reversal balances before/after positions; variants serial",
                    "attribution": "separate observer-inclusive public-load walltime and inclusive callback/backend durations by user_file/project_file/root_other; observer overhead is not a speedup estimate",
                    "cpu": "CPU collection starts after fixture preparation and preload, stops before validation; CPU samples cannot account for blocked syscall walltime",
                    "micro": "real isolated backend/wrapper/Lstat/Stat/read/available Root operations; not public-load timing or a security proof",
                }}
    (output / "metadata.json").write_text(json.dumps(metadata, indent=2) + "\n")
    created, binaries, samples, attribution, micro = [], {}, [], [], []
    sequence = 0
    try:
        for label, source in sources.items():
            top = subprocess.run(["git", "-C", str(source), "rev-parse", "--show-toplevel"], capture_output=True, text=True)
            if top.returncode == 0 and Path(top.stdout.strip()).resolve() == source:
                head = subprocess.check_output(["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()
                if head != (BASELINE if label == "baseline" else CANDIDATE):
                    raise RuntimeError("source head mismatch")
            for relative, data in payload.items():
                path = source / relative
                if path.exists():
                    if path.read_bytes() != data:
                        raise RuntimeError(f"refuse overwrite of differing evidence harness {path}")
                else:
                    path.write_bytes(data)
                    created.append(path)
            binary = output / (label + (".test.exe" if os.name == "nt" else ".test"))
            binaries[label] = binary
            execute(["go", "test", "-c", "-o", str(binary), "./internal/contract/config"], source, env, output / f"compile-{label}.log")
            if args.cross_compile_windows and os.name != "nt":
                cross = output / f"{label}-windows-amd64.test.exe"
                execute(["go", "test", "-c", "-o", str(cross), "./internal/contract/config"],
                        source, env | {"GOOS": "windows", "GOARCH": "amd64"}, output / f"compile-windows-{label}.log")
                cross.unlink()
        with tempfile.TemporaryDirectory(prefix="reasonix-mechanism-") as fixtures:
            env["REASONIX_CONFIG_BENCH_ROOT"] = fixtures
            metadata["fixture_root"] = fixtures
            for label, source in sources.items():
                env["REASONIX_CONFIG_MECHANISM_VARIANT"] = "stock"
                execute([str(binaries[label]), "-test.run=^TestConfigLoadPrepareFixtures$", "-test.v", "-test.count=1", "-test.timeout=10m"], source, env, output / f"prepare-{label}.log")
            orders = []
            for shift in range(len(variants)):
                rotated = variants[shift:] + variants[:shift]
                orders.extend([rotated, list(reversed(rotated))])
            for phase in ("warm", "first"):
                for index in range(args.samples):
                    order = orders[index % len(orders)]
                    for profile in profiles:
                        for mode in ("readonly", "runtime"):
                            for label, source_label, variant in order:
                                local_env = env | {"REASONIX_CONFIG_MECHANISM_VARIANT": variant}
                                log = output / f"{phase}-{index:02d}-{profile}-{mode}-{label}.log"
                                bench = "Warm" if phase == "warm" else "First"
                                text = execute([str(binaries[source_label]), "-test.run=^$",
                                                f"-test.bench=^BenchmarkConfigMechanism{bench}$/^{profile}$/^{mode}$",
                                                "-test.benchtime=" + (args.benchtime if phase == "warm" else "1x"),
                                                "-test.cpu=2", "-test.count=1", "-test.timeout=10m"],
                                               sources[source_label], local_env, log)
                                matches = [PATTERN.match(line) for line in text.splitlines() if PATTERN.match(line)]
                                if len(matches) != 1:
                                    raise RuntimeError(f"invalid measurement record {log}")
                                match = matches[0]
                                _, _, _, iterations, elapsed = match.groups()
                                if phase == "first" and int(iterations) != 1:
                                    raise RuntimeError("first process performed more than one timed load")
                                samples.append({"label": label, "phase": phase, "profile": profile, "mode": mode,
                                                "sample": index, "sequence": sequence, "iterations": int(iterations),
                                                "ns_per_op": float(elapsed), "log": log.name})
                                sequence += 1
            for label, source_label, variant in variants:
                local_env = env | {"REASONIX_CONFIG_MECHANISM_VARIANT": variant}
                for profile in profiles:
                    log = output / f"attribution-{profile}-{label}.log"
                    text = execute([str(binaries[source_label]), f"-test.run=^TestConfigMechanismAttribution$/^{profile}$",
                                    "-test.v", f"-test.count={args.samples}", "-test.timeout=10m"], sources[source_label], local_env, log)
                    records = [json.loads(match.group(1)) for match in re.finditer(r"CONFIG_MECHANISM_ATTRIBUTION (\{[^\n]+\})", text)]
                    if len(records) != args.samples * 2:
                        raise RuntimeError(f"missing attribution records {log}")
                    attribution.extend(dict(record, label=label, log=log.name) for record in records)
                    profile_path = output / f"cpu-{profile}-{label}.pprof"
                    profile_env = local_env | {"REASONIX_CONFIG_CPU_PROFILE": str(profile_path)}
                    execute([str(binaries[source_label]), "-test.run=^$",
                             f"-test.bench=^BenchmarkConfigMechanismWarm$/^{profile}$/^readonly$",
                             "-test.benchtime=" + args.profile_seconds, "-test.cpu=2", "-test.count=1", "-test.timeout=10m"],
                            sources[source_label], profile_env, output / f"cpu-{profile}-{label}.log")
                    execute(["go", "tool", "pprof", "-top", "-nodecount=60", str(binaries[source_label]), str(profile_path)],
                            sources[source_label], env, output / f"cpu-{profile}-{label}-top.txt")
                    execute(["go", "tool", "pprof", "-top", "-cum", "-nodecount=60", str(binaries[source_label]), str(profile_path)],
                            sources[source_label], env, output / f"cpu-{profile}-{label}-cumulative.txt")
            for source_label in ("baseline", "candidate"):
                for index in range(args.micro_samples):
                    for profile in profiles:
                        log = output / f"micro-{index:02d}-{profile}-{source_label}.log"
                        text = execute([str(binaries[source_label]), "-test.run=^$", f"-test.bench=^BenchmarkConfigPathOperation$/^{profile}$",
                                        "-test.benchtime=" + args.micro_benchtime, "-test.cpu=2", "-test.count=1", "-test.timeout=10m"],
                                       sources[source_label], env | {"REASONIX_CONFIG_MECHANISM_VARIANT": "stock"}, log)
                        rows = []
                        for line in text.splitlines():
                            match = MICRO.match(line)
                            if match:
                                profile_name, source_name, operation, iterations, elapsed = match.groups()
                                rows.append({"profile": profile_name, "source": source_name, "operation": operation,
                                             "iterations": int(iterations), "ns_per_op": float(elapsed),
                                             "source_label": source_label, "sample": index, "log": log.name})
                        if not rows:
                            raise RuntimeError(f"no microbench measurements {log}")
                        micro.extend(rows)
            if os.name == "nt":
                for label in sources:
                    text = execute([str(binaries[label]), "-test.run=^TestConfigHandleBackendCompatibility$", "-test.v", "-test.count=1", "-test.timeout=10m"],
                                   sources[label], env, output / f"compatibility-{label}.log")
                    if "CONFIG_HANDLE_COMPATIBILITY" not in text:
                        raise RuntimeError("native compatibility rows missing")
            groups = {}
            for row in samples:
                key = (row["phase"], row["profile"], row["mode"], row["label"])
                groups.setdefault(key, []).append(row["ns_per_op"])
            summary = [dict(phase=key[0], profile=key[1], mode=key[2], label=key[3], **stats(values)) for key, values in sorted(groups.items())]
            result = {"metadata": metadata, "samples": samples, "summary": summary, "attribution": attribution, "micro": micro}
            (output / "results.json").write_text(json.dumps(result, indent=2) + "\n")
            for path, data in payload.items():
                (output / path.name).write_bytes(data)
            shutil.copyfile(__file__, output / Path(__file__).name)
            print(json.dumps(summary, indent=2), flush=True)
    finally:
        for path in created:
            path.unlink()
        for path in binaries.values():
            path.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
