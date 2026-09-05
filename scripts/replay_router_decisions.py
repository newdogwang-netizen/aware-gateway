#!/usr/bin/env python3
"""Replay smart-router decisions against frozen trajectory contexts."""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import re
import sys
import time
from collections import defaultdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import requests


MENU = [
    {
        "name": "z-ai/glm-5.3-flash",
        "tier": "ultra-cheap",
        "input_price": 0.07,
        "output_price": 0.25,
        "capabilities": ["chat", "code", "reasoning"],
    },
    {
        "name": "anthropic/claude-opus-5",
        "tier": "strongest-premium",
        "input_price": 5.00,
        "output_price": 25.00,
        "capabilities": ["chat", "code", "reasoning", "vision"],
    },
]
VALID_MODELS = {item["name"] for item in MENU}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Replay router choices with a prompt variant and write comparison data."
    )
    parser.add_argument(
        "--input",
        default="/mnt/data2/aware-gateway-runs/aware-v4-20260902T115134Z/router-decisions.json",
        help="router-decisions.json produced by build_router_decision_lab.py.",
    )
    parser.add_argument(
        "--output",
        default=None,
        help="Replay JSON output. Defaults next to input.",
    )
    parser.add_argument("--prompt-id", default="cost-aware-v1")
    parser.add_argument("--model", default="openai/gpt-5.6-sol")
    parser.add_argument("--endpoint", default="https://openrouter.ai/api/v1")
    parser.add_argument("--api-key-env", default="GW_OPENROUTER_KEY")
    parser.add_argument("--api-key-file", default="openrouter.env")
    parser.add_argument("--max-workers", type=int, default=6)
    parser.add_argument("--limit", type=int, default=0, help="Debug limit; 0 means all.")
    parser.add_argument("--resume", action="store_true", help="Resume from an existing output file.")
    parser.add_argument(
        "--history-turns",
        type=int,
        default=5,
        help="Replay with compact per-job decision history. Use 0 for independent decisions.",
    )
    parser.add_argument(
        "--history-context-chars",
        type=int,
        default=220,
        help="Max chars kept for each historical context summary/reason.",
    )
    return parser.parse_args()


def read_api_key(args: argparse.Namespace) -> str:
    key = os.environ.get(args.api_key_env)
    if key:
        return key.strip()
    path = Path(args.api_key_file)
    if path.exists():
        return path.read_text(encoding="utf-8").strip()
    return ""


