#!/usr/bin/env python3
"""Run a deterministic safe-control routing probe against a local gateway.

The probe starts two local mock servers:

- a provider endpoint that accepts whichever model the gateway routes to;
- a decision endpoint that returns a fixed JSON decision for fallthrough cases.

This keeps the control-plane experiment free of provider latency and real LLM
spend while still exercising the compiled gateway binary, smart-router plugin,
audit storage, and /v1/traces query path.
"""

from __future__ import annotations

import argparse
import contextlib
import csv
import json
import os
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from threading import Thread
from typing import Any


CHEAP_MODEL = "z-ai/glm-5.3-flash"
PREMIUM_MODEL = "anthropic/claude-opus-5"
DECISION_MODEL = "openai/gpt-5.6-sol"


@dataclass
class ProbeCase:
    name: str
    message: str
    expected_source: str
    expected_model: str


PROBE_CASES = [
    ProbeCase(
        "file-read-search",
        "Inspect file config.yaml and search references with `rg`.",
        "safe-control",
        CHEAP_MODEL,
    ),
    ProbeCase(
        "existing-test-execution",
        "Run the existing tests with go test ./... and report pass/fail.",
        "safe-control",
        CHEAP_MODEL,
    ),
    ProbeCase(
        "fixed-format-output",
        'Return exactly this JSON only: {"ok": true}.',
        "safe-control",
        CHEAP_MODEL,
    ),
    ProbeCase(
        "first-observed-error",
        "pytest failed\nAssertionError: expected relay id 7 got 8",
        "decision-model",
        CHEAP_MODEL,
    ),
    ProbeCase(
        "same-error-repeated",
        "pytest failed\nAssertionError: expected relay id 7 got 8",
        "safe-control",
        PREMIUM_MODEL,
    ),
    ProbeCase(
        "hypothesis-contradiction",
        "The core hypothesis was contradicted by a counterexample; recover before editing more files.",
        "safe-control",
        PREMIUM_MODEL,
    ),
    ProbeCase(
        "premium-cooldown",
        "Summarize current state and propose the next bounded check.",
        "safe-control",
        CHEAP_MODEL,
    ),
    ProbeCase(
        "completion-guardrail",
        'Are you sure you want to mark the task as complete? Reply with {"task_complete": true}.',
        "guardrail",
        PREMIUM_MODEL,
    ),
    ProbeCase(
        "ambiguous-fallthrough",
        "Implement the parser fix now; reason about edge cases before editing.",
        "decision-model",
        PREMIUM_MODEL,
    ),
]


