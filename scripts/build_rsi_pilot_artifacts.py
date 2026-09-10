#!/usr/bin/env python3
"""Build RSI episode artifacts from V4 Harbor experiment outputs."""

from __future__ import annotations

import argparse
import csv
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


def main() -> None:
    args = parse_args()
    artifact_dir = args.artifact_dir.resolve()
    output_dir = (args.output_dir or artifact_dir / "rsi-episode-artifacts").resolve()
    output_dir.mkdir(parents=True, exist_ok=True)

    filters = {
        "strategy": split_filter(args.strategy),
        "task": split_filter(args.task),
        "attempt": split_filter(args.attempt),
    }
    rows = []
    for job_dir in discover_job_dirs(artifact_dir, args.job_glob):
        for trial_result_path in sorted(job_dir.glob("*/result.json")):
            meta = trial_meta(artifact_dir, job_dir, trial_result_path)
            if not selected(meta, filters):
                continue
            row = build_one(args, output_dir, meta)
            rows.append(row)

    manifest = {
        "schema_version": "rsi-pilot-artifacts-manifest.v1",
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "artifact_dir": str(artifact_dir),
        "output_dir": str(output_dir),
        "rows": rows,
        "counts": {
            "jobs": len({row["job"] for row in rows}),
            "episodes": len(rows),
            "ok": sum(1 for row in rows if row["status"] == "ok"),
            "failed": sum(1 for row in rows if row["status"] != "ok"),
        },
    }
    manifest_path = output_dir / "rsi-pilot-artifacts-manifest.json"
    write_json(manifest_path, manifest)
    write_csv(output_dir / "rsi-pilot-artifacts-manifest.csv", rows)

    gate_row = maybe_run_gate(args, output_dir, rows)
    if gate_row:
        manifest["policy_gate"] = gate_row
        write_json(manifest_path, manifest)

    if args.strict:
        problems = strict_problems(rows, gate_row, args)
        if problems:
            for problem in problems:
                print(f"strict validation failed: {problem}", file=sys.stderr)
            raise SystemExit(1)

    print(
        f"built {manifest['counts']['ok']}/{len(rows)} RSI episode artifact sets "
        f"under {output_dir}"
    )
    if gate_row:
        print(f"policy gate: {gate_row['output_path']}")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Convert V4 Harbor jobs into RSI episode artifacts and optional policy gate input."
    )
    parser.add_argument("--artifact-dir", required=True, type=Path)
    parser.add_argument("--output-dir", type=Path)
    parser.add_argument("--job-glob", default="*")
    parser.add_argument("--strategy", action="append", default=[], help="Strategy filter; may be repeated or comma-separated.")
    parser.add_argument("--task", action="append", default=[], help="Task filter; may be repeated or comma-separated.")
    parser.add_argument("--attempt", action="append", default=[], help="Attempt filter; may be repeated or comma-separated.")
    parser.add_argument("--strict", action="store_true")
    parser.add_argument("--python", default=sys.executable)
    parser.add_argument("--baseline-strategy", default="")
    parser.add_argument("--candidate-strategy", default="")
    parser.add_argument("--candidate-replay", action="append", default=[], type=Path)
    parser.add_argument("--manifest", type=Path)
    parser.add_argument("--gate-output", type=Path)
    parser.add_argument("--min-tasks", type=int, default=0)
    parser.add_argument("--runs-per-task", type=int, default=0)
    parser.add_argument("--success-threshold", type=float, default=1.0)
    return parser.parse_args()


def discover_job_dirs(artifact_dir: Path, job_glob: str) -> list[Path]:
    jobs_dir = artifact_dir / "jobs"
    if not jobs_dir.is_dir():
        return []
    return [path for path in sorted(jobs_dir.glob(job_glob)) if path.is_dir()]


def split_filter(values: list[str]) -> set[str]:
    out: set[str] = set()
    for value in values:
        for item in value.split(","):
            item = item.strip()
            if item:
                out.add(item)
    return out


def selected(meta: dict[str, Any], filters: dict[str, set[str]]) -> bool:
    for key, allowed in filters.items():
        if allowed and str(meta.get(key) or "") not in allowed:
            return False
    return True


