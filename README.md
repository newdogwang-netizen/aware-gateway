# aware-gateway

A smart, plugin-based AI model gateway. Single Go binary, zero runtime dependencies.

## What It Is

aware-gateway is a reverse proxy for LLM and ASR backends with a **plugin architecture** that adds:

- **Task-aware routing**: analyzes incoming requests and selects the best model based on task type, cost, latency, and current load
- **LLM-based smart routing**: uses a decision model to route each turn across premium/flash models, with optional cost awareness and compact per-session memory
- **GenAI observability**: native OpenTelemetry + OpenInference semantic conventions (gen_ai.* / llm.*), compatible with Jaeger, Tempo, Grafana, Phoenix, Langfuse
- **Pluggable everything**: routing, auth, audit, middleware, response transformation — all via a clean plugin interface

Built by refactoring the heidi model-gateway into a core engine + plugin system.

## Architecture

```
                         ┌──────────────────────────────────────┐
                         │        aware-gateway (:9090)         │
                         │                                      │
  Client ───────────────→│  Plugin Middleware Chain:            │
                         │  ┌─ RateLimiter (plugin)             │
                         │  ├─ OtelGenAI (GenAI spans+metrics)  │
                         │  ├─ AuditLog (plugin)                │
                         │  └─ Core Handler                     │
                         │     ├─ Authenticators (chain)        │
                         │     ├─ RequestRouters (chain)        │
                         │     │   ├─ SmartRouter: decide with   │
                         │     │   │  LLM + history/cost hints   │
                         │     │   └─ TaskRouter: classify →     │
                         │     │      score → select model       │
                         │     ├─ RequestTransformers (pipeline)│
                         │     ├─ Proxy (retry+breaker+fallback)│
                         │     └─ AuditSinks (fan-out)          │
                         │                                      │
                         │  Endpoints:                          │
                         │  /health, /metrics, /v1/plugins      │
                         │                                      │
                         │  Observability:                      │
                         │  OTel traces (gen_ai.* / llm.*)      │
                         │  Prometheus /metrics                 │
                         └──────────────────────────────────────┘
                                    │
                    ┌───────────────┼───────────────┐
                    ▼               ▼               ▼
              vLLM Pool       OpenAI Pool     Fireworks Pool
              (on-prem)       (cloud)         (cloud)
```

## Plugin System

### Hook Interfaces

| Hook | Execution | Purpose |
|------|-----------|---------|
| `RequestRouter` | Chain (first wins) | Decide pool/model per request |
| `RequestTransformer` | Pipeline | Modify request body |
| `ResponseTransformer` | Pipeline | Modify upstream response |
| `Authenticator` | AND (all must pass) | Validate requests |
| `AuditSink` | Fan-out | Receive completed audit records |
| `MiddlewareProvider` | Wrapped in order | Custom HTTP middleware |
| `HealthReporter` | Called on /health | Plugin health status |

### Built-in Plugins

| Plugin | Hooks | Description |
|--------|-------|-------------|
| `smart-router` | RequestRouter, AuditSink | Uses safe-control rules plus a decision model to choose among configured models per turn; supports warm-start, cost-aware prompts, fallback, compact decision history, and minimal episode state |
| `task-router` | RequestRouter | Classifies LLM requests (chat/code/reasoning/vision) and selects best model by cost/quality/latency/load |
| `otel-genai` | Middleware, Health | Enriches OTel spans with gen_ai.* / llm.* attributes; records GenAI Prometheus metrics |
| `ratelimit` | Middleware | Global + per-key rate limiting (token bucket) |
| `audit` | AuditSink | Structured log + SQLite audit store |

## Experiment Reports

- [Phase 1 Smart Router Experiment Report](docs/smart-router-experiment-report.html) - prompt-based router benchmark runs, decision replay studies, cost/quality tradeoffs, and first-stage lessons.
- [Phase 2 Safe Control Report](docs/smart-router-phase2-safe-control-report.html) - follow-up validation for Issue #1, covering local safe-control rules, real Harbor pilots, and the next budget-control problem.
- [V4 Experiment Plan 中文](docs/experiment-plan-v4.zh.md) - current reduced-scope benchmark design for comparing smart routing against public leaderboard baselines.
- [RSI Improvement Experiment V1 中文](docs/experiment-plan-rsi-v1.zh.md) - controlled router self-improvement plan for adding outcome-aware replay and pilot gates.
- [Router Decision Lab Notes](docs/router-decision-lab.md) - notes for replaying router decisions under different prompt designs.
- [Benchmark Cost Report](docs/benchmark-cost.html) - static benchmark cost reference.

