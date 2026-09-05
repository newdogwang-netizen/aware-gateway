#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

MODE="${1:-all}"
EXP_ROOT="${AWARE_V4_EXP_ROOT:-/mnt/data2/aware-gateway-runs}"
GATEWAY_DATA_ROOT="${AWARE_V4_GATEWAY_DATA_ROOT:-/mnt/data2/aware-gateway-data}"
GW_HOST="${GW_HOST:-172.17.0.1}"
GW_BASE="${GW_BASE:-http://$GW_HOST:12026/v1}"
HARBOR_LLM_ATTEMPTS="${AWARE_HARBOR_LLM_ATTEMPTS:-1}"
TIMEOUT_MULTIPLIER="${AWARE_V4_TIMEOUT_MULTIPLIER:-0.3}"
HARD_BUDGET_USD="${AWARE_V4_HARD_BUDGET_USD:-250}"
MONITOR_INTERVAL_SECONDS="${AWARE_V4_MONITOR_INTERVAL_SECONDS:-10}"
JOB_MAX_SECONDS="${AWARE_V4_JOB_MAX_SECONDS:-10800}"
CANARY_MAX_SECONDS="${AWARE_V4_CANARY_MAX_SECONDS:-900}"
CANARY_TASK="${AWARE_V4_CANARY_TASK:-shadow-relay}"
PILOT_TASK="${AWARE_V4_PILOT_TASK:-shadow-relay}"
PILOT_STRATEGY="${AWARE_V4_PILOT_STRATEGY:-smart-router}"
PILOT_ATTEMPT="${AWARE_V4_PILOT_ATTEMPT:-1}"
EARLY_FAIL_ON_AGENT_5XX="${AWARE_V4_EARLY_FAIL_ON_AGENT_5XX:-1}"
EARLY_FAIL_MIN_AGENT_5XX="${AWARE_V4_EARLY_FAIL_MIN_AGENT_5XX:-2}"
DATA_MIN_GB="${AWARE_V4_DATA_MIN_GB:-40}"
DOCKER_WARMSTART_MIN_GB="${AWARE_V4_DOCKER_WARMSTART_MIN_GB:-30}"
RUN_MIN_GB="${AWARE_V4_RUN_MIN_GB:-20}"
AGENT_LLM_CALL_KWARGS_JSON="${AWARE_V4_LLM_CALL_KWARGS_JSON:-{\"max_tokens\":32768,\"timeout\":900,\"num_retries\":0}}"
AGENT_MODEL_INFO_JSON="${AWARE_V4_MODEL_INFO_JSON:-{\"max_input_tokens\":1000000,\"max_output_tokens\":32768,\"input_cost_per_token\":0,\"output_cost_per_token\":0}}"
FAIL_ON_OUTPUT_TRUNCATION="${AWARE_V4_FAIL_ON_OUTPUT_TRUNCATION:-1}"
DEFAULT_TASKS_CSV="shadow-relay,vpp-loss-divergence"
AWARE_V4_TASKS="${AWARE_V4_TASKS:-$DEFAULT_TASKS_CSV}"
export AWARE_V4_TASKS

usage() {
  cat <<'EOF'
Usage: scripts/run_v4_matrix.sh [all|prepare|plans|canary|pilot|matrix|summary] [artifact_dir]

Modes:
  all      create/prepare, run canaries, then run the V4 formal core
  prepare  create/prepare artifact dir, build image, start gateway, generate plans
  plans    regenerate run-order CSV files in the current artifact dir
  canary   run direct gateway canary and Harbor attribution canary
  pilot    run one isolated non-core trial, controlled by AWARE_V4_PILOT_*
  matrix   run serial rows, then warm-start rows in two task lanes
  summary  aggregate existing per-job analysis files

Environment:
  AWARE_V4_TASKS              comma-separated two-task core matrix
                              (default: shadow-relay,vpp-loss-divergence)
  AWARE_V4_CANARY_TASK        Harbor attribution canary task
                              (default: shadow-relay)
  AWARE_V4_CANARY_MAX_SECONDS wall-clock cap for canary jobs (default: 900)
  AWARE_V4_JOB_MAX_SECONDS    wall-clock cap for formal jobs (default: 10800)
  AWARE_V4_PILOT_TASK         pilot task (default: shadow-relay)
  AWARE_V4_PILOT_STRATEGY     smart-router, smart-router-warmstart, or all-premium
                              (default: smart-router)
  AWARE_V4_PILOT_ATTEMPT      pilot attempt label (default: 1)
EOF
}

