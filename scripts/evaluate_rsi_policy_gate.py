#!/usr/bin/env python3
"""Evaluate an RSI router policy candidate against matched episode summaries."""

from __future__ import annotations

import argparse
import json
import math
from collections import Counter, defaultdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


SCHEMA_VERSION = "rsi-policy-gate.v1"


def main() -> None:
    args = parse_args()
    manifest = read_json(args.manifest) if args.manifest else {}
    baseline = load_summaries(args.baseline_summary)
    candidate = load_summaries(args.candidate_summary)
    candidate_replays = [read_json(path) for path in args.candidate_replay]

    if not baseline:
        raise SystemExit("no baseline summaries found")
    if not candidate:
        raise SystemExit("no candidate summaries found")

    min_tasks = args.min_tasks or int(nested_get(manifest, ["pilot_gate", "minimum_tasks"], 1) or 1)
    runs_per_task = args.runs_per_task or int(
        nested_get(manifest, ["pilot_gate", "acceptance_runs_per_task"], 1) or 1
    )

    baseline_by_task = group_by_task(baseline)
    candidate_by_task = group_by_task(candidate)
    matched_tasks = sorted(set(baseline_by_task) & set(candidate_by_task))

    baseline_matched = flatten_groups(baseline_by_task, matched_tasks)
    candidate_matched = flatten_groups(candidate_by_task, matched_tasks)
    baseline_metrics = aggregate(baseline_matched, args.success_threshold)
    candidate_metrics = aggregate(candidate_matched, args.success_threshold)
    all_baseline_metrics = aggregate(baseline, args.success_threshold)
    all_candidate_metrics = aggregate(candidate, args.success_threshold)

    task_rows = [
        compare_task(task, baseline_by_task[task], candidate_by_task[task], args.success_threshold)
        for task in matched_tasks
    ]
    replay_metrics = aggregate_replay_metrics(candidate_replays)
    gate = evaluate_gate(
        baseline_metrics,
        candidate_metrics,
        task_rows,
        matched_tasks,
        baseline_by_task,
        candidate_by_task,
        replay_metrics,
        min_tasks,
        runs_per_task,
        args.success_threshold,
    )

    payload = {
        "schema_version": SCHEMA_VERSION,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "candidate_id": manifest.get("candidate_id") or args.candidate_id,
        "policy_id": args.policy_id,
        "thresholds": {
            "success_reward": args.success_threshold,
            "minimum_tasks": min_tasks,
            "runs_per_task": runs_per_task,
        },
        "input_counts": {
            "baseline_summaries": len(baseline),
            "candidate_summaries": len(candidate),
            "candidate_replays": len(candidate_replays),
        },
        "input_summaries": {
            "baseline": summarize_inputs(baseline),
            "candidate": summarize_inputs(candidate),
        },
        "matched_tasks": matched_tasks,
        "unmatched_baseline_tasks": sorted(set(baseline_by_task) - set(candidate_by_task)),
        "unmatched_candidate_tasks": sorted(set(candidate_by_task) - set(baseline_by_task)),
        "baseline": baseline_metrics,
        "candidate": candidate_metrics,
        "baseline_all": all_baseline_metrics,
        "candidate_all": all_candidate_metrics,
        "deltas": deltas(baseline_metrics, candidate_metrics),
        "per_task": task_rows,
        "candidate_replay": replay_metrics,
        "gate": gate,
        "decision_text": decision_text(gate, baseline_metrics, candidate_metrics),
    }

    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        write_json(args.output, payload)
    print(payload["decision_text"])


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Compare matched RSI episode summaries and decide accept/reject/needs_more_data."
    )
    parser.add_argument(
        "--baseline-summary",
        action="append",
        required=True,
        type=Path,
        help="episode-summary.json, an episode directory, or a parent containing episode-summary.json files.",
    )
    parser.add_argument(
        "--candidate-summary",
        action="append",
        required=True,
        type=Path,
        help="episode-summary.json, an episode directory, or a parent containing episode-summary.json files.",
    )
    parser.add_argument(
        "--candidate-replay",
        action="append",
        type=Path,
        default=[],
        help="Optional replay JSON from scripts/replay_episode_decisions.py.",
    )
    parser.add_argument("--manifest", type=Path, help="Optional docs/rsi/candidate-manifest*.json.")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--candidate-id", default="")
    parser.add_argument("--policy-id", default="")
    parser.add_argument("--min-tasks", type=int, default=0)
    parser.add_argument("--runs-per-task", type=int, default=0)
    parser.add_argument("--success-threshold", type=float, default=1.0)
    return parser.parse_args()


