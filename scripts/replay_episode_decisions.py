#!/usr/bin/env python3
"""Replay router decisions with offline RSI episode outcome state."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import requests


MENU = [
    {
        "name": "z-ai/glm-5.3-flash",
        "tier": "ultra-cheap",
        "input_price_usd_per_1m": 0.07,
        "output_price_usd_per_1m": 0.25,
        "ability_prior": "about 75% of Opus for general intelligence, reasoning, and coding",
    },
    {
        "name": "anthropic/claude-opus-5",
        "tier": "strongest-premium",
        "input_price_usd_per_1m": 5.00,
        "output_price_usd_per_1m": 25.00,
        "ability_prior": "strongest option; use when this turn can protect final quality or avoid expensive rework",
    },
]
VALID_MODELS = {item["name"] for item in MENU}
VALID_BUDGET_ACTIONS = {
    "cheap_probe",
    "cheap_execute",
    "premium_reason",
    "premium_recover",
    "completion_guardrail",
    "freeze_or_replan",
    "hypothesis_apply",
}


def main() -> None:
    args = parse_args()
    api_key = read_api_key(args)
    if not api_key and not args.dry_run:
        print(f"missing API key: set {args.api_key_env} or create {args.api_key_file}", file=sys.stderr)
        raise SystemExit(2)

    episode_dirs = [Path(path).resolve() for path in args.episode_dir]
    samples = []
    for episode_dir in episode_dirs:
        samples.extend(load_episode_samples(episode_dir, args.recent_events))
    samples = sorted(samples, key=lambda sample: (sample["episode_id"], sample["decision_timestamp"]))
    if args.limit:
        samples = samples[: args.limit]

    output_path = Path(args.output).resolve()
    output_path.parent.mkdir(parents=True, exist_ok=True)
    existing_rows = []
    done_keys = set()
    if args.resume and output_path.exists():
        existing = read_json(output_path)
        loaded_rows = existing.get("replay_decisions", [])
        done_keys = {
            (row.get("episode_id"), row.get("decision_id"))
            for row in loaded_rows
            if row.get("status") == 200 and row.get("candidate_model")
        }
        existing_rows = [
            row
            for row in loaded_rows
            if (row.get("episode_id"), row.get("decision_id")) in done_keys
        ]

    rows = list(existing_rows)
    todo = [
        sample
        for sample in samples
        if (sample.get("episode_id"), sample.get("decision_id")) not in done_keys
    ]
    print(f"replaying {len(todo)} decisions; already_done={len(done_keys)} output={output_path}")
    for index, sample in enumerate(todo, start=1):
        try:
            row = dry_run_one(sample, args) if args.dry_run else call_one(sample, args, api_key)
        except Exception as exc:
            row = error_row(sample, repr(exc))
        rows.append(row)
        if index % args.write_every == 0 or index == len(todo):
            write_json(output_path, replay_payload(args, episode_dirs, rows))
            summary = summarize(rows)
            print(
                f"completed={index}/{len(todo)} ok={summary['ok']} "
                f"mix={summary['candidate_model_mix']} evidence={summary['reason_evidence_coverage']:.2f}"
            )
    if not todo:
        write_json(output_path, replay_payload(args, episode_dirs, rows))


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Replay RSI router decisions using episode outcome state.")
    parser.add_argument(
        "--episode-dir",
        action="append",
        required=True,
        help="Directory containing episode-events.jsonl, episode-summary.json, and replay-cutoff-check.json.",
    )
    parser.add_argument("--output", required=True)
    parser.add_argument("--prompt-id", default="rsi-p1-outcome-aware-v1")
    parser.add_argument("--model", default="openai/gpt-5.6-sol")
    parser.add_argument("--endpoint", default="https://openrouter.ai/api/v1")
    parser.add_argument("--api-key-env", default="GW_OPENROUTER_KEY")
    parser.add_argument("--api-key-file", default="openrouter.env")
    parser.add_argument("--recent-events", type=int, default=6)
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--write-every", type=int, default=5)
    return parser.parse_args()


def read_api_key(args: argparse.Namespace) -> str:
    key = os.environ.get(args.api_key_env)
    if key:
        return key.strip()
    path = Path(args.api_key_file)
    if path.exists():
        return path.read_text(encoding="utf-8").strip()
    return ""


def read_json(path: Path) -> Any:
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def read_jsonl(path: Path) -> list[dict[str, Any]]:
    rows = []
    with path.open(encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def load_episode_samples(episode_dir: Path, recent_events: int) -> list[dict[str, Any]]:
    events = read_jsonl(episode_dir / "episode-events.jsonl")
    summary = read_json(episode_dir / "episode-summary.json")
    cutoff = read_json(episode_dir / "replay-cutoff-check.json")
    out = []
    for sample in cutoff.get("samples", []):
        decision_time = parse_dt(sample["decision_timestamp"])
        visible_events = [event for event in events if parse_dt(event["timestamp"]) < decision_time]
        row = dict(sample)
        row["episode_id"] = cutoff.get("episode_id") or summary.get("episode_id") or ""
        row["episode_dir"] = str(episode_dir)
        row["episode_summary"] = summary
        row["recent_events"] = summarize_events(visible_events[-max(0, recent_events) :])
        row["prompt_evidence_refs"] = prompt_evidence_refs(row["recent_events"], row.get("allowed_evidence_refs") or [])
        out.append(row)
    return out


def summarize_events(events: list[dict[str, Any]]) -> list[dict[str, Any]]:
    out = []
    for event in events:
        observation = event.get("observation") or {}
        summary: dict[str, Any] = {
            "event_id": event.get("event_id"),
            "event_ref": f"event:{event.get('event_id')}",
            "timestamp": event.get("timestamp"),
            "kind": event.get("kind"),
            "certainty": event.get("certainty"),
            "evidence_refs": event.get("evidence_refs") or [],
        }
        for key in (
            "outcome",
            "routed_model",
            "budget_action",
            "finish_reason",
            "route_max_tokens",
            "cost_usd",
            "command_kind",
            "command_preview",
            "result_class",
            "output_preview",
            "target_paths_summary",
            "delivery_target",
            "workspace_target",
            "path_count",
            "failed_count",
            "passed_count",
            "reward",
            "reason",
            "since_turn",
            "threshold",
        ):
            if key in observation:
                summary[key] = observation[key]
        out.append(summary)
    return out


def prompt_evidence_refs(recent_events: list[dict[str, Any]], allowed_refs: list[str]) -> list[str]:
    refs: list[str] = []
    for event in recent_events:
        event_ref = str(event.get("event_ref") or "")
        if event_ref and event_ref not in refs:
            refs.append(event_ref)
    if refs:
        return refs[:12]
    return allowed_refs[:12]


def build_prompt(sample: dict[str, Any], prompt_id: str) -> str:
    menu_json = json.dumps(MENU, ensure_ascii=False, indent=2)
    state_json = json.dumps(sample.get("state_before") or {}, ensure_ascii=False, indent=2, sort_keys=True)
    recent_json = json.dumps(sample.get("recent_events") or [], ensure_ascii=False, indent=2, sort_keys=True)
    prompt_refs = json.dumps(sample.get("prompt_evidence_refs") or [], ensure_ascii=False, indent=2)
    variant_guidance = prompt_variant_guidance(prompt_id)
    episode_summary = sample.get("episode_summary") or {}
    summary_json = json.dumps(
        {
            "episode_id": episode_summary.get("episode_id"),
            "task_name": episode_summary.get("task_name"),
            "reward": episode_summary.get("reward"),
            "event_schema": episode_summary.get("event_schema"),
            "progress_rules": episode_summary.get("progress_rules"),
        },
        ensure_ascii=False,
        indent=2,
        sort_keys=True,
    )

    return f"""You are the aware-gateway smart-router replay policy.
