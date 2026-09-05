#!/usr/bin/env python3
from __future__ import annotations

import argparse
import csv
import html
import json
import re
from collections import Counter, defaultdict
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import quote


ANSI_RE = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]")
WS_RE = re.compile(r"\s+")

LEADERBOARD_JOB_URL = "https://hub.harborframework.com/jobs/a1ac63a1-8a9b-4bc7-9906-2b63657ee1c2"
DEFAULT_CONTEXT_TASKS = ("shadow-relay", "vpp-loss-divergence")
EXPECTED_CORE_ROWS = 10
SOFT_BUDGET_USD = 250.0

OPUS_LEADERBOARD_TRIALS = {
    "shadow-relay": [
        {
            "run": "shadow-relay__380ab45b",
            "trial_id": "e6711c01-5700-4f48-b587-028eb2384557",
            "reward": 1,
            "cost": 2.9384,
            "duration_min": 16.8,
            "prompt_tokens": 1445643,
            "output_tokens": 67292,
            "cache_tokens": 1352877,
        },
        {
            "run": "shadow-relay__df8050ac",
            "trial_id": "e5f6ce17-f9eb-497a-a89d-e9d6cae9673f",
            "reward": 1,
            "cost": 2.6540,
            "duration_min": 14.1,
            "prompt_tokens": 1385206,
            "output_tokens": 58583,
            "cache_tokens": 1298780,
        },
        {
            "run": "shadow-relay__f6bc0d35",
            "trial_id": "d0ffc894-0b1b-4326-a3d2-fa97af1be741",
            "reward": 1,
            "cost": 1.5831,
            "duration_min": 9.0,
            "prompt_tokens": 753933,
            "output_tokens": 35144,
            "cache_tokens": 696966,
        },
        {
            "run": "shadow-relay__382978d0",
            "trial_id": "61540a4a-9caf-4003-a65e-bc64512c2061",
            "reward": 1,
            "cost": 1.5748,
            "duration_min": 9.5,
            "prompt_tokens": 833922,
            "output_tokens": 33442,
            "cache_tokens": 777953,
        },
        {
            "run": "shadow-relay__ccb49925",
            "trial_id": "5da668ed-c68f-400b-b78a-31a34d45f971",
            "reward": 1,
            "cost": 2.0425,
            "duration_min": 11.4,
            "prompt_tokens": 957637,
            "output_tokens": 46189,
            "cache_tokens": 886510,
        },
    ],
    "vpp-loss-divergence": [
        {
            "run": "vpp-loss-divergence__d8ccae31",
            "trial_id": "d5f28e3e-3f91-44ad-8628-4fee3e407a16",
            "reward": 1,
            "cost": 7.4646,
            "duration_min": 31.0,
            "prompt_tokens": 7995769,
            "output_tokens": 93978,
            "cache_tokens": 7801422,
        },
        {
            "run": "vpp-loss-divergence__74b45e85",
            "trial_id": "bcec78ae-b6ed-4742-b400-c81cae505ca6",
            "reward": 1,
            "cost": 22.2297,
            "duration_min": 63.4,
            "prompt_tokens": 29420140,
            "output_tokens": 213706,
            "cache_tokens": 29041460,
        },
        {
            "run": "vpp-loss-divergence__474c9099",
            "trial_id": "a92275fa-65fe-4ab1-b4a2-dabe761dcbd1",
            "reward": 1,
            "cost": 12.2159,
            "duration_min": 38.1,
            "prompt_tokens": 15789211,
            "output_tokens": 114491,
            "cache_tokens": 15535425,
        },
        {
            "run": "vpp-loss-divergence__530bee5c",
            "trial_id": "810df63f-0dc8-4c21-b419-740149fe64fc",
            "reward": 1,
            "cost": 7.9182,
            "duration_min": 29.0,
            "prompt_tokens": 9319257,
            "output_tokens": 85259,
            "cache_tokens": 9123209,
        },
        {
            "run": "vpp-loss-divergence__ea20300d",
            "trial_id": "182849a3-a445-4970-bc76-64470f387db6",
            "reward": 1,
            "cost": 10.7343,
            "duration_min": 37.9,
            "prompt_tokens": 12596769,
            "output_tokens": 125512,
            "cache_tokens": 12370970,
        },
    ],
    "bun-sourcemap-leak": [
        {
            "run": "bun-sourcemap-leak__a9598c52",
            "trial_id": "f4ce51ab-0a1e-4717-a967-8cbc7eb20308",
            "reward": 0,
            "cost": 12.1813,
            "duration_min": 37.3,
            "prompt_tokens": 14129408,
            "output_tokens": 154390,
            "cache_tokens": 13910776,
        },
        {
            "run": "bun-sourcemap-leak__06e4beb9",
            "trial_id": "dbf5dcda-5cd4-4f4a-9cd2-c7e2aeb40a7a",
            "reward": 0,
            "cost": 10.8375,
            "duration_min": 36.8,
            "prompt_tokens": 11704278,
            "output_tokens": 156456,
            "cache_tokens": 11517456,
        },
        {
            "run": "bun-sourcemap-leak__6d7ac098",
            "trial_id": "cda4c5aa-594c-492f-acde-7e8b9e750c14",
            "reward": 0,
            "cost": 4.0731,
            "duration_min": 24.8,
            "prompt_tokens": 2265785,
            "output_tokens": 92721,
            "cache_tokens": 2157561,
        },
        {
            "run": "bun-sourcemap-leak__7751abbe",
            "trial_id": "a09f9eb5-3886-45e2-a5ce-b0f8f42d8e4d",
            "reward": 0,
            "cost": 6.6749,
            "duration_min": 24.8,
            "prompt_tokens": 6713437,
            "output_tokens": 99805,
            "cache_tokens": 6570267,
        },
        {
            "run": "bun-sourcemap-leak__2394a8e7",
            "trial_id": "9dce9d97-bbc2-45b3-a32f-7c22897da8db",
            "reward": 0,
            "cost": 10.4598,
            "duration_min": 36.8,
            "prompt_tokens": 10933429,
            "output_tokens": 152701,
            "cache_tokens": 10728954,
        },
    ],
}


def read_json(path: Path, default):
    if not path.exists():
        return default
    with path.open() as f:
        return json.load(f)


def read_rows(path: Path) -> list[dict[str, str]]:
    with path.open(newline="") as f:
        return list(csv.DictReader(f))