class MockProviderHandler(BaseHTTPRequestHandler):
    server_version = "AwareMockProvider/1.0"

    def log_message(self, fmt: str, *args: Any) -> None:
        return

    def do_GET(self) -> None:
        if self.path.startswith("/health"):
            self._json({"ok": True})
            return
        if self.path.startswith("/v1/models"):
            self._json({"data": [{"id": CHEAP_MODEL}, {"id": PREMIUM_MODEL}]})
            return
        self.send_error(404)

    def do_POST(self) -> None:
        if not self.path.endswith("/chat/completions"):
            self.send_error(404)
            return
        body = self._read_json()
        model = body.get("model") or "unknown"
        prompt_chars = sum(len(m.get("content", "")) for m in body.get("messages", []))
        prompt_tokens = max(1, prompt_chars // 4)
        completion_tokens = 12
        self._json(
            {
                "id": "mock-provider-response",
                "object": "chat.completion",
                "model": model,
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": f"mock response from {model}"},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {
                    "prompt_tokens": prompt_tokens,
                    "completion_tokens": completion_tokens,
                    "total_tokens": prompt_tokens + completion_tokens,
                },
            }
        )

    def _read_json(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0:
            return {}
        return json.loads(self.rfile.read(length))

    def _json(self, payload: dict[str, Any]) -> None:
        raw = json.dumps(payload).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


class MockDecisionHandler(BaseHTTPRequestHandler):
    server_version = "AwareMockDecision/1.0"
    calls: list[dict[str, Any]] = []

    def log_message(self, fmt: str, *args: Any) -> None:
        return

    def do_POST(self) -> None:
        if not self.path.endswith("/chat/completions"):
            self.send_error(404)
            return
        body = self._read_json()
        prompt = ""
        for message in body.get("messages", []):
            prompt += "\n" + str(message.get("content") or "")
        selected = CHEAP_MODEL
        reason = "fallthrough cheap"
        if "Implement the parser fix now" in prompt:
            selected = PREMIUM_MODEL
            reason = "critical implementation"
        MockDecisionHandler.calls.append({"selected_model": selected, "prompt_chars": len(prompt)})
        decision = {
            "model": selected,
            "turn_type": "implementation" if selected == PREMIUM_MODEL else "validation",
            "hypothesis_state": "forming" if selected == PREMIUM_MODEL else "stable",
            "critical_path": selected == PREMIUM_MODEL,
            "recoverability": "hard" if selected == PREMIUM_MODEL else "easy",
            "context_summary": reason,
            "reason": reason,
        }
        self._json(
            {
                "id": "mock-decision-response",
                "object": "chat.completion",
                "model": DECISION_MODEL,
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": json.dumps(decision)},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {
                    "prompt_tokens": max(1, len(prompt) // 4),
                    "completion_tokens": 40,
                    "total_tokens": max(1, len(prompt) // 4) + 40,
                },
            }
        )

    def _read_json(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0:
            return {}
        return json.loads(self.rfile.read(length))

    def _json(self, payload: dict[str, Any]) -> None:
        raw = json.dumps(payload).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


def main() -> None:
    args = parse_args()
    repo = Path(args.repo).resolve()
    gateway_bin = Path(args.gateway_bin).resolve()
    if not gateway_bin.exists():
        raise SystemExit(f"gateway binary not found: {gateway_bin}")

    out_dir = Path(args.out_dir).resolve()
    out_dir.mkdir(parents=True, exist_ok=True)

    with tempfile.TemporaryDirectory(prefix="aware-safe-control-") as temp_name:
        temp_dir = Path(temp_name)
        gateway_port = free_port()
        provider_port = free_port()
        decision_port = free_port()
        provider = serve("127.0.0.1", provider_port, MockProviderHandler)
        decision = serve("127.0.0.1", decision_port, MockDecisionHandler)
        config_path = write_config(temp_dir, gateway_port, provider_port, decision_port)
        gateway = subprocess.Popen(
            [str(gateway_bin), "-config", str(config_path)],
            cwd=repo,
            stdout=(out_dir / "gateway.stdout.log").open("w"),
            stderr=(out_dir / "gateway.stderr.log").open("w"),
            env={**os.environ, "GW_OPENROUTER_KEY": "mock-key"},
        )
        try:
            wait_for_gateway(gateway_port, gateway)
            result = run_probe(gateway_port)
            traces = fetch_json(f"http://127.0.0.1:{gateway_port}/v1/traces?limit=1000")
            trial_traces = [
                trace
                for trace in traces.get("traces", [])
                if trace.get("trial_name") == result["trial_name"]
            ]
            case_session_traces = [
                trace
                for trace in trial_traces
                if trace.get("session_id") == result["session_id"]
            ]
            result["traces"] = trial_traces
            result["case_session_traces"] = case_session_traces
            result["decision_endpoint_calls"] = len(MockDecisionHandler.calls)
            result["summary"] = summarize(result["cases"], trial_traces, result["episode_probe"])
            write_outputs(out_dir, result)
            print(json.dumps(result["summary"], indent=2, sort_keys=True))
            if result["summary"]["failed_expectations"]:
                raise SystemExit(1)
        finally:
            gateway.terminate()
            with contextlib.suppress(subprocess.TimeoutExpired):
                gateway.wait(timeout=5)
            if gateway.poll() is None:
                gateway.kill()
            provider.shutdown()
            decision.shutdown()


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Probe safe-control routing behavior locally.")
    parser.add_argument("--repo", default=".")
    parser.add_argument("--gateway-bin", default="./aware-gateway")
    parser.add_argument("--out-dir", required=True)
    return parser.parse_args()


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def serve(host: str, port: int, handler: type[BaseHTTPRequestHandler]) -> ThreadingHTTPServer:
    server = ThreadingHTTPServer((host, port), handler)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def write_config(temp_dir: Path, gateway_port: int, provider_port: int, decision_port: int) -> Path:
    data_dir = temp_dir / "data"
    data_dir.mkdir()
    config = f"""
server:
  listen: "127.0.0.1:{gateway_port}"
  timeout: 30s
  graceful_shutdown: 5s

retry:
  max_retries: 1
  retryable_statuses: [429, 500, 502, 503]

circuit_breaker:
  threshold: 10
  window: 60s
  open_duration: 30s

pools:
  mock:
    strategy: round_robin
    health_check:
      interval: 60s
      timeout: 5s
    endpoints:
      - name: mock-provider
        url: http://127.0.0.1:{provider_port}
        health_path: /health
        weight: 10
        timeout: 30s
        models:
          - {CHEAP_MODEL}
          - {PREMIUM_MODEL}

routes:
  - pattern: /v1/chat/completions
    pool: mock
  - pattern: /v1/models
    pool: mock

plugins:
  smart-router:
    enabled: true
    endpoint: "http://127.0.0.1:{decision_port}/v1"
    model: "{DECISION_MODEL}"
    api_key_env: "GW_OPENROUTER_KEY"
    max_tokens: 1000
    temperature: 0
    timeout_ms: 5000
    decision_input_price: 2.00
    decision_output_price: 10.00
    decision_retries: 0
    fallback_model: "{PREMIUM_MODEL}"
    fallback_pool: "mock"
    prompt_preview_chars: 2000
    include_system_prompt: true
    include_message_count: true
    decision_history_turns: 5
    decision_history_context_chars: 220
    cache_ttl_seconds: -1
    cache_max_entries: 1000
    safe_control:
      enabled: true
      repeated_error_threshold: 2
      premium_cooldown_after: 2
      premium_cooldown_turns: 1
      cheap_probe_burst_limit: 3
      stop_cost_usd: 4.0
      stop_agent_call_threshold: 50
      stop_length_pressure_threshold: 3
    budgeted_route:
      enabled: true
      profiles:
        cheap_probe:
          max_tokens: 2048
          timeout_ms: 60000
        cheap_execute:
          max_tokens: 1536
          timeout_ms: 60000
        premium_reason:
          max_tokens: 4096
          timeout_ms: 180000
        premium_recover:
          max_tokens: 4096
          timeout_ms: 180000
        completion_guardrail:
          max_tokens: 1024
          timeout_ms: 60000
    episode_runtime:
      enabled: true
      recent_events: 5
      length_streak_threshold: 1
      length_window_threshold: 2
      max_tokens_multiplier: 3
      timeout_multiplier: 2
      max_tokens_ceiling: 8192
      timeout_ms_ceiling: 240000
    models:
      - name: "{CHEAP_MODEL}"
        pool: "mock"
        capabilities: ["chat", "code", "reasoning"]
        context_window: 1310720
        input_price: 0.07
        output_price: 0.25
      - name: "{PREMIUM_MODEL}"
        pool: "mock"
        capabilities: ["chat", "code", "reasoning"]
        context_window: 200000
        input_price: 5.00
        output_price: 25.00

  audit:
    enabled: true
    store: sqlite
    sqlite_path: "{data_dir / "audit.db"}"
    log_truncate_body: 256

pricing:
  enabled: true
  models:
    "{CHEAP_MODEL}":
      prompt: 0.07
      completion: 0.25
    "{PREMIUM_MODEL}":
      prompt: 5.00
      completion: 25.00
    "{DECISION_MODEL}":
      prompt: 2.00
      completion: 10.00
"""
    path = temp_dir / "gateway-safe-control-probe.yaml"
    path.write_text(config.strip() + "\n", encoding="utf-8")
    return path


def wait_for_gateway(port: int, proc: subprocess.Popen[Any]) -> None:
    url = f"http://127.0.0.1:{port}/health"
    for _ in range(60):
        if proc.poll() is not None:
            raise SystemExit(f"gateway exited early with rc={proc.returncode}")
        try:
            fetch_json(url)
            return
        except Exception:
            time.sleep(0.2)
    raise SystemExit("gateway did not become healthy")


def run_probe(port: int) -> dict[str, Any]:
    trial = f"phase2-safe-control-probe-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}"
    session = f"{trial}__agent"
    cases: list[dict[str, Any]] = []
    for index, case in enumerate(PROBE_CASES, start=1):
        step_name = f"{index:02d}-{case.name}"
        response = post_json(
            f"http://127.0.0.1:{port}/v1/chat/completions",
            {
                "model": "auto",
                "messages": [
                    {"role": "system", "content": "You are a terminal coding agent."},
                    {"role": "user", "content": case.message},
                ],
                "temperature": 0,
                "max_tokens": 32,
            },
            headers={
                "X-Trial-Name": trial,
                "X-Session-ID": session,
                "X-Step-Name": step_name,
                "X-Task-Name": "phase2-safe-control-probe",
            },
        )
        agent_trace = wait_for_agent_trace(port, session, step_name)
        reason = str(agent_trace.get("routing_reason") or "")
        source = classify_source(reason)
        routed_model = agent_trace.get("routed_model") or response.get("model") or ""
        has_route_budget = bool(agent_trace.get("route_budget_action"))
        cases.append(
            {
                "index": index,
                "name": case.name,
                "expected_source": case.expected_source,
                "expected_model": case.expected_model,
                "actual_source": source,
                "actual_model": routed_model,
                "route_budget_action": agent_trace.get("route_budget_action") or "",
                "route_max_tokens": agent_trace.get("route_max_tokens") or "",
                "route_timeout_ms": agent_trace.get("route_timeout_ms") or "",
                "pass": source == case.expected_source and routed_model == case.expected_model and has_route_budget,
                "routing_reason": reason,
                "response_model": response.get("model"),
            }
        )
    episode_probe = run_episode_runtime_probe(port, trial)
    return {"trial_name": trial, "session_id": session, "cases": cases, "episode_probe": episode_probe}


def run_episode_runtime_probe(port: int, trial: str) -> dict[str, Any]:
    episode = f"{trial}__episode_runtime"
    session = f"{trial}__episode_runtime_agent"
    failure_episode = f"{trial}__failure_frontier"
    failure_session = f"{trial}__failure_frontier_agent"
    completion_episode = f"{trial}__completion_ready"
    completion_session = f"{trial}__completion_ready_agent"
    completion_regress_episode = f"{trial}__completion_regress"
    completion_regress_session = f"{trial}__completion_regress_agent"
    capability_floor_episode = f"{trial}__capability_floor"
    capability_floor_session = f"{trial}__capability_floor_agent"
    stop_gate_episode = f"{trial}__stop_gate"
    stop_gate_session = f"{trial}__stop_gate_agent"
    provider_incomplete_episode = f"{trial}__provider_incomplete_stop"
    provider_incomplete_session = f"{trial}__provider_incomplete_stop_agent"
    cost_stop_episode = f"{trial}__cost_stop"
    cost_stop_session = f"{trial}__cost_stop_agent"
    length_stop_episode = f"{trial}__length_pressure_stop"
    length_stop_session = f"{trial}__length_pressure_stop_agent"
    task = "phase2-safe-control-probe"
    checks: list[dict[str, Any]] = []

    length_events: list[dict[str, Any]] = []
    for index in range(1, 3):
        event_id = f"{episode}__length-{index}"
        length_events.append(
            {
                "event_id": event_id,
                "episode_id": episode,
                "episode_operation": "continue",
                "sequence": index,
                "kind": "llm_call",
                "source": "safe-control-probe",
                "observation": {
                    "outcome": "length_truncated",
                    "model": CHEAP_MODEL,
                    "routed_model": CHEAP_MODEL,
                    "budget_action": "cheap_execute",
                    "finish_reason": "length",
                    "status": 200,
                    "total_tokens": 1536,
                    "cost_usd": 0.0001,
                    "latency_ms": 60000,
                },
                "evidence_refs": [f"probe:event:{event_id}"],
                "session_id": session,
                "trial_name": trial,
                "step_name": f"episode-runtime-injected-length-{index}",
                "task_name": task,
            }
        )
    batch_response = post_json(
        f"http://127.0.0.1:{port}/v1/episode-events",
        {"events": length_events},
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": session,
            "X-Episode-ID": episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": "episode-runtime-injected-length-batch",
            "X-Task-Name": task,
        },
    )
    checks.extend(
        [
            check_equal("batch-event-ingest-count", batch_response.get("count"), 2),
            check_equal("batch-event-ingest-sinks", batch_response.get("sinks"), 2),
        ]
    )

    state_before = fetch_episode_state(port, episode)
    state_payload = state_before.get("state") or {}
    recent_routes = state_payload.get("recent_route_outcomes") or []
    first_recent_route = recent_routes[0] if recent_routes else {}
    last_recent_route = recent_routes[-1] if recent_routes else {}
    checks.extend(
        [
            check_equal("state-version-after-events", state_before.get("state_version"), 2),
            check_equal("call-count-after-events", state_payload.get("call_count"), 2),
            check_equal("recent-length-after-events", state_payload.get("recent_length_finishes"), 2),
            check_equal("length-streak-after-events", state_payload.get("consecutive_length_finishes"), 2),
            check_equal("no-progress-severity-after-events", state_payload.get("no_progress_severity"), "stale"),
            check_equal("implicit-route-total-after-events", state_payload.get("route_outcome_event_count"), 1),
            check_equal("implicit-route-negative-after-events", state_payload.get("route_outcome_negative_count"), 1),
            check_equal("implicit-route-recent-count-after-events", len(recent_routes), 2),
            check_equal("implicit-route-first-label", first_recent_route.get("outcome_label"), "no_progress"),
            check_equal("implicit-route-first-event", first_recent_route.get("outcome_event_id"), f"{episode}__length-2"),
            check_equal("implicit-route-current-label", last_recent_route.get("outcome_label"), "pending"),
        ]
    )

    duplicate_event_id = f"{episode}__length-2"
    post_json(
        f"http://127.0.0.1:{port}/v1/episode-events",
        {
            "event_id": duplicate_event_id,
            "episode_id": episode,
            "episode_operation": "continue",
            "sequence": 2,
            "kind": "llm_call",
            "source": "safe-control-probe",
            "observation": {
                "outcome": "length_truncated",
                "model": CHEAP_MODEL,
                "routed_model": CHEAP_MODEL,
                "budget_action": "cheap_execute",
                "finish_reason": "length",
                "status": 200,
                "total_tokens": 1536,
                "cost_usd": 0.0001,
                "latency_ms": 60000,
            },
            "evidence_refs": [f"probe:event:{duplicate_event_id}:duplicate"],
            "session_id": session,
            "trial_name": trial,
            "step_name": "episode-runtime-duplicate-length",
            "task_name": task,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": session,
            "X-Episode-ID": episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": "episode-runtime-duplicate-length",
            "X-Task-Name": task,
        },
    )
    state_after_duplicate = fetch_episode_state(port, episode)
    checks.append(
        check_equal(
            "state-version-after-duplicate-event",
            state_after_duplicate.get("state_version"),
            state_before.get("state_version"),
        )
    )

    route_step = "episode-runtime-recovery-route"
    response = post_json(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {"role": "user", "content": "Run the existing tests and recover the blocked path."},
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": session,
            "X-Episode-ID": episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": route_step,
            "X-Task-Name": task,
        },
    )
    route_trace = wait_for_agent_trace(port, session, route_step)
    reason = str(route_trace.get("routing_reason") or "")
    checks.extend(
        [
            check_equal("recovery-route-source", classify_source(reason), "safe-control"),
            check_equal("recovery-route-model", route_trace.get("routed_model") or response.get("model") or "", PREMIUM_MODEL),
            check_equal("recovery-route-budget-action", route_trace.get("route_budget_action") or "", "premium_recover"),
            check_equal("recovery-route-max-tokens", route_trace.get("route_max_tokens") or 0, 4096),
            check_equal("recovery-route-timeout-ms", route_trace.get("route_timeout_ms") or 0, 180000),
            check_contains("recovery-route-rule", reason, "rule_id=episode_no_progress_recovery"),
            check_contains("recovery-route-state-version", reason, "state_version=2"),
            check_contains("recovery-route-length-pressure", reason, "recent_length=2"),
            check_contains("recovery-route-budget-adjust", reason, "episode_adjust=no_progress_freeze"),
        ]
    )

    state_after_route = fetch_episode_state(port, episode)
    state_after_route_payload = state_after_route.get("state") or {}
    checks.extend(
        [
            check_equal("state-version-after-route", state_after_route.get("state_version"), 3),
            check_equal("implicit-route-total-after-recovery-route", state_after_route_payload.get("route_outcome_event_count"), 2),
            check_equal(
                "implicit-route-negative-after-recovery-route",
                state_after_route_payload.get("route_outcome_negative_count"),
                2,
            ),
        ]
    )

    route_outcome_event_id = f"{episode}__post-route-test-passed"
    post_json(
        f"http://127.0.0.1:{port}/v1/episode-events",
        {
            "event_id": route_outcome_event_id,
            "episode_id": episode,
            "episode_operation": "continue",
            "sequence": 4,
            "kind": "test_run",
            "source": "safe-control-probe",
            "observation": {
                "outcome": "passed",
                "command": "python3 validate.py",
                "passed_count": 4,
                "failed_count": 0,
            },
            "evidence_refs": [f"probe:event:{route_outcome_event_id}"],
            "session_id": session,
            "trial_name": trial,
            "step_name": "episode-runtime-route-outcome-test-passed",
            "task_name": task,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": session,
            "X-Episode-ID": episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": "episode-runtime-route-outcome-test-passed",
            "X-Task-Name": task,
        },
    )
    state_after_route_outcome = fetch_episode_state(port, episode)
    route_outcome_payload = state_after_route_outcome.get("state") or {}
    checks.extend(
        [
            check_equal("route-outcome-state-version", state_after_route_outcome.get("state_version"), 4),
            check_equal("route-outcome-trace-id", route_outcome_payload.get("last_route_trace_id"), route_trace.get("trace_id")),
            check_equal("route-outcome-label", route_outcome_payload.get("last_route_outcome_label"), "test_passed"),
            check_equal(
                "route-outcome-event-id",
                route_outcome_payload.get("last_route_outcome_event_id"),
                route_outcome_event_id,
            ),
            check_equal("route-outcome-progress", route_outcome_payload.get("last_route_outcome_progress"), True),
            check_equal("route-outcome-window-event-count", route_outcome_payload.get("last_route_outcome_event_count"), 1),
            check_equal("route-outcome-total-event-count", route_outcome_payload.get("route_outcome_event_count"), 3),
            check_equal("route-outcome-progress-count", route_outcome_payload.get("route_outcome_progress_count"), 1),
            check_equal("route-outcome-negative-count", route_outcome_payload.get("route_outcome_negative_count"), 2),
            check_equal("next-capability-after-route-outcome", route_outcome_payload.get("next_min_capability"), "premium_assess"),
            check_equal("next-budget-after-route-outcome", route_outcome_payload.get("next_budget_action_hint"), "premium_reason"),
            check_equal(
                "next-capability-reason-after-route-outcome",
                route_outcome_payload.get("next_capability_reason"),
                "validation_passed_assess_hidden_gap",
            ),
        ]
    )

    for index in range(1, 3):
        event_id = f"{failure_episode}__test-failed-{index}"
        post_json(
            f"http://127.0.0.1:{port}/v1/episode-events",
            {
                "event_id": event_id,
                "episode_id": failure_episode,
                "episode_operation": "continue",
                "sequence": index,
                "kind": "test_run",
                "source": "safe-control-probe",
                "observation": {
                    "outcome": "failed",
                    "command": "go test ./...",
                    "failure_fingerprint": "AssertionError: expected relay id 7 got 8",
                    "failed_count": 2,
                },
                "evidence_refs": [f"probe:event:{event_id}"],
                "session_id": failure_session,
                "trial_name": trial,
                "step_name": f"episode-failure-injected-test-{index}",
                "task_name": task,
            },
            headers={
                "X-Trial-Name": trial,
                "X-Session-ID": failure_session,
                "X-Episode-ID": failure_episode,
                "X-Episode-Operation": "continue",
                "X-Step-Name": f"episode-failure-injected-test-{index}",
                "X-Task-Name": task,
            },
        )

    failure_state = fetch_episode_state(port, failure_episode)
    failure_payload = failure_state.get("state") or {}
    checks.extend(
        [
            check_equal("failure-state-version-after-events", failure_state.get("state_version"), 2),
            check_equal("failure-same-fingerprint-count", failure_payload.get("same_failure_fingerprint_count"), 2),
            check_equal("failure-frontier-size", failure_payload.get("failure_frontier_size"), 2),
            check_equal(
                "failure-last-fingerprint",
                failure_payload.get("last_failure_fingerprint") or "",
                "assertionerror: expected relay id # got #",
            ),
        ]
    )

    first_failure_step = "episode-failure-recovery-route"
    first_failure_response = post_json(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {"role": "user", "content": "Continue with the next bounded implementation step."},
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": failure_session,
            "X-Episode-ID": failure_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": first_failure_step,
            "X-Task-Name": task,
        },
    )
    first_failure_trace = wait_for_agent_trace(port, failure_session, first_failure_step)
    first_failure_reason = str(first_failure_trace.get("routing_reason") or "")
    checks.extend(
        [
            check_equal("failure-recovery-source", classify_source(first_failure_reason), "safe-control"),
            check_equal(
                "failure-recovery-model",
                first_failure_trace.get("routed_model") or first_failure_response.get("model") or "",
                PREMIUM_MODEL,
            ),
            check_equal("failure-recovery-budget-action", first_failure_trace.get("route_budget_action") or "", "premium_recover"),
            check_contains("failure-recovery-rule", first_failure_reason, "rule_id=episode_repeated_failure_recovery"),
            check_contains("failure-recovery-same-count", first_failure_reason, "same_failure_count=2"),
            check_contains("failure-recovery-frontier-size", first_failure_reason, "failure_frontier_size=2"),
            check_contains(
                "failure-recovery-fingerprint",
                first_failure_reason,
                "failure_fingerprint=assertionerror: expected relay id # got #",
            ),
        ]
    )

    second_failure_step = "episode-failure-after-local-recovery-route"
    second_failure_response = post_json(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {"role": "user", "content": "Continue with the next bounded implementation step."},
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": failure_session,
            "X-Episode-ID": failure_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": second_failure_step,
            "X-Task-Name": task,
        },
    )
    second_failure_trace = wait_for_agent_trace(port, failure_session, second_failure_step)
    second_failure_reason = str(second_failure_trace.get("routing_reason") or "")
    checks.extend(
        [
            check_equal("failure-second-route-source", classify_source(second_failure_reason), "decision-model"),
            check_equal(
                "failure-second-route-model",
                second_failure_trace.get("routed_model") or second_failure_response.get("model") or "",
                CHEAP_MODEL,
            ),
            check_not_contains(
                "failure-second-route-no-repeat-local-rule",
                second_failure_reason,
                "rule_id=episode_repeated_failure_recovery",
            ),
        ]
    )

    completion_events = [
        (
            "delivery-file",
            "file_written",
            {
                "target_paths": ["/app/output/answer.json"],
                "delivery_target": True,
                "workspace_target": False,
            },
        ),
        (
            "validation-passed",
            "test_run",
            {
                "outcome": "passed",
                "command": "python3 validate.py",
                "passed_count": 7,
                "failed_count": 0,
            },
        ),
        (
            "verifier-passed",
            "verifier_result",
            {
                "reward": 1.0,
            },
        ),
    ]
    for index, (name, kind, observation) in enumerate(completion_events, start=1):
        event_id = f"{completion_episode}__{name}"
        post_json(
            f"http://127.0.0.1:{port}/v1/episode-events",
            {
                "event_id": event_id,
                "episode_id": completion_episode,
                "episode_operation": "continue",
                "sequence": index,
                "kind": kind,
                "source": "safe-control-probe",
                "observation": observation,
                "evidence_refs": [f"probe:event:{event_id}"],
                "session_id": completion_session,
                "trial_name": trial,
                "step_name": f"episode-completion-{name}",
                "task_name": task,
            },
            headers={
                "X-Trial-Name": trial,
                "X-Session-ID": completion_session,
                "X-Episode-ID": completion_episode,
                "X-Episode-Operation": "continue",
                "X-Step-Name": f"episode-completion-{name}",
                "X-Task-Name": task,
            },
        )

    completion_state = fetch_episode_state(port, completion_episode)
    completion_payload = completion_state.get("state") or {}
    checks.extend(
        [
            check_equal("completion-state-version-after-events", completion_state.get("state_version"), 3),
            check_equal("completion-readiness", completion_payload.get("completion_readiness"), "verifier_passed"),
            check_equal("completion-delivery-writes", completion_payload.get("delivery_file_write_count"), 1),
            check_equal("completion-test-passed-count", completion_payload.get("test_passed_count"), 1),
            check_equal("completion-verifier-reward", completion_payload.get("verifier_reward"), 1),
            check_equal("completion-next-capability", completion_payload.get("next_min_capability"), "premium_assess"),
            check_equal("completion-next-budget", completion_payload.get("next_budget_action_hint"), "premium_reason"),
        ]
    )

    assessment_step = "episode-completion-assessment-route"
    assessment_response = post_json(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {"role": "user", "content": "Judge whether this delivery is ready for hidden grader submission."},
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": completion_session,
            "X-Episode-ID": completion_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": assessment_step,
            "X-Task-Name": task,
        },
    )
    assessment_trace = wait_for_agent_trace(port, completion_session, assessment_step)
    assessment_reason = str(assessment_trace.get("routing_reason") or "")
    checks.extend(
        [
            check_equal("assessment-route-source", classify_source(assessment_reason), "safe-control"),
            check_equal(
                "assessment-route-model",
                assessment_trace.get("routed_model") or assessment_response.get("model") or "",
                PREMIUM_MODEL,
            ),
            check_equal("assessment-route-budget-action", assessment_trace.get("route_budget_action") or "", "premium_reason"),
            check_contains("assessment-route-rule", assessment_reason, "rule_id=episode_verifier_assessment_floor"),
            check_contains("assessment-route-floor", assessment_reason, "capability_floor status=local"),
            check_contains("assessment-route-expected", assessment_reason, "expected=premium_assess"),
            check_contains("assessment-route-reason", assessment_reason, "reason=verifier_passed_current_delivery"),
            check_contains("assessment-route-readiness", assessment_reason, "completion_readiness=verifier_passed"),
            check_contains("assessment-route-verifier", assessment_reason, "verifier_reward=1.000"),
        ]
    )

    completion_step = "episode-completion-guardrail-route"
    completion_response = post_json(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {
                    "role": "user",
                    "content": 'Are you sure you want to mark the task as complete? Include "task_complete": true.',
                },
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": completion_session,
            "X-Episode-ID": completion_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": completion_step,
            "X-Task-Name": task,
        },
    )
    completion_trace = wait_for_agent_trace(port, completion_session, completion_step)
    completion_reason = str(completion_trace.get("routing_reason") or "")
    checks.extend(
        [
            check_equal("completion-route-source", classify_source(completion_reason), "guardrail"),
            check_equal(
                "completion-route-model",
                completion_trace.get("routed_model") or completion_response.get("model") or "",
                PREMIUM_MODEL,
            ),
            check_equal(
                "completion-route-budget-action",
                completion_trace.get("route_budget_action") or "",
                "completion_guardrail",
            ),
            check_contains("completion-route-readiness", completion_reason, "completion_readiness=verifier_passed"),
            check_contains("completion-route-delivery", completion_reason, "delivery_file_writes=1"),
            check_contains("completion-route-tests", completion_reason, "test_passed=1"),
            check_contains("completion-route-verifier", completion_reason, "verifier_reward=1.000"),
            check_contains("completion-route-last-progress", completion_reason, "last_progress=verifier_result"),
            check_contains("completion-route-next-capability", completion_reason, "next_min_capability=premium_assess"),
            check_contains("completion-route-next-budget", completion_reason, "next_budget_action_hint=premium_reason"),
        ]
    )

    completion_regress_events = [
        (
            "delivery-file",
            "file_written",
            {
                "target_paths": ["/app/output/answer.json"],
                "delivery_target": True,
                "workspace_target": False,
            },
        ),
        (
            "validation-passed",
            "test_run",
            {
                "outcome": "passed",
                "command": "python3 validate.py",
                "passed_count": 7,
                "failed_count": 0,
            },
        ),
        (
            "verifier-passed",
            "verifier_result",
            {
                "reward": 1.0,
            },
        ),
        (
            "delivery-update",
            "file_modified",
            {
                "target_paths": ["/app/output/answer.json"],
                "path_count": 1,
                "delivery_target": True,
                "workspace_target": False,
            },
        ),
    ]
    for index, (name, kind, observation) in enumerate(completion_regress_events, start=1):
        event_id = f"{completion_regress_episode}__{name}"
        post_json(
            f"http://127.0.0.1:{port}/v1/episode-events",
            {
                "event_id": event_id,
                "episode_id": completion_regress_episode,
                "episode_operation": "continue",
                "sequence": index,
                "kind": kind,
                "source": "safe-control-probe",
                "observation": observation,
                "evidence_refs": [f"probe:event:{event_id}"],
                "session_id": completion_regress_session,
                "trial_name": trial,
                "step_name": f"episode-completion-regress-{name}",
                "task_name": task,
            },
            headers={
                "X-Trial-Name": trial,
                "X-Session-ID": completion_regress_session,
                "X-Episode-ID": completion_regress_episode,
                "X-Episode-Operation": "continue",
                "X-Step-Name": f"episode-completion-regress-{name}",
                "X-Task-Name": task,
            },
        )

    completion_regress_state = fetch_episode_state(port, completion_regress_episode)
    completion_regress_payload = completion_regress_state.get("state") or {}
    checks.extend(
        [
            check_equal("completion-regress-state-version-after-events", completion_regress_state.get("state_version"), 4),
            check_equal(
                "completion-regress-readiness",
                completion_regress_payload.get("completion_readiness"),
                "delivery_candidate",
            ),
            check_equal(
                "completion-regress-delivery-writes",
                completion_regress_payload.get("delivery_file_write_count"),
                2,
            ),
            check_equal("completion-regress-verifier-reward", completion_regress_payload.get("verifier_reward"), 0),
            check_equal("completion-regress-next-capability", completion_regress_payload.get("next_min_capability"), "cheap_execute"),
            check_equal(
                "completion-regress-next-capability-reason",
                completion_regress_payload.get("next_capability_reason"),
                "target_changed_needs_validation",
            ),
        ]
    )

    completion_regress_step = "episode-completion-regress-guardrail-route"
    completion_regress_response = post_json(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {
                    "role": "user",
                    "content": 'Are you sure you want to mark the task as complete? Include "task_complete": true.',
                },
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": completion_regress_session,
            "X-Episode-ID": completion_regress_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": completion_regress_step,
            "X-Task-Name": task,
        },
    )
    completion_regress_trace = wait_for_agent_trace(port, completion_regress_session, completion_regress_step)
    completion_regress_reason = str(completion_regress_trace.get("routing_reason") or "")
    checks.extend(
        [
            check_equal("completion-regress-route-source", classify_source(completion_regress_reason), "guardrail"),
            check_equal(
                "completion-regress-route-model",
                completion_regress_trace.get("routed_model") or completion_regress_response.get("model") or "",
                PREMIUM_MODEL,
            ),
            check_contains(
                "completion-regress-route-readiness",
                completion_regress_reason,
                "completion_readiness=delivery_candidate",
            ),
            check_contains("completion-regress-route-delivery", completion_regress_reason, "delivery_file_writes=2"),
            check_contains("completion-regress-route-verifier", completion_regress_reason, "verifier_reward=0.000"),
            check_contains(
                "completion-regress-route-last-progress",
                completion_regress_reason,
                "last_progress=file_modified",
            ),
            check_contains(
                "completion-regress-route-next-capability",
                completion_regress_reason,
                "next_min_capability=cheap_execute",
            ),
            check_contains(
                "completion-regress-route-next-budget",
                completion_regress_reason,
                "next_budget_action_hint=cheap_execute",
            ),
        ]
    )

    capability_floor_event_id = f"{capability_floor_episode}__verifier-failed"
    post_json(
        f"http://127.0.0.1:{port}/v1/episode-events",
        {
            "event_id": capability_floor_event_id,
            "episode_id": capability_floor_episode,
            "episode_operation": "continue",
            "sequence": 1,
            "kind": "verifier_result",
            "source": "safe-control-probe",
            "observation": {
                "reward": 0,
            },
            "evidence_refs": [f"probe:event:{capability_floor_event_id}"],
            "session_id": capability_floor_session,
            "trial_name": trial,
            "step_name": "episode-capability-floor-verifier-failed",
            "task_name": task,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": capability_floor_session,
            "X-Episode-ID": capability_floor_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": "episode-capability-floor-verifier-failed",
            "X-Task-Name": task,
        },
    )
    capability_floor_state = fetch_episode_state(port, capability_floor_episode)
    capability_floor_payload = capability_floor_state.get("state") or {}
    checks.extend(
        [
            check_equal("capability-floor-state-version", capability_floor_state.get("state_version"), 1),
            check_equal("capability-floor-next-capability", capability_floor_payload.get("next_min_capability"), "premium_recover"),
            check_equal(
                "capability-floor-next-reason",
                capability_floor_payload.get("next_capability_reason"),
                "verifier_failed_current_delivery",
            ),
        ]
    )

    capability_floor_step = "episode-capability-floor-route"
    capability_floor_response = post_json(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {"role": "user", "content": "Continue after the failed verifier result."},
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": capability_floor_session,
            "X-Episode-ID": capability_floor_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": capability_floor_step,
            "X-Task-Name": task,
        },
    )
    capability_floor_trace = wait_for_agent_trace(port, capability_floor_session, capability_floor_step)
    capability_floor_reason = str(capability_floor_trace.get("routing_reason") or "")
    checks.extend(
        [
            check_equal("capability-floor-route-source", classify_source(capability_floor_reason), "safe-control"),
            check_equal(
                "capability-floor-route-model",
                capability_floor_trace.get("routed_model") or capability_floor_response.get("model") or "",
                PREMIUM_MODEL,
            ),
            check_equal(
                "capability-floor-route-budget-action",
                capability_floor_trace.get("route_budget_action") or "",
                "premium_recover",
            ),
            check_contains("capability-floor-route-rule", capability_floor_reason, "rule_id=episode_verifier_recovery_floor"),
            check_contains("capability-floor-route-status", capability_floor_reason, "capability_floor status=local"),
            check_contains("capability-floor-route-expected", capability_floor_reason, "expected=premium_recover"),
            check_contains(
                "capability-floor-route-reason",
                capability_floor_reason,
                "reason=verifier_failed_current_delivery",
            ),
            check_contains("capability-floor-route-readiness", capability_floor_reason, "completion_readiness=verifier_failed"),
            check_contains("capability-floor-route-verifier", capability_floor_reason, "verifier_reward=0.000"),
        ]
    )

    stop_gate_events: list[dict[str, Any]] = []
    for index in range(1, 51):
        event_id = f"{stop_gate_episode}__llm-{index}"
        action = "premium_recover" if index == 50 else "cheap_execute"
        stop_gate_events.append(
            {
                "event_id": event_id,
                "episode_id": stop_gate_episode,
                "episode_operation": "continue",
                "sequence": index,
                "kind": "llm_call",
                "source": "safe-control-probe",
                "observation": {
                    "outcome": "response_completed",
                    "model": PREMIUM_MODEL if action == "premium_recover" else CHEAP_MODEL,
                    "routed_model": PREMIUM_MODEL if action == "premium_recover" else CHEAP_MODEL,
                    "budget_action": action,
                    "finish_reason": "stop",
                    "status": 200,
                    "total_tokens": 100,
                    "cost_usd": 0.001,
                    "latency_ms": 1000,
                },
                "evidence_refs": [f"probe:event:{event_id}"],
                "session_id": stop_gate_session,
                "trial_name": trial,
                "step_name": f"episode-stop-gate-llm-{index:02d}",
                "task_name": task,
            }
        )
    stop_batch_response = post_json(
        f"http://127.0.0.1:{port}/v1/episode-events",
        {"events": stop_gate_events},
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": stop_gate_session,
            "X-Episode-ID": stop_gate_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": "episode-stop-gate-injected-llm-batch",
            "X-Task-Name": task,
        },
    )
    checks.extend(
        [
            check_equal("stop-gate-batch-event-ingest-count", stop_batch_response.get("count"), 50),
            check_equal("stop-gate-batch-event-ingest-sinks", stop_batch_response.get("sinks"), 2),
        ]
    )

    stop_gate_state = fetch_episode_state(port, stop_gate_episode)
    stop_gate_payload = stop_gate_state.get("state") or {}
    checks.extend(
        [
            check_equal("stop-gate-state-version", stop_gate_state.get("state_version"), 50),
            check_equal("stop-gate-no-progress-severity", stop_gate_payload.get("no_progress_severity"), "blocked"),
            check_equal("stop-gate-llm-since-progress", stop_gate_payload.get("llm_calls_since_progress"), 50),
            check_equal("stop-gate-last-budget", stop_gate_payload.get("last_budget_action"), "premium_recover"),
            check_equal("stop-gate-last-route-outcome", stop_gate_payload.get("last_route_outcome_label"), "pending"),
        ]
    )

    stop_step = "episode-stop-gate-route"
    stop_status, stop_response = post_json_status(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {"role": "user", "content": "Continue after the blocked premium recovery attempt."},
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": stop_gate_session,
            "X-Episode-ID": stop_gate_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": stop_step,
            "X-Task-Name": task,
        },
    )
    stop_trace = wait_for_agent_trace(port, stop_gate_session, stop_step)
    stop_reason = str(stop_trace.get("routing_reason") or "")
    stop_error = stop_response.get("error") or {}
    checks.extend(
        [
            check_equal("stop-gate-response-status", stop_status, 409),
            check_equal("stop-gate-response-type", stop_error.get("type"), "gateway_stop_gate"),
            check_equal("stop-gate-trace-status", stop_trace.get("status"), 409),
            check_equal("stop-gate-trace-pool", stop_trace.get("pool"), "local"),
            check_equal("stop-gate-trace-error-kind", stop_trace.get("error_kind"), "gateway_stop_gate"),
            check_equal("stop-gate-route-source", classify_source(stop_reason), "safe-control"),
            check_equal("stop-gate-budget-action", stop_trace.get("route_budget_action") or "", "stop_trial"),
            check_contains("stop-gate-route-rule", stop_reason, "rule_id=episode_blocked_stop_gate"),
            check_contains("stop-gate-route-no-progress", stop_reason, "no_progress=blocked"),
            check_contains("stop-gate-route-last-budget", stop_reason, "last_budget=premium_recover"),
            check_contains("stop-gate-route-last-outcome", stop_reason, "last_route_outcome=pending"),
        ]
    )

    provider_incomplete_event_id = f"{provider_incomplete_episode}__llm-provider-incomplete"
    post_json(
        f"http://127.0.0.1:{port}/v1/episode-events",
        {
            "event_id": provider_incomplete_event_id,
            "episode_id": provider_incomplete_episode,
            "episode_operation": "continue",
            "sequence": 1,
            "kind": "llm_call",
            "source": "safe-control-probe",
            "observation": {
                "outcome": "provider_incomplete",
                "model": CHEAP_MODEL,
                "routed_model": CHEAP_MODEL,
                "budget_action": "cheap_execute",
                "status": 200,
                "total_tokens": 0,
                "cost_usd": 0,
                "latency_ms": 60000,
            },
            "evidence_refs": [f"probe:event:{provider_incomplete_event_id}"],
            "session_id": provider_incomplete_session,
            "trial_name": trial,
            "step_name": "episode-provider-incomplete-injected",
            "task_name": task,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": provider_incomplete_session,
            "X-Episode-ID": provider_incomplete_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": "episode-provider-incomplete-injected",
            "X-Task-Name": task,
        },
    )
    provider_incomplete_step = "episode-provider-incomplete-stop-route"
    provider_incomplete_status, provider_incomplete_response = post_json_status(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {"role": "user", "content": "Continue after provider returned an incomplete response."},
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": provider_incomplete_session,
            "X-Episode-ID": provider_incomplete_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": provider_incomplete_step,
            "X-Task-Name": task,
        },
    )
    provider_incomplete_trace = wait_for_agent_trace(port, provider_incomplete_session, provider_incomplete_step)
    provider_incomplete_reason = str(provider_incomplete_trace.get("routing_reason") or "")
    provider_incomplete_error = provider_incomplete_response.get("error") or {}
    checks.extend(
        [
            check_equal("provider-incomplete-stop-status", provider_incomplete_status, 409),
            check_equal(
                "provider-incomplete-stop-response-type",
                provider_incomplete_error.get("type"),
                "gateway_provider_incomplete_stop_gate",
            ),
            check_equal(
                "provider-incomplete-stop-error-kind",
                provider_incomplete_trace.get("error_kind"),
                "gateway_provider_incomplete_stop_gate",
            ),
            check_equal(
                "provider-incomplete-stop-budget-action",
                provider_incomplete_trace.get("route_budget_action") or "",
                "stop_trial",
            ),
            check_contains(
                "provider-incomplete-stop-rule",
                provider_incomplete_reason,
                "rule_id=episode_provider_incomplete_stop_gate",
            ),
            check_contains("provider-incomplete-stop-outcome", provider_incomplete_reason, "last_outcome=provider_incomplete"),
        ]
    )

    cost_stop_event_id = f"{cost_stop_episode}__llm-cost-threshold"
    post_json(
        f"http://127.0.0.1:{port}/v1/episode-events",
        {
            "event_id": cost_stop_event_id,
            "episode_id": cost_stop_episode,
            "episode_operation": "continue",
            "sequence": 1,
            "kind": "llm_call",
            "source": "safe-control-probe",
            "observation": {
                "outcome": "response_completed",
                "model": PREMIUM_MODEL,
                "routed_model": PREMIUM_MODEL,
                "budget_action": "premium_reason",
                "finish_reason": "stop",
                "status": 200,
                "total_tokens": 1000,
                "cost_usd": 4.01,
                "latency_ms": 1000,
            },
            "evidence_refs": [f"probe:event:{cost_stop_event_id}"],
            "session_id": cost_stop_session,
            "trial_name": trial,
            "step_name": "episode-cost-stop-injected",
            "task_name": task,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": cost_stop_session,
            "X-Episode-ID": cost_stop_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": "episode-cost-stop-injected",
            "X-Task-Name": task,
        },
    )
    cost_stop_step = "episode-cost-stop-route"
    cost_stop_status, cost_stop_response = post_json_status(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {"role": "user", "content": "Continue after the expensive attempt."},
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": cost_stop_session,
            "X-Episode-ID": cost_stop_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": cost_stop_step,
            "X-Task-Name": task,
        },
    )
    cost_stop_trace = wait_for_agent_trace(port, cost_stop_session, cost_stop_step)
    cost_stop_reason = str(cost_stop_trace.get("routing_reason") or "")
    cost_stop_error = cost_stop_response.get("error") or {}
    checks.extend(
        [
            check_equal("cost-stop-status", cost_stop_status, 409),
            check_equal("cost-stop-response-type", cost_stop_error.get("type"), "gateway_cost_stop_gate"),
            check_equal("cost-stop-error-kind", cost_stop_trace.get("error_kind"), "gateway_cost_stop_gate"),
            check_equal("cost-stop-budget-action", cost_stop_trace.get("route_budget_action") or "", "stop_trial"),
            check_contains("cost-stop-rule", cost_stop_reason, "rule_id=episode_cost_without_verifier_stop_gate"),
            check_contains("cost-stop-total-cost", cost_stop_reason, "total_cost=$4.0100"),
            check_contains("cost-stop-threshold", cost_stop_reason, "stop_cost_usd=$4.0000"),
            check_contains("cost-stop-readiness", cost_stop_reason, "completion_readiness=none"),
        ]
    )

    length_stop_events: list[dict[str, Any]] = []
    for index in range(1, 4):
        event_id = f"{length_stop_episode}__llm-length-{index}"
        length_stop_events.append(
            {
                "event_id": event_id,
                "episode_id": length_stop_episode,
                "episode_operation": "continue",
                "sequence": index,
                "kind": "llm_call",
                "source": "safe-control-probe",
                "observation": {
                    "outcome": "length_truncated",
                    "model": CHEAP_MODEL,
                    "routed_model": CHEAP_MODEL,
                    "budget_action": "cheap_execute",
                    "finish_reason": "length",
                    "status": 200,
                    "total_tokens": 1536,
                    "cost_usd": 0.0001,
                    "latency_ms": 60000,
                },
                "evidence_refs": [f"probe:event:{event_id}"],
                "session_id": length_stop_session,
                "trial_name": trial,
                "step_name": f"episode-length-stop-llm-{index}",
                "task_name": task,
            }
        )
    post_json(
        f"http://127.0.0.1:{port}/v1/episode-events",
        {"events": length_stop_events},
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": length_stop_session,
            "X-Episode-ID": length_stop_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": "episode-length-stop-injected-batch",
            "X-Task-Name": task,
        },
    )
    length_stop_step = "episode-length-pressure-stop-route"
    length_stop_status, length_stop_response = post_json_status(
        f"http://127.0.0.1:{port}/v1/chat/completions",
        {
            "model": "auto",
            "messages": [
                {"role": "system", "content": "You are a terminal coding agent."},
                {"role": "user", "content": "Continue after repeated truncation."},
            ],
            "temperature": 0,
            "max_tokens": 32,
        },
        headers={
            "X-Trial-Name": trial,
            "X-Session-ID": length_stop_session,
            "X-Episode-ID": length_stop_episode,
            "X-Episode-Operation": "continue",
            "X-Step-Name": length_stop_step,
            "X-Task-Name": task,
        },
    )
    length_stop_trace = wait_for_agent_trace(port, length_stop_session, length_stop_step)
    length_stop_reason = str(length_stop_trace.get("routing_reason") or "")
    length_stop_error = length_stop_response.get("error") or {}
    checks.extend(
        [
            check_equal("length-stop-status", length_stop_status, 409),
            check_equal("length-stop-response-type", length_stop_error.get("type"), "gateway_length_pressure_stop_gate"),
            check_equal(
                "length-stop-error-kind",
                length_stop_trace.get("error_kind"),
                "gateway_length_pressure_stop_gate",
            ),
            check_equal("length-stop-budget-action", length_stop_trace.get("route_budget_action") or "", "stop_trial"),
            check_contains("length-stop-rule", length_stop_reason, "rule_id=episode_length_pressure_stop_gate"),
            check_contains("length-stop-pressure", length_stop_reason, "length_since_progress=3"),
            check_contains("length-stop-threshold", length_stop_reason, "stop_length_pressure_threshold=3"),
            check_contains("length-stop-file-writes", length_stop_reason, "file_writes=0"),
            check_contains("length-stop-test-runs", length_stop_reason, "test_runs=0"),
        ]
    )

    return {
        "name": "episode-runtime-state-controller",
        "episode_id": episode,
        "session_id": session,
        "failure_episode_id": failure_episode,
        "failure_session_id": failure_session,
        "completion_episode_id": completion_episode,
        "completion_session_id": completion_session,
        "completion_regress_episode_id": completion_regress_episode,
        "completion_regress_session_id": completion_regress_session,
        "capability_floor_episode_id": capability_floor_episode,
        "capability_floor_session_id": capability_floor_session,
        "stop_gate_episode_id": stop_gate_episode,
        "stop_gate_session_id": stop_gate_session,
        "provider_incomplete_episode_id": provider_incomplete_episode,
        "provider_incomplete_session_id": provider_incomplete_session,
        "cost_stop_episode_id": cost_stop_episode,
        "cost_stop_session_id": cost_stop_session,
        "length_stop_episode_id": length_stop_episode,
        "length_stop_session_id": length_stop_session,
        "checks": checks,
        "state_before_route": state_before,
        "state_after_duplicate": state_after_duplicate,
        "state_after_route": state_after_route,
        "state_after_route_outcome": state_after_route_outcome,
        "route_trace": route_trace,
        "failure_state": failure_state,
        "first_failure_route_trace": first_failure_trace,
        "second_failure_route_trace": second_failure_trace,
        "completion_state": completion_state,
        "assessment_route_trace": assessment_trace,
        "completion_route_trace": completion_trace,
        "completion_regress_state": completion_regress_state,
        "completion_regress_route_trace": completion_regress_trace,
        "capability_floor_state": capability_floor_state,
        "capability_floor_route_trace": capability_floor_trace,
        "stop_gate_state": stop_gate_state,
        "stop_gate_route_trace": stop_trace,
        "provider_incomplete_stop_route_trace": provider_incomplete_trace,
        "cost_stop_route_trace": cost_stop_trace,
        "length_stop_route_trace": length_stop_trace,
    }