## RSI Outcome Extractor

RSI R1 adds an offline extractor for turning Harbor trial artifacts and gateway traces into auditable episode state. It now projects gateway LLM calls plus Harbor tool calls, file writes, local test/validation runs, final patch/verifier output, tiered progress, and a windowed no-progress state:

- [Event Schema v1](docs/rsi/event-schema-v1.json)
- [Progress Rules v1](docs/rsi/progress-rules-v1.yaml)
- [Candidate Manifest Template](docs/rsi/candidate-manifest.template.json)
- [RSI Contract Notes](docs/rsi/README.md)

Run the extractor with `python3 scripts/extract_episode_outcomes.py --trial-dir <trial-or-job-dir> --traces-json <gateway-traces.json> --output-dir <out> --strict`.

Replay outcome-aware routing with `python3 scripts/replay_episode_decisions.py --episode-dir <out> --output <router-replay-rsi-p2.json> --prompt-id rsi-p2-windowed-progress-v1 --model openai/gpt-5.6-sol --resume`.

Evaluate a candidate policy against matched baseline/candidate episode
summaries with
`python3 scripts/evaluate_rsi_policy_gate.py --baseline-summary <baseline-out> --candidate-summary <candidate-out> --candidate-replay <router-replay-rsi-p2.json> --output <policy-gate.json>`.
The gate returns `accept`, `reject`, or `needs_more_data` from final reward,
cost per success, future-evidence leakage, replay evidence coverage, and
matched task/run counts.

Use `make test-scripts` to validate the extractor fixture and replay cutoff guard.
After building the gateway, run the deterministic runtime probe to exercise the
compiled proxy with mock upstreams:

```bash
make build
python3 scripts/run_safe_control_probe.py \
  --repo . \
  --gateway-bin ./aware-gateway \
  --out-dir /tmp/aware-safe-control-probe
```

The probe covers safe-control rules, decision-model fallthrough, budget actions,
audit trace queries, and the online Episode loop:
`POST /v1/episode-events` -> `GET /v1/episode-state` -> state-driven
`premium_recover` routing. It also checks that repeated non-improving failed
test events trigger one local recovery without creating an endless local
recovery loop, and that completion guardrail traces include delivery,
validation, verifier readiness evidence, and stale-verifier invalidation when
target files change after verification. It also checks the online route-outcome
window that links a posted validation event back to the previous routed agent
call, exposes the derived next minimum capability for the following turn, and
verifies hard capability-floor recovery after a failed verifier result. The
current probe also covers bounded replan hypothesis application, truncated
hypothesis recovery, delivery-candidate floor exhaustion, analysis-progress
application/recovery, execution-stall recovery, and exact safe-control rule-id
matching.

### Writing a Custom Plugin

```go
package myplugin

import "github.com/aware/gateway/internal/plugin"

type Plugin struct{}

func (p *Plugin) Name() string { return "my-plugin" }
func (p *Plugin) Init(ctx *plugin.Context) error { return nil }
func (p *Plugin) Close() error { return nil }

// Implement one or more hooks:
func (p *Plugin) Route(req *http.Request, body []byte) (*plugin.RoutingDecision, error) {
    // Your routing logic
    return &plugin.RoutingDecision{
        Pool:  "my-pool",
        Model: "my-model",
        Reason: "custom logic",
    }, nil
}
```

Register in `cmd/gateway/main.go`:
```go
registry.Register(&myplugin.Plugin{})
```

## GenAI Observability

### Semantic Conventions

The `otel-genai` plugin supports two convention namespaces:

**OTel GenAI (gen_ai.*)** — the standard:
- `gen_ai.system` — provider (openai, anthropic, vllm, ...)
- `gen_ai.operation` — chat, embeddings
- `gen_ai.request.model`, `gen_ai.request.max_tokens`, `gen_ai.request.temperature`
- `gen_ai.response.model`, `gen_ai.response.finish_reasons`
- `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`

**OpenInference (llm.*)** — Arize/Langfuse compatible:
- `llm.vendor`, `llm.model_name`
- `llm.invocation_parameters`
- `llm.token_count.prompt`, `llm.token_count.completion`, `llm.token_count.total`

Configure with `convention: gen_ai | openinference | both`.

### Trace Exporters

| Exporter | Config | Use Case |
|----------|--------|----------|
| `stdout` | `tracing.exporter: stdout` | Local dev, debugging |
| `otlp` | `tracing.exporter: otlp`, `tracing.endpoint: localhost:4318` | Jaeger, Tempo, Grafana, Phoenix |

### GenAI Prometheus Metrics