def esc(value) -> str:
    return html.escape("" if value is None else str(value), quote=True)


def as_float(value, default: float = 0.0) -> float:
    try:
        return float(value)
    except (TypeError, ValueError):
        return default


def as_int(value, default: int = 0) -> int:
    try:
        return int(float(value))
    except (TypeError, ValueError):
        return default


def usd(value) -> str:
    return f"${as_float(value):,.4f}"


def usd2(value) -> str:
    return f"${as_float(value):,.2f}"


def pct(value) -> str:
    return f"{as_float(value) * 100:.1f}%"


def intfmt(value) -> str:
    if value is None:
        return "n/a"
    return f"{as_int(value):,}"


def compact_duration(value) -> str:
    seconds = int(round(as_float(value)))
    h, rem = divmod(seconds, 3600)
    m, s = divmod(rem, 60)
    if h:
        return f"{h}h {m}m"
    return f"{m}m {s}s"


def clean_text(value: str, limit: int = 280) -> str:
    text = ANSI_RE.sub("", value or "")
    text = WS_RE.sub(" ", text).strip()
    if len(text) > limit:
        return text[: limit - 1].rstrip() + "..."
    return text


def rel_href(path: Path, base: Path) -> str:
    try:
        return quote(str(path.relative_to(base)))
    except ValueError:
        return "file://" + quote(str(path))


def link(label: str, path: Path, base: Path) -> str:
    if not path.exists():
        return f"<span class=\"muted\">{esc(label)} missing</span>"
    return f"<a href=\"{rel_href(path, base)}\">{esc(label)}</a>"


def classify_job(path: Path, row: dict[str, str]) -> str:
    name = path.name.removesuffix("-analysis.csv")
    if name.startswith("pilot-"):
        return "pilot"
    if "canary" in name:
        return "canary"
    if row.get("strategy") in {"all-premium", "smart-router", "smart-router-warmstart"}:
        return "core"
    return "other"


def discover_analysis_rows(artifact_dir: Path) -> list[tuple[Path, dict[str, str]]]:
    pairs: list[tuple[Path, dict[str, str]]] = []
    for path in sorted(artifact_dir.glob("*-analysis.csv")):
        for row in read_rows(path):
            row["_analysis_path"] = str(path)
            row["_job"] = path.name.removesuffix("-analysis.csv")
            row["_kind"] = classify_job(path, row)
            pairs.append((path, row))
    return pairs


def first_trial_dir(artifact_dir: Path, job: str) -> Path | None:
    job_dir = artifact_dir / "jobs" / job
    if not job_dir.exists():
        return None
    trials = sorted(p for p in job_dir.iterdir() if p.is_dir())
    return trials[0] if trials else None


def get_tests(ctrf: dict) -> list[dict]:
    current = ctrf
    for key in ("results", "tests"):
        if not isinstance(current, dict):
            return []
        current = current.get(key)
    return current if isinstance(current, list) else []


def failure_snippet(test: dict) -> str:
    trace = test.get("trace") or test.get("message") or ""
    match = re.search(r"AssertionError:\s*(.+)", trace)
    if match:
        return clean_text(match.group(1))
    return clean_text(trace)


def failure_bucket(snippet: str) -> str:
    text = snippet.lower()
    if "provider" in text or "5xx" in text or "timeout" in text:
        return "provider/runtime"
    if "assert" in text or "expected" in text or "mismatch" in text:
        return "wrong output"
    if "missing" in text or "not found" in text or "does not exist" in text:
        return "missing artifact"
    if "leak" in text or "secret" in text or "private" in text:
        return "leakage"
    if "hash" in text:
        return "hash mismatch"
    return "verifier assertion"


def load_trace_summary(artifact_dir: Path, job: str, session_id: str) -> dict:
    candidates = [
        artifact_dir / f"traces-after-{job}.json",
        artifact_dir / "pilot-live-traces.json",
    ]
    traces = []
    for path in candidates:
        data = read_json(path, {})
        possible = data.get("traces", []) if isinstance(data, dict) else []
        traces = [t for t in possible if t.get("session_id") == session_id]
        if traces:
            break

    agent_traces = [t for t in traces if (t.get("pool") or "") != "decision-model"]
    decision_traces = [
        t
        for t in traces
        if (t.get("pool") or "") == "decision-model"
        or (t.get("step_name") or "").startswith("router-decision")
    ]
    models = Counter(t.get("routed_model") or t.get("model") or "unknown" for t in agent_traces)
    statuses = Counter(str(t.get("status") or "unknown") for t in agent_traces)
    return {
        "trace_count": len(traces),
        "agent_trace_count": len(agent_traces),
        "decision_trace_count": len(decision_traces),
        "models": models,
        "statuses": statuses,
        "provider_5xx": sum(
            1
            for t in agent_traces
            if as_int(t.get("status")) >= 500
        ),
    }


def enrich_rows(artifact_dir: Path, rows: list[dict[str, str]]) -> None:
    for row in rows:
        job = row["_job"]
        trial_dir = first_trial_dir(artifact_dir, job)
        row["_trial_dir"] = str(trial_dir) if trial_dir else ""
        if trial_dir:
            ctrf = read_json(trial_dir / "verifier" / "ctrf.json", {})
            tests = get_tests(ctrf)
            passed = [t for t in tests if t.get("status") == "passed"]
            failed = [t for t in tests if t.get("status") != "passed"]
            row["_tests_total"] = str(len(tests))
            row["_tests_passed"] = str(len(passed))
            row["_tests_failed"] = str(len(failed))
            snippets = [failure_snippet(t) for t in failed[:8]]
            buckets = Counter(failure_bucket(s) for s in snippets)
            row["_failure_buckets"] = json.dumps(buckets, sort_keys=True)
            row["_failure_snippets"] = json.dumps(snippets, ensure_ascii=False)
        else:
            row["_tests_total"] = ""
            row["_tests_passed"] = ""
            row["_tests_failed"] = ""
            row["_failure_buckets"] = "{}"
            row["_failure_snippets"] = "[]"

        trace = load_trace_summary(artifact_dir, job, row.get("trace_key") or "")
        row["_trace_count"] = str(trace["trace_count"])
        row["_provider_5xx"] = str(trace["provider_5xx"])
        row["_model_counts"] = json.dumps(dict(trace["models"]), sort_keys=True)
        row["_status_counts"] = json.dumps(dict(trace["statuses"]), sort_keys=True)