def latest_agent_trace(traces: list[dict[str, Any]], step_name: str) -> dict[str, Any]:
    matches = [
        trace
        for trace in traces
        if trace.get("step_name") == step_name and trace.get("pool") != "decision-model"
    ]
    if not matches:
        return {}
    return sorted(matches, key=lambda trace: trace.get("timestamp") or "")[-1]


def wait_for_agent_trace(port: int, session_id: str, step_name: str, timeout_seconds: float = 3.0) -> dict[str, Any]:
    deadline = time.time() + timeout_seconds
    latest: dict[str, Any] = {}
    while time.time() < deadline:
        traces = fetch_json(f"http://127.0.0.1:{port}/v1/traces?session_id={session_id}&limit=1000")
        latest = latest_agent_trace(traces.get("traces", []), step_name)
        if latest:
            return latest
        time.sleep(0.05)
    return latest


def classify_source(reason: str) -> str:
    if reason.startswith("smart-router safe-control:"):
        return "safe-control"
    if reason.startswith("smart-router guardrail:"):
        return "guardrail"
    if reason.startswith("smart-router fallback="):
        return "fallback"
    if reason.startswith("smart-router:"):
        return "decision-model"
    if reason.startswith("cached:"):
        return "cache"
    return "unknown"


def summarize(cases: list[dict[str, Any]], traces: list[dict[str, Any]], episode_probe: dict[str, Any]) -> dict[str, Any]:
    agent_traces = [trace for trace in traces if trace.get("pool") != "decision-model"]
    decision_traces = [trace for trace in traces if trace.get("pool") == "decision-model"]
    source_counts: dict[str, int] = {}
    model_counts: dict[str, int] = {}
    rule_counts: dict[str, int] = {}
    budget_counts: dict[str, int] = {}
    episode_adjust_calls = 0
    for case in cases:
        source_counts[case["actual_source"]] = source_counts.get(case["actual_source"], 0) + 1
        model_counts[case["actual_model"]] = model_counts.get(case["actual_model"], 0) + 1
        budget_action = str(case.get("route_budget_action") or "")
        if budget_action:
            budget_counts[budget_action] = budget_counts.get(budget_action, 0) + 1
        if case["actual_source"] == "safe-control":
            rule_id = extract_rule_id(case["routing_reason"])
            rule_counts[rule_id] = rule_counts.get(rule_id, 0) + 1
        if "episode_adjust=" in case["routing_reason"]:
            episode_adjust_calls += 1
    trace_counts = summarize_traces(agent_traces)
    episode_summary = summarize_episode_probe(episode_probe)
    case_failures = [case["name"] for case in cases if not case["pass"]]
    episode_failures = [f"episode:{name}" for name in episode_summary["failed_checks"]]
    return {
        "cases": len(cases),
        "passed_expectations": sum(1 for case in cases if case["pass"]),
        "failed_expectations": case_failures + episode_failures,
        "failed_case_expectations": case_failures,
        "missing_route_budget": [case["name"] for case in cases if not case.get("route_budget_action")],
        "agent_traces": len(agent_traces),
        "decision_traces": len(decision_traces),
        "source_counts": trace_counts["source_counts"],
        "model_counts": trace_counts["model_counts"],
        "safe_control_rule_counts": trace_counts["safe_control_rule_counts"],
        "route_budget_action_counts": trace_counts["route_budget_action_counts"],
        "episode_adjust_call_count": trace_counts["episode_adjust_call_count"],
        "case_source_counts": source_counts,
        "case_model_counts": model_counts,
        "case_safe_control_rule_counts": rule_counts,
        "case_route_budget_action_counts": budget_counts,
        "case_episode_adjust_call_count": episode_adjust_calls,
        "trace_source_counts": trace_counts["source_counts"],
        "trace_model_counts": trace_counts["model_counts"],
        "trace_safe_control_rule_counts": trace_counts["safe_control_rule_counts"],
        "trace_route_budget_action_counts": trace_counts["route_budget_action_counts"],
        "trace_episode_adjust_call_count": trace_counts["episode_adjust_call_count"],
        "episode_probe": episode_summary,
        "total_cost_usd": round(sum(float(trace.get("cost") or 0) for trace in traces), 8),
    }


