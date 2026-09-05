#!/usr/bin/env python3
"""Render a standalone HTML comparison for router decision replay output."""

from __future__ import annotations

import argparse
import html
import json
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Render router replay comparison HTML.")
    parser.add_argument(
        "--input",
        default="/mnt/data2/aware-gateway-runs/aware-v4-20260902T115134Z/router-decision-replay-cost-aware-v1.json",
    )
    parser.add_argument("--output", default=None)
    return parser.parse_args()


def short_model(model: str) -> str:
    if "glm" in model or "flash" in model:
        return "Flash"
    if "opus" in model:
        return "Opus"
    return "Empty"


def pct(num: int, den: int) -> str:
    return "0%" if den == 0 else f"{num / den * 100:.1f}%"


def group_rows(rows: list[dict[str, Any]], field: str) -> list[dict[str, Any]]:
    grouped: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for row in rows:
        grouped[str(row.get(field) or "")].append(row)
    out = []
    for name, items in sorted(grouped.items()):
        original = Counter(short_model(row.get("original_model") or "") for row in items)
        replay = Counter(short_model(row.get("replay_model") or "") for row in items)
        switched = sum(
            1
            for row in items
            if row.get("replay_model") and row.get("replay_model") != row.get("original_model")
        )
        out.append(
            {
                "name": name,
                "rows": len(items),
                "original": dict(original),
                "replay": dict(replay),
                "switched": switched,
            }
        )
    return out


