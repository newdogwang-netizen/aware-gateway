# aware-gateway Benchmark Experiment Plan v1

## Date: August 31, 2026

## Objective

Prove that aware-gateway's smart-router can achieve the same resolution rate as a
premium model while significantly reducing total cost, by choosing cheaper models
when they are sufficient and upgrading only when necessary.

## Background: Opus 5 Per-Task Data

From Terminal-Bench 4.0 leaderboard #1 (Opus 5 / Claude Code), 330 trials (66 tasks x 5 runs).
We extracted 100 trials covering 54 tasks. Three categories emerge:

| Category | Count | Description |
|----------|-------|-------------|
| A — Always Pass (100%) | 27 | Opus 5 solved every run |
| B — Sometimes Pass (50-75%) | 8 | Opus 5 inconsistent across runs |
| C — Always Fail (0%) | 19 | Opus 5 never solved |

### Cost Breakdown (100 trials, ~$1,880 subtotal)

| Category | Total Cost | Share | Insight |
|----------|-----------|-------|---------|
| A (always pass) | $960 | 51% | If cheap models also pass, this is pure savings |
| B (sometimes) | $449 | 24% | The real uncertainty zone — cross-model differences matter |
| C (always fail) | $470 | 25% | Wasted regardless of model — cheap model fails equally |

## Selected Tasks (6 tasks, 3 groups)

### Group A — Always Pass (cheap model should also pass)

| Task | Expert | Tests | Sol Lines | Opus 5 Avg Cost | Why Selected |
|------|--------|-------|-----------|-----------------|--------------|
| embedding-drift-monitor | 5h | 11 | 201L | $5.98 | Medium cost, 11 tests, debugging pattern |
| distributed-dedup | 10h | 0 | 281L | $35.02 | High cost, 4h runtime, from-scratch implementation |

### Group B — Sometimes Pass (the interesting zone)

| Task | Expert | Tests | Sol Lines | Opus 5 Rate | Opus 5 Avg Cost | Why Selected |
|------|--------|-------|-----------|-------------|-----------------|--------------|
| vf2-speedup-networkx | 4h | 60 | 29L | 50% (1/2) | $37.37 | 60 tests, algorithm optimization, Opus 5 unstable |
| jax-speedrun-gpu | 10h | 8 | 945L | 50% (1/2) | $51.13 | JAX training, most expensive sometimes-pass |

### Group C — Always Fail (expected to fail regardless)

| Task | Expert | Tests | Sol Lines | Opus 5 Rate | Opus 5 Avg Cost | Why Selected |
|------|--------|-------|-----------|-------------|-----------------|--------------|
| session-window-debug | 8h | 7 | 8L | 0% (0/2) | $2.22 | Low cost failure, subtle bug |
| bun-sourcemap-leak | 1.5h | 0 | 8L | 0% (0/3) | $9.20 | Failed 3 times, medium cost |

## Strategies (3 per task, 1 run each)

| Strategy | Model Sent | Price ($/M in/out) | Purpose |
|----------|-----------|-------------------|---------|
| all-premium | openai/gpt-5.6-sol | $2.00 / $10.00 | Baseline — TB #4, 37.3% resolution |
| all-cheap | z-ai/glm-5.3-flash | $0.07 / $0.25 | Ultra-cheap, not on leaderboard |
| smart-router | model="auto" | varies | Qwen3.8-27B decision model picks per turn |

### Smart Router Configuration

- Decision model: Qwen3.8-27B (vLLM on L40S, localhost:18000)
- Thinking mode disabled (chat_template_kwargs.enable_thinking=false)
- Timeout: 10s, max_tokens: 100, temperature: 0
- Cache TTL: 300s (same prompt prefix reuses decision)
- Fallback: task-router (rule-based) if decision model fails

### Model Menu (sent to decision model)

| # | Model | Tier | $/M total | TB Rank | Context |
|---|-------|------|-----------|---------|---------|
| 1 | z-ai/glm-5.3-flash | ultra-cheap | $0.32 | — | 1.3M |
| 2 | openai/gpt-5.6-luna | budget | $1.40 | #8 (17.3%) | 1M |
| 3 | z-ai/glm-5.3 | mid | $5.80 | #3 (41.8%) | 1.3M |
| 4 | openai/gpt-5.6-sol | premium | $12.00 | #4 (37.3%) | 1M |