die() {
  echo "error: $*" >&2
  exit 1
}

log() {
  printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

free_gb() {
  df -Pk "$1" | awk 'NR == 2 { printf "%.1f", $4 / 1024 / 1024 }'
}

line_count() {
  wc -l < "$1" | tr -d ' '
}

require_free_gb() {
  local path="$1"
  local min_gb="$2"
  local label="$3"
  local got
  got="$(free_gb "$path")"
  awk -v got="$got" -v min="$min_gb" -v label="$label" '
    BEGIN {
      if (got + 0 < min + 0) {
        printf "error: %s has %.1fGB free; need at least %.1fGB\n", label, got, min > "/dev/stderr"
        exit 1
      }
    }'
  log "$label free ${got}GB >= ${min_gb}GB"
}

docker_root_dir() {
  docker info --format '{{.DockerRootDir}}'
}

ensure_tools() {
  command -v harbor >/dev/null || die "harbor CLI not found"
  command -v jq >/dev/null || die "jq not found"
  command -v docker >/dev/null || die "docker not found"
  command -v python3 >/dev/null || die "python3 not found"
  command -v curl >/dev/null || die "curl not found"
}

resolve_artifact_dir() {
  if [ -n "${AWARE_V4_ARTIFACT_DIR:-}" ]; then
    ARTIFACT_DIR="$AWARE_V4_ARTIFACT_DIR"
  elif [ "${2:-}" != "" ]; then
    ARTIFACT_DIR="$2"
  elif [ "$MODE" = "prepare" ] || [ "$MODE" = "all" ]; then
    EXP_ID="aware-v4-$(date -u +%Y%m%dT%H%M%SZ)"
    ARTIFACT_DIR="$EXP_ROOT/$EXP_ID"
  elif [ -s .aware-v4-current-artifact ]; then
    ARTIFACT_DIR="$(cat .aware-v4-current-artifact)"
  else
    die "artifact dir not provided and .aware-v4-current-artifact is missing"
  fi

  EXP_ID="$(basename "$ARTIFACT_DIR")"
  GATEWAY_DATA_DIR="${AWARE_V4_GATEWAY_DATA_DIR:-$GATEWAY_DATA_ROOT/$EXP_ID}"
  RUN_ORDER_SERIAL="$ARTIFACT_DIR/run-order-serial.csv"
  RUN_ORDER_WARMSTART="$ARTIFACT_DIR/run-order-warmstart-parallel.csv"
  RUN_ORDER_ALL="$ARTIFACT_DIR/run-order.csv"
  FORMAL_GLOB="$EXP_ID-*-a*"
}

init_artifact_dir() {
  mkdir -p "$ARTIFACT_DIR" "$ARTIFACT_DIR/jobs" "$ARTIFACT_DIR/logs" "$GATEWAY_DATA_DIR"
  printf '%s\n' "$EXP_ID" > "$ARTIFACT_DIR/EXP_ID.txt"
  printf '%s\n' "$ARTIFACT_DIR" > .aware-v4-current-artifact
  printf '%s\n' "$GATEWAY_DATA_DIR" > "$ARTIFACT_DIR/GATEWAY_DATA_DIR.txt"
  printf '%s\n' "$GW_HOST" > "$ARTIFACT_DIR/GW_HOST.txt"
  printf '%s\n' "$GW_BASE" > "$ARTIFACT_DIR/GW_BASE.txt"
}

check_control_plane() {
  test -s openrouter.env || die "openrouter.env is missing or empty"
  curl -sf -m 10 https://openrouter.ai/api/v1/models \
    -H "Authorization: Bearer $(cat openrouter.env)" |
    jq -e '.data[]? | select(.id == "openai/gpt-5.6-sol")' >/dev/null ||
    die "OpenRouter decision model openai/gpt-5.6-sol is not reachable"
}

snapshot_state() {
  git rev-parse HEAD > "$ARTIFACT_DIR/git-sha.txt"
  git diff --stat > "$ARTIFACT_DIR/git-diff-stat.txt"
  git diff > "$ARTIFACT_DIR/git-diff.patch"
  cp configs/gateway-openrouter.yaml "$ARTIFACT_DIR/gateway-openrouter.yaml"

  find terminal-bench -maxdepth 2 -type f \
    \( -name task.toml -o -name instruction.md -o -name README.md \) -print0 \
    | sort -z | xargs -0 sha256sum \
    > "$ARTIFACT_DIR/terminal-bench-task-files.sha256"

  docker info --format '{{.DockerRootDir}}' > "$ARTIFACT_DIR/docker-root-dir.txt"
  docker system df > "$ARTIFACT_DIR/docker-system-df-before.txt"
  df -h / /mnt/data2 > "$ARTIFACT_DIR/df-before.txt"
}

build_gateway_image() {
  log "building aware-gateway:latest"
  make docker > "$ARTIFACT_DIR/logs/make-docker.log" 2>&1
  docker image inspect aware-gateway:latest > "$ARTIFACT_DIR/docker-image.json"
}

start_gateway() {
  log "starting gateway with /data on $GATEWAY_DATA_DIR"
  docker rm -f aware-gateway >/dev/null 2>&1 || true
  docker run -d \
    --name aware-gateway \
    --network host \
    -v "$GATEWAY_DATA_DIR:/data" \
    -e GW_OPENROUTER_KEY="$(cat openrouter.env)" \
    aware-gateway:latest \
    > "$ARTIFACT_DIR/gateway-container-id.txt"

  for _ in $(seq 1 30); do
    if curl -sf http://localhost:12026/health > "$ARTIFACT_DIR/gateway-health.json"; then
      curl -sf http://localhost:12026/v1/models > "$ARTIFACT_DIR/openrouter-models.json"
      log "gateway healthy"
      return
    fi
    sleep 2
  done

  docker logs aware-gateway > "$ARTIFACT_DIR/logs/gateway-start-failure.log" 2>&1 || true
  die "gateway failed health check"
}

generate_run_plans() {
  python3 - <<'PY'
import csv
import os
import random
from pathlib import Path

seed = 20260902
tasks_csv = os.environ.get("AWARE_V4_TASKS", "shadow-relay,vpp-loss-divergence")
tasks = [task.strip() for task in tasks_csv.split(",") if task.strip()]
if len(tasks) != 2:
    raise SystemExit(f"AWARE_V4_TASKS must contain exactly two task names, got {tasks!r}")
serial_strategies = [
    ("all-premium", "anthropic/claude-opus-5", "openai/anthropic/claude-opus-5"),
    ("smart-router", "auto", "openai/auto"),
]
warmstart_strategy = ("smart-router-warmstart", "auto-opus-warmstart", "openai/auto-opus-warmstart")

random.seed(seed)
artifact_dir = Path(os.environ["ARTIFACT_DIR"])
serial_rows = []
warmstart_rows = []

for task in tasks:
    block = [(task, 1, *strategy) for strategy in serial_strategies]
    random.shuffle(block)
    serial_rows.extend(block)

for attempt in (2,):
    shuffled = tasks[:]
    random.shuffle(shuffled)
    for task in shuffled:
        serial_rows.append((task, attempt, "smart-router", "auto", "openai/auto"))

for task in tasks:
    for attempt in (1, 2):
        warmstart_rows.append((task, attempt, *warmstart_strategy, task))

def write(path, header, rows):
    with path.open("w", newline="") as f:
        writer = csv.writer(f, lineterminator="\n")
        writer.writerow(header)
        writer.writerows(rows)

write(
    artifact_dir / "run-order-serial.csv",
    ["task", "attempt", "strategy", "gateway_model", "harbor_model"],
    serial_rows,
)
write(
    artifact_dir / "run-order-warmstart-parallel.csv",
    ["task", "attempt", "strategy", "gateway_model", "harbor_model", "lane"],
    warmstart_rows,
)
write(
    artifact_dir / "run-order.csv",
    ["task", "attempt", "strategy", "gateway_model", "harbor_model"],
    [row[:5] for row in serial_rows] + [row[:5] for row in warmstart_rows],
)
PY

  echo 20260902 > "$ARTIFACT_DIR/random-seed.txt"
  wc -l "$RUN_ORDER_SERIAL" "$RUN_ORDER_WARMSTART" "$RUN_ORDER_ALL" \
    > "$ARTIFACT_DIR/run-order-line-count.txt"

  [ "$(line_count "$RUN_ORDER_SERIAL")" = "7" ] ||
    die "serial run-order line count is not 7 including header"
  [ "$(line_count "$RUN_ORDER_WARMSTART")" = "5" ] ||
    die "warm-start run-order line count is not 5 including header"
  [ "$(line_count "$RUN_ORDER_ALL")" = "11" ] ||
    die "combined run-order line count is not 11 including header"
}

fetch_traces() {
  local path="$1"
  sleep 6
  curl -sf "http://localhost:12026/v1/traces?limit=100000" > "$path"
}

run_direct_canary() {
  local trial="$EXP_ID-direct-canary"
  local session="${trial}__agent"
  log "running direct gateway canary"
  curl -sf http://localhost:12026/v1/chat/completions \
    -H "Content-Type: application/json" \
    -H "X-Trial-Name: $trial" \
    -H "X-Session-ID: $session" \
    -H "X-Task-Name: direct-canary" \
    -d '{
      "model": "auto",
      "messages": [
        {"role": "system", "content": "You are a canary request for routing attribution."},
        {"role": "user", "content": "Reply with the exact text: canary ok"}
      ],
      "max_tokens": 20,
      "temperature": 0
    }' > "$ARTIFACT_DIR/direct-canary-response.json"

  fetch_traces "$ARTIFACT_DIR/direct-canary-traces.json"
  jq -e --arg session "$session" '
    [.traces[]? | select(.session_id == $session)] as $traces
    | ($traces | length) >= 2
    and any($traces[]; ((.pool // "") == "decision-model") or ((.step_name // "") | startswith("router-decision")))
    and any($traces[]; (.routed_model // "") == "z-ai/glm-5.3-flash" or (.routed_model // "") == "anthropic/claude-opus-5")
  ' "$ARTIFACT_DIR/direct-canary-traces.json" >/dev/null ||
    die "direct gateway canary did not produce expected agent and router-decision traces"
}

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

  local failure_count
  failure_count="$(
    jq '[.traces[]? | select((.pool // "") != "decision-model") | select((((.status // 0) | tonumber?) // 0) >= 500)] | length' "$tmp"
  )"

  if [ "$failure_count" -ge "$EARLY_FAIL_MIN_AGENT_5XX" ]; then
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

write_wall_clock_cap_marker() {
  local job="$1"
  local task="$2"
  local attempt="$3"
  local strategy="$4"
  local gateway_model="$5"
  local harbor_model="$6"
  local session_id="$7"
  local trial_name="$8"
  local started_at="$9"
  local elapsed_seconds="${10}"
  local max_seconds="${11}"
  local marker="$ARTIFACT_DIR/jobs/$job/wall-clock-cap.json"

  mkdir -p "$ARTIFACT_DIR/jobs/$job"
  jq -n \
    --arg job "$job" \
    --arg task "$task" \
    --arg attempt "$attempt" \
    --arg strategy "$strategy" \
    --arg gateway_model "$gateway_model" \
    --arg harbor_model "$harbor_model" \
    --arg session_id "$session_id" \
    --arg trial_name "$trial_name" \
    --arg started_at "$started_at" \
    --arg detected_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson elapsed_seconds "$elapsed_seconds" \
    --argjson max_seconds "$max_seconds" \
    '{
      job: $job,
      task: $task,
      attempt: $attempt,
      strategy: $strategy,
      gateway_model: $gateway_model,
      harbor_model: $harbor_model,
      session_id: $session_id,
      trial_name: $trial_name,
      started_at: $started_at,
      detected_at: $detected_at,
      elapsed_seconds: $elapsed_seconds,
      max_seconds: $max_seconds,
      failure_kind: "wall_clock_cap"
    }' > "$marker"
}

check_runtime_disk() {
  local docker_root
  docker_root="$(docker_root_dir)"
  require_free_gb /mnt/data2 "$RUN_MIN_GB" "data disk during run"
  require_free_gb "$docker_root" "$RUN_MIN_GB" "Docker root during run"
}

run_harbor_job() {
  local job="$1"
  local task="$2"
  local attempt="$3"
  local strategy="$4"
  local gateway_model="$5"
  local harbor_model="$6"
  local log_path="$ARTIFACT_DIR/logs/${job}.log"

  if [ -s "$ARTIFACT_DIR/${job}-analysis.csv" ]; then
    log "skip completed $job"
    return
  fi

  log "run $job model=$harbor_model"
  : > "$log_path"
  local job_started_at
  job_started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  AWARE_HARBOR_LLM_ATTEMPTS="$HARBOR_LLM_ATTEMPTS" harbor run \
    --job-name "$job" \
    --jobs-dir "$ARTIFACT_DIR/jobs" \
    --agent terminus-2 \
    --model "$harbor_model" \
    --path terminal-bench \
    --include-task-name "$task" \
    --n-attempts 1 \
    --n-concurrent 1 \
    --timeout-multiplier "$TIMEOUT_MULTIPLIER" \
    --allow-agent-host "$GW_HOST" \
    --allow-environment-host "$GW_HOST" \
    --agent-kwarg "api_base=$GW_BASE" \
    --agent-kwarg "model_info=$AGENT_MODEL_INFO_JSON" \
    --agent-kwarg "llm_call_kwargs=$AGENT_LLM_CALL_KWARGS_JSON" \
    --agent-env "OPENAI_API_KEY=dummy" \
    --agent-env "AWARE_HARBOR_LLM_ATTEMPTS=$HARBOR_LLM_ATTEMPTS" \
    --yes > "$log_path" 2>&1 &

  local harbor_pid=$!
  local harbor_rc=0
  local early_failed=0
  local wall_clock_capped=0
  local job_started_epoch
  local max_seconds
  job_started_epoch="$(date -u +%s)"
  if [ "$attempt" = "canary" ] || [[ "$strategy" == *canary* ]]; then
    max_seconds="$CANARY_MAX_SECONDS"
  else
    max_seconds="$JOB_MAX_SECONDS"
  fi

  while kill -0 "$harbor_pid" 2>/dev/null; do
    if [ "${max_seconds:-0}" -gt 0 ]; then
      local now_epoch
      local elapsed_seconds
      now_epoch="$(date -u +%s)"
      elapsed_seconds=$((now_epoch - job_started_epoch))
      if [ "$elapsed_seconds" -ge "$max_seconds" ]; then
        log "wall-clock cap reached for $job after ${elapsed_seconds}s; interrupting Harbor"
        fetch_traces "$ARTIFACT_DIR/traces-wall-clock-cap-${job}.json" || true
        local cap_trial_dir
        local cap_trial_name
        local cap_session_id
        cap_trial_dir="$(find_trial_dir "$job" || true)"
        cap_trial_name=""
        cap_session_id=""
        if [ -n "$cap_trial_dir" ]; then
          cap_trial_name="$(basename "$cap_trial_dir")"
          cap_session_id="${cap_trial_name}__agent"
        fi
        write_wall_clock_cap_marker \
          "$job" "$task" "$attempt" "$strategy" "$gateway_model" "$harbor_model" \
          "$cap_session_id" "$cap_trial_name" "$job_started_at" "$elapsed_seconds" "$max_seconds"
        kill -INT "$harbor_pid" 2>/dev/null || true
        sleep 20
        if kill -0 "$harbor_pid" 2>/dev/null; then
          kill -TERM "$harbor_pid" 2>/dev/null || true
          sleep 10
        fi
        if kill -0 "$harbor_pid" 2>/dev/null; then
          kill -KILL "$harbor_pid" 2>/dev/null || true
        fi
        wait "$harbor_pid" 2>/dev/null || true
        wall_clock_capped=1
        harbor_rc=124
        break
      fi
    fi

    if [ "$EARLY_FAIL_ON_AGENT_5XX" = "1" ]; then
      local trial_dir
      trial_dir="$(find_trial_dir "$job" || true)"
      if [ -n "$trial_dir" ]; then
        local session_id
        session_id="$(basename "$trial_dir")__agent"
        if session_has_agent_5xx "$session_id" "$job"; then
          log "provider 5xx detected for $job session=$session_id; interrupting Harbor"
          write_early_failure_marker "$job" "$task" "$attempt" "$strategy" "$gateway_model" "$harbor_model" "$session_id"
          early_failed=1
          kill -INT "$harbor_pid" 2>/dev/null || true
          sleep 20
          if kill -0 "$harbor_pid" 2>/dev/null; then
            kill -TERM "$harbor_pid" 2>/dev/null || true
          fi
          break
        fi
      fi
    fi
    sleep "$MONITOR_INTERVAL_SECONDS"
  done

  if [ "$wall_clock_capped" != "1" ]; then
    set +e
    wait "$harbor_pid"
    harbor_rc=$?
    set -e
  fi

  if [ "$wall_clock_capped" = "1" ]; then
    log "recorded wall-clock cap for $job"
  elif [ "$early_failed" = "1" ]; then
    log "recorded early provider failure for $job"
  elif [ "$harbor_rc" -ne 0 ]; then
    log "harbor exited rc=$harbor_rc for $job; attempting strict aggregation"
  fi

  if [ "$FAIL_ON_OUTPUT_TRUNCATION" = "1" ] &&
    grep -Eq 'Output length exceeded|hit max_tokens limit' "$log_path"; then
    fetch_traces "$ARTIFACT_DIR/traces-truncated-${job}.json"
    die "agent output truncation detected for $job; increase AWARE_V4_LLM_CALL_KWARGS_JSON/AWARE_V4_MODEL_INFO_JSON before formal run"
  fi

  fetch_traces "$ARTIFACT_DIR/traces-after-${job}.json"
  python3 scripts/analyze_v3_results.py \
    --artifact-dir "$ARTIFACT_DIR" \
    --gateway-config configs/gateway-openrouter.yaml \
    --traces-json "$ARTIFACT_DIR/traces-after-${job}.json" \
    --job-glob "$job" \
    --expected-rows 1 \
    --strict \
    --output "$ARTIFACT_DIR/${job}-analysis.csv"

  check_progress_gates
  check_runtime_disk
}

check_progress_gates() {
  python3 - "$ARTIFACT_DIR" "$FORMAL_GLOB" "$HARD_BUDGET_USD" "$JOB_MAX_SECONDS" <<'PY'
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
max_duration = max([float(row.get("duration_seconds") or 0) for row in rows] or [0])
print(
    f"formal progress rows={len(rows)} total_cost_usd={total_cost:.4f} "
    f"smart_router_decision_fallback_trials={smart_router_fallback_trials} "
    f"max_duration_seconds={max_duration:.1f}"
)
if total_cost > hard_budget:
    raise SystemExit(f"hard budget exceeded: {total_cost:.4f} > {hard_budget:.4f}")
if smart_router_fallback_trials > 1:
    raise SystemExit(
        f"decision fallback pause gate exceeded: {smart_router_fallback_trials} smart-router trials"
    )
if max_duration >= float(sys.argv[4]):
    raise SystemExit(f"trial wall-clock pause gate reached: {max_duration:.1f}s")
PY
}

run_harbor_canary() {
  local job="$EXP_ID-canary-smart-router-$CANARY_TASK"
  run_harbor_job "$job" "$CANARY_TASK" "canary" "smart-router-canary" "auto" "openai/auto"
  mv "$ARTIFACT_DIR/${job}-analysis.csv" "$ARTIFACT_DIR/harbor-canary-analysis.csv"
  python3 - "$ARTIFACT_DIR/harbor-canary-analysis.csv" <<'PY'
import csv
import sys
from pathlib import Path

path = Path(sys.argv[1])
rows = list(csv.DictReader(path.open(newline="")))
if len(rows) != 1:
    raise SystemExit(f"harbor canary expected 1 row, got {len(rows)}")
row = rows[0]
if not row.get("trace_key"):
    raise SystemExit("harbor canary has no trace_key")
if row.get("failure_kind") in {"provider_5xx", "exception", "unknown"}:
    raise SystemExit(f"harbor canary failed: {row.get('failure_kind')}")
decision_fallback = float(row.get("fallback_to_opus_rate") or row.get("opus_fallback_rate") or 0)
if decision_fallback > 0:
    raise SystemExit(f"harbor canary used decision fallback: {decision_fallback}")
print(
    "harbor canary ok "
    f"reward={row.get('reward')} cost={row.get('total_cost_usd')} "
    f"agent_calls={row.get('agent_call_count')}"
)
PY
}

run_serial_matrix() {
  test -f "$RUN_ORDER_SERIAL" || die "missing $RUN_ORDER_SERIAL"
  while IFS=, read -r task attempt strategy gateway_model harbor_model; do
    task="${task%$'\r'}"
    attempt="${attempt%$'\r'}"
    strategy="${strategy%$'\r'}"
    gateway_model="${gateway_model%$'\r'}"
    harbor_model="${harbor_model%$'\r'}"
    local job="$EXP_ID-${strategy}-${task}-a${attempt}"
    run_harbor_job "$job" "$task" "$attempt" "$strategy" "$gateway_model" "$harbor_model"
  done < <(tail -n +2 "$RUN_ORDER_SERIAL")
}

run_warmstart_lane() {
  local lane="$1"
  awk -F, -v lane="$lane" 'NR == 1 || $6 == lane { print }' "$RUN_ORDER_WARMSTART" |
  tail -n +2 |
  while IFS=, read -r task attempt strategy gateway_model harbor_model lane_name; do
    task="${task%$'\r'}"
    attempt="${attempt%$'\r'}"
    strategy="${strategy%$'\r'}"
    gateway_model="${gateway_model%$'\r'}"
    harbor_model="${harbor_model%$'\r'}"
    lane_name="${lane_name%$'\r'}"
    local job="$EXP_ID-${strategy}-${task}-a${attempt}"
    run_harbor_job "$job" "$task" "$attempt" "$strategy" "$gateway_model" "$harbor_model"
  done
}

run_warmstart_matrix() {
  test -f "$RUN_ORDER_WARMSTART" || die "missing $RUN_ORDER_WARMSTART"
  local docker_root
  docker_root="$(docker_root_dir)"
  require_free_gb "$docker_root" "$DOCKER_WARMSTART_MIN_GB" "Docker root before warm-start parallel run"

  mapfile -t lanes < <(tail -n +2 "$RUN_ORDER_WARMSTART" | cut -d, -f6 | sort -u)
  local pids=()
  for lane in "${lanes[@]}"; do
    log "starting warm-start lane $lane"
    run_warmstart_lane "$lane" > "$ARTIFACT_DIR/logs/warmstart-lane-${lane}.log" 2>&1 &
    pids+=("$!")
  done

  local rc=0
  for pid in "${pids[@]}"; do
    if ! wait "$pid"; then
      rc=1
    fi
  done
  return "$rc"
}

strategy_models() {
  local strategy="$1"
  case "$strategy" in
    all-premium)
      printf '%s,%s\n' "anthropic/claude-opus-5" "openai/anthropic/claude-opus-5"
      ;;
    smart-router)
      printf '%s,%s\n' "auto" "openai/auto"
      ;;
    smart-router-warmstart)
      printf '%s,%s\n' "auto-opus-warmstart" "openai/auto-opus-warmstart"
      ;;
    *)
      die "unknown pilot strategy: $strategy"
      ;;
  esac
}

