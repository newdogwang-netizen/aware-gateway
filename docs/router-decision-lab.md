# Smart Router Decision Lab

This note defines the next experiment layer for studying smart-router decisions.
The unit of analysis is one router decision, not one benchmark trial.

## Current Data

The V4 run already has enough trace data for a first decision lab:

- Decision-model calls are recorded as `pool=decision-model` and `step_name=router-decision`.
- The following agent call records `routed_model`, cost, latency, status, and `routing_reason`.
- Trial-level rows provide task, strategy, attempt, final reward, failure kind, and aggregate call counts.

For `/mnt/data2/aware-gateway-runs/aware-v4-20260902T115134Z`, the extracted formal dataset has:

- `487` paired decision records.
- `505` smart-router/warmstart agent calls.
- `96.4%` overall decision coverage.
- Ordinary `smart-router`: `329 / 332` decision coverage.
- `smart-router-warmstart`: `158 / 173` decision coverage, with the expected gap from forced Opus warm-start calls.

Generated files:

- `router-decisions.json`: structured data, one row per decision.
- `router-decision-lab.html`: local static visualization.

Build command:

```bash
python3 scripts/build_router_decision_lab.py \
  --artifact-dir /mnt/data2/aware-gateway-runs/aware-v4-20260902T115134Z
```

## Questions This Can Answer Now

- How often does the router choose Flash vs Opus?
- Which tasks and strategies cause more Opus upgrades?
- How much does the decision model itself cost?
- What stated reasons lead to expensive model choices?
- Did successful trials have a different model-choice pattern from failed trials?
- Which agent calls lack a matching decision record?

## Replay-First Direction

The current artifacts include `agent/trajectory.json` for each trial. That means we can reconstruct the local decision context around each router call and run a new router prompt against the same point in the trajectory.

This is the best next loop:

- use the existing benchmark trajectory as frozen context
- change only the router prompt
- rerun only the decision model
- compare model choices and reasons by `prompt_id`

This directly shows the decision objective encoded by the prompt: cost-strict prompts should choose Flash more often, quality-guarded prompts should choose Opus more often on risky turns, and balanced prompts should separate cheap observation from expensive implementation/fix turns.

## Current Limitation

The current trace does not store the exact router prompt text as sent on the wire. The lab can reconstruct a close decision context from trajectory steps, and it can infer the selected model and reason from the next agent call. It cannot yet do byte-for-byte replay of the old decision prompt.

Full Harbor reruns are still useful for end-to-end quality and cost, but they mix two effects:

- A changed router prompt changes model choices.
- Changed model choices change the agent trajectory, which changes later router inputs.

For clean prompt research, prefer offline decision replay first, then run Harbor only for the most promising prompt variants.

## History-Aware Router Variant

The live `smart-router` now keeps compact short-term memory per unique
`X-Session-ID` or `X-Trial-Name`.

- Each routed call appends one compact record: selected model, turn type,
  hypothesis state, critical-path flag, recoverability, `context_summary`, and
  short reason.
- The next decision prompt sees at most the latest 5 records for the same trial,
  oldest to newest.
- Each `context_summary` is bounded so the router gets state memory without
  replaying the whole transcript.
- Warm-start and fallback routes are also recorded so later decisions can see
  front-loaded premium spend or control-plane failures.

This supports a deeper prompt study: rerun the same decision contexts with
different memory policies, then compare Flash/Opus mix and stated decision
state. The replay script defaults to sequential per-job history replay; use
`--history-turns 0` for the older independent-decision mode.

## Next Instrumentation

Add a router decision audit payload for every smart-router decision:

- `prompt_id`
- `prompt_sha`
- `menu`
- `phase`
- `message_count`
- `input_token_estimate`
- `selected_model`
- `decision_reason`
- `fallback_reason`, when used
- `decision_status`
- `decision_latency_ms`

Optional exact-replay fields, gated behind an explicit experiment flag:

- redacted latest user message preview
- redacted system prompt preview
- compact decision prompt text

By default, avoid logging full private prompts. For research-only runs, store full decision prompts in the artifact directory rather than general gateway logs.

## Prompt Variant Study

Use a small fixed benchmark set and compare prompt variants by `prompt_id`.

Primary metrics:

- final reward / completion
- total upstream cost
- decision-model cost
- Opus selection rate
- Flash selection rate
- provider failure rate
- unmatched decision/agent trace count

Decision-quality metrics:

- expensive-success: Opus choices inside passed trials
- expensive-failure: Opus choices inside failed trials
- cheap-success: Flash choices inside passed trials
- cheap-risk: Flash choices followed by failure or repeated repair loops
- late-upgrade rate: Opus choices after deep conversation turns
- finalization model choice: model used near completion

Recommended variants:

- `v4-current-gpt5.6-router`: current quality-per-dollar prompt.
- `cost-strict`: require stronger evidence before choosing Opus.
- `quality-guarded`: use Opus for implementation, debugging, and final answer checks more aggressively.
- `phase-budgeted`: route by phase with explicit Opus budget per trial.

## Workflow

1. Run or reuse benchmark artifacts.
2. Build `router-decisions.json` and `router-decision-lab.html`.
3. Change router prompt and set a new `prompt_id`.
4. Rerun the same task set.
5. Build another decision dataset.
6. Compare datasets by `prompt_id`.

The HTML lab is intentionally static so it can be attached to each artifact bundle and opened without a backend.
