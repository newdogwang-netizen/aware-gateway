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
            session_traces = [
                trace
                for trace in traces.get("traces", [])
                if trace.get("session_id") == result["session_id"]
            ]
            result["traces"] = session_traces
            result["decision_endpoint_calls"] = len(MockDecisionHandler.calls)
            result["summary"] = summarize(result["cases"], session_traces)
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
                "X-Step-Name": f"{index:02d}-{case.name}",
                "X-Task-Name": "phase2-safe-control-probe",
            },
        )
        traces = fetch_json(f"http://127.0.0.1:{port}/v1/traces?session_id={session}&limit=1000")
        agent_trace = latest_agent_trace(traces.get("traces", []), f"{index:02d}-{case.name}")
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
    return {"trial_name": trial, "session_id": session, "cases": cases}


def latest_agent_trace(traces: list[dict[str, Any]], step_name: str) -> dict[str, Any]:
    matches = [
        trace
        for trace in traces
        if trace.get("step_name") == step_name and trace.get("pool") != "decision-model"
    ]
    if not matches:
        return {}
    return sorted(matches, key=lambda trace: trace.get("timestamp") or "")[-1]


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


def summarize(cases: list[dict[str, Any]], traces: list[dict[str, Any]]) -> dict[str, Any]:
    agent_traces = [trace for trace in traces if trace.get("pool") != "decision-model"]
    decision_traces = [trace for trace in traces if trace.get("pool") == "decision-model"]
    source_counts: dict[str, int] = {}
    model_counts: dict[str, int] = {}
    rule_counts: dict[str, int] = {}
    budget_counts: dict[str, int] = {}
    for case in cases:
        source_counts[case["actual_source"]] = source_counts.get(case["actual_source"], 0) + 1
        model_counts[case["actual_model"]] = model_counts.get(case["actual_model"], 0) + 1
        budget_action = str(case.get("route_budget_action") or "")
        if budget_action:
            budget_counts[budget_action] = budget_counts.get(budget_action, 0) + 1
        if case["actual_source"] == "safe-control":
            rule_id = extract_rule_id(case["routing_reason"])
            rule_counts[rule_id] = rule_counts.get(rule_id, 0) + 1
    return {
        "cases": len(cases),
        "passed_expectations": sum(1 for case in cases if case["pass"]),
        "failed_expectations": [case["name"] for case in cases if not case["pass"]],
        "missing_route_budget": [case["name"] for case in cases if not case.get("route_budget_action")],
        "agent_traces": len(agent_traces),
        "decision_traces": len(decision_traces),
        "source_counts": source_counts,
        "model_counts": model_counts,
        "safe_control_rule_counts": rule_counts,
        "route_budget_action_counts": budget_counts,
        "total_cost_usd": round(sum(float(trace.get("cost") or 0) for trace in traces), 8),
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


def post_json(url: str, payload: dict[str, Any], headers: dict[str, str]) -> dict[str, Any]:
    raw = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=raw, method="POST")
    req.add_header("Content-Type", "application/json")
    for key, value in headers.items():
        req.add_header(key, value)
    try:
        with urllib.request.urlopen(req, timeout=15) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"POST {url} failed with {exc.code}: {body}") from exc


def fetch_json(url: str) -> dict[str, Any]:
    with urllib.request.urlopen(url, timeout=10) as response:
        return json.loads(response.read())


if __name__ == "__main__":
    main()
