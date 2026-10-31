#!/usr/bin/env python3
"""Exercise exact baseline/candidate sources before the paired timing phase."""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys


COMPATIBILITY_TESTS = (
    "TestWindowsConfigLoadKeepsSDKPathSemantics",
    "TestWindowsConfigLoadObservesSourceReplacement",
)
METRIC_MARKER = "CONFIG_ALLOCATION_EFFECT "


def run(command, source, env, output, name):
    result = subprocess.run(command, cwd=source, env=env, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            timeout=1800, check=False)
    (output / name).write_text(result.stdout, encoding="utf-8")
    return result


def go_events(text):
    events = []
    for line in text.splitlines():
        if line.startswith("{"):
            events.append(json.loads(line))
    return events


def allocation_metric(events, test_name):
    metrics = []
    for event in events:
        text = event.get("Output", "")
        if event.get("Test") == test_name and METRIC_MARKER in text:
            metrics.append(json.loads(text.partition(METRIC_MARKER)[2].strip()))
    if len(metrics) != 1:
        raise RuntimeError("allocation effect must emit exactly one structured metric")
    metric = metrics[0]
    if metric.get("code") != "config.path_allocations" or metric.get("settings_validated") is not True:
        raise RuntimeError("allocation metric lacks validated consumer settings")
    for key in ("sdk_allocs", "default_allocs", "runs"):
        value = metric.get(key)
        if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value <= 0:
            raise RuntimeError("allocation metric has an invalid " + key)
    return metric


def terminal_tests(events):
    return {event["Test"]: event["Action"] for event in events
            if event.get("Test") and event.get("Action") in ("pass", "fail", "skip")}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--before", type=Path, required=True)
    parser.add_argument("--after", type=Path, required=True)
    parser.add_argument("--before-revision", required=True)
    parser.add_argument("--after-revision", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--compatibility-source", type=Path, required=True)
    parser.add_argument("--compatibility-sha256", required=True)
    parser.add_argument("--smoke-source", type=Path, required=True)
    parser.add_argument("--smoke-sha256", required=True)
    parser.add_argument("--effect-test", required=True)
    args = parser.parse_args()
    if os.name != "nt":
        parser.error("this verifier requires native Windows, not cross-compilation")
    sources = {"before": args.before.resolve(), "after": args.after.resolve()}
    if sources["before"] == sources["after"]:
        parser.error("baseline and candidate checkouts must be isolated")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    env = os.environ.copy()
    env.update({"GOMAXPROCS": "2", "GOFLAGS": "-p=2"})
    record = {"started_at": datetime.now(timezone.utc).isoformat(), "sources": {},
              "commands": [], "state": "in_progress", "python": sys.version,
              "definitions": {"allocation_effect": "AllocsPerRun over real public Roots loads; fixture setup and exact-setting validation excluded from allocation measurements",
                              "compatibility": "Same frozen public Windows test source on baseline and candidate; skips recorded, not treated as exercised behavior",
                              "smoke": "Non-test go run through public config Load and real boot.Build; no model/network request"}}
    created = []
    smoke_directory = None
    try:
        compatibility = args.compatibility_source.read_bytes()
        smoke = args.smoke_source.read_bytes()
        for name, data, expected in (("compatibility", compatibility, args.compatibility_sha256),
                                     ("smoke", smoke, args.smoke_sha256)):
            actual = hashlib.sha256(data).hexdigest()
            if actual != expected:
                raise RuntimeError(name + " source checksum mismatch")
            record[name + "_sha256"] = actual
        for label, source in sources.items():
            head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True).strip()
            expected = args.before_revision if label == "before" else args.after_revision
            if head != expected:
                raise RuntimeError(label + " checkout is not its exact frozen revision")
            record["sources"][label] = head
            destination = source / "internal/contract/config/path_access_windows_test.go"
            if destination.exists():
                if destination.read_bytes() != compatibility:
                    raise RuntimeError("refusing to overwrite different compatibility tests")
            else:
                destination.write_bytes(compatibility)
                created.append(destination)
        pattern = "^(" + "|".join((*COMPATIBILITY_TESTS, args.effect_test)) + ")$"
        command = ["go", "test", "-json", "-count=1", "-parallel=2", "-run=" + pattern,
                   "./internal/contract/config"]
        result = run(command, sources["before"], env, output, "baseline-contract.jsonl")
        events = go_events(result.stdout)
        terminals = terminal_tests(events)
        failed = {name for name, action in terminals.items() if action == "fail"}
        metric = allocation_metric(events, args.effect_test)
        if result.returncode != 1 or failed != {args.effect_test}:
            raise RuntimeError("baseline did not fail solely at the expected allocation effect test")
        if any(terminals.get(name) != "pass" for name in COMPATIBILITY_TESTS):
            raise RuntimeError("baseline public compatibility control did not pass")
        if metric["default_allocs"] < metric["sdk_allocs"]:
            raise RuntimeError("baseline allocation metric unexpectedly satisfies the new effect")
        record["baseline"] = {"expected_allocation_failure": True, "allocation": metric,
                              "skipped": sorted(name for name, action in terminals.items() if action == "skip")}
        record["commands"].append({"phase": "baseline-contract", "command": command, "expected_exit": 1})
        command = ["go", "test", "-json", "-count=1", "-parallel=2",
                   "./internal/contract/config", "./internal/base/fileutil"]
        result = run(command, sources["after"], env, output, "candidate-suites.jsonl")
        events = go_events(result.stdout)
        terminals = terminal_tests(events)
        metric = allocation_metric(events, args.effect_test)
        if result.returncode or terminals.get(args.effect_test) != "pass" or metric["default_allocs"] >= metric["sdk_allocs"]:
            raise RuntimeError("candidate native suites or allocation effect failed")
        record["candidate"] = {"suites_passed": True, "allocation": metric,
                               "skipped": sorted(name for name, action in terminals.items() if action == "skip")}
        record["commands"].append({"phase": "candidate-suites", "command": command, "expected_exit": 0})
        destination = sources["after"] / ".cache/windows-config-native-smoke"
        if destination.exists():
            raise RuntimeError("refusing to overwrite existing smoke directory")
        destination.mkdir(parents=True)
        smoke_directory = destination
        (smoke_directory / "main.go").write_bytes(smoke)
        command = ["go", "run", "./.cache/windows-config-native-smoke"]
        result = run(command, sources["after"], env, output, "candidate-actual-build-smoke.log")
        if result.returncode:
            raise RuntimeError("actual Load plus boot.Build smoke failed")
        record["commands"].append({"phase": "actual-build-smoke", "command": command, "expected_exit": 0})
        record["state"] = "success"
        print(json.dumps(record, indent=2), flush=True)
    except Exception as error:
        record["state"] = "failed"
        record["error"] = {"type": type(error).__name__, "message": str(error)}
        raise
    finally:
        record["finished_at"] = datetime.now(timezone.utc).isoformat()
        (output / "verification.json").write_text(json.dumps(record, indent=2) + "\n", encoding="utf-8")
        for destination in created:
            destination.unlink()
        if smoke_directory is not None and (smoke_directory / "main.go").exists():
            (smoke_directory / "main.go").unlink()
            smoke_directory.rmdir()


if __name__ == "__main__":
    main()