- `aware_gateway_gen_ai_request_total{gen_ai_system, gen_ai_operation, gen_ai_model, status}`
- `aware_gateway_gen_ai_tokens_total{gen_ai_system, gen_ai_model, token_type}`
- `aware_gateway_gen_ai_ttft_seconds{gen_ai_system, gen_ai_model}` — time-to-first-token
- `aware_gateway_gen_ai_request_duration_seconds{gen_ai_system, gen_ai_operation, gen_ai_model}`
- `aware_gateway_gen_ai_cost_total{gen_ai_system, gen_ai_model}`

## Multi-Model Endpoints & Auto-Discovery

### The Problem

Real-world vendors serve multiple models from a single endpoint:
- OpenAI: `gpt-4o`, `gpt-4o-mini`, `gpt-4.1`, `o3-mini` all on `api.openai.com`
- Fireworks: dozens of models on `api.fireworks.ai/inference`
- vLLM: any models loaded on the same server

The gateway handles this at three levels:

### 1. Static Config (YAML)

Declare models per endpoint in config:

```yaml
pools:
  openai:
    endpoints:
      - name: openai-primary
        url: https://api.openai.com
        models:              # ← list models this endpoint serves
          - gpt-4o
          - gpt-4o-mini
          - gpt-4.1
```

### 2. API Auto-Discovery (Dynamic)

On startup and periodically (every ~2.5 min), each endpoint's `/v1/models`
API is queried to dynamically discover what models it serves. Results are
stored atomically and merged with static config.

```
INFO model discovery updated  pool=openai  endpoint=openai-primary
     models_count=181  models="[gpt-4o gpt-4o-mini gpt-4.1 o3-mini ...]"
```

This means:
- **Zero-config model discovery**: just configure the endpoint URL + auth key, models are auto-discovered
- **Hot refresh**: new models added on the backend are picked up automatically
- **Graceful fallback**: endpoints that don't implement `/v1/models` (404/405) are silently skipped
- **Auth support**: the endpoint's `auth_token` is used for discovery requests

### 3. Model-Aware Endpoint Selection

When the task-router selects a model (e.g. `gpt-4o`), the pool's
`NextForModel(model)` method filters endpoints to only those that serve
that model:

```
Pool "multi-vendor":
  ep-openai    models=[gpt-4o, gpt-4o-mini]     ✓ serves gpt-4o
  ep-fireworks models=[llama-3.1-70b]           ✗ doesn't serve gpt-4o
  ep-vllm      models=[qwen2.5-72b]             ✗ doesn't serve gpt-4o

→ NextForModel("gpt-4o") picks ep-openai
→ NextForModel("llama-3.1-70b") picks ep-fireworks
```

The selection respects the pool's load-balancing strategy (round-robin,
least-conn, weighted) among the model-serving candidates. If no endpoint
explicitly serves the model, falls back to endpoints with no model info
(assume they serve everything), then to regular `Next()` as last resort.

### How They Work Together

```
Startup:
  1. Pool reads static models from config
  2. Discovery goroutine queries /v1/models → stores discovered models
  3. task-router autoDiscovers: reads AllModels() (static ∪ discovered)
  4. Registry has complete model catalog

Request arrives:
  5. task-router classifies task → selects best model
  6. Handler calls pool.NextForModel(selected_model)
  7. Only endpoints serving that model are candidates
  8. Load balancer picks among candidates (RR / least-conn / weighted)
  9. Request proxied to the right endpoint

Periodic refresh:
  10. Discovery re-queries /v1/models every ~2.5 min
  11. New models appear in AllModels() automatically
```

## Task-Aware Routing

The `task-router` plugin:

1. **Parses** the OpenAI-compatible request body (model, messages, max_tokens, temperature)
2. **Classifies** the task: chat, code, reasoning, vision, embedding, ASR
3. **Filters** candidate models by capability + context window + pool health
4. **Scores** candidates by strategy:
   - `best_quality` — largest context window + most capabilities
   - `cheapest` — lowest $/M tokens
   - `lowest_latency` — lowest rolling average latency
   - `balanced` — normalized cost × latency × load composite
5. **Returns** a routing decision (pool + model + reason)

If no router plugin decides (or none registered), falls back to static route→pool mapping.

## Smart Routing

The `smart-router` plugin asks a small decision model to choose between a configured model menu for each request. It records structured decision metadata for audit and replay, can prefer Opus for the first warm-start turns, and can include recent per-session routing history so later choices are not made in isolation.

Typical research configuration:

