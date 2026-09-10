# RSI R1 Outcome Contracts

This directory pins the first auditable contract for router self-improvement.

## Files

- `event-schema-v1.json` defines the per-episode event shape emitted by the offline extractor.
- `progress-rules-v1.yaml` defines normalized LLM outcomes, progress vs activity, no-progress thresholds, replay cutoff rules, and cost accounting.
- `candidate-manifest.template.json` records one candidate policy change, its evidence, expected effect, rollback conditions, and gates.

## Extractor

Generate the R1 artifacts from a Harbor trial directory and gateway trace JSON:

```bash
python3 scripts/extract_episode_outcomes.py \
  --trial-dir /path/to/harbor/trial-or-job-dir \
  --traces-json /path/to/gateway-traces.json \
  --output-dir /path/to/rsi-output \
  --strict
```

The extractor reads both gateway traces and Harbor `trajectory.json`. Gateway
traces provide reliable LLM outcome metadata; Harbor trajectory records provide
the visible tool trail: shell calls, file writes, and local test or output
validation commands.

The command writes:

- `episode-events.jsonl`
- `episode-summary.json`
- `replay-cutoff-check.json`

`replay-cutoff-check.json` contains two related views. `samples` is strict
replay input and only includes evidence before each semantic decision.
`route_outcomes` is analysis-only: it links every routed agent call to events
observed before the next agent call, including file writes, tests, no-progress
events, and verifier results when they fall in that window. Replay prompts do
not receive `route_outcomes`.

`--strict` fails if required event fields are missing or if replay state includes evidence at or after the decision timestamp.

When gateway traces are unavailable, the extractor can fall back to Harbor
`trajectory.json` for basic `llm_call` events. Those events keep
`outcome=unknown` because trajectory messages do not carry reliable provider
finish metadata.

Projected event kinds now include:

- `llm_call`: gateway or trajectory model turn.
- `tool_call`: visible Harbor tool invocation.
- `file_written`: explicit shell or Python file write detected from the tool command.
- `test_run`: local test or output validation command with `passed`, `failed`, or `unknown` outcome.
- `file_modified`, `test_passed` / `test_failed`, `verifier_result`: final Harbor artifacts.
- `no_progress`: derived pressure signal.

## Online Ingestion

The gateway can ingest the same event shape during a live run:

```bash
curl -s http://localhost:12026/v1/episode-events \
  -H 'Content-Type: application/json' \
  -H 'X-Episode-ID: trial-abc__agent' \
  -d '{
    "kind": "test_run",
    "source": "local-runner",
    "observation": {"outcome": "passed", "command": "go test ./..."},
    "evidence_refs": ["stdout"]
  }'
```

Missing `schema_version`, `event_id`, `timestamp`, `certainty`, and
`extractor_version` fields are filled by the gateway. The audit SQLite store
writes these events to `episode_events`, queryable with
`GET /v1/episode-events?episode_id=...`. The smart-router also projects posted
events into its online episode state, so later decisions can distinguish
activity from progress before a full offline extraction pass. Explicit event
projection is idempotent by `event_id`; repeated sidecar retries do not advance
the in-memory state twice.

The current online projection is queryable:

```bash
curl -s 'http://localhost:12026/v1/episode-state?episode_id=trial-abc__agent'
```

This is a runtime inspection surface for the state the router is using; offline
replay still reads `episode-events.jsonl` and applies its own cutoff reducer.
When smart-router memory has no entry for the requested episode, the runtime
state path can backfill from persisted audit traces and explicit episode
events exposed by installed query plugins.

The session-to-episode stack is queryable separately:

```bash
curl -s 'http://localhost:12026/v1/episode-sessions?session_id=trial-abc__agent'
```

This tells the experiment runner which task line is active before it asks for
the reduced episode state. The returned stack can also be rebuilt best-effort
from persisted audit traces that include `session_id`, `episode_id`, and
`episode_operation`.

## Deterministic Runtime Probe

The local safe-control probe runs the compiled gateway against mock provider and
decision endpoints:

```bash
make build
python3 scripts/run_safe_control_probe.py \
  --repo . \
  --gateway-bin ./aware-gateway \
  --out-dir /tmp/aware-safe-control-probe
```

It verifies the Phase 2 control plane without real LLM spend. In addition to
the fixed safe-control matrix, it posts two explicit `llm_call` events with
`finish_reason=length`, checks `/v1/episode-state` for the reduced stale
no-progress state, retries a duplicate event id to prove projection
idempotency, and verifies that the next routed request locally selects
`premium_recover` with `rule_id=episode_no_progress_recovery` while freezing
the base recovery budget instead of compounding another length-based boost.
It also injects repeated failed test events and checks that the first
non-improving failure frontier triggers `episode_repeated_failure_recovery`,
while the same fingerprint is not repeatedly escalated by the local controller.
Finally, it injects delivery, passed validation, and verifier events, then
checks that final task-completion routing carries `completion_readiness` and
the supporting evidence in the guardrail trace. A second completion scenario
changes the target file after verifier success and verifies that readiness falls
back to `delivery_candidate` instead of reusing stale proof.
The probe also posts validation after a routed recovery call and checks that
the online route-outcome window links that event to the previous LLM route.
The same state now exposes a deterministic next minimum capability hint used by
the next router prompt, and hard recovery floors can override an underpowered
semantic decision when the verifier has already failed the current delivery.