def estimate_tokens(*parts: str) -> int:
    return max(1, sum(len(part or "") for part in parts) // 4)


def phase_from_step(step_id: int) -> str:
    if step_id <= 2:
        return "understand"
    if step_id <= 4:
        return "plan-or-first-implementation"
    if step_id <= 6:
        return "code"
    if step_id <= 8:
        return "review"
    return "fix"


def compact_text(value: Any, limit: int) -> str:
    text = " ".join(str(value or "").split())
    if limit <= 0 or len(text) <= limit:
        return text
    return text[: max(0, limit - 3)].rstrip() + "..."


def format_history(history: list[dict[str, Any]], context_chars: int) -> str:
    if not history:
        return ""
    lines = []
    for idx, row in enumerate(history, start=1):
        critical = row.get("replay_critical_path")
        if critical is None:
            critical_text = "unknown"
        else:
            critical_text = "true" if critical else "false"
        lines.append(
            f'{idx}. model={row.get("replay_model") or "unknown"} '
            f'turn={row.get("replay_turn_type") or "unknown"} '
            f'state={row.get("replay_hypothesis_state") or "unknown"} '
            f"critical={critical_text} "
            f'recover={row.get("replay_recoverability") or "unknown"} '
            f'ctx="{compact_text(row.get("replay_context_summary"), context_chars)}" '
            f'reason="{compact_text(row.get("replay_reason"), 120)}"'
        )
    return "\n".join(lines)


def build_prompt(
    decision: dict[str, Any],
    history: list[dict[str, Any]] | None = None,
    context_chars: int = 220,
) -> str:
    latest = decision.get("latest_user_context_preview") or ""
    recent = decision.get("recent_context_preview") or []
    recent_text = "\n\n".join(
        f"{item.get('source')} step {item.get('step_id')}:\n{item.get('message')}"
        for item in recent
    )
    next_preview = decision.get("next_agent_message_preview") or ""
    step_id = int(decision.get("target_step_id") or decision.get("decision_index") or 0)
    tokens = estimate_tokens(latest, recent_text)
    menu_json = json.dumps(MENU, ensure_ascii=False, indent=2)
    history_text = format_history(history or [], context_chars)
    memory_section = ""
    if history_text:
        memory_section = f"""
Recent router memory for this same trial, oldest to newest:
{history_text}

Use recent memory as routing evidence, not as a rule to repeat the last model.
If previous summaries show the hypothesis is stable or repeated premium calls did not change strategy, prefer Flash unless this turn establishes a new critical hypothesis.
If previous summaries show contradiction, ambiguous validation, or a failed core hypothesis, consider Opus for recovery.
"""

    return f"""You are routing one LLM call inside a terminal coding agent.
Pick the lowest-cost model that is likely to succeed for this call.
Optimize for final task quality per dollar, not for speed.

Models, with prices in USD per 1M tokens:
{menu_json}

Cost signal:
- Opus input tokens cost about 71x Flash input tokens.
- Opus output tokens cost 100x Flash output tokens.
- Treat that price gap as first-class evidence.
- Use this benchmark prior unless the local context clearly contradicts it: Flash has about 75% of Opus's general intelligence, reasoning, and coding ability.
- Choose Opus only when this specific next turn has a clear chance to improve final task success or avoid expensive rework.
- Choose Flash for short, reversible, exploratory, read-only, or simple summarization turns.
- Choose Opus for high-stakes implementation, debugging failed tests, dense evidence synthesis, fragile final fixes, or final task completion checks.

Critical-path policy:
- Spend Opus on path-setting reasoning, not on every hard-looking turn.
- Critical-path turns establish or revise the core hypothesis that later work depends on.
- Treat these as critical path: protocol/schema inference, VM/bytecode semantics, cryptographic/KDF design, root-cause diagnosis after failed tests, concurrency/data-corruption reasoning, first solver architecture, and recovery from a clearly wrong hypothesis.
- A probe can be critical if its result will choose the main direction of the solution. A probe is cheap if it only checks a bounded fact under an already stable hypothesis.
- After the core hypothesis is stable, prefer Flash for mechanical extraction, repetitive brute-force sweeps, formatting outputs, file reads, narrow validation, and simple follow-up checks.
- Final task completion and exact agent-control confirmations remain Opus turns.

Oracle-gap policy:
- Do not treat all validation as cheap. Running a known test with a clear oracle is cheap; deciding whether validation covers hidden requirements is critical-path reasoning.
- Use Opus when the next turn must design tests, judge coverage, interpret ambiguous failures, reason about adversarial/malformed/edge-case inputs, or decide that local evidence is sufficient for final success.
- Use Flash when validation is bounded execution of already-chosen checks and failures would produce concrete, easy-to-act-on output.
- If local tests pass but the real grader may include hidden cases, distinguish running one more check from reasoning about the gap between local evidence and hidden success. Use Opus for the latter.

Decision process:
1. Identify the next response type: orientation, critical_hypothesis, implementation, mechanical_probe, validation, recovery, or finalization.
2. Identify hypothesis_state: none, forming, stable, contradicted, or solved.
3. Before choosing Opus, name the core hypothesis this turn will establish or revise. If there is no such hypothesis, prefer Flash.
4. Read recent router memory for this same trial. Use it to detect repeated bottlenecks, stable hypotheses, prior premium spending, and whether the next turn should continue or change strategy.
5. If the same bottleneck has already consumed multiple Opus turns, do not buy more blind probing. Use Opus only to change the search strategy; use Flash for bounded sweeps and mechanical validation.
6. Prefer Opus for early critical-path modeling, but prefer Flash for late execution once the problem model is stable.

Turn phase: {phase_from_step(step_id)}
Conversation depth: approximately {step_id} trajectory steps, ~{tokens} input tokens in replay context.
{memory_section}

Latest user/context message:
{latest}

Recent trajectory context:
{recent_text}

Historical next agent response preview, for calibration only. Do not copy it; decide which model should have generated this next response:
{next_preview}

Write context_summary as compact memory for future routing: summarize the current bottleneck, evidence state, or hypothesis state in under 14 words.

Return JSON only with keys: model, turn_type, hypothesis_state, critical_path, recoverability, context_summary, reason.
critical_path must be a JSON boolean. recoverability must be easy, medium, or hard.
Keep context_summary under 14 words and reason under 12 words. Do not include any text outside the JSON."""


def parse_decision(text: str) -> dict[str, Any]:
    text = text.strip()
    match = re.search(r"\{.*\}", text, flags=re.S)
    raw = match.group(0) if match else text
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError:
        return {
            "model": "",
            "reason": "",
            "turn_type": "",
            "hypothesis_state": "",
            "critical_path": None,
            "recoverability": "",
            "context_summary": "",
            "raw_response": text[:500],
        }
    model = str(payload.get("model") or "")
    reason = str(payload.get("reason") or "")
    if model not in VALID_MODELS:
        model = ""
    return {
        "model": model,
        "reason": reason,
        "turn_type": str(payload.get("turn_type") or ""),
        "hypothesis_state": str(payload.get("hypothesis_state") or ""),
        "critical_path": payload.get("critical_path") if isinstance(payload.get("critical_path"), bool) else None,
        "recoverability": str(payload.get("recoverability") or ""),
        "context_summary": str(payload.get("context_summary") or ""),
        "raw_response": raw,
    }


def call_one(
    decision: dict[str, Any],
    args: argparse.Namespace,
    api_key: str,
    history: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    prompt = build_prompt(decision, history, args.history_context_chars)
    started = time.time()
    response = requests.post(
        args.endpoint.rstrip("/") + "/chat/completions",
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            "HTTP-Referer": "https://aware-gateway.local/router-decision-replay",
            "X-Title": "Aware Gateway Router Decision Replay",
        },
        json={
            "model": args.model,
            "temperature": 0,
            "max_tokens": 320,
            "messages": [{"role": "user", "content": prompt}],
        },
        timeout=45,
    )
    latency_ms = int((time.time() - started) * 1000)
    status = response.status_code
    text = ""
    usage = {}
    error = ""
    if response.ok:
        payload = response.json()
        text = payload.get("choices", [{}])[0].get("message", {}).get("content") or ""
        usage = payload.get("usage") or {}
    else:
        error = response.text[:500]
    parsed = parse_decision(text)
    return {
        "job": decision.get("job"),
        "task": decision.get("task"),
        "strategy": decision.get("strategy"),
        "attempt": decision.get("attempt"),
        "decision_index": decision.get("decision_index"),
        "target_step_id": decision.get("target_step_id"),
        "reward": decision.get("reward"),
        "original_model": decision.get("selected_model"),
        "original_reason": decision.get("router_reason"),
        "replay_model": parsed["model"],
        "replay_reason": parsed["reason"],
        "replay_turn_type": parsed["turn_type"],
        "replay_hypothesis_state": parsed["hypothesis_state"],
        "replay_critical_path": parsed["critical_path"],
        "replay_recoverability": parsed["recoverability"],
        "replay_context_summary": parsed["context_summary"],
        "raw_response": parsed["raw_response"],
        "status": status,
        "latency_ms": latency_ms,
        "usage": usage,
        "error": error,
    }


def summarize(rows: list[dict[str, Any]]) -> dict[str, Any]:
    def counter(field: str) -> dict[str, int]:
        out: dict[str, int] = {}
        for row in rows:
            key = row.get(field) or "(empty)"
            out[key] = out.get(key, 0) + 1
        return dict(sorted(out.items(), key=lambda item: (-item[1], item[0])))

    switched = sum(
        1 for row in rows if row.get("replay_model") and row.get("replay_model") != row.get("original_model")
    )
    ok = sum(1 for row in rows if row.get("status") == 200 and row.get("replay_model"))
    return {
        "rows": len(rows),
        "ok": ok,
        "switched": switched,
        "switch_rate": switched / max(1, ok),
        "original_model_mix": counter("original_model"),
        "replay_model_mix": counter("replay_model"),
    }


def write_output(path: Path, payload: dict[str, Any]) -> None:
    path.write_text(json.dumps(payload, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")


def sort_index(value: Any) -> int:
    try:
        return int(value or 0)
    except (TypeError, ValueError):
        return 0


def row_key(row: dict[str, Any]) -> tuple[str, int]:
    return (str(row.get("job") or ""), sort_index(row.get("decision_index") or row.get("target_step_id")))


def replay_payload(args: argparse.Namespace, input_path: Path, rows: list[dict[str, Any]]) -> dict[str, Any]:
    return {
        "schema_version": "router-decision-replay.v2",
        "prompt_id": args.prompt_id,
        "decision_model": args.model,
        "source": str(input_path),
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "history_turns": max(0, args.history_turns),
        "history_context_chars": args.history_context_chars,
        "summary": summarize(rows),
        "replay_decisions": rows,
    }


def append_replay_history(
    histories: dict[str, list[dict[str, Any]]],
    row: dict[str, Any],
    args: argparse.Namespace,
) -> None:
    if args.history_turns <= 0 or not row.get("replay_model"):
        return
    key = str(row.get("job") or "")
    if not key:
        return
    append_replay_history_entry(histories[key], row, args)


def append_replay_history_entry(
    history: list[dict[str, Any]],
    row: dict[str, Any],
    args: argparse.Namespace,
) -> None:
    if args.history_turns <= 0 or not row.get("replay_model"):
        return
    history.append(row)
    if len(history) > args.history_turns:
        del history[: len(history) - args.history_turns]


def write_progress(
    output_path: Path,
    args: argparse.Namespace,
    input_path: Path,
    rows: list[dict[str, Any]],
    completed: int,
    total: int,
) -> None:
    payload = replay_payload(args, input_path, rows)
    write_output(output_path, payload)
    summary = payload["summary"]
    print(
        f"completed={completed}/{total} ok={summary['ok']} "
        f"switches={summary['switched']} replay_mix={summary['replay_model_mix']}"
	)


def replay_job_with_history(
    job_decisions: list[dict[str, Any]],
    existing_by_key: dict[tuple[Any, ...], dict[str, Any]],
    args: argparse.Namespace,
    api_key: str,
) -> list[dict[str, Any]]:
    history: list[dict[str, Any]] = []
    new_rows: list[dict[str, Any]] = []
    for decision in sorted(job_decisions, key=row_key):
        key = (decision.get("job"), decision.get("decision_index"))
        existing = existing_by_key.get(key)
        if existing:
            append_replay_history_entry(history, existing, args)
            continue

        try:
            row = call_one(decision, args, api_key, list(history[-args.history_turns :]))
        except Exception as exc:
            row = {
                "job": decision.get("job"),
                "decision_index": decision.get("decision_index"),
                "status": 0,
                "replay_model": "",
                "error": repr(exc),
            }
        new_rows.append(row)
        append_replay_history_entry(history, row, args)
    return new_rows


def main() -> None:
    args = parse_args()
    api_key = read_api_key(args)
    if not api_key:
        print(f"missing API key: set {args.api_key_env} or create {args.api_key_file}", file=sys.stderr)
        sys.exit(2)

    input_path = Path(args.input).resolve()
    output_path = (
        Path(args.output).resolve()
        if args.output
        else input_path.with_name(f"router-decision-replay-{args.prompt_id}.json")
    )
    source = json.loads(input_path.read_text(encoding="utf-8"))
    decisions = sorted(
        [d for d in source.get("decisions", []) if d.get("target_step_id")],
        key=row_key,
    )
    if args.limit:
        decisions = decisions[: args.limit]

    existing_rows: list[dict[str, Any]] = []
    done_keys: set[tuple[Any, ...]] = set()
    if args.resume and output_path.exists():
        existing = json.loads(output_path.read_text(encoding="utf-8"))
        existing_rows = existing.get("replay_decisions", [])
        done_keys = {
            (r.get("job"), r.get("decision_index"))
            for r in existing_rows
            if r.get("status") == 200 and r.get("replay_model")
        }

    todo = [
        decision
        for decision in decisions
        if (decision.get("job"), decision.get("decision_index")) not in done_keys
    ]
    rows = sorted(existing_rows, key=row_key)
    print(
        f"replaying {len(todo)} decisions; already_done={len(done_keys)} "
        f"history_turns={max(0, args.history_turns)} output={output_path}"
    )

    if args.history_turns > 0:
        existing_by_key = {
            (row.get("job"), row.get("decision_index")): row
            for row in rows
            if row.get("status") == 200 and row.get("replay_model")
        }
        groups: dict[str, list[dict[str, Any]]] = defaultdict(list)
        for decision in decisions:
            groups[str(decision.get("job") or "")].append(decision)

        completed = 0
        max_workers = max(1, args.max_workers)
        with concurrent.futures.ThreadPoolExecutor(max_workers=max_workers) as pool:
            future_map = {
                pool.submit(replay_job_with_history, job_decisions, existing_by_key, args, api_key): job
                for job, job_decisions in groups.items()
                if any((d.get("job"), d.get("decision_index")) not in done_keys for d in job_decisions)
            }
            for future in concurrent.futures.as_completed(future_map):
                job_rows = future.result()
                rows.extend(job_rows)
                rows.sort(key=row_key)
                completed += len(job_rows)
                write_progress(output_path, args, input_path, rows, completed, len(todo))
        if not todo:
            write_output(output_path, replay_payload(args, input_path, rows))
        return
    completed = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.max_workers) as pool:
        future_map = {pool.submit(call_one, decision, args, api_key): decision for decision in todo}
        for future in concurrent.futures.as_completed(future_map):
            try:
                row = future.result()
            except Exception as exc:  # keep long replays resumable
                decision = future_map[future]
                row = {
                    "job": decision.get("job"),
                    "decision_index": decision.get("decision_index"),
                    "status": 0,
                    "replay_model": "",
                    "error": repr(exc),
                }
            rows.append(row)
            completed += 1
            if completed % 20 == 0 or completed == len(todo):
                write_progress(output_path, args, input_path, rows, completed, len(todo))


if __name__ == "__main__":
    main()