run_pilot() {
  ensure_tools
  check_control_plane
  curl -sf http://localhost:12026/health > "$ARTIFACT_DIR/gateway-health-pilot.json" ||
    die "gateway is not reachable at localhost:12026; run prepare first"

  local model_pair
  local gateway_model
  local harbor_model
  model_pair="$(strategy_models "$PILOT_STRATEGY")"
  gateway_model="${model_pair%%,*}"
  harbor_model="${model_pair#*,}"

  local job="pilot-$EXP_ID-${PILOT_STRATEGY}-${PILOT_TASK}-a${PILOT_ATTEMPT}"
  log "pilot task=$PILOT_TASK strategy=$PILOT_STRATEGY attempt=$PILOT_ATTEMPT job=$job"
  run_harbor_job "$job" "$PILOT_TASK" "$PILOT_ATTEMPT" "$PILOT_STRATEGY" "$gateway_model" "$harbor_model"
}

write_summary() {
  python3 - "$ARTIFACT_DIR" "$FORMAL_GLOB" <<'PY'
import csv
import glob
import json
import sys
from collections import defaultdict
from pathlib import Path

artifact_dir = Path(sys.argv[1])
job_glob = sys.argv[2]
analysis_paths = sorted(glob.glob(str(artifact_dir / f"{job_glob}-analysis.csv")))
rows = []
for path in analysis_paths:
    rows.extend(csv.DictReader(open(path, newline="")))

by_strategy = defaultdict(lambda: {"rows": 0, "passes": 0, "cost": 0.0, "calls": 0})
for row in rows:
    bucket = by_strategy[row.get("strategy") or ""]
    bucket["rows"] += 1
    bucket["passes"] += 1 if float(row.get("reward") or 0) >= 1 else 0
    bucket["cost"] += float(row.get("total_cost_usd") or 0)
    bucket["calls"] += int(float(row.get("agent_call_count") or 0))

summary = {
    "rows": len(rows),
    "total_cost_usd": round(sum(float(row.get("total_cost_usd") or 0) for row in rows), 8),
    "passes": sum(1 for row in rows if float(row.get("reward") or 0) >= 1),
    "by_strategy": {
        k: {
            "rows": v["rows"],
            "passes": v["passes"],
            "cost_usd": round(v["cost"], 8),
            "agent_calls": v["calls"],
        }
        for k, v in sorted(by_strategy.items())
    },
}
print(json.dumps(summary, indent=2, sort_keys=True))
(artifact_dir / "summary.json").write_text(json.dumps(summary, indent=2, sort_keys=True) + "\n")
PY
}