def row_passed(row: dict[str, str]) -> bool:
    return as_float(row.get("reward")) >= 1.0


def tone_for_row(row: dict[str, str]) -> str:
    if row_passed(row):
        return "good"
    if row.get("failure_kind") in {"provider_5xx", "wall_clock_cap", "exception"}:
        return "warn"
    return "bad"


def model_call_counts(row: dict[str, str]) -> tuple[int, int]:
    counts = json.loads(row.get("_model_counts") or "{}")
    opus_calls = as_int(counts.get("anthropic/claude-opus-5"))
    flash_calls = as_int(counts.get("z-ai/glm-5.3-flash"))
    if opus_calls or flash_calls:
        return opus_calls, flash_calls
    agent_calls = as_int(row.get("agent_call_count"))
    opus_calls = round(agent_calls * as_float(row.get("opus_selected_rate")))
    flash_calls = max(agent_calls - opus_calls, 0)
    return opus_calls, flash_calls


def stat(label: str, value: str, note: str = "", tone: str = "") -> str:
    return (
        f"<section class=\"stat {esc(tone)}\">"
        f"<div class=\"stat-label\">{esc(label)}</div>"
        f"<div class=\"stat-value\">{esc(value)}</div>"
        f"<div class=\"stat-note\">{esc(note)}</div>"
        "</section>"
    )


def render_task_priors(rows: list[dict[str, str]]) -> str:
    tasks = sorted({row.get("task", "") for row in rows if row.get("task")})
    for task in DEFAULT_CONTEXT_TASKS:
        if task not in tasks:
            tasks.append(task)
    rendered = []
    for task in sorted(set(tasks)):
        trials = OPUS_LEADERBOARD_TRIALS.get(task, [])
        if trials:
            passes = sum(1 for trial in trials if as_float(trial.get("reward")) >= 1)
            record = f"{passes}/{len(trials)}"
            costs = [as_float(trial.get("cost")) for trial in trials]
            durations = [as_float(trial.get("duration_min")) for trial in trials]
            note = (
                "公开榜单强模型参照；"
                f"成本 {usd2(min(costs))}-{usd2(max(costs))}，"
                f"耗时 {min(durations):.1f}-{max(durations):.1f} 分钟"
            )
        else:
            passes = 0
            record = "unknown"
            note = "还没有提取到任务级公开参照"
        klass = "prior-good" if trials and passes == len(trials) else "neutral"
        if trials and passes == 0:
            klass = "prior-bad"
        rendered.append(
            "<tr>"
            f"<td><code>{esc(task)}</code></td>"
            f"<td><span class=\"pill {klass}\">{esc(record)}</span></td>"
            f"<td>{esc(note)}</td>"
            "</tr>"
        )
    return "".join(rendered)


def render_leaderboard_rows(rows: list[dict[str, str]]) -> str:
    tasks = {row.get("task", "") for row in rows if row.get("task")}
    tasks.update(DEFAULT_CONTEXT_TASKS)
    rendered = []
    for task in sorted(tasks):
        for trial in OPUS_LEADERBOARD_TRIALS.get(task, []):
            trial_url = f"{LEADERBOARD_JOB_URL}/trials/{trial['trial_id']}"
            tokens = (
                f"输入 {intfmt(trial.get('prompt_tokens'))}<br>"
                f"<span>输出 {intfmt(trial.get('output_tokens'))}; 缓存 {intfmt(trial.get('cache_tokens'))}</span>"
            )
            rendered.append(
                "<tr>"
                f"<td><code>{esc(task)}</code></td>"
                f"<td>{esc(trial['run'])}</td>"
                f"<td><strong>{esc(trial['reward'])}</strong></td>"
                f"<td>{usd(trial.get('cost'))}</td>"
                f"<td>{as_float(trial.get('duration_min')):.1f}m</td>"
                f"<td>{tokens}</td>"
                f"<td><a href=\"{esc(trial_url)}\">trial</a></td>"
                "</tr>"
            )
    if not rendered:
        return "<tr><td colspan=\"7\" class=\"muted\">没有提取到公开榜单 trial。</td></tr>"
    return "".join(rendered)


def render_run_rows(rows: list[dict[str, str]], base: Path) -> str:
    rendered = []
    for row in rows:
        analysis_path = Path(row["_analysis_path"])
        trial_dir = Path(row["_trial_dir"]) if row["_trial_dir"] else None
        test_note = ""
        if row.get("_tests_total"):
            test_note = f"{row['_tests_passed']}/{row['_tests_total']} 个测试"
        elif row.get("failure_kind"):
            test_note = row["failure_kind"]
        opus_calls, flash_calls = model_call_counts(row)
        attempt = row.get("attempt") or ""
        strategy_label = row.get("strategy") or ""
        if attempt and attempt.isdigit():
            strategy_label = f"{strategy_label} a{attempt}"
        artifact_links = link("分析", analysis_path, base)
        if trial_dir:
            artifact_links += " · " + link("轨迹", trial_dir / "agent" / "trajectory.json", base)
            artifact_links += " · " + link("验证器", trial_dir / "verifier" / "ctrf.json", base)
        rendered.append(
            f"<tr class=\"{tone_for_row(row)}\">"
            f"<td><span class=\"kind\">{esc(row['_kind'])}</span></td>"
            f"<td><code>{esc(row.get('task'))}</code></td>"
            f"<td>{esc(strategy_label)}</td>"
            f"<td><strong>{esc(row.get('reward'))}</strong><br><span>{esc(test_note)}</span></td>"
            f"<td>{usd(row.get('total_cost_usd'))}<br><span>{esc(row.get('cost_source'))}</span></td>"
            f"<td>{compact_duration(row.get('duration_seconds'))}<br><span>{esc(row.get('agent_call_count'))} 次 agent 调用</span></td>"
            f"<td>{opus_calls} 次 Opus<br><span>{flash_calls} 次 flash</span></td>"
            f"<td>{artifact_links}</td>"
            "</tr>"
        )
    return "".join(rendered)