For local runners, `scripts/run_episode_command.py` wraps a command and posts
the detected events automatically:

```bash
python3 scripts/run_episode_command.py \
  --gateway http://localhost:12026 \
  --episode-id trial-abc__agent \
  --session-id trial-abc__agent \
  --step-name validation \
  -- go test ./...
```

The wrapper preserves the command stdout/stderr and exits with the wrapped
command's status. It emits a `tool_call` event for every wrapped command,
adds `file_written` when shell/Python writes are detected, adds `test_run` for
test or validation commands, and adds `run_exception` on timeout or local
execution errors. `--dry-run --events-jsonl /tmp/events.jsonl` runs the same
classification without posting to the gateway.

For Harbor runs where commands are executed inside the benchmark agent,
`scripts/watch_harbor_episode_events.py` acts as a sidecar:

```bash
python3 scripts/watch_harbor_episode_events.py \
  --job-dir /path/to/harbor/jobs/job-name \
  --gateway http://localhost:12026 \
  --state-file /path/to/harbor/jobs/job-name/.episode-watcher-state.json
```

It polls `trajectory*.json`, `artifacts/tmp/agent.patch`, `verifier/ctrf.json`,
`verifier/reward.txt`, and `result.json`, posting each deterministic event id
once while the job is running. In the V4 runner this is opt-in:

```bash
AWARE_V4_EPISODE_WATCHER=1 scripts/run_v4_matrix.sh pilot
```

The reducer keeps two no-progress views. `no_progress_event_count` is historical
background. `no_progress_window.severity` is the current state used by P2 replay:
`none`, `watch`, `stale`, or `blocked`.

Online smart-router state uses the same distinction. `watch` is prompt context
only. `stale` and `blocked` are strong enough for the local safe-control layer
to skip the semantic judge and select `premium_recover`; the routing reason
records the episode state version and pressure evidence that triggered it.

## Replay

Replay router decisions with outcome state:

```bash
python3 scripts/replay_episode_decisions.py \
  --episode-dir /path/to/rsi-output-a \
  --episode-dir /path/to/rsi-output-b \
  --output /path/to/router-replay-rsi-p2.json \
  --prompt-id rsi-p2-windowed-progress-v1 \
  --model openai/gpt-5.6-sol \
  --resume
```

The replay output reports candidate model mix, budget-action mix, switch rate,
evidence-reference coverage, estimated decision-model cost, and future evidence
leakage. Replay is a screening gate only; it can reject unsafe or incoherent
policies, but it cannot prove benchmark quality without a real pilot.

## Policy Gate

After a matched baseline/candidate pilot, evaluate the candidate against final
episode summaries:

```bash
python3 scripts/evaluate_rsi_policy_gate.py \
  --baseline-summary /path/to/baseline-rsi-output \
  --candidate-summary /path/to/candidate-rsi-output \
  --candidate-replay /path/to/router-replay-rsi-p2.json \
  --manifest docs/rsi/candidate-manifest.template.json \
  --output /path/to/policy-gate.json
```

The gate groups summaries by task, compares only matched tasks, and reports
`accept`, `reject`, or `needs_more_data`. It rejects future-evidence leakage,
reward regressions, missing successes against a successful baseline, replay
evidence coverage below 100%, and candidates whose cost per success is not
lower than the matched baseline. If the matched task or run count is below the
manifest gate, it keeps the result as `needs_more_data` unless a rollback
condition has already fired.

To build those summaries from an existing V4 Harbor artifact directory:

```bash
python3 scripts/build_rsi_pilot_artifacts.py \
  --artifact-dir /path/to/aware-v4-run \
  --strategy all-premium,smart-router \
  --baseline-strategy all-premium \
  --candidate-strategy smart-router \
  --strict
```

This writes `rsi-episode-artifacts/`, including per-trial
`episode-events.jsonl`, `episode-summary.json`, `replay-cutoff-check.json`, a
manifest, and an optional policy-gate JSON. `episode-summary.total_cost_usd`
includes both agent call cost and decision-model cost; the split is preserved
as `agent_cost_usd` and `decision_cost_usd`.

## Boundary

`finish_reason=stop` is normalized to `response_completed`, which means one model response ended normally. It is not treated as benchmark task completion.

Activity such as reading files, searching, producing tokens, or repeatedly running the same command is not progress by itself. File writes are candidate progress only when they touch the workspace or `/app/output`. Local tests and output validations are stronger progress evidence; final verifier success remains the strongest evidence.