prepare() {
  init_artifact_dir
  ensure_tools
  check_control_plane
  require_free_gb /mnt/data2 "$DATA_MIN_GB" "data disk before run"
  snapshot_state
  build_gateway_image
  start_gateway
  generate_run_plans
}

run_canaries() {
  run_direct_canary
  run_harbor_canary
  printf 'canary ok %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$ARTIFACT_DIR/canary.ok"
}

run_matrix() {
  if [ "${AWARE_V4_ALLOW_MATRIX_AFTER_CANARY_CAP:-0}" != "1" ]; then
    test -e "$ARTIFACT_DIR/canary.ok" ||
      die "canary.ok is missing; rerun canary successfully or set AWARE_V4_ALLOW_MATRIX_AFTER_CANARY_CAP=1 for an explicit full-matrix override"
  fi
  run_serial_matrix
  run_warmstart_matrix
  write_summary
}

case "$MODE" in
  all)
    resolve_artifact_dir "$@"
    export ARTIFACT_DIR
    prepare
    run_canaries
    run_matrix
    ;;
  prepare)
    resolve_artifact_dir "$@"
    export ARTIFACT_DIR
    prepare
    ;;
  plans)
    resolve_artifact_dir "$@"
    export ARTIFACT_DIR
    init_artifact_dir
    generate_run_plans
    ;;
  canary)
    resolve_artifact_dir "$@"
    export ARTIFACT_DIR
    init_artifact_dir
    run_canaries
    ;;
  pilot)
    resolve_artifact_dir "$@"
    export ARTIFACT_DIR
    init_artifact_dir
    run_pilot
    ;;
  matrix)
    resolve_artifact_dir "$@"
    export ARTIFACT_DIR
    init_artifact_dir
    run_matrix
    ;;
  summary)
    resolve_artifact_dir "$@"
    export ARTIFACT_DIR
    write_summary
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    usage
    die "unknown mode: $MODE"
    ;;
esac