def render_failure_rows(rows: list[dict[str, str]]) -> str:
    rendered = []
    for row in rows:
        snippets = json.loads(row.get("_failure_snippets") or "[]")
        if row_passed(row) or row.get("failure_kind") in {"pass", "none"}:
            snippets = []
        elif not snippets and row.get("failure_kind") not in {"", None}:
            snippets = [row.get("failure_kind", "")]
        for snippet in snippets[:4]:
            rendered.append(
                "<tr>"
                f"<td><code>{esc(row.get('task'))}</code></td>"
                f"<td>{esc(row.get('strategy'))}</td>"
                f"<td><span class=\"pill neutral\">{esc(failure_bucket(snippet))}</span></td>"
                f"<td>{esc(snippet)}</td>"
                "</tr>"
            )
    if not rendered:
        return "<tr><td colspan=\"4\" class=\"muted\">当前渲染的行里没有 verifier 失败。</td></tr>"
    return "".join(rendered)


def render_strategy_summary(rows: list[dict[str, str]]) -> str:
    buckets = defaultdict(lambda: {"rows": 0, "passes": 0, "cost": 0.0, "calls": 0})
    for row in rows:
        strategy = row.get("strategy") or "unknown"
        buckets[strategy]["rows"] += 1
        buckets[strategy]["passes"] += 1 if row_passed(row) else 0
        buckets[strategy]["cost"] += as_float(row.get("total_cost_usd"))
        buckets[strategy]["calls"] += as_int(row.get("agent_call_count"))

    rendered = []
    for strategy, vals in sorted(buckets.items()):
        rendered.append(
            "<tr>"
            f"<td>{esc(strategy)}</td>"
            f"<td>{vals['passes']}/{vals['rows']}</td>"
            f"<td>${vals['cost']:,.4f}</td>"
            f"<td>{vals['calls']}</td>"
            "</tr>"
        )
    return "".join(rendered)


def render_local_strategy_cell(rows: list[dict[str, str]], task: str, strategy: str) -> str:
    matched = [
        row
        for row in rows
        if row.get("task") == task
        and row.get("strategy") == strategy
        and row.get("_kind") != "canary"
    ]
    if not matched:
        return '<span class="muted">还没跑</span>'

    passes = sum(1 for row in matched if row_passed(row))
    cost = sum(as_float(row.get("total_cost_usd")) for row in matched)
    calls = sum(as_int(row.get("agent_call_count")) for row in matched)
    duration = sum(as_float(row.get("duration_seconds")) for row in matched)
    opus_calls = 0
    flash_calls = 0
    fallback_rows = 0
    for row in matched:
        opus, flash = model_call_counts(row)
        opus_calls += opus
        flash_calls += flash
        if as_float(row.get("fallback_to_opus_rate")) > 0 or as_float(row.get("opus_fallback_rate")) > 0:
            fallback_rows += 1

    details = [
        f"{passes}/{len(matched)} 通过",
        f"总成本 ${cost:,.4f}",
        f"均 ${cost / len(matched):,.4f}/trial",
        f"耗时 {compact_duration(duration)}",
        f"{calls} 次调用",
    ]
    if strategy in {"smart-router", "smart-router-warmstart"}:
        details.append(f"{opus_calls} Opus / {flash_calls} flash")
        if fallback_rows:
            details.append(f"{fallback_rows} 条有兜底")
    return "<br>".join(esc(part) for part in details)


def local_strategy_plain(rows: list[dict[str, str]], task: str, strategy: str) -> str:
    matched = [
        row
        for row in rows
        if row.get("task") == task
        and row.get("strategy") == strategy
        and row.get("_kind") != "canary"
    ]
    if not matched:
        return "还没跑。"

    passes = sum(1 for row in matched if row_passed(row))
    cost = sum(as_float(row.get("total_cost_usd")) for row in matched)
    calls = sum(as_int(row.get("agent_call_count")) for row in matched)
    opus_calls = 0
    flash_calls = 0
    for row in matched:
        opus, flash = model_call_counts(row)
        opus_calls += opus
        flash_calls += flash

    route = ""
    if strategy in {"smart-router", "smart-router-warmstart"}:
        route = f"，路由用了 {opus_calls} 次 Opus、{flash_calls} 次 flash"
    return (
        f"{passes}/{len(matched)} 通过，总成本 ${cost:,.4f}，"
        f"平均 ${cost / len(matched):,.4f}/trial，agent 调用 {calls} 次{route}。"
    )


def render_public_anchor_cell(task: str) -> str:
    trials = OPUS_LEADERBOARD_TRIALS.get(task, [])
    if not trials:
        return '<span class="muted">没有公开榜单参照</span>'
    passes = sum(1 for trial in trials if as_float(trial.get("reward")) >= 1)
    costs = [as_float(trial.get("cost")) for trial in trials]
    durations = [as_float(trial.get("duration_min")) for trial in trials]
    return (
        f"{passes}/{len(trials)} 通过<br>"
        f"<span>成本 {usd2(min(costs))}-{usd2(max(costs))}</span><br>"
        f"<span>耗时 {min(durations):.1f}-{max(durations):.1f} 分钟</span>"
    )


def comparison_takeaway(rows: list[dict[str, str]], task: str) -> str:
    public = OPUS_LEADERBOARD_TRIALS.get(task, [])
    public_passes = sum(1 for trial in public if as_float(trial.get("reward")) >= 1)
    public_costs = [as_float(trial.get("cost")) for trial in public if as_float(trial.get("reward")) >= 1]
    smart_rows = [
        row
        for row in rows
        if row.get("task") == task
        and row.get("strategy") == "smart-router"
        and row.get("_kind") != "canary"
    ]
    warm_rows = [
        row
        for row in rows
        if row.get("task") == task
        and row.get("strategy") == "smart-router-warmstart"
        and row.get("_kind") != "canary"
    ]
    if not smart_rows and not warm_rows:
        return "待跑本地 smart-router。"

    smart_passes = sum(1 for row in smart_rows if row_passed(row))
    warm_passes = sum(1 for row in warm_rows if row_passed(row))
    if public_passes == 0:
        return "公开强模型也没做对；这题只适合当压力测试。"
    if smart_passes or warm_passes:
        passing = [row for row in smart_rows + warm_rows if row_passed(row)]
        avg_local = sum(as_float(row.get("total_cost_usd")) for row in passing) / len(passing)
        if public_costs:
            avg_public = sum(public_costs) / len(public_costs)
            if avg_local < avg_public:
                return "本地 router 质量追上公开强模型，而且平均成本更低。"
            if avg_local <= max(public_costs) * 1.1:
                return "本地 router 质量追上公开强模型，成本接近公开参照。"
            return "本地 router 质量追上公开强模型，但平均成本更高，省钱信号还不成立。"
        return "本地 router 已经追上公开强模型的质量信号；成本要等更多公开参照。"
    return "公开强模型能做对，但本地 router 还没追上；先看失败原因。"