## Execution

### Infrastructure

- aware-gateway running on localhost:12026 (OpenRouter pool)
- Qwen3.8-27B on localhost:18000 (vLLM, L40S) — decision model
- DiffusionGemma-26B on localhost:18001 (available but not used for routing)
- OpenRouter API key configured in gateway config

### Harbor Command

```bash
harbor run \
  --agent terminus-2 \
  --model "openrouter/z-ai/glm-5.3-flash" \
  --dataset "terminal-bench/terminal-bench@4.0" \
  --include-task-name "embedding-drift-monitor" \
  --include-task-name "distributed-dedup" \
  --include-task-name "vf2-speedup-networkx" \
  --include-task-name "jax-speedrun-gpu" \
  --include-task-name "session-window-debug" \
  --include-task-name "bun-sourcemap-leak" \
  --n-attempts 1 \
  --n-concurrent 1 \
  --agent-kwarg "api_base=http://localhost:12026/v1" \
  --agent-kwarg "session_id_headers=[\"X-Session-ID\"]" \
  --agent-env "OPENAI_API_KEY=*** \
  --yes
```

Note: Run 3 times with different `--model` and `--agent-env`:
1. `--model "openai/gpt-5.6-sol"` (all-premium)
2. `--model "z-ai/glm-5.3-flash"` (all-cheap)
3. `--model "auto"` (smart-router — gateway decides)

### Cost Control

- 1 run per task (not 5 like leaderboard) — saves 5x cost
- Total estimated cost:
  - all-premium: ~$150 (6 tasks x ~$25/task avg)
  - all-cheap: ~$3 (6 tasks x ~$0.5/task avg)
  - smart-router: ~$12 (mostly cheap, occasional upgrade)
  - Grand total: ~$165

### Verification

Each trial produces:
- `reward.txt` = 1 (pass) or 0 (fail) — from verifier pytest
- Trajectory JSON with per-step tokens, cost, model
- Gateway traces via `/v1/traces/{trial}/summary` with per-step + per-model breakdown

## Expected Results

| Group | all-premium | all-cheap | smart-router | What it proves |
|-------|------------|-----------|-------------|----------------|
| A (always pass) | pass | pass (hypothesis) | pass, uses cheap | Cheap model sufficient for solvable tasks |
| B (sometimes) | may pass | may fail | **key observation** | Does smart-router's upgrade help? |
| C (always fail) | fail | fail | fail, uses cheap | No waste on unsolvable tasks |

### Success Criteria

1. **Group A**: smart-router passes with cost < 20% of all-premium
2. **Group B**: smart-router resolution rate >= all-cheap (upgrade helps when needed)
3. **Group C**: smart-router cost < 10% of all-premium (cheap model on hopeless tasks)
4. **Overall**: smart-router total cost < 30% of all-premium, resolution rate >= all-premium

## Metrics Collected

Per trial:
- reward (0 or 1)
- total cost (from gateway trace summary)
- total tokens (prompt + completion)
- per-turn model selection (from gateway traces)
- agent duration (from harbor)
- decision model cost (step=router-decision in traces)

Per strategy (aggregated across 6 tasks):
- resolution rate (passes / 6)
- total cost
- avg cost per task
- cost per pass (total cost / passes)

## Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Cheap model fails Group A tasks | Accept — that's a valid finding (cheap model not sufficient) |
| Smart-router always picks cheap | Check per-turn logs — is the decision model seeing enough context? |
| OpenRouter rate limits | Run with --n-concurrent 1, 0.5s sleep between calls |
| Harbor Docker build fails | Run `harbor run --install-only` first to pre-build |
| Task timeout (8h max) | Use --timeout-multiplier 0.5 to halve max timeout for cost control |
| GPT-5.6 Sol unavailable on OpenRouter | Verified 200 OK on Aug 31 — use gpt-5.6-sol-pro as backup |

## Next Steps After Experiment

1. Update blog with real resolution rate results
2. If Group A passes with cheap: publish as "aware-gateway achieves 100% resolution at 5% cost"
3. If Group B shows smart-router > all-cheap: publish as "LLM-based routing improves resolution on hard tasks"
4. If Group C confirms fail-with-low-cost: publish as "cost-aware routing avoids waste on unsolvable tasks"
5. Compare per-turn decisions: which turns triggered upgrades, and did those upgrades help?
