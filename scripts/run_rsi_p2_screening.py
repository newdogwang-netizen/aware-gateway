#!/usr/bin/env python3
"""Build and evaluate an RSI P2 screening pilot from V4 artifacts."""

from __future__ import annotations

import argparse
import csv
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


SCHEMA_VERSION = "rsi-p2-screening.v1"


def main() -> None:
    args = parse_args()
    artifact_dir = args.artifact_dir.resolve()
    output_dir = (args.output_dir or artifact_dir / "rsi-p2-screening").resolve()
    output_dir.mkdir(parents=True, exist_ok=True)

    candidate_manifest = read_json(args.manifest) if args.manifest else {}
    min_tasks = args.min_tasks or int(nested_get(candidate_manifest, ["pilot_gate", "minimum_tasks"], 2) or 2)
    runs_per_task = args.runs_per_task or int(
        nested_get(candidate_manifest, ["pilot_gate", "screening_runs_per_task"], 1) or 1
    )

    episode_artifacts_dir = output_dir / "episode-artifacts"
    gate_output = args.gate_output or output_dir / "rsi-p2-policy-gate.json"
    build_cmd = build_artifact_command(
        args,
        artifact_dir,
        episode_artifacts_dir,
        gate_output,
        min_tasks,
        runs_per_task,
    )
    completed = subprocess.run(
        build_cmd,
        cwd=Path(__file__).resolve().parents[1],
        text=True,
        capture_output=True,
    )

    build_manifest_path = episode_artifacts_dir / "rsi-pilot-artifacts-manifest.json"
    build_manifest = read_json(build_manifest_path) if build_manifest_path.exists() else {}
    gate = read_json(gate_output) if gate_output.exists() else {}
    summary = build_summary(
        args,
        artifact_dir,
        output_dir,
        episode_artifacts_dir,
        build_manifest_path,
        gate_output,
        completed,
        build_manifest,
        gate,
        min_tasks,
        runs_per_task,
    )

    summary_path = output_dir / "rsi-p2-screening-summary.json"
    rows_path = output_dir / "rsi-p2-screening-summary.csv"
    write_json(summary_path, summary)
    write_csv(rows_path, build_manifest.get("rows") or [])

    print(summary["decision_text"])
    print(f"summary: {summary_path}")
    print(f"rows: {rows_path}")

    if completed.returncode != 0:
        raise SystemExit(completed.returncode)
    if args.fail_on_reject and summary["screening"]["status"] == "reject":
        raise SystemExit(1)
    if args.require_screening_pass and summary["screening"]["status"] != "repeat_for_acceptance":
        raise SystemExit(1)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description=(
            "Run the RSI P2 screening gate over existing V4 Harbor artifacts. "
            "This does not run Harbor; use scripts/run_v4_matrix.sh pilot to create matched trials first."
        )
    )
    parser.add_argument("--artifact-dir", required=True, type=Path)
    parser.add_argument("--output-dir", type=Path)
    parser.add_argument("--baseline-strategy", default="all-premium")
    parser.add_argument("--candidate-strategy", default="smart-router")
    parser.add_argument("--job-glob", default="*")
    parser.add_argument("--task", action="append", default=[], help="Task filter; may be repeated or comma-separated.")
    parser.add_argument("--attempt", action="append", default=[], help="Attempt filter; may be repeated or comma-separated.")
    parser.add_argument("--candidate-replay", action="append", default=[], type=Path)
    parser.add_argument("--manifest", type=Path)
    parser.add_argument("--gate-output", type=Path)
    parser.add_argument("--min-tasks", type=int, default=0)
    parser.add_argument("--runs-per-task", type=int, default=0)
    parser.add_argument("--success-threshold", type=float, default=1.0)
    parser.add_argument("--python", default=sys.executable)
    parser.add_argument("--no-strict", action="store_true", help="Allow extraction warnings instead of strict artifact checks.")
    parser.add_argument("--fail-on-reject", action="store_true")
    parser.add_argument("--require-screening-pass", action="store_true")
    return parser.parse_args()


def build_artifact_command(
    args: argparse.Namespace,
    artifact_dir: Path,
    episode_artifacts_dir: Path,
    gate_output: Path,
    min_tasks: int,
    runs_per_task: int,
) -> list[str]:
    cmd = [
        args.python,
        "scripts/build_rsi_pilot_artifacts.py",
        "--artifact-dir",
        str(artifact_dir),
        "--output-dir",
        str(episode_artifacts_dir),
        "--job-glob",
        args.job_glob,
        "--strategy",
        f"{args.baseline_strategy},{args.candidate_strategy}",
        "--baseline-strategy",
        args.baseline_strategy,
        "--candidate-strategy",
        args.candidate_strategy,
        "--gate-output",
        str(gate_output),
        "--min-tasks",
        str(min_tasks),
        "--runs-per-task",
        str(runs_per_task),
        "--success-threshold",
        str(args.success_threshold),
    ]
    for task in args.task:
        cmd.extend(["--task", task])
    for attempt in args.attempt:
        cmd.extend(["--attempt", attempt])
    for replay in args.candidate_replay:
        cmd.extend(["--candidate-replay", str(replay)])
    if args.manifest:
        cmd.extend(["--manifest", str(args.manifest)])
    if not args.no_strict:
        cmd.append("--strict")
    return cmd