def render(payload: dict[str, Any]) -> str:
    rows = payload.get("replay_decisions", [])
    summary = payload.get("summary", {})
    data = {
        "prompt_id": payload.get("prompt_id"),
        "decision_model": payload.get("decision_model"),
        "summary": summary,
        "by_task": group_rows(rows, "task"),
        "by_strategy": group_rows(rows, "strategy"),
        "rows": rows,
    }
    embedded = html.escape(json.dumps(data, ensure_ascii=False), quote=False)
    return f"""<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Router Prompt Replay</title>
  <style>
    :root {{
      --ink:#17202c; --muted:#667085; --line:#d9dee7; --paper:#f7f8fb;
      --panel:#fff; --flash:#0f8b8d; --opus:#b54708; --switch:#344054;
    }}
    * {{ box-sizing:border-box; }}
    body {{ margin:0; font-family:ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; color:var(--ink); background:var(--paper); }}
    header {{ padding:28px 32px 22px; background:var(--panel); border-bottom:1px solid var(--line); }}
    h1 {{ margin:0 0 8px; font-size:30px; line-height:1.15; letter-spacing:0; }}
    h2 {{ margin:0 0 14px; font-size:18px; letter-spacing:0; }}
    p {{ margin:0; color:var(--muted); line-height:1.55; max-width:960px; }}
    main {{ padding:24px 32px 36px; display:grid; gap:20px; }}
    .metrics {{ display:grid; grid-template-columns:repeat(5,minmax(130px,1fr)); gap:12px; }}
    .metric {{ min-height:92px; padding:14px; border:1px solid var(--line); border-radius:8px; background:#fff; }}
    .label {{ color:var(--muted); font-size:12px; font-weight:700; }}
    .value {{ font-size:26px; font-weight:760; margin-top:8px; line-height:1.1; }}
    .note {{ color:var(--muted); font-size:12px; margin-top:6px; }}
    .band {{ background:#fff; border-top:1px solid var(--line); border-bottom:1px solid var(--line); padding:18px; }}
    .grid {{ display:grid; grid-template-columns:1fr 1fr; gap:20px; }}
    .row {{ display:grid; grid-template-columns:minmax(170px,240px) 1fr 92px; gap:10px; align-items:center; min-height:34px; margin:8px 0; font-size:13px; }}
    .bars {{ display:grid; grid-template-columns:1fr 1fr; gap:6px; }}
    .barWrap {{ height:12px; background:#eef1f6; border-radius:999px; overflow:hidden; }}
    .bar {{ height:12px; width:0; }}
    .flash {{ background:var(--flash); }}
    .opus {{ background:var(--opus); }}
    .switch {{ background:var(--switch); }}
    table {{ width:100%; min-width:980px; border-collapse:collapse; }}
    .tableWrap {{ overflow:auto; border:1px solid var(--line); border-radius:8px; background:#fff; }}
    th,td {{ padding:9px 10px; border-bottom:1px solid #edf0f5; text-align:left; vertical-align:top; font-size:13px; }}
    th {{ position:sticky; top:0; background:#f2f4f7; color:#475467; font-size:12px; }}
    .pill {{ display:inline-flex; min-height:24px; align-items:center; border-radius:999px; padding:3px 8px; font-size:12px; font-weight:700; border:1px solid var(--line); white-space:nowrap; }}
    .pill.flash {{ color:#07595b; background:#e6f7f7; border-color:#9cd5d7; }}
    .pill.opus {{ color:#8a3a05; background:#fff2e4; border-color:#f7c58c; }}
    .small {{ color:var(--muted); font-size:12px; }}
    @media (max-width:980px) {{ header,main {{ padding-left:16px; padding-right:16px; }} .metrics,.grid {{ grid-template-columns:1fr; }} h1 {{ font-size:24px; }} }}
  </style>
</head>
<body>
  <header>
    <h1>Router Prompt Replay</h1>
    <p>固定同一批 trajectory 上下文，只把决策提示词换成 cost-aware-v1，观察模型选择比例如何变化。</p>
  </header>
  <main>
    <section class="metrics" id="metrics"></section>
    <section class="grid">
      <div class="band"><h2>按任务</h2><div id="byTask"></div></div>
      <div class="band"><h2>按策略</h2><div id="byStrategy"></div></div>
    </section>
    <section class="band">
      <h2>切换样本</h2>
      <div class="tableWrap"><table><thead><tr><th>#</th><th>任务</th><th>策略</th><th>Step</th><th>原始</th><th>重判</th><th>重判理由</th></tr></thead><tbody id="rows"></tbody></table></div>
    </section>
  </main>
  <script id="data" type="application/json">{embedded}</script>
  <script>
    const data = JSON.parse(document.getElementById('data').textContent);
    const esc = (s) => String(s || '').replace(/[&<>"']/g, c => ({{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}}[c]));
    const sm = (m) => String(m || '').includes('glm') || String(m || '').includes('flash') ? 'Flash' : (String(m || '').includes('opus') ? 'Opus' : 'Empty');
    const cls = (m) => sm(m).toLowerCase();
    const pct = (n,d) => d ? (n/d*100).toFixed(1)+'%' : '0%';
    const mix = (obj, key) => obj[key] || 0;
    const s = data.summary;
    document.getElementById('metrics').innerHTML = `
      <div class="metric"><div class="label">重判成功</div><div class="value">${{s.ok}} / ${{s.rows}}</div><div class="note">${{data.decision_model}}</div></div>
      <div class="metric"><div class="label">原始 Opus</div><div class="value">${{pct(mix(s.original_model_mix,'anthropic/claude-opus-5'), s.rows)}}</div><div class="note">${{mix(s.original_model_mix,'anthropic/claude-opus-5')}} 条</div></div>
      <div class="metric"><div class="label">重判 Opus</div><div class="value">${{pct(mix(s.replay_model_mix,'anthropic/claude-opus-5'), s.ok)}}</div><div class="note">${{mix(s.replay_model_mix,'anthropic/claude-opus-5')}} 条</div></div>
      <div class="metric"><div class="label">重判 Flash</div><div class="value">${{pct(mix(s.replay_model_mix,'z-ai/glm-5.3-flash'), s.ok)}}</div><div class="note">${{mix(s.replay_model_mix,'z-ai/glm-5.3-flash')}} 条</div></div>
      <div class="metric"><div class="label">切换率</div><div class="value">${{pct(s.switched, s.ok)}}</div><div class="note">${{s.switched}} 条改变选择</div></div>
    `;
    function renderGroup(id, rows) {{
      document.getElementById(id).innerHTML = rows.map(r => {{
        const flash = mix(r.replay, 'Flash'), opus = mix(r.replay, 'Opus'), max = Math.max(1, flash, opus);
        return `<div class="row"><div>${{esc(r.name)}}<div class="small">${{r.rows}} 条，切换 ${{r.switched}}</div></div><div class="bars"><div class="barWrap"><div class="bar flash" style="width:${{Math.max(3, flash/max*100)}}%"></div></div><div class="barWrap"><div class="bar opus" style="width:${{Math.max(3, opus/max*100)}}%"></div></div></div><div class="small">F ${{pct(flash,r.rows)}}<br>O ${{pct(opus,r.rows)}}</div></div>`;
      }}).join('');
    }}
    renderGroup('byTask', data.by_task);
    renderGroup('byStrategy', data.by_strategy);
    const switched = data.rows.filter(r => r.replay_model && r.replay_model !== r.original_model).slice(0, 120);
    document.getElementById('rows').innerHTML = switched.map((r,i) => `<tr><td>${{i+1}}</td><td>${{esc(r.task)}}</td><td>${{esc(r.strategy)}}</td><td>${{r.target_step_id || ''}}</td><td><span class="pill ${{cls(r.original_model)}}">${{sm(r.original_model)}}</span></td><td><span class="pill ${{cls(r.replay_model)}}">${{sm(r.replay_model)}}</span></td><td>${{esc(r.replay_reason)}}</td></tr>`).join('');
  </script>
</body>
</html>
"""


def main() -> None:
    args = parse_args()
    input_path = Path(args.input).resolve()
    output_path = (
        Path(args.output).resolve()
        if args.output
        else input_path.with_name(input_path.stem + ".html")
    )
    payload = json.loads(input_path.read_text(encoding="utf-8"))
    output_path.write_text(render(payload), encoding="utf-8")
    print(output_path)


if __name__ == "__main__":
    main()
