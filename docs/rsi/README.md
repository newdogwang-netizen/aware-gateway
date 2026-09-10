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

## Boundary

`finish_reason=stop` is normalized to `response_completed`, which means one model response ended normally. It is not treated as benchmark task completion.

Activity such as reading files, searching, producing tokens, or repeatedly running the same command is not progress by itself. File writes are candidate progress only when they touch the workspace or `/app/output`. Local tests and output validations are stronger progress evidence; final verifier success remains the strongest evidence.
