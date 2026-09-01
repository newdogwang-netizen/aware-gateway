# aware-gateway Benchmark Experiment Plan v2

## Date: August 31, 2026

## Objective

Prove that smart-router can solve "Opus-only" tasks at lower cost by using
GLM-5.3-flash for simple turns and upgrading to premium only when needed.

## Key Discovery: Opus 5 vs GLM-5.3 Cross-Comparison

From 47 common tasks in Terminal-Bench 4.0 leaderboard data:

| Category | Count | Meaning |
|----------|-------|---------|
| Both Pass | 17 | Both models solved — flash likely also works |
| Opus Only | 12 | **Opus passed, GLM-5.3 failed — our target** |
| GLM Only | 3 | GLM passed where Opus failed (different model strengths) |
| Both Fail | 15 | Neither solved — waste regardless of model |

### Critical Cost Insight

GLM-5.3 ($1.40/$4.40) is NOT cheaper than Opus 5 ($5.00/$25.00) in practice:

| Metric | Opus 5 | GLM-5.3 |
|--------|--------|---------|
| Unit price ($/M in/out) | $5.00 / $25.00 | $1.40 / $4.40 |
| Total tokens (330 trials) | 6.5B | 8.7B |
| Total cost (330 trials) | $6,000 | $2,700 |
| Avg cost per Both-Pass task | $21 | $262 |

GLM-5.3 burns 10x more tokens per task than Opus 5. Lower unit price,
higher total cost. **GLM-5.3-flash ($0.07/$0.25) is the real cheap option** —
even at 10x token volume, total cost stays negligible.

### GLM-5.3-flash vs GLM-5.3

GLM-5.3-flash is the same model family, distilled/quantized for speed.
Quality should be close to GLM-5.3 for most tasks. We use flash as the
ultra-cheap tier throughout.

## Task Selection: Opus-Only Tasks

These 12 tasks Opus 5 solved but GLM-5.3 failed. This is the zone where
smart-router proves value: if some turns only need flash (understanding,
planning) while others need premium (coding, debugging), smart-router
can reduce cost while maintaining resolution.

| Task | Expert | Opus5 Cost | GLM53 Cost | Opus5 Runs | GLM53 Runs | Why Selected for Experiment |
|------|--------|-----------|-----------|-----------|-----------|---------------------------|
| html-js-filter | 0.75h | $23.46 | $152.27 | 2/2 pass | 0/1 fail | Easy task, Opus solves cheaply, GLM burns $152 and fails |
| gsea-proteomics | 4h | $2.47 | $11.97 | 2/2 pass | 0/2 fail | Low cost, biology data processing |
| cad-model | 2h | $5.20 | $213.14 | 4/4 pass | 0/1 fail | Hardware/CAD, Opus efficient |
| freecad-platform-drawing | 1.5h | $4.74 | $247.69 | 2/2 pass | 0/1 fail | CAD drawing, Opus efficient |
| vpp-loss-divergence | 2h | $7.80 | $166.25 | 1/1 pass | 0/2 fail | ML training debug, quick solve |
| kv-live-surgery | 4h | $15.20 | $629.08 | 3/3 pass | 0/1 fail | Systems, Opus consistent |
| ks-solver-cpp | 10h | $20.25 | $301.07 | 1/1 pass | 0/1 fail | Physics solver, hard |
| roy-polymorph-cn | 3h | $0.82 | $9.30 | 1/2 pass | 0/4 fail | Chemistry, both inconsistent |
| jax-speedrun-gpu | 10h | $51.13 | $257.27 | 1/2 pass | 0/1 fail | JAX training, most expensive |
| vf2-speedup-networkx | 4h | $37.37 | $617.27 | 1/2 pass | 0/1 fail | Algorithm, Opus barely passes |
| rs-archive-clone | 16h | $71.18 | $680.37 | 2/3 pass | 0/1 fail | Very hard, Opus inconsistent |
| live-database-cutover | 8h | $35.61 | $543.92 | 1/1 pass | 0/5 fail | DB migration, GLM catastrophic |

### Selected 5 Tasks for Experiment

Choose 5 that span difficulty range and are generic (no exotic domains):

