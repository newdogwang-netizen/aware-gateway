#!/usr/bin/env python3
"""Build a standalone smart-router decision lab from V4 gateway traces."""

from __future__ import annotations

import argparse
import csv
import html
import json
from collections import Counter, defaultdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


SMART_STRATEGIES = {"smart-router", "smart-router-warmstart"}


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Extract smart-router decision records and render an HTML lab."
    )
    parser.add_argument(
        "--artifact-dir",
        default="/mnt/data2/aware-gateway-runs/aware-v4-20260902T115134Z",
        help="Directory containing *-analysis.csv and traces-after-*.json files.",
    )
    parser.add_argument(
        "--out-html",
        default=None,
        help="Output HTML path. Defaults to <artifact-dir>/router-decision-lab.html.",
    )
    parser.add_argument(
        "--out-json",
        default=None,
        help="Output JSON path. Defaults to <artifact-dir>/router-decisions.json.",
    )
    parser.add_argument(
        "--include-canary",
        action="store_true",
        help="Include canary rows. Defaults to formal experiment rows only.",
    )
    parser.add_argument(
        "--prompt-id",
        default="v4-current-gpt5.6-router",
        help="Prompt variant label to attach to this dataset.",
    )
    return parser.parse_args()


def iso_to_dt(value: str) -> datetime:
    if not value:
        return datetime.fromtimestamp(0, timezone.utc)
    value = value.replace("Z", "+00:00")
    try:
        return datetime.fromisoformat(value)
    except ValueError:
        if "." in value:
            head, tail = value.split(".", 1)
            offset = "+00:00" if "+" not in tail else "+" + tail.split("+", 1)[1]
            frac = tail.split("+", 1)[0][:6]
            return datetime.fromisoformat(f"{head}.{frac}{offset}")
        raise


def as_int(value: Any) -> int:
    try:
        return int(float(value or 0))
    except (TypeError, ValueError):
        return 0


def as_float(value: Any) -> float:
    try:
        return float(value or 0)
    except (TypeError, ValueError):
        return 0.0


def read_analysis_rows(artifact_dir: Path, include_canary: bool) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for path in sorted(artifact_dir.glob("*-analysis.csv")):
        if not include_canary and "canary" in path.name:
            continue
        with path.open(newline="", encoding="utf-8") as handle:
            for row in csv.DictReader(handle):
                if row.get("strategy") not in SMART_STRATEGIES:
                    continue
                job = path.name.removesuffix("-analysis.csv")
                enriched = dict(row)
                enriched["job"] = job
                enriched["analysis_csv"] = str(path)
                rows.append(enriched)
    return rows


def load_traces(artifact_dir: Path, job: str, session_id: str) -> list[dict[str, Any]]:
    trace_file = artifact_dir / f"traces-after-{job}.json"
    if not trace_file.exists():
        return []
    with trace_file.open(encoding="utf-8") as handle:
        payload = json.load(handle)
    traces = [
        trace
        for trace in payload.get("traces", [])
        if (not session_id) or trace.get("session_id") == session_id
    ]
    return sorted(traces, key=lambda trace: trace.get("timestamp") or "")


def load_trajectory(artifact_dir: Path, job: str) -> tuple[str, list[dict[str, Any]]]:
    matches = sorted((artifact_dir / "jobs" / job).glob("*/agent/trajectory.json"))
    if not matches:
        return "", []
    with matches[0].open(encoding="utf-8") as handle:
        payload = json.load(handle)
    return str(matches[0]), payload.get("steps", [])


def is_decision_trace(trace: dict[str, Any]) -> bool:
    return (trace.get("pool") or "") == "decision-model" or str(
        trace.get("step_name") or ""
    ).startswith("router-decision")


def clean_reason(reason: str) -> str:
    for prefix in ("smart-router: ", "smart-router warm-start: "):
        if reason.startswith(prefix):
            return reason[len(prefix) :].strip()
    return reason.strip()


def phase_from_index(index: int) -> str:
    if index <= 1:
        return "understand"
    if index <= 3:
        return "plan / first implementation"
    if index <= 5:
        return "code"
    if index <= 7:
        return "review"
    return "fix / final"


def clip(value: str, limit: int = 1400) -> str:
    value = value.strip()
    if len(value) <= limit:
        return value
    return value[:limit].rstrip() + "..."