Replay one historical router decision using only the evidence visible before that decision.

Prompt variant:
{prompt_id}

Goal:
- Preserve final task quality.
- Minimize whole-trajectory cost per success.
- Speed is diagnostic only; do not optimize for speed.

Model menu:
{menu_json}

Decision principles:
- Flash is cheap and roughly 75% of Opus in general intelligence/reasoning/coding.
- Opus is 71x Flash input cost and 100x Flash output cost.
- Use Flash for reversible probes, narrow edits, mechanical validation, formatting, and stable-hypothesis execution.
- Use Opus when this exact turn sets or revises the core hypothesis, recovers from contradiction, interprets ambiguous validation, designs hidden-case coverage, or performs a final completion guardrail.
- Do not treat response_completed as task completion. It only means one model response ended normally.
- Activity is not progress. Read progress_tier: exploration is only movement, implementation/delivery are candidate progress, validation counts only when tied to the current target, verifier success is strongest.
- If no_progress is present and there is no newer progress event, avoid blind budget expansion. Choose freeze_or_replan or premium_recover only if the reason names a new strategy.
- If state shows repeated length_truncated without progress, decide whether the bottleneck is output room or wrong direction. More tokens alone is not a plan.
- If last_replan_event_id is present and llm_calls_since_replan is growing without implementation/delivery/validation progress, prefer stopping expansion or recovery only with a new concrete pivot.
- If replan_hypothesis_status is open, prefer hypothesis_apply: validate, implement, or explicitly abandon last_replan_hypothesis; avoid broad exploration that does not test it.
- If next_capability_reason is hypothesis_apply_length_truncated, prefer premium_recover for one concise recovery turn that keeps the same hypothesis and restores a valid command or deliverable.
- If next_capability_reason is delivery_candidate_needs_delivery, prefer cheap_execute and convert the current candidate into the required deliverable or run one direct validation check instead of continuing broad analysis.
- If next_capability_reason is delivery_candidate_floor_exhausted, do not keep cheap-looping; use premium_recover to resolve the delivery gap or stop expansion.
{variant_guidance}

