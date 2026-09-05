#!/usr/bin/env python3
"""Aggregate V3 Harbor trials with gateway traces."""

from __future__ import annotations

import argparse
import csv
import json
import sys
from datetime import datetime
from pathlib import Path
from typing import Any

import yaml


HARBOR_MODEL_TO_GATEWAY = {
    "openai/auto": "auto",
    "openai/auto-opus-warmstart": "auto-opus-warmstart",
    "openai/z-ai/glm-5.3-flash": "z-ai/glm-5.3-flash",
    "openai/openai/gpt-5.6-sol": "openai/gpt-5.6-sol",
    "openai/anthropic/claude-opus-5": "anthropic/claude-opus-5",
}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--artifact-dir", required=True, type=Path)
    parser.add_argument("--gateway-config", default="configs/gateway-openrouter.yaml", type=Path)
    parser.add_argument("--traces-json", action="append", type=Path, default=[])
    parser.add_argument("--job-glob", default="*")
    parser.add_argument("--expected-rows", type=int)
    parser.add_argument("--strict", action="store_true")
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()

    prices = load_prices(args.gateway_config)
    traces = load_traces(args.traces_json)
    traces_by_session: dict[str, list[dict[str, Any]]] = {}
    for trace in traces:
        session_id = trace.get("session_id") or ""
        if session_id:
            traces_by_session.setdefault(session_id, []).append(trace)

    rows = []
    for job_dir in sorted((args.artifact_dir / "jobs").glob(args.job_glob)):
        if not job_dir.is_dir():
            continue
        emitted_job_rows = 0
        trial_result_paths = sorted(job_dir.glob("*/result.json"))
        for trial_result_path in trial_result_paths:
            result = json.loads(trial_result_path.read_text())
            if "verifier_result" not in result and "exception_info" not in result:
                continue
            rows.append(build_row(args.artifact_dir, trial_result_path, result, traces_by_session, prices))
            emitted_job_rows += 1

        cap_marker_path = job_dir / "wall-clock-cap.json"
        if emitted_job_rows == 0 and cap_marker_path.exists():
            rows.append(
                build_wall_clock_cap_row(args.artifact_dir, job_dir, cap_marker_path, traces_by_session, prices)
            )

    args.output.parent.mkdir(parents=True, exist_ok=True)
    with args.output.open("w", newline="") as f:
        fieldnames = [
            "experiment_id",
            "task",
            "attempt",
            "strategy",
            "model_sent",
            "agent_models",
            "reward",
            "total_cost_usd",
            "agent_cost_usd",
            "decision_cost_usd",
            "prompt_tokens",
            "completion_tokens",
            "total_tokens",
            "agent_call_count",
            "decision_call_count",
            "guardrail_call_count",
            "warm_start_call_count",
            "premium_upgrade_rate",
            "sol_upgrade_rate",
            "opus_selected_rate",
            "fallback_to_opus_rate",
            "opus_fallback_rate",
            "cache_hit_rate",
            "duration_seconds",
            "trace_key",
            "failure_kind",
            "exception_type",
            "cost_source",
        ]
        writer = csv.DictWriter(f, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerows(rows)

    print(f"wrote {len(rows)} rows to {args.output}")
    if args.strict:
        problems = validate_rows(rows, args.expected_rows)
        if problems:
            for problem in problems:
                print(f"strict validation failed: {problem}", file=sys.stderr)
            raise SystemExit(1)


def load_prices(path: Path) -> dict[str, tuple[float, float]]:
    data = yaml.safe_load(path.read_text()) or {}
    models = ((data.get("pricing") or {}).get("models") or {})
    prices: dict[str, tuple[float, float]] = {}
    for model, price in models.items():
        prices[model] = (float(price.get("prompt", 0)), float(price.get("completion", 0)))
    return prices


def load_traces(paths: list[Path]) -> list[dict[str, Any]]:
    traces: list[dict[str, Any]] = []
    for path in paths:
        data = json.loads(path.read_text())
        if isinstance(data, list):
            traces.extend(data)
        else:
            traces.extend(data.get("traces") or [])
    return traces


def build_row(
    artifact_dir: Path,
    trial_result_path: Path,
    result: dict[str, Any],
    traces_by_session: dict[str, list[dict[str, Any]]],
    prices: dict[str, tuple[float, float]],
) -> dict[str, Any]:
    trial_dir = trial_result_path.parent
    job_dir = trial_dir.parent
    trial_name = result["trial_name"]
    session_id = f"{trial_name}__agent"
    traces = traces_by_session.get(session_id, [])
    agent_traces = [t for t in traces if t.get("pool") != "decision-model"]
    decision_traces = [
        t
        for t in traces
        if t.get("pool") == "decision-model" or str(t.get("step_name") or "").startswith("router-decision")
    ]

    trajectory = load_trajectories(trial_dir / "agent")
    usage = aggregate_agent_usage(trajectory, prices)
    trace_cost = aggregate_agent_trace_cost(agent_traces)
    agent_cost_usd = usage["agent_cost_usd"]
    cost_source = usage["cost_source"]
    if trace_cost["complete"]:
        agent_cost_usd = trace_cost["agent_cost_usd"]
        cost_source = "gateway_trace_upstream_cost"
    agent_models = sorted(
        {
            normalize_model_for_gateway(step.get("model_name") or "")
            for step in trajectory
            if step.get("source") == "agent" and step.get("model_name")
        }
    )
    decision_cost = sum(float(t.get("cost") or 0) for t in decision_traces)
    decision_prompt = sum(int(t.get("prompt_tokens") or 0) for t in decision_traces)
    decision_completion = sum(int(t.get("completion_tokens") or 0) for t in decision_traces)
    reward = (((result.get("verifier_result") or {}).get("rewards") or {}).get("reward"))

    model_name = (((result.get("config") or {}).get("agent") or {}).get("model_name")) or ""
    model_sent = normalize_model_for_gateway(model_name)
    strategy = infer_strategy(model_sent, job_dir.name)
    duration = duration_seconds(result.get("started_at"), result.get("finished_at"))

    agent_count = len(agent_traces) or usage["agent_call_count"]
    sol_calls = sum(1 for t in agent_traces if t.get("routed_model") == "openai/gpt-5.6-sol")
    opus_calls = sum(1 for t in agent_traces if t.get("routed_model") == "anthropic/claude-opus-5")
    guardrail_calls = sum(
        1 for t in agent_traces if str(t.get("routing_reason") or "").startswith("smart-router guardrail")
    )
    warm_start_calls = sum(
        1 for t in agent_traces if str(t.get("routing_reason") or "").startswith("smart-router warm-start:")
    )
    fallback_to_opus_calls = sum(
        1
        for t in agent_traces
        if t.get("routed_model") == "anthropic/claude-opus-5"
        and str(t.get("routing_reason") or "").startswith("smart-router fallback=")
    )
    opus_guardrail_calls = sum(
        1
        for t in agent_traces
        if t.get("routed_model") == "anthropic/claude-opus-5"
        and str(t.get("routing_reason") or "").startswith("smart-router guardrail")
    )
    opus_selected_calls = max(opus_calls - fallback_to_opus_calls - opus_guardrail_calls, 0)
    premium_calls = sol_calls + opus_calls
    cached_calls = sum(1 for t in agent_traces if str(t.get("routing_reason") or "").startswith("cached:"))
    provider_error = any(int(t.get("status") or 0) >= 500 for t in agent_traces)
    early_failure_marker = job_dir / "early-provider-failure.json"
    if early_failure_marker.exists():
        provider_error = True

    exception_info = result.get("exception_info") or {}
    exception_type = exception_info.get("exception_type") or ""
    if reward is None and exception_info:
        reward = 0.0
    failure_kind = classify_failure(reward, provider_error, exception_type)

    return {
        "experiment_id": artifact_dir.name,
        "task": task_name(result),
        "attempt": infer_attempt(job_dir.name),
        "strategy": strategy,
        "model_sent": model_sent,
        "agent_models": ";".join(agent_models),
        "reward": reward,
        "total_cost_usd": round(agent_cost_usd + decision_cost, 8),
        "agent_cost_usd": round(agent_cost_usd, 8),
        "decision_cost_usd": round(decision_cost, 8),
        "prompt_tokens": usage["prompt_tokens"] + decision_prompt,
        "completion_tokens": usage["completion_tokens"] + decision_completion,
        "total_tokens": usage["total_tokens"] + decision_prompt + decision_completion,
        "agent_call_count": agent_count,
        "decision_call_count": len(decision_traces),
        "guardrail_call_count": guardrail_calls,
        "warm_start_call_count": warm_start_calls,
        "premium_upgrade_rate": round(premium_calls / agent_count, 4) if agent_count else "",
        "sol_upgrade_rate": round(sol_calls / agent_count, 4) if agent_count else "",
        "opus_selected_rate": round(opus_selected_calls / agent_count, 4) if agent_count else "",
        "fallback_to_opus_rate": round(fallback_to_opus_calls / agent_count, 4) if agent_count else "",
        "opus_fallback_rate": round(fallback_to_opus_calls / agent_count, 4) if agent_count else "",
        "cache_hit_rate": round(cached_calls / agent_count, 4) if agent_count else "",
        "duration_seconds": duration,
        "trace_key": session_id if traces else "",
        "failure_kind": failure_kind,
        "exception_type": exception_type,
        "cost_source": cost_source,
    }


def build_wall_clock_cap_row(
    artifact_dir: Path,
    job_dir: Path,
    marker_path: Path,
    traces_by_session: dict[str, list[dict[str, Any]]],
    prices: dict[str, tuple[float, float]],
) -> dict[str, Any]:
    marker = json.loads(marker_path.read_text())
    trial_name = marker.get("trial_name") or infer_trial_name(job_dir)
    synthetic_result = {
        "trial_name": trial_name,
        "task_name": f"terminal-bench/{marker.get('task') or ''}",
        "config": {"agent": {"model_name": marker.get("harbor_model") or ""}},
        "started_at": marker.get("started_at") or "",
        "finished_at": marker.get("detected_at") or "",
        "exception_info": {"exception_type": "wall_clock_cap"},
    }
    synthetic_result_path = job_dir / trial_name / "result.json"
    row = build_row(artifact_dir, synthetic_result_path, synthetic_result, traces_by_session, prices)
    row["attempt"] = marker.get("attempt") or row["attempt"]
    row["strategy"] = marker.get("strategy") or row["strategy"]
    row["model_sent"] = marker.get("gateway_model") or row["model_sent"]
    row["reward"] = 0.0
    row["failure_kind"] = "wall_clock_cap"
    if marker.get("session_id") and not row.get("trace_key"):
        row["trace_key"] = marker["session_id"]
    return row


def infer_trial_name(job_dir: Path) -> str:
    for child in sorted(job_dir.iterdir()):
        if child.is_dir() and (child / "agent").is_dir():
            return child.name
    return job_dir.name


def load_trajectories(agent_dir: Path) -> list[dict[str, Any]]:
    paths = sorted(agent_dir.glob("trajectory*.json"))
    steps: list[dict[str, Any]] = []
    for path in paths:
        data = json.loads(path.read_text())
        if isinstance(data, list):
            steps.extend(data)
        else:
            steps.extend(data.get("steps", []))
    return steps


def aggregate_agent_usage(steps: list[dict[str, Any]], prices: dict[str, tuple[float, float]]) -> dict[str, Any]:
    prompt = 0
    completion = 0
    cost = 0.0
    calls = 0
    recomputed = 0
    for step in steps:
        if step.get("source") != "agent":
            continue
        metrics = step.get("metrics") or {}
        model = step.get("model_name") or ""
        pt = int(metrics.get("prompt_tokens") or 0)
        ct = int(metrics.get("completion_tokens") or 0)
        prompt += pt
        completion += ct
        calls += 1
        computed_cost = compute_cost(model, pt, ct, prices)
        reported_cost = metrics.get("cost_usd")
        if reported_cost is None or (float(reported_cost) == 0 and (pt or ct) and computed_cost > 0):
            cost += computed_cost
            recomputed += 1
        else:
            cost += float(reported_cost)
    return {
        "prompt_tokens": prompt,
        "completion_tokens": completion,
        "total_tokens": prompt + completion,
        "agent_call_count": calls,
        "agent_cost_usd": cost,
        "cost_source": "trajectory_mixed_recomputed" if recomputed else "trajectory_cost_usd",
    }


def aggregate_agent_trace_cost(traces: list[dict[str, Any]]) -> dict[str, Any]:
    cost = 0.0
    token_calls = 0
    costed_token_calls = 0
    for trace in traces:
        total_tokens = int(trace.get("total_tokens") or 0)
        trace_cost = float(trace.get("cost") or 0)
        cost += trace_cost
        if total_tokens > 0:
            token_calls += 1
            if trace_cost > 0:
                costed_token_calls += 1
    return {
        "agent_cost_usd": cost,
        "complete": token_calls > 0 and token_calls == costed_token_calls,
    }


def compute_cost(model: str, prompt_tokens: int, completion_tokens: int, prices: dict[str, tuple[float, float]]) -> float:
    prompt_price, completion_price = prices.get(normalize_model_for_gateway(model), (0.0, 0.0))
    return (prompt_tokens * prompt_price + completion_tokens * completion_price) / 1_000_000


def normalize_model_for_gateway(model: str) -> str:
    if model in HARBOR_MODEL_TO_GATEWAY:
        return HARBOR_MODEL_TO_GATEWAY[model]
    return model


def infer_strategy(model_sent: str, job_name: str) -> str:
    for strategy in ("smart-router-warmstart", "all-premium", "all-flash", "smart-router"):
        if strategy in job_name:
            return strategy
    if model_sent == "auto-opus-warmstart":
        return "smart-router-warmstart"
    if model_sent == "auto":
        return "smart-router"
    if model_sent == "openai/gpt-5.6-sol":
        return "all-premium"
    if model_sent == "z-ai/glm-5.3-flash":
        return "all-flash"
    return ""


def infer_attempt(job_name: str) -> str:
    marker = "-a"
    idx = job_name.rfind(marker)
    if idx < 0:
        return ""
    suffix = job_name[idx + len(marker) :]
    return suffix if suffix.isdigit() else ""


def task_name(result: dict[str, Any]) -> str:
    raw = result.get("task_name") or ""
    return raw.rsplit("/", 1)[-1]


def classify_failure(reward: Any, provider_error: bool, exception_type: str) -> str:
    if provider_error:
        return "provider_5xx"
    if exception_type == "wall_clock_cap":
        return "wall_clock_cap"
    if exception_type:
        return "exception"
    try:
        if float(reward) >= 1.0:
            return "pass"
        return "verifier_failed"
    except (TypeError, ValueError):
        return "unknown"


def duration_seconds(start: str | None, end: str | None) -> str:
    if not start or not end:
        return ""
    return str(round((parse_time(end) - parse_time(start)).total_seconds(), 3))


def parse_time(value: str) -> datetime:
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    return datetime.fromisoformat(value)


def validate_rows(rows: list[dict[str, Any]], expected_rows: int | None) -> list[str]:
    problems = []
    if expected_rows is not None and len(rows) != expected_rows:
        problems.append(f"expected {expected_rows} rows, got {len(rows)}")
    for row in rows:
        label = f"{row.get('strategy')}/{row.get('task')}/a{row.get('attempt') or '?'}"
        if not row.get("trace_key"):
            problems.append(f"{label} has no matching trace_key")
        if int(row.get("agent_call_count") or 0) == 0:
            problems.append(f"{label} has no agent calls")
        try:
            reward = float(row.get("reward"))
            if reward not in (0.0, 1.0):
                problems.append(f"{label} has non-binary reward {row.get('reward')!r}")
        except (TypeError, ValueError):
            problems.append(f"{label} has missing/non-numeric reward {row.get('reward')!r}")
        if row.get("strategy") == "smart-router" and int(row.get("decision_call_count") or 0) == 0:
            problems.append(f"{label} has no router decision calls")
        expected_model = ""
        if row.get("strategy") in ("all-premium", "all-flash"):
            expected_model = str(row.get("model_sent") or "")
        agent_models = [m for m in str(row.get("agent_models") or "").split(";") if m]
        if expected_model and any(model != expected_model for model in agent_models):
            problems.append(f"{label} used {agent_models}, expected only {expected_model}")
    return problems


if __name__ == "__main__":
    main()
