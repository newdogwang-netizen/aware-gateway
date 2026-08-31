# aware-gateway

A smart, plugin-based AI model gateway. Single Go binary, zero runtime dependencies.

## What It Is

aware-gateway is a reverse proxy for LLM and ASR backends with a **plugin architecture** that adds:

- **Task-aware routing**: analyzes incoming requests and selects the best model based on task type, cost, latency, and current load
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
                         │     │   └─ TaskRouter: classify →    │
                         │     │      score → select model      │
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
| `task-router` | RequestRouter | Classifies LLM requests (chat/code/reasoning/vision) and selects best model by cost/quality/latency/load |
| `otel-genai` | Middleware, Health | Enriches OTel spans with gen_ai.* / llm.* attributes; records GenAI Prometheus metrics |
| `ratelimit` | Middleware | Global + per-key rate limiting (token bucket) |
| `audit` | AuditSink | Structured log + SQLite audit store |

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
│   ├── taskrouter/router.go         # Task-aware model routing
│   ├── otelgenai/plugin.go          # GenAI OTel observability
│   ├── ratelimit/plugin.go          # Rate limiting
│   └── audit/plugin.go              # Audit sink (log + SQLite)
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