```yaml
plugins:
  smart-router:
    enabled: true
    endpoint: https://openrouter.ai/api/v1
    model: openai/gpt-5.6-sol
    api_key_env: GW_OPENROUTER_KEY
    fallback_model: anthropic/claude-opus-5
    fallback_pool: openrouter
    decision_history_turns: 5
    decision_history_context_chars: 220
    safe_control:
      enabled: true
      repeated_error_threshold: 2
      premium_cooldown_after: 2
      premium_cooldown_turns: 1
      cheap_probe_burst_limit: 3
      stop_cost_usd: 3.0
      stop_agent_call_threshold: 25
      stop_length_pressure_threshold: 3
      long_exploration_threshold: 12
      long_exploration_call_threshold: 12
      post_replan_no_progress_call_limit: 3
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
        freeze_or_replan:
          max_tokens: 2048
          timeout_ms: 60000
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
      - name: z-ai/glm-5.3-flash
        pool: openrouter
        capabilities: [chat, code, reasoning]
        context_window: 1310720
        input_price: 0.07
        output_price: 0.25
      - name: anthropic/claude-opus-5
        pool: openrouter
        capabilities: [chat, code, reasoning, vision]
        context_window: 200000
        input_price: 5.00
        output_price: 25.00
```

With `safe_control.enabled`, a conservative local controller runs before the
decision model. It routes file reads/search, known test execution, and ordinary
fixed-format replies to the cheapest configured model; it upgrades repeated
identical failures and contradicted core hypotheses to the strongest configured
model; it inserts a short cheap cooldown after consecutive premium calls; and
it returns to the prompt router after too many consecutive cheap probes. It can
also stop locally before another upstream call when provider metadata is
incomplete, cost has crossed the trial gate before verifier proximity, length
pressure repeats without implementation/validation/delivery/strong progress,
too many agent calls happen without effective progress, a bounded replan has
not produced effective progress, or a blocked episode has already spent a
premium recovery turn without observable progress. Long exploration without
effective progress first triggers a bounded Opus `freeze_or_replan` action;
if the following calls still fail to produce implementation, validation,
delivery, or strong progress, the next call is rejected locally. These stops
are audited with `route_budget_action=stop_trial` and a specific `error_kind`.
Requests outside those rules continue through the prompt-based smart-router.
With `budgeted_route.enabled`, the router also attaches a route action profile
to each decision. The gateway rewrites `max_tokens`, shortens the upstream
timeout when configured, injects a short route instruction for bounded replan
turns, and records the budget action in audit traces.
With `episode_runtime.enabled`, the router now resolves a minimal online task
line before routing. An explicit `X-Episode-ID` wins. Otherwise the resolver
uses `X-Session-ID`/`X-Trial-Name` as the main episode and recognizes a small
set of `continue`, `interrupt`, `resume`, `global`, and `unknown` signals from
`X-Episode-Operation` or obvious latest-message language. Interrupts push a
temporary episode on a per-session stack; resumes return to the previous task
line. Finished agent calls are then projected into that episode state. Recent
outcomes, including repeated `finish_reason=length` in either a streak or the
recent event window, are fed into the next router prompt and can dynamically
increase the next route budget within configured ceilings. Audit traces carry
the resolved episode id, operation, state version before routing, compact
state-before JSON, and state-after JSON after the request is projected.
After a bounded replan, the projection also captures the next router context
summary as a `replan_hypothesis`. While that hypothesis is open, the next
minimum capability becomes `cheap_execute`, so the next useful turn must
validate, implement, or explicitly abandon the hypothesis instead of drifting
back into broad exploration. If the stop threshold is reached exactly when a
fresh open hypothesis appears, the gateway allows one validation turn before
closing the run on continued no-progress.
When the episode has accumulated implementation progress but still has no
delivery-target write, validation, or test evidence, the controller derives
`delivery_candidate_needs_delivery`. That state is handled locally with
`cheap_execute` and a short injected instruction telling the agent to convert
the current candidate into the required deliverable, or run exactly one
bounded check that closes the delivery/validation gap.

External runners can also post explicit progress events:

```bash
curl -s http://localhost:12026/v1/episode-events \
  -H 'Content-Type: application/json' \
  -H 'X-Episode-ID: trial-abc__agent' \
  -d '{
    "kind": "test_run",
    "source": "local-runner",
    "observation": {"outcome": "passed", "command": "go test ./...", "validation_target": true},
    "evidence_refs": ["stdout"]
  }'
```