def build_summary(
    args: argparse.Namespace,
    artifact_dir: Path,
    output_dir: Path,
    episode_artifacts_dir: Path,
    build_manifest_path: Path,
    gate_output: Path,
    completed: subprocess.CompletedProcess[str],
    build_manifest: dict[str, Any],
    gate: dict[str, Any],
    min_tasks: int,
    runs_per_task: int,
) -> dict[str, Any]:
    gate_status = str(nested_get(gate, ["gate", "status"], "") or "")
    screening_status = screening_status_from_gate(completed.returncode, build_manifest, gate_status)
    decision = decision_text(screening_status, gate)
    return {
        "schema_version": SCHEMA_VERSION,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "artifact_dir": str(artifact_dir),
        "output_dir": str(output_dir),
        "episode_artifacts_dir": str(episode_artifacts_dir),
        "build_manifest_path": str(build_manifest_path),
        "gate_output": str(gate_output),
        "baseline_strategy": args.baseline_strategy,
        "candidate_strategy": args.candidate_strategy,
        "thresholds": {
            "minimum_tasks": min_tasks,
            "screening_runs_per_task": runs_per_task,
            "success_reward": args.success_threshold,
        },
        "build": {
            "returncode": completed.returncode,
            "stdout": tail(completed.stdout),
            "stderr": tail(completed.stderr),
            "counts": build_manifest.get("counts") or {},
        },
        "gate": gate,
        "screening": {
            "status": screening_status,
            "gate_status": gate_status or "missing",
            "matched_tasks": gate.get("matched_tasks") or [],
            "warnings": nested_get(gate, ["gate", "warnings"], []) or [],
            "triggered_rollbacks": nested_get(gate, ["gate", "triggered_rollbacks"], []) or [],
            "next_step": next_step(screening_status),
        },
        "decision_text": decision,
    }


def screening_status_from_gate(returncode: int, manifest: dict[str, Any], gate_status: str) -> str:
    if returncode != 0:
        return "build_failed"
    counts = manifest.get("counts") or {}
    if int(counts.get("failed") or 0) > 0:
        return "build_failed"
    if gate_status == "reject":
        return "reject"
    if gate_status == "accept":
        return "repeat_for_acceptance"
    if gate_status == "needs_more_data":
        return "needs_more_data"
    return "needs_more_data"


def decision_text(status: str, gate: dict[str, Any]) -> str:
    matched = ", ".join(gate.get("matched_tasks") or []) or "none"
    baseline = gate.get("baseline") or {}
    candidate = gate.get("candidate") or {}
    lines = [
        f"RSI P2 screening: {status}",
        f"Matched tasks: {matched}",
        (
            "Baseline: "
            f"reward={baseline.get('avg_reward')} "
            f"success={baseline.get('success_count')}/{baseline.get('run_count')} "
            f"cost_per_success={baseline.get('cost_per_success')}"
        ),
        (
            "Candidate: "
            f"reward={candidate.get('avg_reward')} "
            f"success={candidate.get('success_count')}/{candidate.get('run_count')} "
            f"cost_per_success={candidate.get('cost_per_success')}"
        ),
        f"Next: {next_step(status)}",
    ]
    warnings = nested_get(gate, ["gate", "warnings"], []) or []
    rollbacks = nested_get(gate, ["gate", "triggered_rollbacks"], []) or []
    if warnings:
        lines.append("Warnings: " + ", ".join(str(item) for item in warnings))
    if rollbacks:
        lines.append("Rollback triggers: " + ", ".join(str(item) for item in rollbacks))
    return "\n".join(lines)


def next_step(status: str) -> str:
    if status == "repeat_for_acceptance":
        return "repeat matched pilot to at least 3 runs per accepted task before canary"
    if status == "reject":
        return "stop this policy candidate and inspect rollback triggers"
    if status == "build_failed":
        return "fix missing or invalid V4 artifacts before judging the policy"
    return "collect more matched baseline/candidate pilot runs"


def write_csv(path: Path, rows: list[dict[str, Any]]) -> None:
    fields = [
        "strategy",
        "task",
        "attempt",
        "reward",
        "total_cost_usd",
        "agent_cost_usd",
        "decision_cost_usd",
        "agent_call_count",
        "decision_call_count",
        "future_evidence_leakage",
        "status",
        "summary_path",
    ]
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=fields, extrasaction="ignore")
        writer.writeheader()
        for row in rows:
            writer.writerow(row)


def read_json(path: Path) -> Any:
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def write_json(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=False, sort_keys=True)
        handle.write("\n")


def nested_get(payload: dict[str, Any], keys: list[str], default: Any = None) -> Any:
    current: Any = payload
    for key in keys:
        if not isinstance(current, dict):
            return default
        current = current.get(key)
    return default if current is None else current


def tail(value: str, limit: int = 2000) -> str:
    value = value.strip()
    return value[-limit:]


if __name__ == "__main__":
    main()
