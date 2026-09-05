#!/usr/bin/env bash
set -euo pipefail

ARTIFACT_DIR="${1:-$(cat .aware-v3-current-artifact)}"
EXP_ID="$(cat "$ARTIFACT_DIR/EXP_ID.txt")"
RUN_ORDER="$ARTIFACT_DIR/run-order.csv"
GW_HOST="${GW_HOST:-$(cat "$ARTIFACT_DIR/GW_HOST.txt" 2>/dev/null || printf '172.17.0.1')}"
GW_BASE="${GW_BASE:-http://$GW_HOST:12026/v1}"
HARD_BUDGET_USD="${V3_HARD_BUDGET_USD:-2000}"
FORMAL_GLOB="$EXP_ID-*-a*"
MONITOR_INTERVAL_SECONDS="${V3_MONITOR_INTERVAL_SECONDS:-10}"
EARLY_FAIL_ON_AGENT_5XX="${V3_EARLY_FAIL_ON_AGENT_5XX:-1}"
HARBOR_LLM_ATTEMPTS="${AWARE_HARBOR_LLM_ATTEMPTS:-1}"

if [ ! -f "$RUN_ORDER" ]; then
  echo "missing run order: $RUN_ORDER" >&2
  exit 2
fi

find_trial_dir() {
  local job="$1"
  find "$ARTIFACT_DIR/jobs/$job" -mindepth 1 -maxdepth 1 -type d 2>/dev/null |
    sort |
    head -n 1
}

session_has_agent_5xx() {
  local session_id="$1"
  local job="$2"
  local tmp="$ARTIFACT_DIR/.${job}.session-traces.tmp"

  curl -sf "http://localhost:12026/v1/traces?session_id=$session_id&limit=1000" > "$tmp" ||
    return 1

  if jq -e '.traces[]? | select((.pool // "") != "decision-model") | select((((.status // 0) | tonumber?) // 0) >= 500)' "$tmp" >/dev/null; then
    cp "$tmp" "$ARTIFACT_DIR/traces-early-${job}.json"
    return 0
  fi

  return 1
}

