#!/usr/bin/env python3
"""Extract RSI episode outcome events from Harbor artifacts and gateway traces."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import yaml


EXTRACTOR_VERSION = "outcome-extractor-v1"
EVENT_SCHEMA_VERSION = "event-schema-v1"
PROGRESS_RULES_VERSION = "progress-rules-v1"

PATCH_DIFF_RE = re.compile(r"^diff --git a/(.*?) b/(.*?)$")


def main() -> None:
    args = parse_args()
    schema = read_json(args.event_schema)
    rules = read_yaml(args.progress_rules)
    validate_schema_and_rules(schema, rules)

    trial_dir = resolve_trial_dir(args.trial_dir)
    result = read_json(trial_dir / "result.json") if (trial_dir / "result.json").exists() else {}
    trajectory = load_trajectory(trial_dir / "agent")
    traces = load_traces(args.traces_json)
    episode_id = infer_episode_id(trial_dir, result, traces)
    session_id = f"{episode_id}__agent"
    traces = filter_episode_traces(traces, episode_id, session_id)

    events = extract_base_events(trial_dir, result, trajectory, traces, episode_id)
    events.extend(derive_no_progress_events(events, episode_id, rules))
    events = sorted_events(events)
    for sequence, event in enumerate(events):
        event["sequence"] = sequence

    decisions = [trace for trace in traces if is_decision_trace(trace)]
    cutoff_check = build_replay_cutoff_check(events, decisions, episode_id)
    summary = build_summary(trial_dir, result, trajectory, traces, events, cutoff_check)

    args.output_dir.mkdir(parents=True, exist_ok=True)
    write_jsonl(args.output_dir / "episode-events.jsonl", events)
    write_json(args.output_dir / "episode-summary.json", summary)
    write_json(args.output_dir / "replay-cutoff-check.json", cutoff_check)

    if args.strict:
        problems = validate_outputs(events, cutoff_check)
        if problems:
            for problem in problems:
                print(f"strict validation failed: {problem}", file=sys.stderr)
            raise SystemExit(1)

    print(
        "wrote "
        f"{len(events)} events, {len(cutoff_check['samples'])} cutoff samples "
        f"to {args.output_dir}"
    )


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Build RSI episode-events.jsonl, episode-summary.json, and replay cutoff checks."
    )
    parser.add_argument(
        "--trial-dir",
        required=True,
        type=Path,
        help="Harbor trial directory, or a job directory containing one trial.",
    )
    parser.add_argument(
        "--traces-json",
        action="append",
        type=Path,
        default=[],
        help="Gateway traces JSON. May be passed multiple times.",
    )
    parser.add_argument("--output-dir", required=True, type=Path)
    parser.add_argument("--event-schema", default=Path("docs/rsi/event-schema-v1.json"), type=Path)
    parser.add_argument("--progress-rules", default=Path("docs/rsi/progress-rules-v1.yaml"), type=Path)
    parser.add_argument("--strict", action="store_true")
    return parser.parse_args()


def read_json(path: Path) -> Any:
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def read_yaml(path: Path) -> dict[str, Any]:
    with path.open(encoding="utf-8") as handle:
        return yaml.safe_load(handle) or {}


def validate_schema_and_rules(schema: dict[str, Any], rules: dict[str, Any]) -> None:
    if schema.get("properties", {}).get("schema_version", {}).get("const") != EVENT_SCHEMA_VERSION:
        raise SystemExit(f"unexpected event schema version in {schema}")
    if rules.get("schema_version") != PROGRESS_RULES_VERSION:
        raise SystemExit(f"unexpected progress rules version in {rules}")


def resolve_trial_dir(path: Path) -> Path:
    if (path / "agent").is_dir():
        return path
    matches = sorted(path.glob("*/agent/trajectory*.json"))
    if matches:
        return matches[0].parent.parent
    raise SystemExit(f"could not resolve Harbor trial directory from {path}")


def load_trajectory(agent_dir: Path) -> list[dict[str, Any]]:
    steps: list[dict[str, Any]] = []
    for path in sorted(agent_dir.glob("trajectory*.json")):
        payload = read_json(path)
        raw_steps = payload if isinstance(payload, list) else payload.get("steps") or []
        for index, step in enumerate(raw_steps):
            if not isinstance(step, dict):
                continue
            annotated = dict(step)
            annotated.setdefault("_trajectory_ref", f"trajectory:{path}#step:{index}")
            steps.append(annotated)
    return steps


def load_traces(paths: list[Path]) -> list[dict[str, Any]]:
    traces: list[dict[str, Any]] = []
    for path in paths:
        payload = read_json(path)
        if isinstance(payload, list):
            traces.extend(payload)
        else:
            traces.extend(payload.get("traces") or [])
    return sorted(traces, key=lambda trace: trace.get("timestamp") or "")


def infer_episode_id(trial_dir: Path, result: dict[str, Any], traces: list[dict[str, Any]]) -> str:
    if result.get("trial_name"):
        return str(result["trial_name"])
    for trace in traces:
        if trace.get("trial_name"):
            return str(trace["trial_name"])
        session_id = str(trace.get("session_id") or "")
        if session_id.endswith("__agent"):
            return session_id[: -len("__agent")]
    return trial_dir.name


def filter_episode_traces(traces: list[dict[str, Any]], episode_id: str, session_id: str) -> list[dict[str, Any]]:
    if not traces:
        return []
    filtered = [
        trace
        for trace in traces
        if trace.get("session_id") == session_id or trace.get("trial_name") == episode_id
    ]
    return filtered or traces


def extract_base_events(
    trial_dir: Path,
    result: dict[str, Any],
    trajectory: list[dict[str, Any]],
    traces: list[dict[str, Any]],
    episode_id: str,
) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    agent_traces = [trace for trace in traces if not is_decision_trace(trace)]
    if agent_traces:
        for index, trace in enumerate(agent_traces):
            events.append(llm_call_event(episode_id, trace, index))
    else:
        for index, step in enumerate(step for step in trajectory if step.get("source") == "agent"):
            events.append(trajectory_llm_call_event(episode_id, step, index))

    patch_path = trial_dir / "artifacts" / "tmp" / "agent.patch"
    if patch_path.exists() and patch_path.stat().st_size > 0:
        events.append(file_modified_event(episode_id, patch_path, result, traces))

    ctrf_path = trial_dir / "verifier" / "ctrf.json"
    if ctrf_path.exists():
        ctrf = read_json(ctrf_path)
        event = test_event_from_ctrf(episode_id, ctrf, result, ctrf_path)
        if event:
            events.append(event)

    verifier = verifier_result_event(episode_id, result, ctrf_path)
    if verifier:
        events.append(verifier)

    exception = result.get("exception_info")
    if exception:
        events.append(run_exception_event(episode_id, result, exception))

    return events


def llm_call_event(episode_id: str, trace: dict[str, Any], index: int) -> dict[str, Any]:
    timestamp = normalize_timestamp(str(trace.get("timestamp") or ""))
    outcome = normalize_llm_outcome(trace)
    trace_id = str(trace.get("trace_id") or "")
    evidence = f"trace:{trace_id}" if trace_id else f"trace:index:{index}"
    observation = {
        "trace_id": trace_id,
        "status": as_int(trace.get("status")),
        "finish_reason": str(trace.get("finish_reason") or ""),
        "outcome": outcome,
        "model": str(trace.get("model") or ""),
        "routed_model": str(trace.get("routed_model") or ""),
        "pool": str(trace.get("pool") or ""),
        "budget_action": str(trace.get("route_budget_action") or ""),
        "route_max_tokens": as_int(trace.get("route_max_tokens")),
        "route_timeout_ms": as_int(trace.get("route_timeout_ms")),
        "prompt_tokens": as_int(trace.get("prompt_tokens")),
        "completion_tokens": as_int(trace.get("completion_tokens")),
        "total_tokens": as_int(trace.get("total_tokens")),
        "cost_usd": as_float(trace.get("cost")),
        "latency_ms": as_int(trace.get("latency_ms")),
        "episode_adjust": "episode_adjust=" in str(trace.get("routing_reason") or ""),
        "routing_reason": str(trace.get("routing_reason") or ""),
    }
    return make_event(
        episode_id,
        "llm_call",
        timestamp,
        "gateway_trace",
        observation,
        [evidence],
        "observed",
        "trace.timestamp",
    )


def normalize_llm_outcome(trace: dict[str, Any]) -> str:
    status = as_int(trace.get("status"))
    finish = str(trace.get("finish_reason") or "").strip().lower()
    if status >= 400 or trace.get("error_kind"):
        return "error"
    if finish == "length":
        return "length_truncated"
    if finish == "stop":
        return "response_completed"
    if 200 <= status < 300 and not finish and as_int(trace.get("total_tokens")) == 0:
        return "provider_incomplete"
    return "unknown"


def trajectory_llm_call_event(episode_id: str, step: dict[str, Any], index: int) -> dict[str, Any]:
    timestamp = normalize_timestamp(str(step.get("timestamp") or ""))
    metrics = step.get("metrics") or {}
    model = str(step.get("model_name") or "")
    message = str(step.get("message") or "")
    observation = {
        "trace_id": "",
        "status": 0,
        "finish_reason": "",
        "outcome": "unknown",
        "model": model,
        "routed_model": model,
        "pool": "harbor_trajectory",
        "budget_action": "",
        "route_max_tokens": 0,
        "route_timeout_ms": 0,
        "prompt_tokens": as_int(metrics.get("prompt_tokens")),
        "completion_tokens": as_int(metrics.get("completion_tokens")),
        "total_tokens": as_int(metrics.get("total_tokens"))
        or as_int(metrics.get("prompt_tokens")) + as_int(metrics.get("completion_tokens")),
        "cost_usd": 0.0,
        "latency_ms": 0,
        "episode_adjust": "episode_adjust=" in message,
        "routing_reason": "",
        "message_chars": len(message),
    }
    return make_event(
        episode_id,
        "llm_call",
        timestamp,
        "harbor_trajectory",
        observation,
        [str(step.get("_trajectory_ref") or f"trajectory:index:{index}")],
        "observed",
        "trajectory.timestamp",
    )


def file_modified_event(
    episode_id: str,
    patch_path: Path,
    result: dict[str, Any],
    traces: list[dict[str, Any]],
) -> dict[str, Any]:
    patch = patch_path.read_text(encoding="utf-8", errors="replace")
    changed_paths = changed_paths_from_patch(patch)
    timestamp = first_timestamp(
        nested_get(result, ["agent_execution", "finished_at"]),
        latest_agent_trace_timestamp(traces),
        result.get("finished_at"),
    )
    observation = {
        "path_count": len(changed_paths),
        "changed_paths_summary": changed_paths[:12],
        "paths": changed_paths,
        "patch_bytes": len(patch.encode("utf-8")),
    }
    return make_event(
        episode_id,
        "file_modified",
        timestamp,
        "agent_patch",
        observation,
        [f"agent.patch:{patch_path}"],
        "observed",
        "agent_execution.finished_at",
    )


def changed_paths_from_patch(patch: str) -> list[str]:
    paths: list[str] = []
    for line in patch.splitlines():
        match = PATCH_DIFF_RE.match(line)
        if not match:
            continue
        old, new = match.groups()
        path = new if new != "/dev/null" else old
        if path and path not in paths:
            paths.append(path)
    return paths


def test_event_from_ctrf(
    episode_id: str,
    ctrf: dict[str, Any],
    result: dict[str, Any],
    ctrf_path: Path,
) -> dict[str, Any] | None:
    results = ctrf.get("results") or {}
    summary = results.get("summary") or {}
    tests = results.get("tests") or []
    total = as_int(summary.get("tests"))
    if total <= 0:
        return None
    failed = as_int(summary.get("failed"))
    passed = as_int(summary.get("passed"))
    kind = "test_failed" if failed else "test_passed"
    timestamp = timestamp_from_epoch_ms(summary.get("stop")) or first_timestamp(
        nested_get(result, ["verifier", "finished_at"]),
        result.get("finished_at"),
    )
    failures = [
        str(test.get("name") or "")
        for test in tests
        if str(test.get("status") or "").lower() not in ("passed", "skipped")
    ]
    observation = {
        "command": "verifier",
        "tests": total,
        "passed_count": passed,
        "failed_count": failed,
        "failure_fingerprints": failures[:20],
    }
    return make_event(
        episode_id,
        kind,
        timestamp,
        "verifier_ctrf",
        observation,
        [f"ctrf:{ctrf_path}"],
        "observed",
        "ctrf.results.summary.stop",
    )


def verifier_result_event(episode_id: str, result: dict[str, Any], ctrf_path: Path) -> dict[str, Any] | None:
    reward = nested_get(result, ["verifier_result", "rewards", "reward"])
    reward_path = ctrf_path.parent / "reward.txt"
    if reward is None and reward_path.exists():
        text = reward_path.read_text(encoding="utf-8", errors="replace").strip()
        reward = as_float(text, None)
    if reward is None:
        return None

    ctrf_summary: dict[str, Any] = {}
    if ctrf_path.exists():
        ctrf_summary = ((read_json(ctrf_path).get("results") or {}).get("summary") or {})
    timestamp = first_timestamp(
        nested_get(result, ["verifier", "finished_at"]),
        result.get("finished_at"),
    )
    observation = {
        "reward": as_float(reward),
        "passed_count": as_int(ctrf_summary.get("passed")),
        "failed_count": as_int(ctrf_summary.get("failed")),
    }
    evidence = ["result.json:verifier_result"]
    if reward_path.exists():
        evidence.append(f"reward:{reward_path}")
    if ctrf_path.exists():
        evidence.append(f"ctrf:{ctrf_path}")
    return make_event(
        episode_id,
        "verifier_result",
        timestamp,
        "harbor_result",
        observation,
        evidence,
        "observed",
        "verifier.finished_at",
    )


def run_exception_event(episode_id: str, result: dict[str, Any], exception: dict[str, Any]) -> dict[str, Any]:
    timestamp = first_timestamp(result.get("finished_at"), result.get("updated_at"), result.get("started_at"))
    observation = {
        "exception_type": str(exception.get("exception_type") or ""),
        "message": str(exception.get("message") or exception.get("exception_message") or ""),
    }
    return make_event(
        episode_id,
        "run_exception",
        timestamp,
        "harbor_result",
        observation,
        ["result.json:exception_info"],
        "observed",
        "result.finished_at",
    )


def derive_no_progress_events(
    events: list[dict[str, Any]],
    episode_id: str,
    rules: dict[str, Any],
) -> list[dict[str, Any]]:
    threshold = as_int(nested_get(rules, ["no_progress", "length_pressure_without_progress_threshold"]), 3)
    threshold = max(1, threshold)
    derived: list[dict[str, Any]] = []
    length_pressure_without_progress = 0
    emitted_open_no_progress = False

    for event in sorted_events(events):
        if is_progress_event(event):
            length_pressure_without_progress = 0
            emitted_open_no_progress = False
            continue
        if event["kind"] != "llm_call":
            continue
        observation = event.get("observation") or {}
        has_length_pressure = observation.get("outcome") == "length_truncated" or bool(
            observation.get("episode_adjust")
        )
        if not has_length_pressure:
            continue
        length_pressure_without_progress += 1
        if length_pressure_without_progress < threshold or emitted_open_no_progress:
            continue
        derived.append(
            make_event(
                episode_id,
                "no_progress",
                event["timestamp"],
                "progress_reducer",
                {
                    "reason": "length_pressure_without_progress",
                    "since_turn": length_pressure_without_progress,
                    "threshold": threshold,
                    "trigger_event_id": event["event_id"],
                },
                [event["event_id"]],
                "derived",
                "trigger_event.timestamp",
            )
        )
        emitted_open_no_progress = True
    return derived


def is_progress_event(event: dict[str, Any]) -> bool:
    kind = event.get("kind")
    observation = event.get("observation") or {}
    if kind == "test_passed" and as_int(observation.get("failed_count")) == 0:
        return True
    if kind == "verifier_result" and as_float(observation.get("reward")) > 0:
        return True
    return False


def build_replay_cutoff_check(
    events: list[dict[str, Any]],
    decisions: list[dict[str, Any]],
    episode_id: str,
) -> dict[str, Any]:
    samples: list[dict[str, Any]] = []
    violations: list[dict[str, Any]] = []
    for index, decision in enumerate(sorted(decisions, key=lambda trace: trace.get("timestamp") or ""), start=1):
        decision_timestamp = normalize_timestamp(str(decision.get("timestamp") or ""))
        cutoff = parse_dt(decision_timestamp)
        allowed = [event for event in events if parse_dt(event["timestamp"]) < cutoff]
        leaked = [event for event in allowed if parse_dt(event["timestamp"]) >= cutoff]
        if leaked:
            violations.append(
                {
                    "decision_id": decision_id(decision, index),
                    "leaked_event_ids": [event["event_id"] for event in leaked],
                }
            )
        samples.append(
            {
                "decision_id": decision_id(decision, index),
                "decision_timestamp": decision_timestamp,
                "event_cutoff": decision_timestamp,
                "state_before": reduce_state(allowed),
                "allowed_evidence_refs": sorted(
                    {
                        ref
                        for event in allowed
                        for ref in event.get("evidence_refs", [])
                    }
                ),
                "original_decision": {
                    "trace_id": decision.get("trace_id") or "",
                    "model": decision.get("routed_model") or decision.get("model") or "",
                    "routing_reason": decision.get("routing_reason") or "",
                },
                "candidate_decision": None,
                "future_evidence_leakage": len(leaked),
            }
        )
    return {
        "schema_version": "replay-cutoff-check-v1",
        "episode_id": episode_id,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "decision_count": len(samples),
        "future_evidence_leakage": sum(sample["future_evidence_leakage"] for sample in samples),
        "violations": violations,
        "samples": samples,
    }


def reduce_state(events: list[dict[str, Any]]) -> dict[str, Any]:
    llm_events = [event for event in events if event.get("kind") == "llm_call"]
    outcomes = Counter((event.get("observation") or {}).get("outcome") or "unknown" for event in llm_events)
    models = Counter((event.get("observation") or {}).get("routed_model") or "unknown" for event in llm_events)
    budget_actions = Counter((event.get("observation") or {}).get("budget_action") or "" for event in llm_events)
    progress_events = [event for event in events if is_progress_event(event)]
    last_event = events[-1] if events else {}
    return {
        "event_count": len(events),
        "llm_call_count": len(llm_events),
        "cost_usd": round(sum(as_float((event.get("observation") or {}).get("cost_usd")) for event in llm_events), 8),
        "outcomes": dict(sorted(outcomes.items())),
        "models": dict(sorted(models.items())),
        "budget_actions": dict(sorted((k, v) for k, v in budget_actions.items() if k)),
        "progress_event_count": len(progress_events),
        "no_progress_event_count": sum(1 for event in events if event.get("kind") == "no_progress"),
        "last_event_id": last_event.get("event_id", ""),
        "last_event_kind": last_event.get("kind", ""),
    }


def build_summary(
    trial_dir: Path,
    result: dict[str, Any],
    trajectory: list[dict[str, Any]],
    traces: list[dict[str, Any]],
    events: list[dict[str, Any]],
    cutoff_check: dict[str, Any],
) -> dict[str, Any]:
    by_kind = Counter(event["kind"] for event in events)
    llm_events = [event for event in events if event["kind"] == "llm_call"]
    outcomes = Counter((event["observation"] or {}).get("outcome") or "unknown" for event in llm_events)
    models = Counter((event["observation"] or {}).get("routed_model") or "unknown" for event in llm_events)
    reward = next(
        (
            (event["observation"] or {}).get("reward")
            for event in events
            if event.get("kind") == "verifier_result"
        ),
        None,
    )
    total_cost = sum(as_float((event.get("observation") or {}).get("cost_usd")) for event in llm_events)
    agent_call_count = len(llm_events)
    length_count = outcomes.get("length_truncated", 0)
    return {
        "schema_version": "episode-summary-v1",
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "episode_id": infer_episode_id(trial_dir, result, traces),
        "trial_dir": str(trial_dir),
        "task_name": result.get("task_name") or "",
        "trial_name": result.get("trial_name") or "",
        "event_schema": EVENT_SCHEMA_VERSION,
        "progress_rules": PROGRESS_RULES_VERSION,
        "event_count": len(events),
        "event_counts": dict(sorted(by_kind.items())),
        "llm_outcomes": dict(sorted(outcomes.items())),
        "model_counts": dict(sorted(models.items())),
        "agent_call_count": agent_call_count,
        "decision_call_count": sum(1 for trace in traces if is_decision_trace(trace)),
        "trajectory_agent_turn_count": sum(1 for step in trajectory if step.get("source") == "agent"),
        "total_cost_usd": round(total_cost, 8),
        "length_finish_count": length_count,
        "length_finish_rate": round(length_count / agent_call_count, 4) if agent_call_count else 0,
        "episode_adjust_call_count": sum(
            1 for event in llm_events if (event.get("observation") or {}).get("episode_adjust")
        ),
        "progress_event_count": sum(1 for event in events if is_progress_event(event)),
        "no_progress_turn_count": by_kind.get("no_progress", 0),
        "provider_incomplete_count": outcomes.get("provider_incomplete", 0),
        "reward": reward,
        "future_evidence_leakage": cutoff_check["future_evidence_leakage"],
    }


def validate_outputs(events: list[dict[str, Any]], cutoff_check: dict[str, Any]) -> list[str]:
    problems: list[str] = []
    for event in events:
        for field in (
            "schema_version",
            "event_id",
            "episode_id",
            "timestamp",
            "kind",
            "source",
            "observation",
            "evidence_refs",
            "certainty",
            "extractor_version",
        ):
            if field not in event:
                problems.append(f"{event.get('event_id', '<missing>')} missing {field}")
        if event.get("schema_version") != EVENT_SCHEMA_VERSION:
            problems.append(f"{event.get('event_id')} has wrong schema version")
        if not event.get("evidence_refs"):
            problems.append(f"{event.get('event_id')} has no evidence refs")
    if cutoff_check.get("future_evidence_leakage") != 0:
        problems.append(f"future evidence leakage = {cutoff_check.get('future_evidence_leakage')}")
    return problems


def make_event(
    episode_id: str,
    kind: str,
    timestamp: str,
    source: str,
    observation: dict[str, Any],
    evidence_refs: list[str],
    certainty: str,
    timestamp_source: str,
) -> dict[str, Any]:
    timestamp = normalize_timestamp(timestamp)
    seed = json.dumps(
        {
            "episode_id": episode_id,
            "kind": kind,
            "timestamp": timestamp,
            "source": source,
            "observation": observation,
            "evidence_refs": evidence_refs,
            "certainty": certainty,
        },
        ensure_ascii=False,
        sort_keys=True,
    )
    event_id = "evt_" + hashlib.sha1(seed.encode("utf-8")).hexdigest()[:16]
    return {
        "schema_version": EVENT_SCHEMA_VERSION,
        "event_id": event_id,
        "episode_id": episode_id,
        "timestamp": timestamp,
        "timestamp_source": timestamp_source,
        "kind": kind,
        "source": source,
        "observation": observation,
        "evidence_refs": evidence_refs,
        "certainty": certainty,
        "extractor_version": EXTRACTOR_VERSION,
    }


def is_decision_trace(trace: dict[str, Any]) -> bool:
    return (trace.get("pool") or "") == "decision-model" or str(
        trace.get("step_name") or ""
    ).startswith("router-decision")


def sorted_events(events: list[dict[str, Any]]) -> list[dict[str, Any]]:
    return sorted(events, key=lambda event: (event.get("timestamp") or "", event.get("event_id") or ""))


def latest_agent_trace_timestamp(traces: list[dict[str, Any]]) -> str:
    timestamps = [str(trace.get("timestamp") or "") for trace in traces if not is_decision_trace(trace)]
    return max(timestamps) if timestamps else ""


def decision_id(decision: dict[str, Any], index: int) -> str:
    trace_id = decision.get("trace_id")
    if trace_id:
        return f"decision:{trace_id}"
    return f"decision:index:{index}"


def first_timestamp(*values: Any) -> str:
    for value in values:
        if value:
            return normalize_timestamp(str(value))
    return datetime.now(timezone.utc).isoformat()


def normalize_timestamp(value: str) -> str:
    if not value:
        return datetime.now(timezone.utc).isoformat()
    return parse_dt(value).isoformat().replace("+00:00", "Z")


def parse_dt(value: str) -> datetime:
    value = value.strip()
    if not value:
        return datetime.now(timezone.utc)
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(value)
    except ValueError:
        parsed = datetime.fromtimestamp(float(value), tz=timezone.utc)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def timestamp_from_epoch_ms(value: Any) -> str:
    if value is None:
        return ""
    try:
        return datetime.fromtimestamp(float(value) / 1000.0, tz=timezone.utc).isoformat().replace("+00:00", "Z")
    except (TypeError, ValueError, OSError):
        return ""


def nested_get(data: dict[str, Any], keys: list[str]) -> Any:
    current: Any = data
    for key in keys:
        if not isinstance(current, dict):
            return None
        current = current.get(key)
    return current


def as_int(value: Any, default: int = 0) -> int:
    try:
        if value is None or value == "":
            return default
        return int(float(value))
    except (TypeError, ValueError):
        return default


def as_float(value: Any, default: float | None = 0.0) -> float | None:
    try:
        if value is None or value == "":
            return default
        return float(value)
    except (TypeError, ValueError):
        return default


def write_json(path: Path, payload: Any) -> None:
    with path.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=2, sort_keys=True)
        handle.write("\n")


def write_jsonl(path: Path, rows: list[dict[str, Any]]) -> None:
    with path.open("w", encoding="utf-8") as handle:
        for row in rows:
            handle.write(json.dumps(row, ensure_ascii=False, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