def trial_meta(artifact_dir: Path, job_dir: Path, trial_result_path: Path) -> dict[str, Any]:
    result = read_json(trial_result_path)
    job = job_dir.name
    task = task_name(result, job)
    strategy = infer_strategy(job, result)
    attempt = infer_attempt(job)
    return {
        "artifact_dir": artifact_dir,
        "job_dir": job_dir,
        "trial_dir": trial_result_path.parent,
        "result_path": trial_result_path,
        "job": job,
        "trial_name": str(result.get("trial_name") or trial_result_path.parent.name),
        "task": task,
        "strategy": strategy,
        "attempt": attempt,
        "traces": trace_paths_for_job(artifact_dir, job),
    }


def task_name(result: dict[str, Any], job: str) -> str:
    raw = str(result.get("task_name") or "")
    if raw:
        return raw.rsplit("/", 1)[-1]
    parts = job.split("-")
    for marker in ("all-premium", "all-flash", "smart-router-warmstart", "smart-router"):
        if marker in job:
            suffix = job.split(marker + "-", 1)[-1]
            return suffix.rsplit("-a", 1)[0]
    return ""


def infer_strategy(job: str, result: dict[str, Any]) -> str:
    for strategy in ("smart-router-warmstart", "all-premium", "all-flash", "smart-router"):
        if strategy in job:
            return strategy
    model_name = str((((result.get("config") or {}).get("agent") or {}).get("model_name")) or "")
    if model_name.endswith("/auto-opus-warmstart"):
        return "smart-router-warmstart"
    if model_name.endswith("/auto"):
        return "smart-router"
    if model_name.endswith("/anthropic/claude-opus-5"):
        return "all-premium"
    if model_name.endswith("/z-ai/glm-5.3-flash"):
        return "all-flash"
    return ""


def infer_attempt(job: str) -> str:
    if "-a" not in job:
        return ""
    suffix = job.rsplit("-a", 1)[-1]
    return suffix if suffix.isdigit() else ""


def trace_paths_for_job(artifact_dir: Path, job: str) -> list[Path]:
    candidates = [
        artifact_dir / f"traces-after-{job}.json",
        artifact_dir / f"traces-truncated-{job}.json",
        artifact_dir / f"traces-wall-clock-cap-{job}.json",
        artifact_dir / f"traces-early-{job}.json",
    ]
    return [path for path in candidates if path.exists()]


def build_one(args: argparse.Namespace, output_dir: Path, meta: dict[str, Any]) -> dict[str, Any]:
    relative_dir = Path(safe_name(meta["strategy"] or "unknown")) / safe_name(
        f"{meta['task'] or 'unknown'}-a{meta['attempt'] or 'unknown'}"
    )
    episode_dir = output_dir / relative_dir
    cmd = [
        args.python,
        "scripts/extract_episode_outcomes.py",
        "--trial-dir",
        str(meta["trial_dir"]),
        "--output-dir",
        str(episode_dir),
    ]
    for trace_path in meta["traces"]:
        cmd.extend(["--traces-json", str(trace_path)])
    if args.strict:
        cmd.append("--strict")

    completed = subprocess.run(
        cmd,
        cwd=Path(__file__).resolve().parents[1],
        text=True,
        capture_output=True,
    )
    summary_path = episode_dir / "episode-summary.json"
    row = {
        "job": meta["job"],
        "trial_name": meta["trial_name"],
        "task": meta["task"],
        "strategy": meta["strategy"],
        "attempt": meta["attempt"],
        "trial_dir": str(meta["trial_dir"]),
        "traces_json": ";".join(str(path) for path in meta["traces"]),
        "episode_dir": str(episode_dir),
        "summary_path": str(summary_path) if summary_path.exists() else "",
        "status": "ok" if completed.returncode == 0 and summary_path.exists() else "failed",
        "returncode": completed.returncode,
        "stdout": completed.stdout.strip()[-1000:],
        "stderr": completed.stderr.strip()[-1000:],
    }
    if summary_path.exists():
        summary = read_json(summary_path)
        row.update(
            {
                "episode_id": summary.get("episode_id") or "",
                "reward": summary.get("reward"),
                "total_cost_usd": summary.get("total_cost_usd"),
                "agent_cost_usd": summary.get("agent_cost_usd"),
                "decision_cost_usd": summary.get("decision_cost_usd"),
                "agent_call_count": summary.get("agent_call_count"),
                "decision_call_count": summary.get("decision_call_count"),
                "future_evidence_leakage": summary.get("future_evidence_leakage"),
            }
        )
    return row