def render_smart_comparison_rows(rows: list[dict[str, str]]) -> str:
    tasks = sorted({row.get("task", "") for row in rows if row.get("task")})
    for task in DEFAULT_CONTEXT_TASKS:
        if task not in tasks:
            tasks.append(task)

    rendered = []
    for task in sorted(set(tasks)):
        rendered.append(
            "<tr>"
            f"<td><code>{esc(task)}</code></td>"
            f"<td>{render_public_anchor_cell(task)}</td>"
            f"<td>{render_local_strategy_cell(rows, task, 'smart-router')}</td>"
            f"<td>{render_local_strategy_cell(rows, task, 'smart-router-warmstart')}</td>"
            f"<td>{render_local_strategy_cell(rows, task, 'all-premium')}</td>"
            f"<td>{esc(comparison_takeaway(rows, task))}</td>"
            "</tr>"
        )
    return "".join(rendered)


def find_local_row(rows: list[dict[str, str]], task: str, strategy: str = "smart-router") -> dict[str, str] | None:
    matches = [
        row
        for row in rows
        if row.get("task") == task
        and row.get("strategy") == strategy
        and row.get("_kind") == "pilot"
    ]
    if not matches:
        matches = [
            row
            for row in rows
            if row.get("task") == task
            and row.get("strategy") == strategy
        ]
    return matches[-1] if matches else None


def local_row_brief(row: dict[str, str] | None) -> str:
    if not row:
        return "还没跑本地 trial。"
    outcome = "通过" if row_passed(row) else "没通过"
    test_note = ""
    if row.get("_tests_total"):
        test_note = f"，测试 {row.get('_tests_passed')}/{row.get('_tests_total')}"
    return (
        f"{outcome}{test_note}，成本 {usd(row.get('total_cost_usd'))}，"
        f"耗时 {compact_duration(row.get('duration_seconds'))}，"
        f"agent 调用 {row.get('agent_call_count')} 次。"
    )


def leaderboard_brief(task: str) -> str:
    trials = OPUS_LEADERBOARD_TRIALS.get(task, [])
    if not trials:
        return "公开榜单里还没有提取到这题的任务级明细。"
    passes = sum(1 for trial in trials if as_float(trial.get("reward")) >= 1)
    costs = [as_float(trial.get("cost")) for trial in trials]
    durations = [as_float(trial.get("duration_min")) for trial in trials]
    return (
        f"公开榜单参照是 {passes}/{len(trials)}，"
        f"成本区间 {usd2(min(costs))}-{usd2(max(costs))}，"
        f"耗时 {min(durations):.1f}-{max(durations):.1f} 分钟。"
    )


def routing_brief(row: dict[str, str] | None) -> str:
    if not row:
        return "还没有本地路由记录。"
    opus_calls, flash_calls = model_call_counts(row)
    fallback_rate = as_float(row.get("fallback_to_opus_rate"))
    fallback_text = "路由器没有因为判断失败而兜底到 Opus"
    if fallback_rate > 0:
        fallback_text = f"有 {fallback_rate * 100:.1f}% 的调用是判断失败后的 Opus 兜底"
    return (
        f"这次用了 {opus_calls} 次 Opus、{flash_calls} 次 flash；"
        f"{fallback_text}。"
    )


def render_story(rows: list[dict[str, str]], core_rows: list[dict[str, str]]) -> str:
    shadow = find_local_row(rows, "shadow-relay")
    vpp = find_local_row(rows, "vpp-loss-divergence")
    expected_core = EXPECTED_CORE_ROWS
    core_count = len(core_rows)
    core_passes = sum(1 for row in core_rows if row_passed(row))
    core_cost = sum(as_float(row.get("total_cost_usd")) for row in core_rows)
    if core_count >= expected_core:
        matrix_status = f"正式实验已跑完 {core_count}/{expected_core} 条，{core_passes} 条通过。"
    elif core_count and core_cost >= SOFT_BUDGET_USD:
        matrix_status = (
            f"正式实验本轮已在软预算处收尾：已分析 {core_count}/{expected_core} 条，"
            f"{core_passes} 条通过；剩余 trial 没再启动或没有完整落盘。"
        )
    elif core_count:
        matrix_status = f"正式实验进行中：已经分析 {core_count}/{expected_core} 条，{core_passes} 条通过。"
    else:
        matrix_status = f"正式实验还没开始；目标是 {expected_core} 条本地 trial。"

    return f"""
    <section class="panel article">
      <h2>先用一句话讲清楚</h2>
      <p>我们在测试一个省钱想法：写代码时，不是每一步都叫最贵、最强的 Opus 5；简单步骤交给便宜的 flash，难步骤再叫 Opus 5。负责判断“这一步该叫谁”的，就是 <code>smart-router</code>。</p>
      <p>所以这不是在比谁跑得快。我们主要看两件事：第一，最后有没有把题做对；第二，花了多少钱。</p>
    </section>

    <section class="panel article">
      <h2>为什么要看公开榜单</h2>
      <p>公开 leaderboard 像一张大家都能看的成绩单。上面有强模型已经跑过的结果。我们把它当成外部参照：如果强模型在某道题上经常做对，这道题就更适合拿来问“smart-router 能不能也做对，并少花一点钱”。</p>
      <p>但它不是完全一样的本地对照。公开榜单用的是 Claude Code，我们本地用的是 aware-gateway 和 smart-router；公开榜单也看不到 decision model 每一步为什么选 flash 或 Opus。</p>
    </section>

    <section class="story-grid">
      <div class="story-card neutral-card">
        <div class="step">第一步</div>
        <h3>先挑公平的题</h3>
        <p>我们不用“强模型自己都做不对”的题当主考题。这样的题更像压力测试，不适合拿来判断 smart-router 有没有用。</p>
        <p>正式 V4 只看两道有代表性的题：一题安全取证，一题 ML / systems。它们在公开榜单参照里都是 <strong>5/5</strong>。</p>
      </div>
      <div class="story-card good">
        <div class="step">第二步</div>
        <h3>第一道：安全取证题</h3>
        <p><code>shadow-relay</code> 在公开榜单参照里是 <strong>5/5</strong>，说明强模型确实能解。</p>
        <p>{esc(leaderboard_brief("shadow-relay"))}</p>
        <p>本地 smart-router：{esc(local_strategy_plain(rows, "shadow-relay", "smart-router"))}</p>
        <p>本地 warm-start：{esc(local_strategy_plain(rows, "shadow-relay", "smart-router-warmstart"))}</p>
        <p>{esc(routing_brief(shadow))}</p>
      </div>
      <div class="story-card neutral-card">
        <div class="step">第三步</div>
        <h3>第二道：系统训练题</h3>
        <p><code>vpp-loss-divergence</code> 是 ML / systems 方向，不是安全取证题。公开榜单参照是 <strong>5/5</strong>，但成本和耗时明显更高。</p>
        <p>{esc(leaderboard_brief("vpp-loss-divergence"))}</p>
        <p>本地 smart-router：{esc(local_strategy_plain(rows, "vpp-loss-divergence", "smart-router"))}</p>
        <p>本地 warm-start：{esc(local_strategy_plain(rows, "vpp-loss-divergence", "smart-router-warmstart"))}</p>
        <p>{esc(routing_brief(vpp))}</p>
      </div>
      <div class="story-card warn">
        <div class="step">现在</div>
        <h3>本轮进度</h3>
        <p>{esc(matrix_status)}</p>
        <p>本轮不是比速度。我们更关心：同一道题，smart-router 能不能像强模型那样做对，同时少花钱。</p>
        <p>正式矩阵是：每题 1 次本地纯 Opus 校准、2 次普通 smart-router、2 次前 5 步先用 Opus 的 warm-start。</p>
      </div>
    </section>
    """