write_early_failure_marker() {
  local job="$1"
  local task="$2"
  local attempt="$3"
  local strategy="$4"
  local gateway_model="$5"
  local harbor_model="$6"
  local session_id="$7"
  local marker="$ARTIFACT_DIR/jobs/$job/early-provider-failure.json"
  local traces="$ARTIFACT_DIR/traces-early-${job}.json"

  mkdir -p "$ARTIFACT_DIR/jobs/$job"
  jq -n \
    --arg job "$job" \
    --arg task "$task" \
    --arg attempt "$attempt" \
    --arg strategy "$strategy" \
    --arg gateway_model "$gateway_model" \
    --arg harbor_model "$harbor_model" \
    --arg session_id "$session_id" \
    --arg detected_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --slurpfile traces "$traces" \
    '{
      job: $job,
      task: $task,
      attempt: $attempt,
      strategy: $strategy,
      gateway_model: $gateway_model,
      harbor_model: $harbor_model,
      session_id: $session_id,
      failure_kind: "provider_5xx",
      detected_at: $detected_at,
      first_error_trace: (
        $traces[0].traces
        | map(select((.pool // "") != "decision-model"))
        | map(select((((.status // 0) | tonumber?) // 0) >= 500))
        | first
      )
    }' > "$marker"
}

run_harbor_job() {
  local job="$1"
  local task="$2"
  local attempt="$3"
  local strategy="$4"
  local gateway_model="$5"
  local harbor_model="$6"
  local log="$ARTIFACT_DIR/${job}.log"

  : > "$log"
  tail -n +1 -F "$log" &
  local tail_pid=$!

  AWARE_HARBOR_LLM_ATTEMPTS="$HARBOR_LLM_ATTEMPTS" harbor run \
    --job-name "$job" \
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
	    --agent-env "AWARE_HARBOR_LLM_ATTEMPTS=$HARBOR_LLM_ATTEMPTS" \
	    --yes > "$log" 2>&1 &
  local harbor_pid=$!
  local harbor_rc=0
  local early_failed=0

  while kill -0 "$harbor_pid" 2>/dev/null; do
    if [ "$EARLY_FAIL_ON_AGENT_5XX" = "1" ]; then
      local trial_dir
      trial_dir="$(find_trial_dir "$job" || true)"
      if [ -n "$trial_dir" ]; then
        local session_id
        session_id="$(basename "$trial_dir")__agent"
        if session_has_agent_5xx "$session_id" "$job"; then
          echo "provider 5xx detected for $job session=$session_id; interrupting Harbor"
          write_early_failure_marker "$job" "$task" "$attempt" "$strategy" "$gateway_model" "$harbor_model" "$session_id"
          early_failed=1
          kill -INT "$harbor_pid" 2>/dev/null || true
          sleep 20
          if kill -0 "$harbor_pid" 2>/dev/null; then
            echo "Harbor still running after SIGINT for $job; sending SIGTERM"
            kill -TERM "$harbor_pid" 2>/dev/null || true
          fi
          break
        fi
      fi
    fi
    sleep "$MONITOR_INTERVAL_SECONDS"
  done

  set +e
  wait "$harbor_pid"
  harbor_rc=$?
  kill "$tail_pid" 2>/dev/null
  wait "$tail_pid" 2>/dev/null
  set -e

  if [ "$early_failed" = "1" ]; then
    echo "recorded early provider failure for $job"
  elif [ "$harbor_rc" -ne 0 ]; then
    echo "harbor exited with rc=$harbor_rc for $job; attempting strict aggregation"
  fi
}

mkdir -p "$ARTIFACT_DIR/jobs"
printf '%s\n' "$GW_HOST" > "$ARTIFACT_DIR/GW_HOST.txt"
printf '%s\n' "$GW_BASE" > "$ARTIFACT_DIR/GW_BASE.txt"

tail -n +2 "$RUN_ORDER" |
while IFS=, read -r task attempt strategy gateway_model harbor_model; do
  task="${task%$'\r'}"
  attempt="${attempt%$'\r'}"
  strategy="${strategy%$'\r'}"
  gateway_model="${gateway_model%$'\r'}"
  harbor_model="${harbor_model%$'\r'}"
  JOB="$EXP_ID-${strategy}-${task}-a${attempt}"
  ANALYSIS="$ARTIFACT_DIR/${JOB}-analysis.csv"

  if [ -s "$ANALYSIS" ]; then
    echo "skip completed $JOB"
  else
    echo "run $JOB model=$harbor_model"
    run_harbor_job "$JOB" "$task" "$attempt" "$strategy" "$gateway_model" "$harbor_model"

    sleep 5
    curl -sf "http://localhost:12026/v1/traces?limit=100000" \
      > "$ARTIFACT_DIR/traces-after-${JOB}.json"

    python3 scripts/analyze_v3_results.py \
      --artifact-dir "$ARTIFACT_DIR" \
      --gateway-config configs/gateway-openrouter.yaml \
      --traces-json "$ARTIFACT_DIR/traces-after-${JOB}.json" \
      --job-glob "$JOB" \
      --expected-rows 1 \
      --strict \
      --output "$ANALYSIS"
  fi

  python3 - "$ARTIFACT_DIR" "$FORMAL_GLOB" "$HARD_BUDGET_USD" <<'PY'
import csv
import glob
import sys
from pathlib import Path

artifact_dir = Path(sys.argv[1])
job_glob = sys.argv[2]
hard_budget = float(sys.argv[3])
analysis_paths = sorted(glob.glob(str(artifact_dir / f"{job_glob}-analysis.csv")))
rows = []
for path in analysis_paths:
    rows.extend(csv.DictReader(open(path, newline="")))

total_cost = sum(float(row.get("total_cost_usd") or 0) for row in rows)
smart_router_fallback_trials = sum(
    1
    for row in rows
    if row.get("strategy") in ("smart-router", "smart-router-warmstart")
    and float(row.get("fallback_to_opus_rate") or row.get("opus_fallback_rate") or 0) > 0
)
print(
    f"formal progress rows={len(rows)} total_cost_usd={total_cost:.4f} "
    f"smart_router_decision_fallback_trials={smart_router_fallback_trials}"
)
if total_cost > hard_budget:
    raise SystemExit(f"hard budget exceeded: {total_cost:.4f} > {hard_budget:.4f}")
if smart_router_fallback_trials > 1:
    raise SystemExit(
        f"decision fallback pause gate exceeded: {smart_router_fallback_trials} smart-router trials"
    )
PY
done