def summarize_traces(agent_traces: list[dict[str, Any]]) -> dict[str, Any]:
    source_counts: dict[str, int] = {}
    model_counts: dict[str, int] = {}
    rule_counts: dict[str, int] = {}
    budget_counts: dict[str, int] = {}
    episode_adjust_calls = 0
    for trace in agent_traces:
        reason = str(trace.get("routing_reason") or "")
        source = classify_source(reason)
        source_counts[source] = source_counts.get(source, 0) + 1
        model = str(trace.get("routed_model") or trace.get("model") or "")
        if model:
            model_counts[model] = model_counts.get(model, 0) + 1
        budget_action = str(trace.get("route_budget_action") or "")
        if budget_action:
            budget_counts[budget_action] = budget_counts.get(budget_action, 0) + 1
        if source == "safe-control":
            rule_id = extract_rule_id(reason)
            rule_counts[rule_id] = rule_counts.get(rule_id, 0) + 1
        if "episode_adjust=" in reason:
            episode_adjust_calls += 1
    return {
        "source_counts": source_counts,
        "model_counts": model_counts,
        "safe_control_rule_counts": rule_counts,
        "route_budget_action_counts": budget_counts,
        "episode_adjust_call_count": episode_adjust_calls,
    }


def summarize_episode_probe(episode_probe: dict[str, Any]) -> dict[str, Any]:
    checks = episode_probe.get("checks") or []
    failed = [check["name"] for check in checks if not check.get("pass")]
    return {
        "name": episode_probe.get("name") or "",
        "checks": len(checks),
        "passed_checks": len(checks) - len(failed),
        "failed_checks": failed,
        "episode_id": episode_probe.get("episode_id") or "",
    }