def render_conclusion(rows: list[dict[str, str]], core_rows: list[dict[str, str]]) -> str:
    if not core_rows:
        return """
      <p>目前还没有正式 core 结果。页面先记录实验逻辑：选公开强模型能做对的题，再看 smart-router 是否也能做对并减少成本。</p>
      <p class="muted">这份 HTML 是阅读版实验报告，不是拿去刷榜的正式提交。本地实验为了控制成本设置了更短时间上限；公开榜单只是外部参照。</p>
        """

    expected_core = EXPECTED_CORE_ROWS
    core_count = len(core_rows)
    passes = sum(1 for row in core_rows if row_passed(row))
    core_cost = sum(as_float(row.get("total_cost_usd")) for row in core_rows)
    smart_rows = [row for row in core_rows if row.get("strategy") == "smart-router"]
    warm_rows = [row for row in core_rows if row.get("strategy") == "smart-router-warmstart"]
    smart_passes = sum(1 for row in smart_rows if row_passed(row))
    warm_passes = sum(1 for row in warm_rows if row_passed(row))

    if core_count >= expected_core:
        status = f"正式实验已跑完 {core_count}/{expected_core} 条。"
    elif core_cost >= SOFT_BUDGET_USD:
        status = (
            f"正式实验本轮已在软预算处收尾：已分析 {core_count}/{expected_core} 条，"
            f"本地 cost 已到 ${core_cost:,.2f}。"
        )
    else:
        status = f"正式实验正在按计划推进：已分析 {core_count}/{expected_core} 条。"
    return f"""
      <p>{esc(status)} 目前 {passes} 条通过。普通 smart-router 是 {smart_passes}/{len(smart_rows) or 0}，warm-start 是 {warm_passes}/{len(warm_rows) or 0}。</p>
      <p>现在最重要的读法还是两步：先问有没有做对，再问为这个结果花了多少钱。如果 smart-router 每一步都选 Opus，它也许能做对，但省钱能力就要打折；如果它能把简单步骤交给 flash，同时最终过 verifier，那才是好信号。</p>
      <p class="muted">这份 HTML 是阅读版实验报告，不是拿去刷榜的正式提交。本地实验为了控制成本设置了更短时间上限；公开榜单只是外部参照。</p>
    """


def render_glossary() -> str:
    items = [
        ("leaderboard", "公开成绩单。大家能看到不同模型在同一批题上的分数和花费。"),
        ("trial", "一次独立尝试。像同一道题重新做一遍。"),
        ("reward=1", "裁判说做对了。"),
        ("reward=0", "裁判说没做对。可能做对了一部分，但最终答案不够好。"),
        ("verifier", "自动裁判。它跑测试，判断答案是不是真的完成任务。"),
        ("hidden tests", "隐藏测试。做题的人提前看不到，用来检查答案是不是真的可靠。"),
        ("cost", "模型调用花的钱。这里比速度更重要。"),
        ("agent call", "做题助手每思考或行动一步，通常会调用一次模型。"),
        ("Opus 5", "强但贵的模型。适合难判断、改代码、收尾检查。"),
        ("flash", "便宜的模型。适合读信息、做简单检查、短计划。"),
        ("smart-router", "调度员。它每一步决定用 flash 还是 Opus 5。"),
        ("warm-start", "前几步先用强模型打好基础，后面再让 smart-router 自由选择。"),
    ]
    rendered = [
        "<tr>"
        f"<td><code>{esc(term)}</code></td>"
        f"<td>{esc(explanation)}</td>"
        "</tr>"
        for term, explanation in items
    ]
    return "".join(rendered)