The accepted event kinds mirror the RSI event schema:
`llm_call`, `tool_call`, `file_written`, `file_modified`, `test_run`,
`test_failed`, `test_passed`, `verifier_result`, `no_progress`, and
`run_exception`. The audit SQLite plugin stores these events in
`episode_events`, and `GET /v1/episode-events?episode_id=...` returns them for
replay or visualization. Posted events update the online episode state
immediately. The router separates exploration from implementation, delivery,
validation, and strong verifier progress, so a bare baseline `test_run passed`
is recorded but does not by itself clear no-progress pressure or prove
completion readiness.

For a first live integration, wrap local validation commands:

```bash
python3 scripts/run_episode_command.py \
  --gateway http://localhost:12026 \
  --episode-id trial-abc__agent \
  --session-id trial-abc__agent \
  --step-name validation \
  -- go test ./...
```

The wrapper exits with the wrapped command status, preserves stdout/stderr, and
posts `tool_call`, `file_written`, `test_run`, and `run_exception` events when
they apply.

For Harbor jobs, a sidecar watcher can stream already-written artifacts:

```bash
python3 scripts/watch_harbor_episode_events.py \
  --job-dir /path/to/harbor/jobs/job-name \
  --gateway http://localhost:12026
```

It watches trajectory files plus final patch, CTRF, reward, and result artifacts,
posting each deterministic event id once. V4 experiments can enable it with:

```bash
AWARE_V4_EPISODE_WATCHER=1 scripts/run_v4_matrix.sh pilot
```

The V4 runner also watches gateway traces while Harbor is running. If an agent
trace records `route_budget_action=stop_trial` or a `gateway_*stop_gate`
`error_kind`, the runner writes `gateway-stop-gate.json`, interrupts the Harbor
job, and the analyzer reports that stop type as `failure_kind`.

Inspect the router's current online projection for one task line:

```bash
curl -s 'http://localhost:12026/v1/episode-state?episode_id=trial-abc__agent'
```

When the in-memory projection is missing, smart-router can backfill it from
persisted audit traces and explicit episode events if query plugins are
available.

## Project Structure

```
aware-gateway/
├── cmd/gateway/main.go              # Entry: config → pools → plugins → server
├── internal/
│   ├── config/config.go             # YAML config + validation
│   ├── pool/pool.go                 # Endpoint pool: RR, least-conn, weighted SWRR
│   ├── proxy/reverse.go             # httputil.ReverseProxy + token parsing
│   ├── routing/                     # Context metadata across proxy boundary
│   ├── metrics/metrics.go           # Prometheus metrics (generic + GenAI)
│   ├── trace/trace.go               # OTel tracer setup (stdout + OTLP)
│   ├── plugin/                      # Plugin interfaces + registry
│   │   ├── plugin.go                # Hook interface definitions
│   │   ├── context.go               # Plugin context + audit record types
│   │   └── registry.go              # Lifecycle management
│   ├── otel/genai/                  # GenAI semantic conventions
│   │   ├── conventions.go           # Attribute key constants (gen_ai.* / llm.*)
│   │   └── attributes.go            # Request/response attribute extraction
│   └── core/                        # Core engine
│       ├── handler.go               # Plugin chain + proxy + retry + audit
│       ├── writer.go                # decisionWriter (stream/buffer) + statusWriter
│       └── server.go                # Router build, pool manager, health, metrics
├── plugins/
│   ├── smartrouter/plugin.go        # LLM decision router with cost/history-aware prompts
│   ├── taskrouter/router.go         # Task-aware model routing
│   ├── otelgenai/plugin.go          # GenAI OTel observability
│   ├── ratelimit/plugin.go          # Rate limiting
│   └── audit/plugin.go              # Audit sink (log + SQLite)
├── scripts/                         # Terminal-Bench experiment runners and report tooling
├── docs/                            # Design docs, experiment plans, and GitHub Pages reports
├── configs/gateway.yaml             # Default config
├── go.mod
└── Makefile
```

## Build & Run

```bash
make build          # Build binary
make run            # Build + run with default config
make test           # Run all tests
make vet            # Run go vet
make static         # Static binary (CGO_ENABLED=0, for scratch/airgap)
make bundle         # Tar binary + config for airgap deploy
```

## Dependencies

| Package | Purpose |
|---------|---------|
| go-chi/chi/v5 | HTTP router + middleware |
| golang.org/x/time/rate | Token bucket rate limiter |
| gopkg.in/yaml.v3 | Config parsing |
| sony/gobreaker/v2 | Circuit breaker |
| modernc.org/sqlite | Pure-Go SQLite (audit store) |
| prometheus/client_golang | Prometheus metrics |
| go.opentelemetry.io/otel | Distributed tracing |
| go.opentelemetry.io/contrib/.../otelhttp | HTTP span instrumentation |
