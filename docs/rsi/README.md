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

The command writes:

- `episode-events.jsonl`
- `episode-summary.json`
- `replay-cutoff-check.json`

`--strict` fails if required event fields are missing or if replay state includes evidence at or after the decision timestamp.

When gateway traces are unavailable, the extractor can fall back to Harbor
`trajectory.json` for basic `llm_call` events. Those events keep
`outcome=unknown` because trajectory messages do not carry reliable provider
finish metadata.

## Boundary

`finish_reason=stop` is normalized to `response_completed`, which means one model response ended normally. It is not treated as benchmark task completion.

Activity such as reading files, searching, producing tokens, or repeatedly running the same command is not progress by itself. Progress must come from validation, verifier improvement, or a deterministic reducer over failure/frontier evidence.
