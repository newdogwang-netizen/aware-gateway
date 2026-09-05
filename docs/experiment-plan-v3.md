# aware-gateway Benchmark Experiment Plan v3

## Date: August 31, 2026

## Last Reviewed: September 1, 2026

## Objective

**Test whether** smart-router can achieve comparable resolution rate to a
premium model at significantly lower cost, by using GLM-5.3-flash for
low-risk turns, upgrading to GPT-5.6 Sol when the decision model judges it
necessary, and using Opus only as the strongest failure fallback when the
decision model itself fails.

The primary outcomes are task completion quality and total LLM cost.
Speed/latency is diagnostic only; it should explain operational behavior,
not determine success.

This is an exploratory experiment — not a proof. Results may show that
flash alone is sufficient, that smart-router helps, or that neither helps.

## GQM Mapping

### Goal

Analyze `smart-router` for the purpose of evaluating cost reduction while
preserving Terminal-Bench task completion quality, from the perspective of
an operator choosing a production routing policy, in the context of
Terminus2 agent runs through aware-gateway and OpenRouter.

### Questions

1. Does smart-router reach a resolution rate close to the premium baseline?
2. Does smart-router reduce total LLM cost compared with the premium baseline?
3. Does smart-router improve quality over the all-flash baseline on at least
   one selected task?
4. Does Qwen3 make differentiated routing choices, or does the policy collapse
   into one fixed model?
5. Are reward, cost, model choices, decision calls, and fallback calls
   attributable to the same unique trial/session?

### Metrics

- Quality: verifier reward per trial and resolution rate per strategy.
- Cost: total LLM cost per trial/strategy, including agent calls, decision
  model calls, retries, and any Opus fallback calls.
- Routing behavior: per-turn selected model, Sol upgrade rate, Opus fallback
  rate, cache-hit rate, and Qwen3 stated `reason`.
- Reliability: trace attribution canary result and missing-trace count.
- Diagnostic only: wall-clock duration and decision latency.

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
| Unit price ($/M in/out) | $5.00 / $25.00 | configured $1.40 / $4.40 | configured $0.07 / $0.25 |
| Total tokens (330 trials) | 6.5B | 8.7B | unknown |
| Total cost (330 trials) | $6,000 | $2,700 | est. $200-400 |
| Avg cost per Both-Pass task | $21 | $262 | est. $5-15 |

GLM-5.3's aggregate token count (8.7B) is ~1.34x Opus 5's (6.5B). However,
on individual tasks GLM-5.3's cost is often 5-15x higher than Opus 5's
(e.g. kv-live-surgery: $629 vs $15). This per-task cost explosion — not
aggregate token ratio — is why GLM-5.3 is not a drop-in cost saver.
GLM-5.3-flash at the configured $0.07/$0.25 keeps total cost negligible
even at high token volumes. OpenRouter prices can change and vary by
provider route; snapshot `/v1/models` and the gateway pricing config
immediately before the run.

**Note:** GLM-5.3-flash quality vs GLM-5.3 is an assumption, not verified.
This trimmed experiment does not test that assumption directly. GLM-5.3
historical results are used only to motivate task selection; do not claim
flash is equivalent to GLM-5.3 from this run.

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
| 5 | cad-model | 2h | 4/4 | 0/1 | Hardware* | Opus most stable 4/4 — strong premium-baseline contrast |

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

## Strategies (3 per task, 3 runs each)

