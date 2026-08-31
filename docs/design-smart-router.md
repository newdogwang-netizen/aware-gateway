# Design: Smart Router — LLM-Based Model Selection

## Overview

A new `smart-router` plugin that uses a small LLM to make per-call model
selection decisions. It runs as a `RequestRouter` in the existing
chain-of-responsibility, before the rule-based `task-router`.

```
Request (model="auto" or unknown)
  → smart-router (priority 50, runs first)
      → cache hit? return cached decision
      → call decision LLM → valid model? return decision
      → failed? return Skip=true
  → task-router (priority 100, runs next)
      → keyword classify + score → return decision
  → no router decides → static config routing
```

The two routers are completely independent plugins. No shared code, no shared
state. The gateway's chain-of-responsibility handles the fallback naturally:
first non-Skip decision wins.

## Why

The rule-based task-router uses keyword matching to classify tasks
(chat/code/reasoning) then applies a fixed strategy per category. This is
crude:
- "Write a Python script" and "Explain quantum computing" both classify as "code"
- A simple 5-line function and a 500-line system design both get the same model
- No awareness of conversation history or what the agent is trying to do

A small LLM can understand nuance: "this is a simple formatting task, use
flash" vs "this requires deep reasoning about distributed systems, use
GLM-5.3".

## Architecture

### New Plugin: `smart-router`

Completely independent from `task-router`. Owns its own model registry copy
(populated from the same config block, but does not share Go objects).

```
plugins/
  taskrouter/router.go    — existing rule-based (unchanged, stays as fallback)
  smartrouter/plugin.go   — NEW: LLM-based decision router
```

### Decision Flow

```
1. smart-router.Route() is called with the request body:

   a. Parse request: extract messages, model field, estimated tokens
   b. If model is known (in registry) → return Skip (respect client pin)
   c. If model is "auto" or unknown → proceed to LLM decision

   d. Build model menu from own registry:
      - Model ID, $/M prompt, $/M completion, capabilities, context window
      - Sorted by cost (cheapest first)
      - Only include models whose context_window >= estimated_tokens

   e. Build decision prompt:
      - Model menu (numbered list)
      - Message count (conversation depth)
      - System message preview (first 200 chars, if present)
      - Latest user message preview (first 500 chars)

   f. Call decision model:
      POST {endpoint}/v1/chat/completions
      {
        "model": "{decision_model}",
        "messages": [{"role": "user", "content": "{decision_prompt}"}],
        "max_tokens": 100,
        "temperature": 0
      }

   g. Parse response:
      - Extract JSON: {"model": "model-id", "reason": "..."}
      - Validate: model exists in menu?
        YES → return RoutingDecision{Model, Pool, Reason}
        NO  → return Skip (let task-router handle)

   h. On any failure (timeout, 5xx, invalid JSON, parse error):
      → return Skip (let task-router handle)
```

### Router Prompt

```
You are a model router for an AI gateway. Select the most cost-effective
model for this request. Consider: task complexity, required capabilities,
and cost. Prefer cheaper models unless the task genuinely requires a
stronger one.

Available models (sorted by cost):
1. z-ai/glm-5.3-flash    $0.07/$0.25 per M  chat,code,reasoning  1.3M ctx
2. google/gemma-4-31b    $0.09/$0.34 per M  chat,code,reasoning  262K ctx
3. openai/gpt-5.6-luna   $0.20/$1.20 per M  chat,code,reasoning  1M ctx
4. z-ai/glm-5.3          $1.40/$4.40 per M  chat,code,reasoning  1.3M ctx
5. openai/gpt-5.6-sol    $2.00/$10.00 per M chat,code,reasoning  1M ctx

Request context:
- Conversation depth: 5 messages
- Estimated input tokens: 450
- System: "You are a software engineer working on a task."
- User message: "Write the first function. Keep it under 30 lines."

Respond with ONLY a JSON object:
{"model": "model-id-from-list", "reason": "one sentence"}
```

### Decision Model Requirements

- **Small parameters** (≤8B) — fast inference, low cost per decision
- **Structured output** — must return valid JSON reliably
- **Low latency** — decision adds to total request latency; target < 500ms
- **Cost** — negligible vs the request it routes (target < $0.0001/decision)

### Configuration

