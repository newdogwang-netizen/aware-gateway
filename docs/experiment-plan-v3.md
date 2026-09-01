# aware-gateway Benchmark Experiment Plan v3

## Date: August 31, 2026

## Objective

**Test whether** smart-router can achieve comparable resolution rate to a
premium model at significantly lower cost, by using GLM-5.3-flash for
non-critical turns and upgrading to premium only when the decision model
judges it necessary.

This is an exploratory experiment — not a proof. Results may show that
flash alone is sufficient, that smart-router helps, or that neither helps.

## Background: Opus 5 vs GLM-5.3 Cross-Comparison

From 47 common tasks in Terminal-Bench 4.0 leaderboard data:

| Category | Count | Meaning |
|----------|-------|---------|
| Both Pass | 17 | Both models solved |
| Opus Only | 12 | Opus passed, GLM-5.3 failed — our target zone |
| GLM Only | 3 | GLM passed where Opus failed (different model strengths) |
| Both Fail | 15 | Neither solved — waste regardless |

### Critical Cost Insight

| Metric | Opus 5 | GLM-5.3 | GLM-5.3-flash |
|--------|--------|---------|---------------|
| Unit price ($/M in/out) | $5.00 / $25.00 | $1.40 / $4.40 | $0.07 / $0.25 |
| Total tokens (330 trials) | 6.5B | 8.7B | unknown |
| Total cost (330 trials) | $6,000 | $2,700 | est. $200-400 |
| Avg cost per Both-Pass task | $21 | $262 | est. $5-15 |

GLM-5.3's aggregate token count (8.7B) is ~1.34x Opus 5's (6.5B). However,
on individual tasks GLM-5.3's cost is often 5-15x higher than Opus 5's
(e.g. kv-live-surgery: $629 vs $15). This per-task cost explosion — not
aggregate token ratio — is why GLM-5.3 is not a drop-in cost saver.
GLM-5.3-flash at $0.07/$0.25 keeps total cost negligible even at high
token volumes.

**Note:** GLM-5.3-flash quality vs GLM-5.3 is an assumption, not verified.
This experiment includes `all-glm-5.3` as a control strategy to test it.
If flash and GLM-5.3 produce same pass/fail on these tasks, the
assumption holds.

## Task Selection: Opus-Only, Stable Pass

To reduce noise, we prioritize tasks where Opus 5 passed consistently
(multiple runs, not 1/1) and GLM-5.3 failed consistently.

| Task | Expert | Opus5 (pass/run) | GLM53 (pass/run) | Opus Cost | GLM53 Cost | Stability |
|------|--------|-----------------|------------------|-----------|-----------|-----------|
| html-js-filter | 0.75h | 2/2 | 0/1 | $23.46 | $152.27 | Opus stable, GLM 1 run |
| kv-live-surgery | 4h | 3/3 | 0/1 | $15.20 | $629.08 | Opus very stable, GLM 1 run |
| cad-model | 2h | 4/4 | 0/1 | $5.20 | $213.14 | Opus very stable, GLM 1 run |
| gsea-proteomics | 4h | 2/2 | 0/2 | $2.47 | $11.97 | Opus stable, GLM 2 runs both fail |
| vpp-loss-divergence | 2h | 1/1 | 0/2 | $7.80 | $166.25 | Opus 1 run, GLM 2 runs both fail |

**Selected 5 tasks** (prioritize Opus pass stability over domain generality;
some tasks are domain-specific — see limitations):

| # | Task | Expert | Opus pass/run | GLM fail/run | Domain | Why |
|---|------|--------|--------------|-------------|--------|-----|
| 1 | html-js-filter | 0.75h | 2/2 | 0/1 | Security | Easiest — if flash can't do it alone, can mix? |
| 2 | gsea-proteomics | 4h | 2/2 | 0/2 | Biology* | Opus stable 2/2, GLM stable 0/2 — clean contrast |
| 3 | vpp-loss-divergence | 2h | 1/1 | 0/2 | ML | Opus 1/1, GLM 0/2 — GLM definitively failed |
| 4 | kv-live-surgery | 4h | 3/3 | 0/1 | Systems | Opus very stable 3/3 — strongest signal |
| 5 | cad-model | 2h | 4/4 | 0/1 | Hardware* | Opus most stable 4/4 — if flash+premium can't solve, nothing can |

*Domain-specific: gsea-proteomics (biology) and cad-model (hardware) are
not "generic software" tasks. They were selected because they have the
cleanest Opus-stable / GLM-fail signal. This is a trade-off: stability
of the cross-model contrast vs. generality of the task. Results on these
two tasks may not generalize to pure software tasks.