Episode summary:
{summary_json}

State available before this decision:
{state_json}

Recent visible events:
{recent_json}

Relevant visible evidence refs you may cite:
{prompt_refs}

Return JSON only:
{{
  "model": "z-ai/glm-5.3-flash | anthropic/claude-opus-5",
  "budget_action": "cheap_probe | cheap_execute | hypothesis_apply | premium_reason | premium_recover | completion_guardrail | freeze_or_replan",
  "progress_state": "unknown | no_progress | candidate_progress | validation_progress | ready_for_completion",
  "critical_path": true,
  "evidence_refs": ["exactly one ref copied exactly from Relevant visible evidence refs"],
  "reason": "under 16 words"
}}

Keep reason short. Cite exactly one evidence ref. Do not cite evidence that is not in Relevant visible evidence refs."""


def prompt_variant_guidance(prompt_id: str) -> str:
    if "p2" not in prompt_id and "window" not in prompt_id:
        return ""
    return """
P2 windowed progress rules:
- Use state.no_progress_window.severity as the current stuck signal: none, watch, stale, or blocked.
- Old no_progress_event_count is background. Do not freeze just because old no_progress exists.
- If recent_window has implementation_progress_count, delivery_progress_count, validation_progress_count, or strong_progress_count, treat the episode as moving again.
- watch means pressure exists but the run is not stuck; prefer cheap_probe or cheap_execute unless this turn protects final quality.
- stale means repeated recent pressure with no recent progress; choose freeze_or_replan or premium_recover only with a concrete recovery purpose.
- blocked means too many calls since progress; stop expanding budget and force replan or recovery.
- file_written to /app/output is candidate delivery progress, not proof of final success.
- A bare passed baseline test is not delivery proof. Validation progress means the test checks current code/output or /app/output.
- Opus should be reserved for root-cause changes, ambiguous validation, hidden-case reasoning, recovery, and final guardrails."""


def call_one(sample: dict[str, Any], args: argparse.Namespace, api_key: str) -> dict[str, Any]:
    prompt = build_prompt(sample, args.prompt_id)
    started = time.time()
    response = requests.post(
        args.endpoint.rstrip("/") + "/chat/completions",
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            "HTTP-Referer": "https://aware-gateway.local/rsi-replay",
            "X-Title": "Aware Gateway RSI Replay",
        },
        json={
            "model": args.model,
            "temperature": 0,
            "max_tokens": 768,
            "messages": [{"role": "user", "content": prompt}],
        },
        timeout=45,
    )
    latency_ms = int((time.time() - started) * 1000)
    text = ""
    usage = {}
    error = ""
    if response.ok:
        payload = response.json()
        text = payload.get("choices", [{}])[0].get("message", {}).get("content") or ""
        usage = payload.get("usage") or {}
    else:
        error = response.text[:500]
    parsed = parse_candidate(text, sample.get("prompt_evidence_refs") or sample.get("allowed_evidence_refs") or [])
    row = row_base(sample)
    row.update(
        {
            "status": response.status_code,
            "latency_ms": latency_ms,
            "candidate_model": parsed["model"],
            "candidate_budget_action": parsed["budget_action"],
            "candidate_progress_state": parsed["progress_state"],
            "candidate_critical_path": parsed["critical_path"],
            "candidate_evidence_refs": parsed["evidence_refs"],
            "candidate_reason": parsed["reason"],
            "evidence_refs_valid": parsed["evidence_refs_valid"],
            "raw_response": parsed["raw_response"],
            "usage": usage,
            "error": error,
        }
    )
    return row


def dry_run_one(sample: dict[str, Any], args: argparse.Namespace) -> dict[str, Any]:
    row = row_base(sample)
    row.update(
        {
            "status": 200,
            "latency_ms": 0,
            "candidate_model": "z-ai/glm-5.3-flash",
            "candidate_budget_action": "cheap_probe",
            "candidate_progress_state": "unknown",
            "candidate_critical_path": False,
            "candidate_evidence_refs": sample.get("prompt_evidence_refs", [])[:1],
            "candidate_reason": "dry run",
            "evidence_refs_valid": bool(sample.get("prompt_evidence_refs")),
            "raw_response": "",
            "usage": {},
            "error": "",
        }
    )
    return row


def row_base(sample: dict[str, Any]) -> dict[str, Any]:
    original = sample.get("original_decision") or {}
    summary = sample.get("episode_summary") or {}
    state = sample.get("state_before") or {}
    post_outcome = sample.get("post_decision_outcome") or {}
    return {
        "episode_id": sample.get("episode_id"),
        "episode_dir": sample.get("episode_dir"),
        "task_name": summary.get("task_name") or "",
        "reward": summary.get("reward"),
        "decision_id": sample.get("decision_id"),
        "decision_timestamp": sample.get("decision_timestamp"),
        "event_cutoff": sample.get("event_cutoff"),
        "future_evidence_leakage": sample.get("future_evidence_leakage"),
        "prompt_evidence_refs": sample.get("prompt_evidence_refs") or [],
        "original_selected_model": original.get("selected_model") or "",
        "original_selected_budget_action": original.get("selected_budget_action") or "",
        "original_selected_reason": original.get("selected_reason") or "",
        "original_selected_trace_id": original.get("selected_trace_id") or "",
        "original_post_outcome_label": post_outcome.get("outcome_label") or "",
        "original_post_event_count": post_outcome.get("event_count", 0),
        "original_post_candidate_progress_event_count": post_outcome.get("candidate_progress_event_count", 0),
        "original_post_progress_event_count": post_outcome.get("progress_event_count", 0),
        "original_post_exploration_event_count": post_outcome.get("exploration_event_count", 0),
        "original_post_implementation_progress_event_count": post_outcome.get("implementation_progress_event_count", 0),
        "original_post_validation_progress_event_count": post_outcome.get("validation_progress_event_count", 0),
        "original_post_delivery_progress_event_count": post_outcome.get("delivery_progress_event_count", 0),
        "original_post_no_progress_event_count": post_outcome.get("no_progress_event_count", 0),
        "original_post_test_run_outcomes": post_outcome.get("test_run_outcomes") or {},
        "original_post_verifier_reward": post_outcome.get("verifier_reward"),
        "state_event_count": state.get("event_count", 0),
        "state_llm_call_count": state.get("llm_call_count", 0),
        "state_tool_call_count": state.get("tool_call_count", 0),
        "state_file_write_count": state.get("file_write_count", 0),
        "state_delivery_file_write_count": state.get("delivery_file_write_count", 0),
        "state_test_run_count": state.get("test_run_count", 0),
        "state_completion_readiness": state.get("completion_readiness", "none"),
        "state_next_min_capability": state.get("next_min_capability", "unknown"),
        "state_next_budget_action_hint": state.get("next_budget_action_hint", ""),
        "state_next_capability_reason": state.get("next_capability_reason", ""),
        "state_last_budget_action": state.get("last_budget_action", ""),
        "state_last_finish_reason": state.get("last_finish_reason", ""),
        "state_recent_delivery_floor_attempts": state.get("recent_delivery_floor_attempts", 0),
        "state_progress_event_count": state.get("progress_event_count", 0),
        "state_candidate_progress_event_count": state.get("candidate_progress_event_count", 0),
        "state_exploration_event_count": state.get("exploration_event_count", 0),
        "state_implementation_progress_event_count": state.get("implementation_progress_event_count", 0),
        "state_validation_progress_event_count": state.get("validation_progress_event_count", 0),
        "state_delivery_progress_event_count": state.get("delivery_progress_event_count", 0),
        "state_exploration_since_progress": state.get("exploration_since_progress", 0),
        "state_replan_count": state.get("replan_count", 0),
        "state_last_replan_event_id": state.get("last_replan_event_id", ""),
        "state_llm_calls_since_replan": state.get("llm_calls_since_replan", 0),
        "state_exploration_since_replan": state.get("exploration_since_replan", 0),
        "state_replan_hypothesis_count": state.get("replan_hypothesis_count", 0),
        "state_last_replan_hypothesis_event_id": state.get("last_replan_hypothesis_event_id", ""),
        "state_last_replan_hypothesis": state.get("last_replan_hypothesis", ""),
        "state_replan_hypothesis_status": state.get("replan_hypothesis_status", "none"),
        "state_llm_calls_since_replan_hypothesis": state.get("llm_calls_since_replan_hypothesis", 0),
        "state_exploration_since_replan_hypothesis": state.get("exploration_since_replan_hypothesis", 0),
        "state_no_progress_event_count": state.get("no_progress_event_count", 0),
        "state_events_since_progress": state.get("events_since_progress", 0),
        "state_llm_calls_since_progress": state.get("llm_calls_since_progress", 0),
        "state_length_pressure_since_progress": state.get("length_pressure_since_progress", 0),
        "state_recent_window": state.get("recent_window") or {},
        "state_no_progress_window": state.get("no_progress_window") or {},
        "state_no_progress_window_severity": (state.get("no_progress_window") or {}).get("severity") or "unknown",
        "state_cost_usd": state.get("cost_usd", 0),
        "state_outcomes": state.get("outcomes") or {},
        "state_models": state.get("models") or {},
        "state_budget_actions": state.get("budget_actions") or {},
    }


def parse_candidate(text: str, allowed_refs: list[str]) -> dict[str, Any]:
    raw_text = text.strip()
    match = re.search(r"\{.*\}", raw_text, flags=re.S)
    raw = match.group(0) if match else raw_text
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError:
        payload = {}
    model = str(payload.get("model") or "")
    if model not in VALID_MODELS:
        model = ""
    budget_action = str(payload.get("budget_action") or "")
    if budget_action not in VALID_BUDGET_ACTIONS:
        budget_action = ""
    progress_state = str(payload.get("progress_state") or "")
    evidence_refs = payload.get("evidence_refs") if isinstance(payload.get("evidence_refs"), list) else []
    evidence_refs = [str(ref) for ref in evidence_refs if str(ref)]
    allowed_set = set(allowed_refs)
    return {
        "model": model,
        "budget_action": budget_action,
        "progress_state": progress_state,
        "critical_path": payload.get("critical_path")
        if isinstance(payload.get("critical_path"), bool)
        else None,
        "evidence_refs": evidence_refs,
        "reason": str(payload.get("reason") or "")[:240],
        "evidence_refs_valid": bool(evidence_refs) and all(ref in allowed_set for ref in evidence_refs),
        "raw_response": raw[:1000],
    }


def error_row(sample: dict[str, Any], error: str) -> dict[str, Any]:
    row = row_base(sample)
    row.update(
        {
            "status": 0,
            "latency_ms": 0,
            "candidate_model": "",
            "candidate_budget_action": "",
            "candidate_progress_state": "",
            "candidate_critical_path": None,
            "candidate_evidence_refs": [],
            "candidate_reason": "",
            "evidence_refs_valid": False,
            "raw_response": "",
            "usage": {},
            "error": error,
        }
    )
    return row


def replay_payload(args: argparse.Namespace, episode_dirs: list[Path], rows: list[dict[str, Any]]) -> dict[str, Any]:
    rows = sorted(rows, key=lambda row: (row.get("episode_id") or "", row.get("decision_timestamp") or ""))
    return {
        "schema_version": "rsi-router-replay.v1",
        "prompt_id": args.prompt_id,
        "decision_model": args.model,
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "episode_dirs": [str(path) for path in episode_dirs],
        "summary": summarize(rows),
        "replay_decisions": rows,
    }


def summarize(rows: list[dict[str, Any]]) -> dict[str, Any]:
    ok_rows = [row for row in rows if row.get("status") == 200 and row.get("candidate_model")]
    switched = [
        row
        for row in ok_rows
        if row.get("original_selected_model")
        and row.get("candidate_model") != row.get("original_selected_model")
    ]
    valid_evidence = [row for row in ok_rows if row.get("evidence_refs_valid")]
    future_leakage = sum(as_int(row.get("future_evidence_leakage")) for row in rows)
    by_window: dict[str, Counter[str]] = {}
    for row in ok_rows:
        severity = str(row.get("state_no_progress_window_severity") or "unknown")
        by_window.setdefault(severity, Counter())[str(row.get("candidate_model") or "(empty)")] += 1
    return {
        "rows": len(rows),
        "ok": len(ok_rows),
        "status_mix": dict(Counter(str(row.get("status")) for row in rows)),
        "original_model_mix": dict(Counter(row.get("original_selected_model") or "(empty)" for row in rows)),
        "candidate_model_mix": dict(Counter(row.get("candidate_model") or "(empty)" for row in rows)),
        "candidate_budget_action_mix": dict(Counter(row.get("candidate_budget_action") or "(empty)" for row in rows)),
        "candidate_progress_state_mix": dict(Counter(row.get("candidate_progress_state") or "(empty)" for row in rows)),
        "original_post_outcome_label_mix": dict(
            Counter(row.get("original_post_outcome_label") or "(empty)" for row in rows)
        ),
        "original_post_outcome_with_progress_count": sum(
            1
            for row in rows
            if as_int(row.get("original_post_candidate_progress_event_count")) > 0
            or as_int(row.get("original_post_progress_event_count")) > 0
        ),
        "state_no_progress_window_severity_mix": dict(
            Counter(row.get("state_no_progress_window_severity") or "unknown" for row in rows)
        ),
        "candidate_model_by_no_progress_window": {
            severity: dict(counter)
            for severity, counter in sorted(by_window.items())
        },
        "switched": len(switched),
        "switch_rate": round(len(switched) / max(1, len(ok_rows)), 4),
        "reason_evidence_coverage": round(len(valid_evidence) / max(1, len(ok_rows)), 4),
        "future_evidence_leakage": future_leakage,
        "estimated_replay_decision_cost_usd": round(sum(estimate_decision_cost(row) for row in ok_rows), 8),
    }


def estimate_decision_cost(row: dict[str, Any]) -> float:
    usage = row.get("usage") or {}
    direct_cost = as_float(usage.get("cost"), None)
    if direct_cost and direct_cost > 0:
        return direct_cost
    cost_details = usage.get("cost_details") or {}
    detailed_cost = as_float(cost_details.get("upstream_inference_cost"), None)
    if detailed_cost and detailed_cost > 0:
        return detailed_cost
    prompt_cost = as_float(cost_details.get("upstream_inference_prompt_cost"), 0.0) or 0.0
    completion_cost = as_float(cost_details.get("upstream_inference_completions_cost"), 0.0) or 0.0
    if prompt_cost or completion_cost:
        return prompt_cost + completion_cost
    prompt_tokens = as_int(usage.get("prompt_tokens"))
    completion_tokens = as_int(usage.get("completion_tokens"))
    if not prompt_tokens and not completion_tokens:
        return 0.0
    return (prompt_tokens * 5.0 / 1_000_000) + (completion_tokens * 25.0 / 1_000_000)


def parse_dt(value: str) -> datetime:
    value = value.strip()
    if value.endswith("Z"):
        value = value[:-1] + "+00:00"
    parsed = datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def as_int(value: Any) -> int:
    try:
        if value is None or value == "":
            return 0
        return int(float(value))
    except (TypeError, ValueError):
        return 0


def as_float(value: Any, default: float | None = 0.0) -> float | None:
    try:
        if value is None or value == "":
            return default
        return float(value)
    except (TypeError, ValueError):
        return default


def write_json(path: Path, payload: Any) -> None:
    with path.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, ensure_ascii=False, sort_keys=True)
        handle.write("\n")


if __name__ == "__main__":
    main()