```yaml
plugins:
  smart-router:
    enabled: true

    # Decision model endpoint (OpenAI-compatible /v1/chat/completions)
    endpoint: "http://localhost:8080/v1"    # user-provided
    model: "decision-model"                  # user-provided
    api_key: ""                              # optional

    # Decision call settings
    max_tokens: 100
    temperature: 0
    timeout_ms: 2000         # fail fast → fallback to task-router

    # Prompt construction
    prompt_preview_chars: 500    # truncate user message
    include_system_prompt: true   # include system message preview
    include_message_count: true   # include conversation depth

    # Model menu (same format as task-router, or leave empty to
    # auto-populate from pool discovery)
    models: []

    # Cache
    cache_ttl_seconds: 300    # reuse decisions for similar prompts
    cache_max_entries: 10000
```

### Caching

Identical requests within `cache_ttl_seconds` reuse the decision:

```
cache_key = hash(sorted_model_ids + message_count + first_500_chars_of_latest_user_msg)
cache_value = { model, reason, expires_at }
```

- Same prompt prefix + same model menu = same decision
- Cache hit → skip decision model call → zero overhead
- LRU eviction at `cache_max_entries`

### Cost Tracking

The decision model call is a real LLM call with real cost. It is recorded as
a separate trace entry via the audit plugin, using step="router-decision":

```
Trace entries for one user request:

  step=router-decision  model=decision-model   tokens=120+30   cost=$0.00002
  step=turn3            model=glm-5.3-flash     tokens=450+500  cost=$0.00015

Trial summary per_model:
  decision-model:  { calls: 1, cost: $0.00002 }
  glm-5.3-flash:   { calls: 1, cost: $0.00015 }
```

The smart-router sends the decision call through the gateway itself (loopback
to localhost:12026) with X-Step-Name=router-decision, so the audit plugin
records it transparently. Alternatively, it can call the endpoint directly and
emit an AuditRecord itself.

### Fallback Chain (chain-of-responsibility)

```
Priority 50: smart-router
  → LLM decision success → return RoutingDecision
  → cache hit → return RoutingDecision
  → any failure → return Skip=true

Priority 100: task-router (existing, unchanged)
  → keyword classify + score → return RoutingDecision
  → no candidates → return Skip=true

(no router decides) → static config routing
```

The gateway's `RequestRouter` chain calls routers in priority order. First
non-Skip decision wins. Smart-router failing silently falls through to
task-router with zero coupling.

### Latency Budget

```
Decision model call:   100-500ms (small model, ≤100 tokens output)
Cache hit:             <1ms
Fallback to task-router: <1ms (in-process, no network)

Typical upstream LLM:  3-15s
Acceptable overhead:   <5% of upstream latency
```

### Benchmark Integration

The benchmark's `gateway-router` strategy sends `model="auto"`:

```python
body = {"model": "auto", "messages": [...], ...}
```

Smart-router sees "auto" (not in registry) → calls decision model → selects
best model for that turn. The benchmark queries `/v1/traces/{trial}/summary`
to see per-turn model choices, total cost (including decision overhead), and
quality scores.

### Model Pinning

Clients can pin specific models by sending a known model ID:

```json
{"model": "z-ai/glm-5.3-flash", "messages": [...]}
```

When `model` is in the registry → smart-router returns `Skip: true` (respect
client choice). Only `model="auto"`, empty, or unknown triggers LLM decision.

## Implementation Plan

1. `plugins/smartrouter/plugin.go` — SmartRouter struct, Init, Close, Route
2. `plugins/smartrouter/registry.go` — own ModelRegistry (copy from config + discovery)
3. `plugins/smartrouter/prompt.go` — decision prompt builder
4. `plugins/smartrouter/cache.go` — LRU decision cache with TTL
5. `plugins/smartrouter/client.go` — HTTP client for decision model endpoint
6. `cmd/gateway/main.go` — register smart-router before task-router
7. `configs/gateway-openrouter.yaml` — add smart-router config block (disabled until endpoint provided)
8. `scripts/benchmark.py` — gateway-router strategy sends model="auto"

## Open Questions

1. **Decision model endpoint** — user will provide later
2. **Should the decision model see full conversation history or just the latest
   message?** — current design: latest user message + system message + depth count
3. **Budget cap per trial?** — should the router stop upgrading when cumulative
   cost exceeds a threshold?