def render(artifact_dir: Path, output: Path) -> None:
    pairs = discover_analysis_rows(artifact_dir)
    rows = [row for _, row in pairs]
    enrich_rows(artifact_dir, rows)

    rows.sort(key=lambda r: (r.get("_kind") != "pilot", r.get("task", ""), r.get("strategy", ""), r.get("attempt", "")))
    pilot_rows = [r for r in rows if r.get("_kind") == "pilot"]
    core_rows = [r for r in rows if r.get("_kind") == "core"]
    rendered_scope = core_rows or pilot_rows or rows

    total_cost = sum(as_float(r.get("total_cost_usd")) for r in rendered_scope)
    passes = sum(1 for r in rendered_scope if row_passed(r))
    total_agent_calls = sum(as_int(r.get("agent_call_count")) for r in rendered_scope)
    duration = sum(as_float(r.get("duration_seconds")) for r in rendered_scope)
    latest = max([Path(r["_analysis_path"]).stat().st_mtime for r in rows] or [0])
    latest_text = datetime.fromtimestamp(latest, tz=timezone.utc).strftime("%Y-%m-%d %H:%M UTC") if latest else "n/a"

    if core_rows:
        headline = "smart-router 省钱实验：正式结果更新"
        verdict = "正式 matrix 已经有结果进入报告；读法还是一样：先看做没做对，再看花了多少钱。"
    elif pilot_rows:
        headline = "给 10 岁也能看懂的 smart-router 实验故事"
        verdict = "我们在问一个朴素问题：能不能把简单步骤交给便宜模型，难步骤交给强模型，最后既做对又少花钱？"
    else:
        headline = "smart-router 省钱实验：准备中"
        verdict = "还没有核心结果；这份页面先记录实验为什么这样设计。"

    generated_at = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")
    html_text = f"""<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>aware-gateway V4 实验报告</title>
  <style>
    :root {{
      color-scheme: light;
      --bg: #f5f6f8;
      --paper: #ffffff;
      --ink: #18202a;
      --muted: #667085;
      --line: #d9dee8;
      --soft: #f9fafb;
      --green: #067647;
      --green-bg: #edfdf4;
      --red: #b42318;
      --red-bg: #fff1f0;
      --amber: #92400e;
      --amber-bg: #fffbeb;
      --blue: #175cd3;
      --blue-bg: #eff8ff;
      --violet: #6941c6;
      --violet-bg: #f5f3ff;
    }}
    * {{ box-sizing: border-box; }}
    body {{
      margin: 0;
      background: var(--bg);
      color: var(--ink);
      font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      font-size: 16px;
      line-height: 1.65;
    }}
    header {{
      background: linear-gradient(180deg, #ffffff 0%, #f1f5f9 100%);
      border-bottom: 1px solid var(--line);
    }}
    .wrap {{
      width: min(1120px, calc(100vw - 40px));
      margin: 0 auto;
    }}
    header .wrap {{
      padding: 34px 0 26px;
    }}
    main.wrap {{
      padding: 24px 0 44px;
    }}
    h1 {{
      margin: 0 0 10px;
      max-width: 920px;
      font-size: 42px;
      line-height: 1.15;
      letter-spacing: 0;
    }}
    h2 {{
      margin: 0 0 12px;
      font-size: 22px;
      letter-spacing: 0;
    }}
    h3 {{
      margin: 18px 0 8px;
      font-size: 17px;
      letter-spacing: 0;
    }}
    p {{ margin: 0 0 12px; }}
    a {{ color: var(--blue); text-decoration: none; }}
    a:hover {{ text-decoration: underline; }}
    code {{
      padding: 1px 5px;
      border: 1px solid var(--line);
      border-radius: 5px;
      background: #f2f4f7;
      font-size: 0.92em;
    }}
    .muted, td span {{ color: var(--muted); }}
    .lede {{
      max-width: 900px;
      color: #344054;
      font-size: 19px;
    }}
    .stamp {{
      color: var(--muted);
      font-size: 13px;
    }}
    .note {{
      margin-top: 18px;
      padding: 16px 18px;
      border: 1px solid #b2ddff;
      border-radius: 8px;
      background: var(--blue-bg);
    }}
    .stats {{
      display: grid;
      grid-template-columns: repeat(4, minmax(0, 1fr));
      gap: 12px;
      margin: 20px 0;
    }}
    .stat, .panel {{
      border: 1px solid var(--line);
      border-radius: 8px;
      background: var(--paper);
      box-shadow: 0 1px 2px rgba(16, 24, 40, 0.04);
    }}
    .stat {{
      padding: 14px;
      min-height: 110px;
    }}
    .stat.good {{ border-color: #abefc6; background: var(--green-bg); }}
    .stat.bad {{ border-color: #f2b8b5; background: var(--red-bg); }}
    .stat.warn {{ border-color: #f6c768; background: var(--amber-bg); }}
    .stat-label {{
      color: var(--muted);
      font-size: 12px;
      font-weight: 700;
      text-transform: uppercase;
      letter-spacing: 0;
    }}
    .stat-value {{
      margin-top: 8px;
      font-size: 25px;
      font-weight: 750;
      line-height: 1.15;
    }}
    .stat-note {{
      margin-top: 8px;
      color: var(--muted);
      font-size: 13px;
    }}
    .panel {{
      margin-top: 18px;
      padding: 18px;
      overflow: hidden;
    }}
    .article {{
      max-width: 920px;
      padding: 22px;
    }}
    .article p {{
      font-size: 18px;
    }}
    .grid-2 {{
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 18px;
    }}
    .story-grid {{
      display: grid;
      grid-template-columns: repeat(2, minmax(0, 1fr));
      gap: 18px;
      margin-top: 18px;
    }}
    .story-card {{
      border: 1px solid var(--line);
      border-radius: 8px;
      background: var(--paper);
      padding: 18px;
      box-shadow: 0 1px 2px rgba(16, 24, 40, 0.04);
    }}
    .story-card.good {{ border-color: #abefc6; background: var(--green-bg); }}
    .story-card.bad {{ border-color: #f2b8b5; background: var(--red-bg); }}
    .story-card.warn {{ border-color: #f6c768; background: var(--amber-bg); }}
    .story-card.neutral-card {{ background: var(--soft); }}
    .story-card h3 {{
      margin-top: 6px;
      font-size: 20px;
    }}
    .step {{
      color: var(--muted);
      font-size: 12px;
      font-weight: 800;
      text-transform: uppercase;
      letter-spacing: 0;
    }}
    .plain-list {{
      margin: 0;
      padding-left: 22px;
    }}
    .plain-list li {{
      margin: 8px 0;
    }}
    .table-wrap {{
      overflow-x: auto;
    }}
    table {{
      width: 100%;
      border-collapse: collapse;
      font-size: 14px;
    }}
    th, td {{
      padding: 10px 9px;
      border-bottom: 1px solid var(--line);
      text-align: left;
      vertical-align: top;
    }}
    th {{
      color: #344054;
      font-weight: 700;
      background: #fbfcfe;
    }}
    tr.good td:first-child {{ border-left: 4px solid var(--green); }}
    tr.bad td:first-child {{ border-left: 4px solid var(--red); }}
    tr.warn td:first-child {{ border-left: 4px solid var(--amber); }}
    .kind {{
      color: var(--muted);
      font-size: 12px;
      font-weight: 700;
      text-transform: uppercase;
    }}
    .pill {{
      display: inline-flex;
      align-items: center;
      min-height: 24px;
      padding: 2px 8px;
      border-radius: 999px;
      font-size: 12px;
      font-weight: 700;
      white-space: nowrap;
    }}
    .prior-good {{ color: var(--green); background: var(--green-bg); border: 1px solid #abefc6; }}
    .prior-bad {{ color: var(--red); background: var(--red-bg); border: 1px solid #f2b8b5; }}
    .neutral {{ color: #344054; background: #f2f4f7; border: 1px solid var(--line); }}
    @media (max-width: 920px) {{
      .wrap {{ width: min(100vw - 24px, 1120px); }}
      .stats, .grid-2, .story-grid {{ grid-template-columns: 1fr; }}
      h1 {{ font-size: 31px; }}
      .article p {{ font-size: 16px; }}
      table {{ font-size: 13px; }}
      th, td {{ padding: 8px 7px; }}
    }}
  </style>
</head>
<body>
  <header>
    <div class="wrap">
      <h1>{esc(headline)}</h1>
      <p class="lede">{esc(verdict)}</p>
      <p class="stamp">产物目录：<code>{esc(str(artifact_dir))}</code> · 最新分析：{esc(latest_text)} · 生成时间：{esc(generated_at)}</p>
    </div>
  </header>
  <main class="wrap">
    {render_story(rows, core_rows)}

    <div class="stats">
      {stat("本地结果", str(len(rendered_scope)), f"{len(pilot_rows)} 条 pilot，{len(core_rows)} 条正式 core")}
      {stat("做对了几条", f"{passes}/{len(rendered_scope)}", "reward=1 才算通过", "good" if passes else "bad")}
      {stat("本地花费", f"${total_cost:,.4f}", "优先看成本，不优先看速度")}
      {stat("agent 调用", str(total_agent_calls), f"总耗时 {compact_duration(duration)}", "warn" if duration > 3600 else "")}
    </div>

    <section class="panel article">
      <h2>怎么读这些数字</h2>
      <p><strong>先看质量。</strong> 如果 reward 是 1，表示 verifier 判定任务完成；如果是 0，表示还有关键要求没满足。成本只有在质量接近时才有意义。</p>
      <p><strong>再看成本。</strong> 我们希望 smart-router 把简单步骤交给 flash，把难步骤交给 Opus 5。如果它几乎每一步都选 Opus 5，那质量可能不错，但省钱空间就小。</p>
      <p><strong>最后看公开参照。</strong> 公开榜单结果告诉我们：这道题对强模型来说是不是可解，以及强模型大概会花多少钱。但它没有我们的路由日志，所以不能替代本地实验。</p>
    </section>

    <section class="panel article">
      <h2>这个 cost 真实吗</h2>
      <p>这里的本地 cost 优先使用 gateway trace 里捕获到的 OpenRouter <code>upstream_inference_cost</code>。这比 Harbor trajectory 里的估算更接近真实扣费，因为它来自 provider 返回的 usage / cost 字段。</p>
      <p>它仍然不是账单截图，所以不能保证和最终账单一分钱不差。若某条 trace 没拿到 upstream cost，分析脚本才会退回到 token usage × 配置单价重算。现在 decision model 切到 GPT-5.6 后，decision call 的成本也会计入总成本。</p>
    </section>

    <section class="panel">
      <h2>smart-router 对比榜</h2>
      <p class="muted">每个任务一行：公开榜单只作为参照摘要，不单独展开强模型刷榜明细。真正要看的，是本地 smart-router 和 warm-start 能不能接近公开强模型质量，并少花钱。</p>
      <div class="table-wrap"><table>
        <thead>
          <tr><th>任务</th><th>公开榜单参照</th><th>本地 smart-router</th><th>本地 warm-start</th><th>本地 all-premium 校准</th><th>读法</th></tr>
        </thead>
        <tbody>{render_smart_comparison_rows(rows)}</tbody>
      </table></div>
    </section>

    <section class="panel">
      <h2>本地策略概览</h2>
      <div class="table-wrap"><table>
        <thead><tr><th>策略</th><th>通过</th><th>成本</th><th>agent 调用</th></tr></thead>
        <tbody>{render_strategy_summary(rendered_scope)}</tbody>
      </table></div>
    </section>

    <section class="panel">
      <h2>本地 run log</h2>
      <p class="muted">这里才是我们自己的实验记录：aware-gateway、smart-router、decision model，以及本地 verifier 的结果。</p>
      <div class="table-wrap"><table>
        <thead>
          <tr><th>类型</th><th>任务</th><th>策略</th><th>reward</th><th>成本</th><th>耗时</th><th>路由</th><th>产物</th></tr>
        </thead>
        <tbody>{render_run_rows(rendered_scope, output.parent)}</tbody>
      </table></div>
    </section>

    <section class="panel">
      <h2>失败测试是什么意思</h2>
      <p>失败测试就像裁判手里的隐藏题。做题的人看不到它们，所以它们能检查答案是不是真的稳。本轮 <code>vpp-loss-divergence</code> 的典型情况是：前面 4 个结构测试通过，但最后的 loss 轨迹没有和参考答案对齐，所以 reward 还是 0。</p>
      <div class="table-wrap"><table>
        <thead><tr><th>任务</th><th>策略</th><th>类型</th><th>测试提示</th></tr></thead>
        <tbody>{render_failure_rows(rendered_scope)}</tbody>
      </table></div>
    </section>

    <section class="panel">
      <h2>小词典</h2>
      <div class="table-wrap"><table>
        <thead><tr><th>词</th><th>意思</th></tr></thead>
        <tbody>{render_glossary()}</tbody>
      </table></div>
    </section>

    <section class="panel article">
      <h2>现在的结论</h2>
      {render_conclusion(rows, core_rows)}
    </section>
  </main>
</body>
</html>
"""
    output.write_text(html_text)


def main() -> None:
    parser = argparse.ArgumentParser(description="Render a blog-style V4 experiment report.")
    parser.add_argument("--artifact-dir", type=Path, default=None)
    parser.add_argument("--output", type=Path, default=None)
    args = parser.parse_args()

    artifact_dir = args.artifact_dir
    if artifact_dir is None:
        current = Path(".aware-v4-current-artifact")
        if not current.exists():
            raise SystemExit("--artifact-dir is required when .aware-v4-current-artifact is missing")
        artifact_dir = Path(current.read_text().strip())
    artifact_dir = artifact_dir.resolve()
    output = args.output or artifact_dir / "aware-v4-blog-report.html"
    output.parent.mkdir(parents=True, exist_ok=True)
    render(artifact_dir, output.resolve())
    print(output)


if __name__ == "__main__":
    main()