Excluded: jax-speedrun-gpu (Opus only 1/2 — too random), live-database-cutover
(GLM 0/5 but 8h runtime — too expensive for 3 runs), rs-archive-clone ($71/task
× 3 strategies × 3 runs = $639 — too expensive), vf2-speedup-networkx (Opus 1/2
— too random), freecad-platform-drawing (needs CAD vision), ks-solver-cpp
(physics), roy-polymorph-cn (both inconsistent).

## Strategies (4 per task, 3 runs each)

| Strategy | Model Sent | Price ($/M in/out) | Purpose |
|----------|-----------|-------------------|---------|
| all-premium | openai/gpt-5.6-sol | $2.00 / $10.00 | Premium baseline (TB #4, 37.3%) |
| all-flash | z-ai/glm-5.3-flash | $0.07 / $0.25 | Ultra-cheap, expected to fail on some |
| all-glm-5.3 | z-ai/glm-5.3 | $1.40 / $4.40 | Mid-tier control — is flash ≈ GLM-5.3? |
| smart-router | model="auto" | varies | LLM decides per turn |

### Why 4 strategies (not 3)

Added `all-glm-5.3` as control to test the assumption that flash ≈ GLM-5.3
in quality. If flash and GLM-5.3 produce same pass/fail, the assumption
holds. If they differ, we know flash is not a drop-in replacement.

### Why 3 runs (not 1)

Opus 5 data shows same task same model sometimes passes sometimes fails
(e.g. jax-speedrun-gpu 1/2). 3 runs give a coarse but meaningful signal:
3/3 = stable, 2/3 = likely, 1/3 = unlikely, 0/3 = never.

### Why GPT-5.6 Sol as premium (not Opus 5)

- Opus 5 ($5/$25) would cost ~$450 for 5 tasks × 3 runs
- GPT-5.6 Sol ($2/$10) is TB #4 at 37.3% — strong, affordable baseline
- **Acknowledged limitation:** Sol is not Opus 5. Tasks were selected
  based on Opus 5 data. If Sol fails where Opus passed, we report it
  honestly — it means Sol is weaker than Opus on these tasks, not that
  smart-router doesn't work.

## Smart Router Implementation Fixes (addressing review)

### Fix 1: Decision model cost now tracked in audit trail

Decision model calls now emit AuditRecord with:
- `step=router-decision`
- `model=qwen3.8-27b`
- `pool=decision-model`
- `prompt_tokens + completion_tokens` from vLLM response

This appears in `/v1/traces/{trial}/summary` under `per_model`.

### Fix 2: System prompt included in decision prompt

`include_system_prompt: true` now actually adds the system message
(first 200 chars) to the decision prompt. Previously configured but
not used.

### Fix 3: Cache key includes system message

Cache key now hashes: sorted model IDs + message count + system message
(200 chars) + latest user message (500 chars). This prevents cross-task
cache pollution when different tasks share similar user messages but
different system prompts.

### Fix 4: Harbor header configuration

**Known limitation:** Harbor's `session_id_headers` sets which header name
receives the session ID, but LiteLLM only sends `X-Session-ID` if the
session_id was set at LLM construction time. The runner's later
`agent.session_id` assignment may not propagate to LiteLLM's internal
state. Static `HARBOR_TRIAL/HARBOR_STEP` values would mix all trials together.

**Approach for this experiment:**
1. Use `X-Session-ID` as the trial identifier (Harbor does send this)
2. The gateway already extracts `SessionID` from `X-Session-ID` header
3. Query traces via `/v1/traces` and filter by `session_id` field in the
   audit SQLite directly (bypass `/v1/traces/{trial}/summary` which
   filters by `TrialName`)
4. For per-trial aggregation, write a post-processing script that:
   - Reads all traces from `/v1/traces?limit=1000`
   - Groups by `session_id` (which Harbor sets per trial)
   - Sums cost + tokens per session_id
   - Joins with harbor's trial results (reward.txt)

**Alternative (if Harbor supports it):** Configure
`llm_call_kwargs.extra_headers` with `X-Trial-Name` etc. and verify
whether Harbor propagates runtime trial names. If not, fall back to
session_id grouping.

**Code change needed in audit plugin:** Add `SessionID` to `TraceFilter`
and the SQL query so `/v1/traces?session_id=XXX` works as a query
parameter. Currently `TraceFilter` only has `TrialName`, `TaskName`,
`StepName`, `Limit` — no `SessionID` field.

## Execution

### Infrastructure

- aware-gateway on localhost:12026 (OpenRouter pool, smart-router enabled)
- Qwen3.8-27B on localhost:18000 (vLLM, L40S) — decision model
- OpenRouter API key in gateway config
- Docker available for harbor trial environments
- SSH tunnels to Qwen/DiffusionGemma VMs active

### Run Matrix

| Run | Strategy | Model | Tasks | Attempts | Total Trials |
|-----|----------|-------|-------|----------|-------------|
| 1 | all-premium | gpt-5.6-sol | 5 | 3 | 15 |
| 2 | all-flash | glm-5.3-flash | 5 | 3 | 15 |
| 3 | all-glm-5.3 | glm-5.3 | 5 | 3 | 15 |
| 4 | smart-router | auto | 5 | 3 | 15 |
| | | | | **Total** | **60 trials** |

### Cost Estimate

| Strategy | Per task/run (est) | 5 tasks × 3 runs | Notes |
|----------|-------------------|------------------|-------|
| all-premium | $5-50 | $75-750 | GPT-5.6 Sol, varies by task length |
| all-flash | $0.2-1 | $3-15 | GLM-5.3-flash, very cheap |
| all-glm-5.3 | $5-50 | $75-750 | GLM-5.3, 10x token volume |
| smart-router | $1-10 | $15-150 | Mostly flash, upgrade on ~20% turns |
| **Total** | | **$168-1665** | |

Worst case ~$1,665 (if all tasks run max time with premium).
Likely ~$500-800 (most tasks complete in 1-2h).

### Timeout Control

Use `--timeout-multiplier 0.3` to cap agent runtime at 30% of task default.
This bounds cost while still giving the agent a real chance to solve:
- 0.75h task → 13 min cap
- 4h task → 72 min cap
- 8h task → 144 min cap

## Metrics

### Per Trial
- reward (0 or 1) — from verifier pytest
- total cost — from gateway /v1/traces/{trial}/summary
- per-turn model — from gateway traces
- agent duration — from harbor
- decision model cost — step=router-decision in traces (newly tracked)

### Per Strategy (5 tasks × 3 runs = 15 trials)
- resolution rate (passes / 15)
- total cost
- avg cost per task
- cost per successful pass (total cost / passes)
- 95% CI for resolution rate (Wilson interval, n=15)

## Success Criteria (strengthened)

1. **smart_router_passes >= premium_passes - 1** (within 1 task of premium, out of 15 trials)
2. **smart_router_total_cost <= 30% of premium_total_cost**
3. **At least 1 task where smart-router passes but all-flash fails** (upgrade value)
4. **all_flash_passes vs all_glm53_passes within 1** (flash ≈ GLM-5.3 assumption check)
5. **Per-turn logs show differentiated selection** (not all-flash or all-premium)
6. **Decision model token usage tracked** (step=router-decision in traces with token counts)

Report all results with n=15 and Wilson 95% CI for resolution rate.
If criteria not met, report honestly as negative result.

## Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| all-flash passes everything | Valid finding — flash is sufficient for these tasks |
| all-flash fails everything | Valid finding — flash too weak, smart-router must upgrade |
| Sol fails where Opus passed | Report honestly — Sol ≠ Opus, note as limitation |
| flash ≠ GLM-5.3 quality | all-glm-5.3 control strategy will reveal this |
| Harbor can't send per-trial headers | Use X-Session-ID as fallback trial identifier |
| Decision model timeout | 10s timeout, falls back to task-router (rule-based) |
| Task timeout too short (--timeout-multiplier 0.3) | Accept — if agent can't solve in 30% time, it likely won't in 100% either |
| Cost exceeds budget | Monitor per-trial cost in real-time via /v1/traces; abort if > $2000 total |

## What This Experiment Does NOT Prove

- Does not prove smart-router is universally better than fixed-model strategies
- Does not prove flash = GLM-5.3 (only tests on 5 tasks, 2 of which are domain-specific)
- Does not prove the decision model makes optimal choices (only that its choices work)
- Does not account for agent loop variance (3 runs reduce but don't eliminate noise)
- Results are specific to these 5 tasks + terminus_2 agent + OpenRouter providers
- gsea-proteomics and cad-model are domain-specific — results may not generalize to pure software tasks
- Sol ≠ Opus — tasks selected on Opus data may behave differently with Sol as premium baseline
- OpenRouter pricing may change — snapshot /v1/models before running to lock prices

## Blog Update Plan

After experiment completes, update blog with:
1. Honest results table (pass/fail per task per strategy, all 3 runs)
2. Resolution rate comparison with Wilson CI
3. Cost comparison (including decision model overhead)
4. Per-turn model selection for smart-router (which turns upgraded?)
5. all-flash vs all-glm-5.3 comparison (flash quality assumption check)
6. If success criteria met: "smart-router achieved X% resolution at Y% cost"
7. If not met: honest reporting of what didn't work and why