| Strategy | Gateway Model | Harbor/LiteLLM Model | Price ($/M in/out) | Purpose |
|----------|---------------|---------------------|-------------------|---------|
| all-premium | openai/gpt-5.6-sol | openai/openai/gpt-5.6-sol | $2.00 / $10.00 | Premium baseline (TB #4, 37.3%) |
| all-flash | z-ai/glm-5.3-flash | openai/z-ai/glm-5.3-flash | $0.07 / $0.25 | Ultra-cheap, expected to fail on some |
| smart-router | auto | openai/auto | varies | LLM decides per turn |

Harbor uses LiteLLM locally, so every Harbor `--model` value must include the
`openai/` provider prefix for the gateway's OpenAI-compatible endpoint. The
gateway treats `openai/auto` as the smart-router trigger and canonicalizes
explicit fixed-model Harbor IDs through `model_map`, for example
`openai/z-ai/glm-5.3-flash` to `z-ai/glm-5.3-flash` and
`openai/openai/gpt-5.6-sol` to `openai/gpt-5.6-sol`. Explicit fixed-model
baselines bypass Qwen3; only `openai/auto` should create `router-decision`
traces.

The removed GLM-5.3 fixed-model strategy is not a baseline in the run
matrix, and it is also excluded from the smart-router decision menu for
this experiment. Qwen3 can choose only between `z-ai/glm-5.3-flash` and
`openai/gpt-5.6-sol`. This keeps the smart-router result attributable to
"cheap unless premium is needed" rather than an unbaselined mid-tier
model. Opus is intentionally not in the Qwen3 decision menu; it is reserved
only for configured decision-failure fallback.

### Why 3 strategies

Keep the matrix focused on the product question:
- Can smart-router match the premium baseline?
- Does smart-router beat the all-flash baseline on tasks where flash is
  too weak?
- Does smart-router save enough total LLM cost after counting decision
  calls and any Opus fallback calls?

Dropping the GLM-5.3 fixed-model control reduces cost and removes a
diagnostic comparison that is not required for the
smart-router-vs-fixed-baseline claim. The trade-off is that this run no
longer tests whether GLM-5.3-flash is a valid proxy for GLM-5.3 quality.

### Why 3 runs (not 1)

Opus 5 data shows same task same model sometimes passes sometimes fails
(e.g. jax-speedrun-gpu 1/2). 3 runs give a coarse but meaningful signal:
3/3 = stable, 2/3 = likely, 1/3 = unlikely, 0/3 = never.

### Why GPT-5.6 Sol as premium (not Opus 5)

- Opus 5 ($5/$25) would be materially more expensive; using the selected
  historical task costs gives roughly $160 for 5 tasks × 3 runs, but
  rerun cost can vary sharply with agent loop length and retries
- GPT-5.6 Sol ($2/$10) is TB #4 at 37.3% — strong, affordable baseline
- **Acknowledged limitation:** Sol is not Opus 5. Tasks were selected
  based on Opus 5 data. If Sol fails where Opus passed, we report it
  honestly — it means Sol is weaker than Opus on these tasks, not that
  smart-router doesn't work.

### Why Opus as fallback

Fallback is for decision-system failure, not normal optimization. If Qwen3
times out, returns invalid JSON, or names a model outside the menu,
smart-router routes directly to `anthropic/claude-opus-5` through
`fallback_model` / `fallback_pool`. This preserves quality when the router
control plane fails and avoids unplanned task-router choices like Luna or
GLM-5.3. All Opus fallback calls count toward smart-router total cost and
must be reported separately as `opus_fallback_rate`.

## Smart Router Implementation Fixes (addressing review)

### Fix 1: Decision model usage/cost now tracked in audit trail

Decision model calls now emit AuditRecord with:
- `step=router-decision` or `router-decision-turnN`
- `model=qwen3.8-27b`
- `pool=decision-model`
- `prompt_tokens + completion_tokens` from vLLM response
- `trial_name`, `task_name`, and `session_id` copied from the original
  gateway request when those headers are present
- `cost`, calculated from `decision_input_price` / `decision_output_price`
  when configured; for self-hosted vLLM this may intentionally be $0

This appears in `/v1/traces/{trial}/summary` under `per_model` only if
`X-Trial-Name` is present. If Harbor only provides `X-Session-ID`, use
`/v1/traces?session_id=...` or a post-processing script grouped by
`session_id`.

### Fix 2: System prompt included in decision prompt

`include_system_prompt: true` now actually adds the system message
(first 200 chars) to the decision prompt. Previously configured but
not used.

### Fix 3: Cache key includes system message

Cache key now hashes: sorted model IDs + message count + system message
(200 chars) + latest user message (2000 chars). This prevents cross-task
cache pollution when different tasks share similar user messages but
different system prompts.

### Decision Prompt: Qwen3 Judgement Basis

Qwen3 does not see the Terminal-Bench verifier, repository files, hidden
task state, or historical pass/fail table. Its routing decision is based
only on the prompt assembled by `smart-router` for the current LLM call,
except for the deterministic guardrail below:

- Harbor task-completion confirmation is routed directly to the strongest
  configured menu model. The confirmation asks the agent to emit exact
  `task_complete` protocol output; a malformed response can lose reward after
  the solution is already complete. This is logged through the normal agent
  call audit record with a `smart-router guardrail` reason and does not count
  as a Qwen3 decision call.

For ordinary `auto` calls, Qwen3 sees:

- Routing instruction: choose the lowest-cost model likely to succeed for
  this one call while optimizing final task quality per dollar, not speed.
  Ordinary terminal-agent work is iterative, so the stronger model should be
  used only when it is likely to materially improve final success or avoid
  expensive rework.
- Model menu: candidate model IDs, pools, input/output prices,
  capabilities, and context windows, sorted cheapest first.
- Turn phase inferred from message count:
  `understand`, `plan`, `code`, `review`, or `fix`.
- Conversation depth: message count and rough input token estimate.
- Request preview: first 200 chars of the system message and up to
  2000 chars of the latest user message.
- Guidelines: reversible progress — comprehension, environment inspection,
  command-output summaries, routine planning, short answers, and drafting
  low-risk commands/tests — uses the cheapest model. Qwen3 must not upgrade
  solely because the task domain mentions security, performance, data, or
  systems work; it should upgrade when the current turn changes
  safety-critical logic, diagnoses failed tests, handles subtle edge cases,
  or makes a hard-to-recover decision. High-leverage turns — nontrivial core
  code changes, failed-test debugging, multi-file/stateful changes,
  concurrency/performance/data-corruption risks, specialized domain
  reasoning, exact agent-control protocols, or fragile final fixes — use the
  strongest model. Normal terminal-agent JSON replies and shell-command batches
  are not by themselves an upgrade reason; they are the ordinary iterative
  protocol unless the turn is final submission or otherwise hard to recover.
  Finalization or submission turns after local checks pass also use the
  strongest model, even when the latest message looks like a simple
  confirmation, because malformed `task_complete` output or a missed last check
  loses the whole trial. It must judge both the underlying task and current
  turn: a hard task can still have cheap observation/planning turns, while a
  short follow-up can require the strongest model if the state is complex.
  Prefer the cheaper model when it can safely advance the task; choose the
  stronger model when its expected quality gain justifies the much higher cost.
- Output contract: JSON only, `{"model":"id","reason":"why"}`.

Decision call settings are part of the experimental condition:
`temperature=0`, `max_tokens=100`, and
`chat_template_kwargs.enable_thinking=false` for fast JSON-only output.
If the smart-router cache hits, Qwen3 is not called for that turn; the
cached route should be analyzed separately or avoided by the cache policy.

For the experiment report, treat `reason` as the model's stated rationale,
not ground truth. The audit log should be used to check whether Qwen3
actually differentiated turns and whether those choices improved pass rate
or cost.

### Fix 4a: Completion confirmation guardrail

Harbor/Terminus2 requires a second confirmation after the agent first marks a
task complete. The prompt contains:

- "Are you sure you want to mark the task as complete?"
- `"task_complete": true` for JSON mode, or
  `<task_complete>true</task_complete>` for XML mode

This is a high-impact control-protocol turn, not a simple chat response. If a
cheap model fails to emit the exact confirmation, the trial can keep running or
lose reward even though the solution has passed local checks. Therefore
smart-router bypasses Qwen3 for this exact confirmation message and routes to
the strongest configured menu model, currently `openai/gpt-5.6-sol`.

The guardrail is part of the declared routing policy for V3. It should be
reported separately from Qwen3 choices by filtering `routing_reason` beginning
with `smart-router guardrail`.

### Fix 4: Opus fallback for decision failures

If Qwen3 fails to produce a valid in-menu decision, smart-router now routes
directly to:

- `fallback_model: anthropic/claude-opus-5`
- `fallback_pool: openrouter`

This keeps failure handling quality-first while preventing the request from
falling through to task-router's general-purpose model list. Opus fallback
is not evidence that Qwen3 made a good routing decision; it is a reliability
guardrail. Report fallback count and cost separately, and treat frequent
fallbacks as a router availability problem.

### Fix 5: Harbor session attribution patch

**Chosen approach:** patch local Harbor/Terminus2 before the formal matrix.
Timestamp joins are acceptable only as a forensic fallback, not as the primary
measurement path, because they can silently misattribute traces under retries,
summarization calls, or any accidental overlap.

**Observed break:** the local `lite_llm.py` already writes its `_session_id` to
both `extra_body.session_id` and `X-Session-ID`. The missing link is later:
`harbor/trial/trial.py` sets `agent.session_id = {trial_name}__agent` after
Terminus2 constructs LiteLLM, so LiteLLM can keep a construction-time
`session_id` that is empty or random.

**Required local patch:** in the installed Harbor file
`harbor/agents/terminus_2/terminus_2.py`, `_reset_per_run_state()` must:

1. Prefer the runtime `self.session_id` set by `trial.py`.
2. Fall back to the constructor-provided `session_id`.
3. Generate a UUID only if neither exists.
4. Copy the selected value into `self._llm._session_id` before the first chat
   call of the run.

**Implemented gateway support:** `/v1/traces?session_id=XXX` is supported
by `TraceFilter.SessionID` and the audit SQL query. The gateway also
normalizes body-derived `session_id` back into internal headers before router
hooks run, so smart-router decision traces can share the same session group.
Before proxying to commercial upstreams, the gateway strips the internal
`session_id` field from the JSON body and removes correlation headers.

**Canary acceptance:** one trial must show all agent LLM calls and all
`router-decision-*` calls grouped under the same unique `trial_name` or
`session_id`. If this fails, cost and model-selection analysis will be
misattributed.

### Fix 6: Streaming usage accounting

Some Harbor/Terminus2 calls can produce model responses that are rejected by
the agent parser and therefore do not appear as successful agent steps in
`trajectory.json`. They still consume tokens and must count toward the
strategy's cost.

For OpenAI-compatible chat requests with `stream: true`, the gateway now adds
`stream_options.include_usage: true` while preserving any existing
`stream_options`. The response writer parses the final SSE usage chunk and
records prompt/completion/total tokens and recomputed gateway cost in the audit
trace. Non-streaming requests are unchanged.

If a provider does not return streaming usage despite the option, report the
affected strategy's cost as a lower bound and do not claim a precise
cost-saving ratio for those rows.

### Current Runner Status

`scripts/benchmark.py` is a legacy 5-turn simulation benchmark. It does
not run Terminal-Bench verifier trials and should not be used as evidence
for this V3 plan.

V3 requires:
- Harbor commands that run this 3-strategy × 5-task × 3-attempt matrix, and
- A post-processing script that joins Harbor `reward.txt` results with
  gateway traces by `trial_name` or `session_id`.

Do not start the full 45-trial experiment until the canary confirms trace
correlation and the post-processing path is working.

## Execution

### Infrastructure

- aware-gateway on localhost:12026 (OpenRouter pool, smart-router enabled)
- Qwen3.8-27B on localhost:18000 (vLLM, L40S) — decision model
- OpenRouter API key in gateway config
- Docker available for harbor trial environments
- SSH tunnels to Qwen/DiffusionGemma VMs active
- Local `terminal-bench/` task directory present; snapshot task file hashes
  before the run instead of depending on a mutable registry tag.

### Experiment Steps

Run the experiment in two gates: first prove measurement integrity with
canaries, then run the 45-trial matrix. Do not optimize for speed; run
serially unless the canary shows attribution, cost, and provider behavior are
stable.

#### Step 0: Freeze run identity

```bash
export EXP_ID="aware-v3-$(date -u +%Y%m%dT%H%M%SZ)"
export ARTIFACT_DIR="artifacts/$EXP_ID"
mkdir -p "$ARTIFACT_DIR" "$ARTIFACT_DIR/jobs"
```

#### Step 1: Snapshot code, config, and prices

```bash
git rev-parse HEAD > "$ARTIFACT_DIR/git-sha.txt"
git diff --stat > "$ARTIFACT_DIR/git-diff-stat.txt"
git diff > "$ARTIFACT_DIR/git-diff.patch"
cp configs/gateway-openrouter.yaml "$ARTIFACT_DIR/gateway-openrouter.yaml"
find terminal-bench -maxdepth 2 -name task.toml -print | sort \
  > "$ARTIFACT_DIR/terminal-bench-task-files.txt"
find terminal-bench -maxdepth 2 -type f \
  \( -name task.toml -o -name instruction.md -o -name README.md \) -print0 \
  | sort -z | xargs -0 sha256sum \
  > "$ARTIFACT_DIR/terminal-bench-task-files.sha256"

make docker
docker image inspect aware-gateway:latest > "$ARTIFACT_DIR/docker-image.json"
```

#### Step 2: Start gateway for experiment runs

Use host networking because `smart-router.endpoint` points at
`localhost:18000`; with normal Docker bridge networking, `localhost` would be
inside the gateway container. Mount `/data` so `./data/audit-openrouter.db`
survives container restarts.

```bash
docker rm -f aware-gateway 2>/dev/null || true
docker run -d \
  --name aware-gateway \
  --network host \
  -v "$PWD/data:/data" \
  -e GW_OPENROUTER_KEY="$(cat openrouter.env)" \
  aware-gateway:latest

curl -sf http://localhost:12026/health > "$ARTIFACT_DIR/gateway-health.json"
curl -sf http://localhost:12026/v1/models > "$ARTIFACT_DIR/openrouter-models.json"
```

#### Step 3: Direct gateway canary

Verify smart-router before Harbor is involved.

```bash
TRIAL="$EXP_ID-direct-canary"
SESSION="${TRIAL}__agent"

curl -sf http://localhost:12026/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-Trial-Name: $TRIAL" \
  -H "X-Session-ID: $SESSION" \
  -H "X-Step-Name: turn1" \
  -H "X-Task-Name: html-js-filter" \
  -d '{
    "model": "auto",
    "messages": [
      {"role": "system", "content": "You are a terminal coding agent."},
      {"role": "user", "content": "Inspect a small HTML/JS filtering bug and propose the safest model choice."}
    ],
    "max_tokens": 120
  }' > "$ARTIFACT_DIR/direct-canary-response.json"

sleep 5
curl -sf "http://localhost:12026/v1/traces/$TRIAL/summary" \
  > "$ARTIFACT_DIR/direct-canary-summary.json"
curl -sf "http://localhost:12026/v1/traces?session_id=$SESSION&limit=1000" \
  > "$ARTIFACT_DIR/direct-canary-session-traces.json"
```

Acceptance:
- `router-decision` or `router-decision-turn1` appears in traces.
- The routed agent model is either `z-ai/glm-5.3-flash` or
  `openai/gpt-5.6-sol`.
- `anthropic/claude-opus-5` does not appear unless the decision call failed.
- Prompt/completion tokens and cost are present for both decision and agent
  calls.

#### Step 4: Harbor canary

Run one cheap `smart-router` Terminal-Bench attempt. The goal is trace/reward
alignment, not benchmark performance. Precondition: the Harbor session
attribution patch in Fix 5 is applied and Python syntax-checks cleanly.

For Docker-based Harbor agents on Linux, start with the Docker bridge host IP:

```bash
export GW_HOST="172.17.0.1"
export GW_BASE="http://$GW_HOST:12026/v1"
```

If the agent runs on the host instead of inside Docker, use:

```bash
export GW_HOST="127.0.0.1"
export GW_BASE="http://127.0.0.1:12026/v1"
```

Then run:

```bash
AWARE_HARBOR_LLM_ATTEMPTS=1 harbor run \
  --job-name "$EXP_ID-canary-smart-router-html-js-filter" \
  --jobs-dir "$ARTIFACT_DIR/jobs" \
  --agent terminus-2 \
  --model "openai/auto" \
  --path terminal-bench \
  --include-task-name "html-js-filter" \
  --n-attempts 1 \
  --n-concurrent 1 \
  --timeout-multiplier 0.3 \
  --allow-agent-host "$GW_HOST" \
  --allow-environment-host "$GW_HOST" \
  --agent-kwarg "api_base=$GW_BASE" \
  --agent-env "OPENAI_API_KEY=dummy" \
  --agent-env "AWARE_HARBOR_LLM_ATTEMPTS=1" \
  --yes | tee "$ARTIFACT_DIR/harbor-canary.log"

sleep 5
curl -sf "http://localhost:12026/v1/traces?limit=10000" \
  > "$ARTIFACT_DIR/harbor-canary-traces.json"

python3 scripts/analyze_v3_results.py \
  --artifact-dir "$ARTIFACT_DIR" \
  --gateway-config configs/gateway-openrouter.yaml \
  --traces-json "$ARTIFACT_DIR/harbor-canary-traces.json" \
  --job-glob "$EXP_ID-canary-smart-router-html-js-filter" \
  --expected-rows 1 \
  --strict \
  --output "$ARTIFACT_DIR/harbor-canary-analysis.csv"
```

Acceptance:
- Harbor produces a `reward.txt` for the canary trial.
- Gateway traces contain all agent calls and all `router-decision-*` calls for
  the same unique `trial_name` or `session_id`.
- The post-processing path can join Harbor reward with gateway traces and
  produce exactly one canary result row.
- If trace attribution fails, stop here and fix headers/session IDs before
  running the full matrix.

#### Step 5: Generate randomized run order

Use a fixed seed and treat `(task, attempt)` as the block. Each block contains
all three strategies in randomized order.

```bash
python3 - <<'PY' > "$ARTIFACT_DIR/run-order.csv"
import csv
import random
import sys

seed = 20260901
tasks = [
    "html-js-filter",
    "vpp-loss-divergence",
    "gsea-proteomics",
    "kv-live-surgery",
    "cad-model",
]
strategies = [
    ("all-premium", "openai/gpt-5.6-sol", "openai/openai/gpt-5.6-sol"),
    ("all-flash", "z-ai/glm-5.3-flash", "openai/z-ai/glm-5.3-flash"),
    ("smart-router", "auto", "openai/auto"),
]

random.seed(seed)
writer = csv.writer(sys.stdout, lineterminator="\n")
writer.writerow(["task", "attempt", "strategy", "gateway_model", "harbor_model"])
for task in tasks:
    for attempt in range(1, 4):
        block = strategies[:]
        random.shuffle(block)
        for strategy, gateway_model, harbor_model in block:
            writer.writerow([task, attempt, strategy, gateway_model, harbor_model])
PY

echo 20260901 > "$ARTIFACT_DIR/random-seed.txt"
```

#### Step 6: Run the formal matrix

Run one Harbor job per row so each attempt can be traced and recovered
independently. Keep `--n-concurrent 1`; speed is not an evaluation target.
The checked-in helper `scripts/run_v3_matrix.sh` implements this loop with
per-job strict aggregation, resume support, provider-5xx fail-fast monitoring,
a cumulative cost hard stop, and the Opus fallback pause gate.

```bash
tail -n +2 "$ARTIFACT_DIR/run-order.csv" |
while IFS=, read -r task attempt strategy gateway_model harbor_model; do
  JOB="$EXP_ID-${strategy}-${task}-a${attempt}"

  AWARE_HARBOR_LLM_ATTEMPTS=1 harbor run \
    --job-name "$JOB" \
    --jobs-dir "$ARTIFACT_DIR/jobs" \
    --agent terminus-2 \
    --model "$harbor_model" \
    --path terminal-bench \
    --include-task-name "$task" \
    --n-attempts 1 \
    --n-concurrent 1 \
    --timeout-multiplier 0.3 \
    --allow-agent-host "$GW_HOST" \
	    --allow-environment-host "$GW_HOST" \
	    --agent-kwarg "api_base=$GW_BASE" \
	    --agent-env "OPENAI_API_KEY=dummy" \
	    --agent-env "AWARE_HARBOR_LLM_ATTEMPTS=1" \
	    --yes | tee "$ARTIFACT_DIR/${JOB}.log"

  # The checked-in runner additionally polls the current trial's
  # `session_id` traces while Harbor is running. If an agent model call returns
  # `status >= 500`, the runner records `early-provider-failure.json`,
  # interrupts Harbor, and aggregates the trial as `reward=0`.
  # `AWARE_HARBOR_LLM_ATTEMPTS=1` disables Harbor/LiteLLM automatic LLM retries
  # for this experiment so provider failures are not hidden or multiplied.

  sleep 5
  curl -sf "http://localhost:12026/v1/traces?limit=10000" \
    > "$ARTIFACT_DIR/traces-after-${JOB}.json"

  python3 scripts/analyze_v3_results.py \
    --artifact-dir "$ARTIFACT_DIR" \
    --gateway-config configs/gateway-openrouter.yaml \
    --traces-json "$ARTIFACT_DIR/traces-after-${JOB}.json" \
    --job-glob "$JOB" \
    --expected-rows 1 \
    --strict \
    --output "$ARTIFACT_DIR/${JOB}-analysis.csv"
done
```

#### Step 7: Pause gates during the matrix

Pause the run and diagnose before continuing if any of these happen:
- A completed Harbor trial cannot be matched to a unique trace group.
- Gateway trace agent-call count materially exceeds trajectory agent-step count
  while trace token usage is zero, which would undercount malformed responses.
- `anthropic/claude-opus-5` appears in more than one smart-router trial.
- Any single trial exceeds the expected per-trial cost range by more than 2x
  in Harbor `agent_result.cost_usd` or recomputed trajectory cost.
- Qwen3 decision calls time out repeatedly or return unknown models.
- Harbor task-completion confirmation repeats after local checks have passed,
  indicating the completion guardrail is missing or ineffective.
- OpenRouter model availability or price snapshot changes materially.

#### Step 8: Aggregate and analyze

The aggregation output must have one row per formal trial:

```bash
sleep 5
curl -sf "http://localhost:12026/v1/traces?limit=100000" \
  > "$ARTIFACT_DIR/traces-final.json"

python3 scripts/analyze_v3_results.py \
  --artifact-dir "$ARTIFACT_DIR" \
  --gateway-config configs/gateway-openrouter.yaml \
  --traces-json "$ARTIFACT_DIR/traces-final.json" \
  --job-glob "$EXP_ID-*-a*" \
  --expected-rows 45 \
  --strict \
  --output "$ARTIFACT_DIR/results.csv"
```

```text
experiment_id,task,attempt,strategy,model_sent,agent_models,reward,total_cost_usd,
agent_cost_usd,decision_cost_usd,prompt_tokens,completion_tokens,total_tokens,agent_call_count,
decision_call_count,guardrail_call_count,sol_upgrade_rate,opus_fallback_rate,cache_hit_rate,
duration_seconds,trace_key,failure_kind,exception_type,cost_source
```

Report:
- Aggregate pass rate, total cost, cost per successful pass, and Wilson 95% CI.
- Paired per-task pass/cost deltas against `all-premium` and `all-flash`.
- Smart-router model breakdown: flash calls, Sol upgrade calls, Opus fallback
  calls, completion-guardrail calls, and decision-call overhead.
- Negative findings explicitly, especially if all-flash is sufficient or if
  smart-router quality depends on Opus fallback rather than Sol upgrades.

### Run Matrix

| Run | Strategy | Model | Tasks | Attempts | Total Trials |
|-----|----------|-------|-------|----------|-------------|
| 1 | all-premium | openai/gpt-5.6-sol | 5 | 3 | 15 |
| 2 | all-flash | z-ai/glm-5.3-flash | 5 | 3 | 15 |
| 3 | smart-router | auto | 5 | 3 | 15 |
| | | | | **Total** | **45 trials** |

Default execution is 45 separate one-attempt jobs, generated from
`run-order.csv`, so each trial can be traced and recovered independently.
Use `--n-attempts 3` only as an optimization after the canary proves
per-attempt headers are unique and the post-processing path can separate
attempts correctly.

### Randomization and Blocking

Treat task as the block. For each `(task, attempt)` pair, run all three
strategies and randomize their order using a recorded seed. This reduces
time-of-day, provider-route, quota, and transient infrastructure bias. The
report should include both aggregate results and a per-task paired table:
`all-premium`, `all-flash`, `smart-router`, pass/fail, cost, and selected
models for each attempt.

### Runtime and Cost Estimate

| Strategy | Per task/run (est) | 5 tasks × 3 runs | Notes |
|----------|-------------------|------------------|-------|
| all-premium | $5-50 | $75-750 | GPT-5.6 Sol, varies by task length |
| all-flash | $0.2-1 | $3-15 | GLM-5.3-flash, very cheap |
| smart-router | $1-10 | $15-150 | Mostly flash, upgrade on ~20% turns; Opus fallback cost counted if triggered |
| **Total** | | **$93-915** | |

Treat these as planning estimates, not a budget guarantee. Recompute after
the canary from observed tokens/minute and the current `/v1/models` pricing
snapshot. If Opus fallback triggers repeatedly, pause the run and fix the
decision path before interpreting smart-router quality.

Canary-derived serial runtime estimate with `--timeout-multiplier 0.3`:

| Task | Expert estimate | Canary / extrapolated single trial |
|------|-----------------|------------------------------------|
| html-js-filter | 0.75h | ~15 min |
| vpp-loss-divergence | 2h | ~40 min |
| cad-model | 2h | ~40 min |
| gsea-proteomics | 4h | ~70 min |
| kv-live-surgery | 4h | ~70 min |

For 45 serial trials, this implies roughly 18h optimistic, 34h realistic,
and 52h pessimistic wall-clock time. Runtime is not a success metric, but it
sets operator expectations for monitoring and pause points. Current planning
budget is `$200-500`; keep the hard stop at `$2000` unless a new canary shows
substantially higher per-trial cost.

### Timeout Control

Use `--timeout-multiplier 0.3` to cap agent runtime at 30% of the
Terminal-Bench task timeout, not 30% of the expert-time estimate. Verify the
actual timeout from the resolved Harbor task config before extrapolating
cost. If the task timeout is 28,800 seconds, `0.3` still allows 8,640
seconds (144 minutes), regardless of the task's expert-time label. Timeout
is a cost guardrail, not a speed success criterion.

Gateway OpenRouter calls use a 300s server/endpoint timeout during the
experiment. This leaves room for slow flash responses while still closing
streaming response bodies at the endpoint timeout so abandoned client calls do
not leave stale in-flight requests.

Harbor/LiteLLM and Terminus2 normally retry LLM failures up to three times. The
local Harbor runtime is patched so the default is one LLM attempt, while
`AWARE_HARBOR_LLM_ATTEMPTS` can explicitly override it. The V3 runner still sets
`AWARE_HARBOR_LLM_ATTEMPTS=1` both on the outer `harbor run` process and via
`--agent-env` for the agent phase as an audit-visible experiment condition. This
keeps timeout and 5xx semantics at one provider call per agent turn; the
analyzer records the trial as `reward=0` with `failure_kind=provider_5xx` when
the fail-fast marker is present.

### Reasoning Control

OpenRouter metadata for `z-ai/glm-5.3-flash` shows reasoning is mandatory,
enabled by default, and defaults to `max` effort. Since this experiment's
primary optimization target is cost while preserving quality, the gateway
normalizes unpinned flash calls to:

```json
{"reasoning": {"effort": "low", "exclude": true}}
```

This applies to both all-flash trials and smart-router turns routed to flash.
It preserves the flash model identity while avoiding a hidden max-reasoning
cost/latency confound. If a caller explicitly supplies `reasoning` or
`reasoning_effort`, the gateway preserves that caller policy.

### Cache Policy

For a clean measurement of routing decisions, disable or reset the
smart-router cache between attempts. The current experiment config sets
`cache_ttl_seconds: -1`, which disables decision caching for measurement
runs. Note that `cache_ttl_seconds: 0` means "use the default 300 seconds",
so do not use 0 as a disable switch. Options:
- Keep `cache_ttl_seconds: -1` for measurement runs, or
- Restart the gateway before each attempt, or
- Keep the cache enabled and report that the result measures cached product
  behavior, not independent routing decisions.

### Failure Semantics

- Verifier failure, timeout, infrastructure crash, or malformed final output
  counts as `reward=0` unless the trial is explicitly excluded before seeing
  the verifier result because the trace attribution canary failed.
- The OpenRouter experiment config sets `retry.max_retries: 1` for LLM
  generation calls. Provider timeouts are therefore recorded as that trial's
  failure/cost outcome instead of being automatically retried, which keeps the
  cost measurement aligned with the decision prompt's "one call, no second
  chance" assumption.
- The matrix runner polls the current trial's gateway trace group. If an agent
  OpenRouter call records `status >= 500`, the runner writes an
  `early-provider-failure.json` marker, interrupts Harbor, and the analyzer
  records `reward=0` with `failure_kind=provider_5xx`.
- If a future rerun enables retries, all retry attempts and provider fallbacks
  must count toward the strategy's total cost and be reported separately.
- Opus fallback from smart-router decision failure counts toward
  smart-router total cost and `opus_fallback_rate`; it is not counted as a
  Qwen3 upgrade decision.
- If Opus fallback happens on more than one trial, report the run as a
  routing-availability problem before making a smart-router quality claim.

### Reproducibility Checklist

Record the following before the canary and before the full matrix:

- Git SHA and `git diff --stat` for aware-gateway.
- Full gateway config used for each strategy, with secrets removed.
- OpenRouter `/v1/models` pricing snapshot and gateway pricing config.
- Harbor/Terminus2 version, local task file hashes, and Docker image digest.
- Randomization seed and generated run order.
- Smart-router cache policy and whether the gateway was restarted per attempt.

## Metrics

### Per Trial, Primary
- reward (0 or 1) — from verifier pytest
- agent token usage and cost — from Harbor `agent/trajectory.json` and
  `agent_result`; if per-step `cost_usd` is missing or zero while token usage
  is nonzero, recompute from `model_name`, prompt/completion tokens, cached
  tokens, and the locked OpenRouter pricing snapshot
- decision token usage/cost — `step_name` beginning with `router-decision` in
  gateway traces; current Qwen3 local decision cost is expected to be `$0`
- total cost — `agent_cost_usd + decision_cost_usd + any Opus fallback cost`;
  gateway `/v1/traces` is the attribution source, not the only token source
- per-turn model — from gateway traces
- trace key — prefer unique `session_id`; use `trial_name` only when it is
  exactly the Harbor trial name
- Sol upgrade rate and Opus fallback rate for smart-router trials
- Completion-guardrail call count for smart-router trials

### Per Trial, Diagnostic
- agent duration — from Harbor, used only to interpret cost/runtime anomalies
- decision latency — from gateway traces, not part of success criteria

### Per Strategy (5 tasks × 3 runs = 15 trials)
- resolution rate (passes / 15)
- total cost
- avg cost per task
- cost per successful pass (total cost / passes)
- 95% CI for resolution rate (Wilson interval, n=15)
- paired per-task pass/cost deltas against all-premium and all-flash

## Success Criteria (strengthened)

Primary claim criteria:

1. **smart_router_passes >= premium_passes - 1** (within 1 pass of premium, out of 15 trials)
2. **smart_router_total_cost <= 30% of premium_total_cost**
3. **Trace attribution is valid** (`trial_name` or `session_id` uniquely groups each attempt)
4. **Decision model token usage is tracked** (`step_name` starts with `router-decision`)
5. **Opus fallback is rare** (`opus_fallback_trials <= 1`); if higher,
   report the result as fallback reliability behavior, not clean Qwen3
   routing behavior

Upgrade-value criteria:

1. **At least 1 task where smart-router passes but all-flash fails**
2. **Per-turn logs show differentiated selection** (not all-flash or all-premium)
3. **Qwen3-selected Sol upgrades, not Opus fallbacks or completion-guardrail
   calls alone, explain any quality gain over all-flash**

Diagnostic checks:

1. If all-flash passes everything, report "flash is sufficient for these tasks"; do not claim smart-router added resolution value
2. If smart-router uses one fixed model for nearly all turns, report that the decision model did not differentiate this sample
3. If Opus fallback drives a pass, report it as quality-preserving fallback,
   not as evidence that Qwen3 selected the right model
4. Treat flash-vs-GLM-5.3 equivalence as untested in this trimmed plan

Report all results with n=15, Wilson 95% CI for resolution rate, and
paired per-task pass/cost deltas.
If primary criteria are not met, report honestly as a negative result for
the smart-router cost-savings claim.

## Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| all-flash passes everything | Valid finding — flash is sufficient for these tasks |
| all-flash fails everything | Valid finding — flash too weak, smart-router must upgrade |
| Sol fails where Opus passed | Report honestly — Sol ≠ Opus, note as limitation |
| flash ≠ GLM-5.3 quality | Not measured in this trimmed plan; do not claim equivalence |
| Harbor session attribution patch does not produce unique trace groups | Stop after canary; do not run the matrix until one attempt maps to exactly one session group containing both agent calls and `router-decision-*` calls |
| Decision model timeout or invalid output | Route directly to Opus via `fallback_model`; count cost and fallback rate separately |
| Task timeout too short (--timeout-multiplier 0.3) | Report as a limitation; do not infer full-time leaderboard-equivalent capability |
| Cost exceeds budget | Monitor per-trial cost in real-time via /v1/traces; abort if > $2000 total |
| Provider/model availability drift | Snapshot `/v1/models`, pricing, and resolved provider route before the run |
| Sequential run-order bias | Randomize strategy order within each task/attempt block and record seed |

## What This Experiment Does NOT Prove

- Does not prove smart-router is universally better than fixed-model strategies
- Does not prove flash = GLM-5.3 because the GLM-5.3 fixed-model control was removed
- Does not prove the decision model makes optimal choices (only that its choices work)
- Does not prove Opus fallback is part of the smart-router decision policy;
  fallback is only a quality-first safety path for decision failures
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
5. all-flash vs smart-router comparison (upgrade value check)
6. If success criteria met: "smart-router achieved X% resolution at Y% cost"
7. If not met: honest reporting of what didn't work and why