def extract_rule_id(reason: str) -> str:
    marker = "rule_id="
    if marker not in reason:
        return "unknown"
    tail = reason.split(marker, 1)[1]
    return tail.split()[0]


def write_outputs(out_dir: Path, result: dict[str, Any]) -> None:
    (out_dir / "safe-control-probe.json").write_text(
        json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    with (out_dir / "safe-control-probe.csv").open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(
            handle,
            fieldnames=[
                "index",
                "name",
                "expected_source",
                "expected_model",
                "actual_source",
                "actual_model",
                "route_budget_action",
                "route_max_tokens",
                "route_timeout_ms",
                "pass",
                "routing_reason",
                "response_model",
            ],
        )
        writer.writeheader()
        writer.writerows(result["cases"])


def fetch_episode_state(port: int, episode_id: str) -> dict[str, Any]:
    encoded = urllib.parse.quote(episode_id, safe="")
    body = fetch_json(f"http://127.0.0.1:{port}/v1/episode-state?episode_id={encoded}&limit=10")
    states = body.get("states") or []
    if not states:
        return {}
    for state in states:
        if state.get("episode_id") == episode_id:
            return state
    return states[0]


def check_equal(name: str, actual: Any, expected: Any) -> dict[str, Any]:
    return {
        "name": name,
        "pass": actual == expected,
        "actual": actual,
        "expected": expected,
    }


def check_contains(name: str, actual: str, expected_fragment: str) -> dict[str, Any]:
    return {
        "name": name,
        "pass": expected_fragment in actual,
        "actual": actual,
        "expected": expected_fragment,
    }


def check_not_contains(name: str, actual: str, forbidden_fragment: str) -> dict[str, Any]:
    return {
        "name": name,
        "pass": forbidden_fragment not in actual,
        "actual": actual,
        "forbidden": forbidden_fragment,
    }


def post_json_status(url: str, payload: dict[str, Any], headers: dict[str, str]) -> tuple[int, dict[str, Any]]:
    raw = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=raw, method="POST")
    req.add_header("Content-Type", "application/json")
    for key, value in headers.items():
        req.add_header(key, value)
    try:
        with urllib.request.urlopen(req, timeout=15) as response:
            return response.status, json.loads(response.read())
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")
        try:
            parsed = json.loads(body)
        except json.JSONDecodeError:
            parsed = {"error": {"message": body}}
        return exc.code, parsed


def post_json(url: str, payload: dict[str, Any], headers: dict[str, str]) -> dict[str, Any]:
    status, body = post_json_status(url, payload, headers)
    if status >= 400:
        raise RuntimeError(f"POST {url} failed with {status}: {json.dumps(body)}")
    return body


def fetch_json(url: str) -> dict[str, Any]:
    with urllib.request.urlopen(url, timeout=10) as response:
        return json.loads(response.read())


if __name__ == "__main__":
    main()