def maybe_run_gate(args: argparse.Namespace, output_dir: Path, rows: list[dict[str, Any]]) -> dict[str, Any] | None:
    if not args.baseline_strategy and not args.candidate_strategy:
        return None
    if not args.baseline_strategy or not args.candidate_strategy:
        raise SystemExit("--baseline-strategy and --candidate-strategy must be supplied together")
    baseline = ok_summary_paths(rows, args.baseline_strategy)
    candidate = ok_summary_paths(rows, args.candidate_strategy)
    gate_output = args.gate_output or (
        output_dir
        / f"policy-gate-{safe_name(args.baseline_strategy)}-vs-{safe_name(args.candidate_strategy)}.json"
    )
    cmd = [
        args.python,
        "scripts/evaluate_rsi_policy_gate.py",
        "--output",
        str(gate_output),
        "--policy-id",
        f"{args.baseline_strategy}-vs-{args.candidate_strategy}",
    ]
    for path in baseline:
        cmd.extend(["--baseline-summary", str(path)])
    for path in candidate:
        cmd.extend(["--candidate-summary", str(path)])
    for replay in args.candidate_replay:
        cmd.extend(["--candidate-replay", str(replay)])
    if args.manifest:
        cmd.extend(["--manifest", str(args.manifest)])
    if args.min_tasks > 0:
        cmd.extend(["--min-tasks", str(args.min_tasks)])
    if args.runs_per_task > 0:
        cmd.extend(["--runs-per-task", str(args.runs_per_task)])
    cmd.extend(["--success-threshold", str(args.success_threshold)])

    completed = subprocess.run(
        cmd,
        cwd=Path(__file__).resolve().parents[1],
        text=True,
        capture_output=True,
    )
    return {
        "baseline_strategy": args.baseline_strategy,
        "candidate_strategy": args.candidate_strategy,
        "baseline_summary_count": len(baseline),
        "candidate_summary_count": len(candidate),
        "output_path": str(gate_output),
        "status": "ok" if completed.returncode == 0 and gate_output.exists() else "failed",
        "returncode": completed.returncode,
        "stdout": completed.stdout.strip()[-1000:],
        "stderr": completed.stderr.strip()[-1000:],
    }


def ok_summary_paths(rows: list[dict[str, Any]], strategy: str) -> list[Path]:
    return [
        Path(row["summary_path"])
        for row in rows
        if row.get("strategy") == strategy and row.get("status") == "ok" and row.get("summary_path")
    ]


def strict_problems(rows: list[dict[str, Any]], gate_row: dict[str, Any] | None, args: argparse.Namespace) -> list[str]:
    problems = []
    if not rows:
        problems.append("no matching trials found")
    for row in rows:
        label = f"{row.get('strategy')}/{row.get('task')}/a{row.get('attempt') or '?'}"
        if row.get("status") != "ok":
            problems.append(f"{label} extraction failed: {row.get('stderr') or row.get('stdout')}")
        if row.get("future_evidence_leakage") not in (0, "0", None):
            problems.append(f"{label} future_evidence_leakage={row.get('future_evidence_leakage')}")
    if (args.baseline_strategy or args.candidate_strategy) and gate_row:
        if gate_row.get("status") != "ok":
            problems.append(f"policy gate failed: {gate_row.get('stderr') or gate_row.get('stdout')}")
        if gate_row.get("baseline_summary_count") == 0:
            problems.append(f"no baseline summaries for {args.baseline_strategy}")
        if gate_row.get("candidate_summary_count") == 0:
            problems.append(f"no candidate summaries for {args.candidate_strategy}")
    return problems


def safe_name(value: str) -> str:
    return "".join(ch if ch.isalnum() or ch in "._-" else "_" for ch in value)


def read_json(path: Path) -> Any:
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def write_json(path: Path, payload: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=False, sort_keys=True)
        handle.write("\n")


def write_csv(path: Path, rows: list[dict[str, Any]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fields = [
        "job",
        "trial_name",
        "task",
        "strategy",
        "attempt",
        "episode_id",
        "reward",
        "total_cost_usd",
        "agent_cost_usd",
        "decision_cost_usd",
        "agent_call_count",
        "decision_call_count",
        "future_evidence_leakage",
        "trial_dir",
        "traces_json",
        "episode_dir",
        "summary_path",
        "status",
        "returncode",
        "stderr",
    ]
    with path.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=fields, extrasaction="ignore")
        writer.writeheader()
        writer.writerows(rows)


if __name__ == "__main__":
    main()