def read_json(path: Path) -> Any:
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def write_json(path: Path, payload: dict[str, Any]) -> None:
    with path.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=2, sort_keys=True)
        handle.write("\n")


def load_summaries(paths: list[Path]) -> list[dict[str, Any]]:
    summaries = []
    seen: set[Path] = set()
    for raw_path in paths:
        for path in resolve_summary_paths(raw_path):
            resolved = path.resolve()
            if resolved in seen:
                continue
            seen.add(resolved)
            summary = read_json(resolved)
            summary["_summary_path"] = str(resolved)
            summaries.append(summary)
    return summaries


def resolve_summary_paths(path: Path) -> list[Path]:
    if path.is_file():
        return [path]
    direct = path / "episode-summary.json"
    if direct.exists():
        return [direct]
    return sorted(path.glob("**/episode-summary.json"))


def group_by_task(summaries: list[dict[str, Any]]) -> dict[str, list[dict[str, Any]]]:
    grouped: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for summary in summaries:
        grouped[task_key(summary)].append(summary)
    return dict(grouped)


def task_key(summary: dict[str, Any]) -> str:
    for key in ("task_name", "task", "benchmark_task"):
        value = str(summary.get(key) or "").strip()
        if value:
            return value
    for key in ("trial_name", "episode_id"):
        value = str(summary.get(key) or "").strip()
        if value:
            return value
    return str(summary.get("_summary_path") or "unknown")


def summarize_inputs(summaries: list[dict[str, Any]]) -> list[dict[str, Any]]:
    out = []
    for summary in sorted(summaries, key=lambda item: (task_key(item), str(item.get("trial_name") or ""))):
        out.append(
            {
                "task": task_key(summary),
                "episode_id": summary.get("episode_id") or "",
                "trial_name": summary.get("trial_name") or "",
                "reward": summary.get("reward"),
                "total_cost_usd": summary.get("total_cost_usd"),
                "agent_call_count": summary.get("agent_call_count"),
                "decision_call_count": summary.get("decision_call_count"),
                "future_evidence_leakage": summary.get("future_evidence_leakage"),
                "summary_path": summary.get("_summary_path") or "",
            }
        )
    return out


def flatten_groups(groups: dict[str, list[dict[str, Any]]], tasks: list[str]) -> list[dict[str, Any]]:
    rows = []
    for task in tasks:
        rows.extend(groups.get(task, []))
    return rows


def aggregate(summaries: list[dict[str, Any]], success_threshold: float) -> dict[str, Any]:
    run_count = len(summaries)
    rewards = [as_float(summary.get("reward")) for summary in summaries]
    success_count = sum(1 for reward in rewards if reward >= success_threshold)
    total_cost = round(sum(as_float(summary.get("total_cost_usd")) for summary in summaries), 8)
    agent_calls = sum(as_int(summary.get("agent_call_count")) for summary in summaries)
    decision_calls = sum(as_int(summary.get("decision_call_count")) for summary in summaries)
    future_leakage = sum(as_int(summary.get("future_evidence_leakage")) for summary in summaries)
    model_counts: Counter[str] = Counter()
    route_outcomes: Counter[str] = Counter()
    for summary in summaries:
        model_counts.update(counter_from_mapping(summary.get("model_counts")))
        route_outcomes.update(counter_from_mapping(summary.get("route_outcome_labels")))

    return {
        "run_count": run_count,
        "task_count": len({task_key(summary) for summary in summaries}),
        "success_count": success_count,
        "success_rate": round(success_count / run_count, 4) if run_count else 0,
        "avg_reward": round(sum(rewards) / run_count, 4) if run_count else 0,
        "total_cost_usd": total_cost,
        "cost_per_attempt": round(total_cost / run_count, 8) if run_count else None,
        "cost_per_success": round(total_cost / success_count, 8) if success_count else None,
        "agent_call_count": agent_calls,
        "decision_call_count": decision_calls,
        "length_finish_count": sum(as_int(summary.get("length_finish_count")) for summary in summaries),
        "episode_adjust_call_count": sum(as_int(summary.get("episode_adjust_call_count")) for summary in summaries),
        "provider_incomplete_count": sum(as_int(summary.get("provider_incomplete_count")) for summary in summaries),
        "future_evidence_leakage": future_leakage,
        "model_counts": dict(sorted(model_counts.items())),
        "route_outcome_labels": dict(sorted(route_outcomes.items())),
    }