| # | Task | Expert | Opus Cost | Pattern | Why |
|---|------|--------|-----------|---------|-----|
| 1 | html-js-filter | 0.75h | $23.46 | Security/AppSec | Easiest Opus-only — if flash can't solve this alone, can smart-router mix solve it? |
| 2 | vpp-loss-divergence | 2h | $7.80 | ML/Debug | Quick for Opus, GLM burned $166 and failed — good cost contrast |
| 3 | kv-live-surgery | 4h | $15.20 | Systems/Debug | Opus 3/3 consistent, GLM failed — is flash + premium mix enough? |
| 4 | jax-speedrun-gpu | 10h | $51.13 | ML/Training | Most expensive Opus-only, hard — tests if smart-router knows when to upgrade |
| 5 | live-database-cutover | 8h | $35.61 | DB/Migration | GLM failed 0/5 (catastrophic), Opus 1/1 — extreme contrast |

Excluded: cad-model, freecad-platform-drawing (CAD/Hardware, needs vision),
gsea-proteomics (biology), ks-solver-cpp (physics), roy-polymorph-cn (chemistry,
both inconsistent), rs-archive-clone (too expensive at $71),
vf2-speedup-networkx (Opus only 50% pass — too random).

## Strategies

| Strategy | Model | Price ($/M) | Purpose |
|----------|-------|-------------|---------|
| all-premium | openai/gpt-5.6-sol | $2.00 / $10.00 | Baseline (TB #4, 37.3%) |
| all-flash | z-ai/glm-5.3-flash | $0.07 / $0.25 | Ultra-cheap, expected to fail on some |
| smart-router | model="auto" | varies | LLM decides per turn |

### Why GPT-5.6 Sol as premium (not Opus 5)

- Opus 5 costs $5/$25 per M — would make experiment ~$300 for 5 tasks
- GPT-5.6 Sol is TB #4 at 37.3% resolution, $2/$10 — strong but affordable
- If smart-router matches Sol's resolution at lower cost, that's the win

### Smart Router Setup

- Decision model: Qwen3.8-27B (vLLM on L40S, localhost:18000)
- Thinking disabled (chat_template_kwargs.enable_thinking=false)
- Model menu: flash ($0.07/$0.25) → Luna ($0.20/$1.20) → GLM-5.3 ($1.40/$4.40) → Sol ($2/$10)
- Prompt includes: turn phase, conversation depth, full request preview, tier labels
- Fallback: task-router (rule-based) if decision model fails

## Hypothesis

On Opus-only tasks, GLM-5.3-flash alone fails because some turns need
stronger reasoning. But not ALL turns need premium — the agent's
understanding and planning turns can use flash, while only code-writing
and complex debugging turns need premium.

**If smart-router correctly identifies which turns need premium and
which can use flash, it will:**
1. Pass tasks that all-flash fails on (premium on critical turns)
2. Cost less than all-premium (flash on non-critical turns)
3. Approach all-premium resolution rate at a fraction of cost

## Expected Results

| Task | all-premium | all-flash | smart-router | What it proves |
|------|------------|-----------|-------------|----------------|
| html-js-filter | pass | ? | pass (flash mostly) | Even "Opus-only" easy tasks may pass with flash |
| vpp-loss-divergence | pass | fail | pass (upgrade on debug turn) | Smart-router upgrades when needed |
| kv-live-surgery | pass | fail | pass (upgrade on code turn) | Mixed model solves what flash alone can't |
| jax-speedrun-gpu | may pass | fail | may pass (upgrade on complex turns) | Hard task — does upgrade help enough? |
| live-database-cutover | may pass | fail | may pass (upgrade on DB turns) | Extreme case — GLM 0/5, can mix do better? |

### Success Criteria

1. smart-router resolution rate > all-flash resolution rate (upgrade helps)
2. smart-router total cost < all-premium total cost (savings achieved)
3. At least 1 task where smart-router passes but all-flash fails (proof of value)
4. Per-turn logs show differentiated model selection (not all-flash or all-premium)

## Execution

### Infrastructure

- aware-gateway on localhost:12026 (OpenRouter pool)
- Qwen3.8-27B on localhost:18000 (vLLM, L40S) — decision model
- OpenRouter API key in gateway config
- Docker available for harbor trial environments

### Harbor Commands (3 runs)

```bash
# Run 1: all-premium (baseline)
harbor run \
  --agent terminus-2 \
  --model "openai/gpt-5.6-sol" \
  --dataset "terminal-bench/terminal-bench@4.0" \
  --include-task-name "html-js-filter" \
  --include-task-name "vpp-loss-divergence" \
  --include-task-name "kv-live-surgery" \
  --include-task-name "jax-speedrun-gpu" \
  --include-task-name "live-database-cutover" \
  --n-attempts 1 \
  --n-concurrent 1 \
  --agent-kwarg "api_base=http://localhost:12026/v1" \
  --agent-kwarg "session_id_headers=[\"X-Session-ID\"]" \
  --agent-env "OPENAI_API_KEY=*** \
  --yes

# Run 2: all-flash
harbor run \
  --agent terminus-2 \
  --model "z-ai/glm-5.3-flash" \
  --dataset "terminal-bench/terminal-bench@4.0" \
  --include-task-name "html-js-filter" \
  --include-task-name "vpp-loss-divergence" \
  --include-task-name "kv-live-surgery" \
  --include-task-name "jax-speedrun-gpu" \
  --include-task-name "live-database-cutover" \
  --n-attempts 1 \
  --n-concurrent 1 \
  --agent-kwarg "api_base=http://localhost:12026/v1" \
  --agent-kwarg "session_id_headers=[\"X-Session-ID\"]" \
  --agent-env "OPENAI_API_KEY=*** \
  --yes

# Run 3: smart-router (model=auto)
harbor run \
  --agent terminus-2 \
  --model "auto" \
  --dataset "terminal-bench/terminal-bench@4.0" \
  --include-task-name "html-js-filter" \
  --include-task-name "vpp-loss-divergence" \
  --include-task-name "kv-live-surgery" \
  --include-task-name "jax-speedrun-gpu" \
  --include-task-name "live-database-cutover" \
  --n-attempts 1 \
  --n-concurrent 1 \
  --agent-kwarg "api_base=http://localhost:12026/v1" \
  --agent-kwarg "session_id_headers=[\"X-Session-ID\"]" \
  --agent-env "OPENAI_API_KEY=*** \
  --yes
```

### Cost Estimate

| Strategy | Per task (est) | 5 tasks | Notes |
|----------|---------------|---------|-------|
| all-premium | $20-50 | $100-250 | GPT-5.6 Sol, avg 50-100 turns/task |
| all-flash | $0.5-2 | $2.5-10 | GLM-5.3-flash, even at 10x tokens |
| smart-router | $2-10 | $10-50 | Mostly flash, upgrade on ~20% of turns |
| **Total** | | **$112-310** | |

### Metrics

Per trial:
- reward (0 or 1) — from verifier pytest
- total cost — from gateway /v1/traces/{trial}/summary
- per-turn model — from gateway traces
- agent duration — from harbor
- decision model cost — step=router-decision in traces

Per strategy (5 tasks):
- resolution rate (passes / 5)
- total cost
- avg cost per task
- cost per successful pass

## Risks

| Risk | Mitigation |
|------|-----------|
| all-flash passes everything | Valid finding — flash is sufficient, smart-router not needed for these tasks |
| smart-router fails everything | Check if decision model is upgrading too aggressively or not enough |
| Harbor Docker build fails | Pre-build with harbor run --install-only |
| Task timeout | Use --timeout-multiplier 0.5 for cost control |
| GPT-5.6 Sol rate limited | Verified working; fallback to gpt-5.6-sol-pro |
| GLM-5.3-flash token explosion | Monitor per-turn tokens in traces; flash may generate very long outputs |

## Blog Update Plan

After experiment completes:
1. Add "Run 3: Real Terminal-Bench Trial" section to blog
2. Include real resolution rate table (pass/fail per task per strategy)
3. Include per-turn model selection heatmap (which turns upgraded)
4. Cost comparison: smart-router vs all-premium vs all-flash
5. Key finding: "smart-router achieved X% resolution at Y% of premium cost"