def trajectory_context(
    trajectory_path: str, steps: list[dict[str, Any]], agent_timestamp: str
) -> dict[str, Any]:
    if not steps or not agent_timestamp:
        return {}
    agent_dt = iso_to_dt(agent_timestamp)
    best_index = None
    best_delta = None
    for index, step in enumerate(steps):
        if step.get("source") != "agent":
            continue
        delta = abs((iso_to_dt(step.get("timestamp") or "") - agent_dt).total_seconds())
        if best_delta is None or delta < best_delta:
            best_index = index
            best_delta = delta
    if best_index is None or best_delta is None or best_delta > 90:
        return {"trajectory_path": trajectory_path}

    prior_steps = steps[:best_index]
    latest_user = next(
        (step for step in reversed(prior_steps) if step.get("source") == "user"),
        {},
    )
    recent = prior_steps[-4:]
    return {
        "trajectory_path": trajectory_path,
        "target_step_id": steps[best_index].get("step_id"),
        "target_step_timestamp": steps[best_index].get("timestamp"),
        "target_step_delta_seconds": round(best_delta, 3),
        "latest_user_context_preview": clip(latest_user.get("message") or ""),
        "recent_context_preview": [
            {
                "step_id": step.get("step_id"),
                "source": step.get("source"),
                "message": clip(step.get("message") or "", 700),
            }
            for step in recent
        ],
        "next_agent_message_preview": clip(steps[best_index].get("message") or "", 900),
    }


def pair_decisions(
    row: dict[str, Any], traces: list[dict[str, Any]], trajectory_path: str, trajectory_steps: list[dict[str, Any]]
) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    decision_traces = [trace for trace in traces if is_decision_trace(trace)]
    agent_traces = [trace for trace in traces if not is_decision_trace(trace)]
    used_agent_ids: set[int] = set()
    records: list[dict[str, Any]] = []

    for index, decision in enumerate(decision_traces, start=1):
        decision_ts = iso_to_dt(decision.get("timestamp") or "")
        next_agent_index = None
        for agent_index, agent in enumerate(agent_traces):
            if agent_index in used_agent_ids:
                continue
            if iso_to_dt(agent.get("timestamp") or "") >= decision_ts:
                next_agent_index = agent_index
                break

        paired_agent = agent_traces[next_agent_index] if next_agent_index is not None else {}
        if next_agent_index is not None:
            used_agent_ids.add(next_agent_index)

        raw_reason = paired_agent.get("routing_reason") or ""
        selected_model = paired_agent.get("routed_model") or paired_agent.get("model") or ""
        context = trajectory_context(
            trajectory_path, trajectory_steps, paired_agent.get("timestamp") or ""
        )
        record = {
            "job": row.get("job"),
            "task": row.get("task"),
            "strategy": row.get("strategy"),
            "attempt": as_int(row.get("attempt")),
            "prompt_id": row.get("prompt_id"),
            "session_id": row.get("trace_key") or decision.get("session_id") or "",
            "trial_name": decision.get("trial_name") or paired_agent.get("trial_name") or "",
            "reward": as_float(row.get("reward")),
            "failure_kind": row.get("failure_kind") or "",
            "decision_index": index,
            "phase_guess": phase_from_index(index - 1),
            "decision_timestamp": decision.get("timestamp") or "",
            "decision_model": decision.get("routed_model") or decision.get("model") or "",
            "decision_step": decision.get("step_name") or "",
            "decision_status": as_int(decision.get("status")),
            "decision_tokens": as_int(decision.get("total_tokens")),
            "decision_cost_usd": as_float(decision.get("cost")),
            "decision_latency_ms": as_int(decision.get("latency_ms")),
            "selected_model": selected_model,
            "router_reason": clean_reason(raw_reason),
            "agent_timestamp": paired_agent.get("timestamp") or "",
            "agent_status": as_int(paired_agent.get("status")),
            "agent_tokens": as_int(paired_agent.get("total_tokens")),
            "agent_cost_usd": as_float(paired_agent.get("cost")),
            "agent_latency_ms": as_int(paired_agent.get("latency_ms")),
            "agent_trace_id": paired_agent.get("trace_id") or "",
            "gap_seconds": (
                round((iso_to_dt(paired_agent.get("timestamp") or "") - decision_ts).total_seconds(), 3)
                if paired_agent
                else None
            ),
            **context,
        }
        records.append(record)

    unmatched_agents = [
        agent
        for index, agent in enumerate(agent_traces)
        if index not in used_agent_ids
    ]
    run_summary = {
        "job": row.get("job"),
        "task": row.get("task"),
        "strategy": row.get("strategy"),
        "attempt": as_int(row.get("attempt")),
        "reward": as_float(row.get("reward")),
        "failure_kind": row.get("failure_kind") or "",
        "agent_calls_reported": as_int(row.get("agent_call_count")),
        "decision_calls_reported": as_int(row.get("decision_call_count")),
        "agent_traces": len(agent_traces),
        "decision_traces": len(decision_traces),
        "paired_decisions": len(records),
        "unmatched_agent_traces": len(unmatched_agents),
        "unmatched_agent_models": dict(Counter(a.get("routed_model") or a.get("model") or "" for a in unmatched_agents)),
    }
    return records, run_summary