def compare_task(
    task: str,
    baseline: list[dict[str, Any]],
    candidate: list[dict[str, Any]],
    success_threshold: float,
) -> dict[str, Any]:
    base = aggregate(baseline, success_threshold)
    cand = aggregate(candidate, success_threshold)
    return {
        "task": task,
        "baseline": base,
        "candidate": cand,
        "deltas": deltas(base, cand),
    }


def deltas(baseline: dict[str, Any], candidate: dict[str, Any]) -> dict[str, Any]:
    return {
        "success_rate": round(as_float(candidate.get("success_rate")) - as_float(baseline.get("success_rate")), 4),
        "avg_reward": round(as_float(candidate.get("avg_reward")) - as_float(baseline.get("avg_reward")), 4),
        "total_cost_usd": round(as_float(candidate.get("total_cost_usd")) - as_float(baseline.get("total_cost_usd")), 8),
        "cost_per_attempt": delta_optional(candidate.get("cost_per_attempt"), baseline.get("cost_per_attempt")),
        "cost_per_success": delta_optional(candidate.get("cost_per_success"), baseline.get("cost_per_success")),
        "agent_call_count": as_int(candidate.get("agent_call_count")) - as_int(baseline.get("agent_call_count")),
        "decision_call_count": as_int(candidate.get("decision_call_count")) - as_int(baseline.get("decision_call_count")),
    }


def delta_optional(candidate: Any, baseline: Any) -> float | None:
    if candidate is None or baseline is None:
        return None
    return round(as_float(candidate) - as_float(baseline), 8)


def aggregate_replay_metrics(replays: list[dict[str, Any]]) -> dict[str, Any]:
    if not replays:
        return {
            "present": False,
            "future_evidence_leakage": 0,
            "reason_evidence_coverage": None,
            "rows": 0,
            "ok": 0,
        }
    rows = 0
    ok = 0
    leak = 0
    coverages: list[float] = []
    for replay in replays:
        summary = replay.get("summary") or {}
        rows += as_int(summary.get("rows"))
        ok += as_int(summary.get("ok"))
        leak += as_int(summary.get("future_evidence_leakage"))
        if summary.get("reason_evidence_coverage") is not None:
            coverages.append(as_float(summary.get("reason_evidence_coverage")))
    return {
        "present": True,
        "future_evidence_leakage": leak,
        "reason_evidence_coverage": round(min(coverages), 4) if coverages else None,
        "rows": rows,
        "ok": ok,
    }