def build_dataset(artifact_dir: Path, include_canary: bool, prompt_id: str) -> dict[str, Any]:
    rows = read_analysis_rows(artifact_dir, include_canary)
    decisions: list[dict[str, Any]] = []
    runs: list[dict[str, Any]] = []
    for row in rows:
        row["prompt_id"] = prompt_id
        traces = load_traces(artifact_dir, row.get("job") or "", row.get("trace_key") or "")
        trajectory_path, trajectory_steps = load_trajectory(artifact_dir, row.get("job") or "")
        records, run_summary = pair_decisions(row, traces, trajectory_path, trajectory_steps)
        decisions.extend(records)
        runs.append(run_summary)

    by_strategy: dict[str, dict[str, Any]] = {}
    for strategy in sorted(SMART_STRATEGIES):
        strategy_runs = [run for run in runs if run["strategy"] == strategy]
        strategy_decisions = [d for d in decisions if d["strategy"] == strategy]
        if not strategy_runs and not strategy_decisions:
            continue
        by_strategy[strategy] = {
            "runs": len(strategy_runs),
            "passed_runs": sum(1 for run in strategy_runs if run["reward"] == 1),
            "agent_calls": sum(run["agent_calls_reported"] for run in strategy_runs),
            "decision_calls": sum(run["decision_calls_reported"] for run in strategy_runs),
            "paired_decisions": len(strategy_decisions),
            "decision_coverage": (
                sum(run["decision_calls_reported"] for run in strategy_runs)
                / max(1, sum(run["agent_calls_reported"] for run in strategy_runs))
            ),
            "decision_cost_usd": sum(d["decision_cost_usd"] for d in strategy_decisions),
            "paired_agent_cost_usd": sum(d["agent_cost_usd"] for d in strategy_decisions),
            "model_mix": dict(Counter(d["selected_model"] or "(unpaired)" for d in strategy_decisions)),
        }

    summary = {
        "runs": len(runs),
        "passed_runs": sum(1 for run in runs if run["reward"] == 1),
        "agent_calls": sum(run["agent_calls_reported"] for run in runs),
        "decision_calls": sum(run["decision_calls_reported"] for run in runs),
        "paired_decisions": len(decisions),
        "decision_coverage": sum(run["decision_calls_reported"] for run in runs)
        / max(1, sum(run["agent_calls_reported"] for run in runs)),
        "decision_cost_usd": sum(d["decision_cost_usd"] for d in decisions),
        "paired_agent_cost_usd": sum(d["agent_cost_usd"] for d in decisions),
        "by_strategy": by_strategy,
    }
    return {
        "schema_version": "router-decision-lab.v1",
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "artifact_dir": str(artifact_dir),
        "prompt_id": prompt_id,
        "summary": summary,
        "runs": runs,
        "decisions": decisions,
    }


def html_page(dataset: dict[str, Any]) -> str:
    data = json.dumps(dataset, ensure_ascii=False)
    escaped_data = html.escape(data, quote=False)
    title = "Smart Router Decision Lab"
    return f"""<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{title}</title>
  <style>
    :root {{
      color-scheme: light;
      --ink: #18202a;
      --muted: #667085;
      --line: #d9dee7;
      --paper: #f7f8fb;
      --panel: #ffffff;
      --flash: #0f8b8d;
      --opus: #b54708;
      --good: #147a42;
      --bad: #b42318;
      --accent: #344054;
    }}
    * {{ box-sizing: border-box; }}
    body {{
      margin: 0;
      font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: var(--ink);
      background: var(--paper);
    }}
    header {{
      padding: 28px 32px 22px;
      border-bottom: 1px solid var(--line);
      background: var(--panel);
    }}
    h1 {{ margin: 0 0 8px; font-size: 30px; line-height: 1.15; letter-spacing: 0; }}
    h2 {{ margin: 0 0 14px; font-size: 18px; line-height: 1.2; letter-spacing: 0; }}
    p {{ margin: 0; color: var(--muted); line-height: 1.55; max-width: 960px; }}
    main {{ padding: 24px 32px 36px; display: grid; gap: 20px; }}
    .controls {{
      display: grid;
      grid-template-columns: repeat(5, minmax(140px, 1fr));
      gap: 10px;
      align-items: end;
    }}
    label {{ display: grid; gap: 6px; color: var(--muted); font-size: 12px; font-weight: 650; }}
    select, input {{
      width: 100%;
      min-height: 38px;
      border: 1px solid var(--line);
      border-radius: 6px;
      padding: 8px 10px;
      background: var(--panel);
      color: var(--ink);
      font: inherit;
    }}
    .band {{
      background: var(--panel);
      border-top: 1px solid var(--line);
      border-bottom: 1px solid var(--line);
      padding: 18px;
    }}
    .metrics {{
      display: grid;
      grid-template-columns: repeat(5, minmax(130px, 1fr));
      gap: 12px;
    }}
    .metric {{
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 14px;
      background: #fff;
      min-height: 92px;
    }}
    .metric .label {{ color: var(--muted); font-size: 12px; font-weight: 700; }}
    .metric .value {{ font-size: 26px; font-weight: 760; margin-top: 8px; line-height: 1.1; }}
    .metric .note {{ color: var(--muted); font-size: 12px; margin-top: 6px; line-height: 1.35; }}
    .grid {{
      display: grid;
      grid-template-columns: minmax(280px, 0.85fr) minmax(360px, 1.15fr);
      gap: 20px;
      align-items: start;
    }}
    .chartRow {{
      display: grid;
      grid-template-columns: minmax(170px, 220px) 1fr 82px;
      gap: 10px;
      align-items: center;
      min-height: 30px;
      margin: 8px 0;
      font-size: 13px;
    }}
    .barWrap {{ background: #eef1f6; height: 12px; border-radius: 999px; overflow: hidden; }}
    .bar {{ height: 12px; width: 0; background: var(--accent); }}
    .bar.flash {{ background: var(--flash); }}
    .bar.opus {{ background: var(--opus); }}
    .bar.good {{ background: var(--good); }}
    .bar.bad {{ background: var(--bad); }}
    .tableWrap {{ overflow: auto; border: 1px solid var(--line); border-radius: 8px; background: #fff; }}
    table {{ width: 100%; border-collapse: collapse; min-width: 1120px; }}
    th, td {{ padding: 9px 10px; border-bottom: 1px solid #edf0f5; text-align: left; vertical-align: top; }}
    th {{
      position: sticky;
      top: 0;
      background: #f2f4f7;
      color: #475467;
      font-size: 12px;
      z-index: 1;
    }}
    td {{ font-size: 13px; line-height: 1.35; }}
    .pill {{
      display: inline-flex;
      align-items: center;
      min-height: 24px;
      border-radius: 999px;
      padding: 3px 8px;
      font-size: 12px;
      font-weight: 700;
      border: 1px solid var(--line);
      white-space: nowrap;
    }}
    .pill.flash {{ color: #07595b; background: #e6f7f7; border-color: #9cd5d7; }}
    .pill.opus {{ color: #8a3a05; background: #fff2e4; border-color: #f7c58c; }}
    .pill.pass {{ color: var(--good); background: #e9f7ef; border-color: #a8dfbf; }}
    .pill.fail {{ color: var(--bad); background: #fff0ee; border-color: #f4b0aa; }}
    .reason {{ max-width: 520px; color: #344054; }}
    .small {{ color: var(--muted); font-size: 12px; }}
    @media (max-width: 980px) {{
      header, main {{ padding-left: 16px; padding-right: 16px; }}
      .controls, .metrics, .grid {{ grid-template-columns: 1fr; }}
      h1 {{ font-size: 24px; }}
    }}
  </style>
</head>
<body>
  <header>
    <h1>Smart Router Decision Lab</h1>
    <p>每一行是一条路由决策：GPT-5.6 判断下一步该用 Flash 还是 Opus，然后我们观察这个选择后面的成本、状态和整轮任务结果。</p>
  </header>
  <main>
    <section class="band">
      <div class="controls">
        <label>策略<select id="strategy"></select></label>
        <label>任务<select id="task"></select></label>
        <label>模型<select id="model"></select></label>
        <label>结果<select id="outcome"></select></label>
        <label>理由搜索<input id="search" placeholder="security / final / debug"></label>
      </div>
    </section>
    <section class="metrics" id="metrics"></section>
    <section class="grid">
      <div class="band">
        <h2>模型选择</h2>
        <div id="modelMix"></div>
      </div>
      <div class="band">
        <h2>任务与策略</h2>
        <div id="taskStrategy"></div>
      </div>
    </section>
    <section class="band">
      <h2>决策流水</h2>
      <div class="tableWrap">
        <table>
          <thead>
            <tr>
              <th>#</th>
              <th>任务</th>
              <th>策略</th>
              <th>轮次</th>
	              <th>选择</th>
	              <th>阶段</th>
	              <th>理由</th>
	              <th>上下文</th>
	              <th>决策成本</th>
              <th>后续成本</th>
              <th>状态</th>
            </tr>
          </thead>
          <tbody id="rows"></tbody>
        </table>
      </div>
    </section>
  </main>
  <script id="dataset" type="application/json">{escaped_data}</script>
  <script>
    const dataset = JSON.parse(document.getElementById('dataset').textContent);
    const decisions = dataset.decisions || [];
    const $ = (id) => document.getElementById(id);
    const fmtUsd = (n) => '$' + Number(n || 0).toFixed(4);
	    const pct = (n, d) => d ? Math.round((n / d) * 1000) / 10 + '%' : '0%';
	    const modelClass = (m) => m.includes('glm') || m.includes('flash') ? 'flash' : (m.includes('opus') ? 'opus' : '');
	    const shortModel = (m) => m.includes('glm') ? 'GLM Flash' : (m.includes('opus') ? 'Opus' : (m || 'unpaired'));
	    const esc = (s) => String(s || '').replace(/[&<>"']/g, c => ({{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}}[c]));

    function optionsFor(field, label) {{
      const values = [...new Set(decisions.map(d => d[field]).filter(Boolean))].sort();
      return ['<option value="">全部' + label + '</option>'].concat(values.map(v => `<option>${{v}}</option>`)).join('');
    }}

    $('strategy').innerHTML = optionsFor('strategy', '策略');
    $('task').innerHTML = optionsFor('task', '任务');
    $('model').innerHTML = '<option value="">全部模型</option>' +
      [...new Set(decisions.map(d => shortModel(d.selected_model)))].sort().map(v => `<option>${{v}}</option>`).join('');
    $('outcome').innerHTML = '<option value="">全部结果</option><option value="pass">完成</option><option value="fail">未完成</option>';

    for (const id of ['strategy', 'task', 'model', 'outcome', 'search']) {{
      $(id).addEventListener('input', render);
    }}

    function filtered() {{
      const strategy = $('strategy').value;
      const task = $('task').value;
      const model = $('model').value;
      const outcome = $('outcome').value;
      const search = $('search').value.trim().toLowerCase();
      return decisions.filter(d => {{
        if (strategy && d.strategy !== strategy) return false;
        if (task && d.task !== task) return false;
        if (model && shortModel(d.selected_model) !== model) return false;
        if (outcome === 'pass' && Number(d.reward) !== 1) return false;
        if (outcome === 'fail' && Number(d.reward) === 1) return false;
        if (search && !(d.router_reason || '').toLowerCase().includes(search)) return false;
        return true;
      }});
    }}

    function countBy(rows, keyFn) {{
      const out = new Map();
      for (const row of rows) {{
        const key = keyFn(row);
        out.set(key, (out.get(key) || 0) + 1);
      }}
      return [...out.entries()].sort((a, b) => b[1] - a[1] || String(a[0]).localeCompare(String(b[0])));
    }}

    function sum(rows, key) {{
      return rows.reduce((acc, row) => acc + Number(row[key] || 0), 0);
    }}

    function bars(target, rows, keyFn, classFn = () => '') {{
      const counts = countBy(rows, keyFn);
      const max = Math.max(1, ...counts.map(([, v]) => v));
      target.innerHTML = counts.map(([name, value]) => `
        <div class="chartRow">
          <div>${{name}}</div>
          <div class="barWrap"><div class="bar ${{classFn(name)}}" style="width:${{Math.max(3, value / max * 100)}}%"></div></div>
          <div class="small">${{value}} · ${{pct(value, rows.length)}}</div>
        </div>
      `).join('') || '<div class="small">没有匹配数据</div>';
    }}

    function render() {{
      const rows = filtered();
      const passRows = rows.filter(d => Number(d.reward) === 1).length;
      const opusRows = rows.filter(d => shortModel(d.selected_model) === 'Opus').length;
      const flashRows = rows.filter(d => shortModel(d.selected_model) === 'GLM Flash').length;
      $('metrics').innerHTML = `
        <div class="metric"><div class="label">决策记录</div><div class="value">${{rows.length}}</div><div class="note">当前筛选后的 router decisions</div></div>
        <div class="metric"><div class="label">任务完成率</div><div class="value">${{pct(passRows, rows.length)}}</div><div class="note">按该决策所在 trial 的最终 reward</div></div>
        <div class="metric"><div class="label">Opus 选择率</div><div class="value">${{pct(opusRows, rows.length)}}</div><div class="note">${{opusRows}} 次选择 Opus</div></div>
        <div class="metric"><div class="label">Flash 选择率</div><div class="value">${{pct(flashRows, rows.length)}}</div><div class="note">${{flashRows}} 次选择 Flash</div></div>
        <div class="metric"><div class="label">可归因成本</div><div class="value">${{fmtUsd(sum(rows, 'decision_cost_usd') + sum(rows, 'agent_cost_usd'))}}</div><div class="note">决策模型 + 配对后的 agent call</div></div>
      `;
      bars($('modelMix'), rows, d => shortModel(d.selected_model), modelClass);
      bars($('taskStrategy'), rows, d => `${{d.task}} / ${{d.strategy}}`, name => name.includes('warmstart') ? 'good' : 'bad');
      $('rows').innerHTML = rows.map((d, idx) => `
        <tr>
          <td>${{idx + 1}}</td>
	          <td>${{esc(d.task)}}</td>
	          <td>${{esc(d.strategy)}}</td>
	          <td>a${{d.attempt}} · #${{d.decision_index}}</td>
	          <td><span class="pill ${{modelClass(d.selected_model)}}">${{shortModel(d.selected_model)}}</span></td>
	          <td>${{esc(d.phase_guess)}}</td>
	          <td class="reason">${{d.router_reason ? esc(d.router_reason) : '<span class="small">无理由文本</span>'}}</td>
	          <td class="reason"><details><summary>step ${{d.target_step_id || '-'}}</summary><div class="small">${{esc(d.latest_user_context_preview || '')}}</div></details></td>
          <td>${{fmtUsd(d.decision_cost_usd)}}<div class="small">${{d.decision_tokens}} tok</div></td>
          <td>${{fmtUsd(d.agent_cost_usd)}}<div class="small">${{d.agent_latency_ms}} ms</div></td>
          <td><span class="pill ${{Number(d.reward) === 1 ? 'pass' : 'fail'}}">${{Number(d.reward) === 1 ? '完成' : '未完成'}}</span></td>
        </tr>
      `).join('');
    }}

    render();
  </script>
</body>
</html>
"""


def main() -> None:
    args = parse_args()
    artifact_dir = Path(args.artifact_dir).resolve()
    out_json = Path(args.out_json).resolve() if args.out_json else artifact_dir / "router-decisions.json"
    out_html = Path(args.out_html).resolve() if args.out_html else artifact_dir / "router-decision-lab.html"
    dataset = build_dataset(artifact_dir, args.include_canary, args.prompt_id)
    out_json.write_text(json.dumps(dataset, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    out_html.write_text(html_page(dataset), encoding="utf-8")
    summary = dataset["summary"]
    print(f"wrote {out_json}")
    print(f"wrote {out_html}")
    print(
        "decisions={paired_decisions} agent_calls={agent_calls} "
        "decision_calls={decision_calls} coverage={coverage:.1%}".format(
            paired_decisions=summary["paired_decisions"],
            agent_calls=summary["agent_calls"],
            decision_calls=summary["decision_calls"],
            coverage=summary["decision_coverage"],
        )
    )


if __name__ == "__main__":
    main()