def evaluate_gate(
    baseline: dict[str, Any],
    candidate: dict[str, Any],
    task_rows: list[dict[str, Any]],
    matched_tasks: list[str],
    baseline_by_task: dict[str, list[dict[str, Any]]],
    candidate_by_task: dict[str, list[dict[str, Any]]],
    replay: dict[str, Any],
    min_tasks: int,
    runs_per_task: int,
    success_threshold: float,
) -> dict[str, Any]:
    triggered: list[str] = []
    warnings: list[str] = []

    if not matched_tasks:
        triggered.append("no_matched_tasks")

    if candidate.get("future_evidence_leakage") or replay.get("future_evidence_leakage"):
        triggered.append("future_evidence_leakage")

    if as_float(candidate.get("avg_reward")) < as_float(baseline.get("avg_reward")):
        triggered.append("reward_below_matched_baseline")

    for row in task_rows:
        if as_float(row["candidate"].get("avg_reward")) < as_float(row["baseline"].get("avg_reward")):
            triggered.append(f"task_reward_regression:{row['task']}")

    base_cps = baseline.get("cost_per_success")
    cand_cps = candidate.get("cost_per_success")
    if baseline.get("success_count", 0) and not candidate.get("success_count", 0):
        triggered.append("candidate_has_no_successes")
    elif base_cps is not None and cand_cps is not None and as_float(cand_cps) >= as_float(base_cps):
        triggered.append("cost_per_success_not_lower")

    if replay.get("reason_evidence_coverage") is not None and as_float(replay["reason_evidence_coverage"]) < 1.0:
        triggered.append("reason_evidence_coverage_below_100pct")

    enough_tasks = len(matched_tasks) >= min_tasks
    if not enough_tasks:
        warnings.append(f"matched_tasks_below_minimum:{len(matched_tasks)}/{min_tasks}")

    enough_runs = True
    for task in matched_tasks:
        base_runs = len(baseline_by_task.get(task, []))
        cand_runs = len(candidate_by_task.get(task, []))
        if base_runs < runs_per_task or cand_runs < runs_per_task:
            enough_runs = False
            warnings.append(f"runs_below_minimum:{task}:baseline={base_runs}:candidate={cand_runs}:need={runs_per_task}")

    sufficient_data = enough_tasks and enough_runs
    hard_triggered = [item for item in triggered if item != "cost_per_success_not_lower"]
    if hard_triggered or "cost_per_success_not_lower" in triggered:
        status = "reject"
    elif not sufficient_data:
        status = "needs_more_data"
    elif candidate.get("success_count", 0) >= baseline.get("success_count", 0) and cost_per_success_improved(baseline, candidate):
        status = "accept"
    else:
        status = "needs_more_data"
        warnings.append("candidate_did_not_meet_acceptance_margin")

    return {
        "status": status,
        "sufficient_data": sufficient_data,
        "triggered_rollbacks": dedupe(triggered),
        "warnings": dedupe(warnings),
        "minimum_tasks_met": enough_tasks,
        "runs_per_task_met": enough_runs,
        "success_threshold": success_threshold,
    }


def cost_per_success_improved(baseline: dict[str, Any], candidate: dict[str, Any]) -> bool:
    base = baseline.get("cost_per_success")
    cand = candidate.get("cost_per_success")
    if base is None or cand is None:
        return False
    return as_float(cand) < as_float(base)


def decision_text(gate: dict[str, Any], baseline: dict[str, Any], candidate: dict[str, Any]) -> str:
    status = gate["status"]
    parts = [
        f"Decision: {status}",
        f"Baseline reward={baseline['avg_reward']} success={baseline['success_count']}/{baseline['run_count']} cost_per_success={baseline['cost_per_success']}",
        f"Candidate reward={candidate['avg_reward']} success={candidate['success_count']}/{candidate['run_count']} cost_per_success={candidate['cost_per_success']}",
    ]
    if gate["triggered_rollbacks"]:
        parts.append("Triggered: " + ", ".join(gate["triggered_rollbacks"]))
    if gate["warnings"]:
        parts.append("Warnings: " + ", ".join(gate["warnings"]))
    return "\n".join(parts)


def counter_from_mapping(value: Any) -> Counter[str]:
    counter: Counter[str] = Counter()
    if not isinstance(value, dict):
        return counter
    for key, raw_count in value.items():
        counter[str(key)] += as_int(raw_count)
    return counter


def nested_get(payload: dict[str, Any], keys: list[str], default: Any = None) -> Any:
    current: Any = payload
    for key in keys:
        if not isinstance(current, dict):
            return default
        current = current.get(key)
    return default if current is None else current


def as_float(value: Any) -> float:
    if value is None:
        return 0.0
    try:
        number = float(value)
    except (TypeError, ValueError):
        return 0.0
    if math.isnan(number) or math.isinf(number):
        return 0.0
    return number


def as_int(value: Any) -> int:
    return int(round(as_float(value)))


def dedupe(values: list[str]) -> list[str]:
    seen: set[str] = set()
    out: list[str] = []
    for value in values:
        if value in seen:
            continue
        seen.add(value)
        out.append(value)
    return out


if __name__ == "__main__":
    main()
