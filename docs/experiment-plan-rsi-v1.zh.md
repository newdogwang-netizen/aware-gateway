# aware-gateway RSI 打磨实验 V1

## 2026-09-08 架构评审修订

外部评审结论是：方向可以保留，但实验设计在正式开工前必须修五个问题：

- `finish_reason=stop` 不能被当成任务完成，只能表示本次响应正常结束。
- `activity` 不能被当成 `progress`；重复改文件、重复跑测试、输出更多 token 都不天然代表任务推进。
- 离线 replay 必须有严格时间截止，任何 decision 只能读取它发生前的事件。
- 验收不能只和 A4/P0 比，还要保留 best-known 历史样本和 premium-only 质量锚点。
- 单次 pilot 只能筛掉明显坏策略，不能接受策略；正式验收需要重复运行。

本文档已按这些约束修订。2026-09-08 的 R1 工程状态是：

```text
Concept approved.
Outcome extractor foundation implemented.
Policy replay and Harbor pilot still gated by extractor audit.
```

## RSI 在本项目里的含义

这里的 RSI 取 Recursive Self-Improvement 的思想，但工程落点改成
**Router Self-Improvement**：

```text
真实运行数据 -> 找出策略问题 -> 生成候选策略 -> 离线重放 -> 小流量实跑 ->
验收/回滚 -> 写回下一轮策略
```

它不是让 agent 自动改代码并直接上线。aware-gateway 的改进必须经过固定门禁：
单元测试、确定性 probe、离线 replay、小规模 Harbor pilot、成本止损线和人工验收。

RSI 的输出也不能直接改生产策略。它只能生成 candidate policy；candidate 必须通过
replay screening 和重复 pilot acceptance 后，才允许进入 canary。

## 当前切入点

当前已经完成：

- Prompt Router
- Safe-control 本地规则
- Session 级决策历史
- Budgeted Route Action
- 最小 Episode 投影
- `finish_reason=length` 动态预算反馈
- `stale/blocked` no-progress 状态触发本地 recovery 路由
- provider incomplete、成本超线、agent-call 无有效进展、length pressure 和 blocked recovery 的本地 stop gate

A5 说明了新的主要矛盾：

```text
Episode 反馈链路能工作，
但只根据 length 放大预算，不能判断任务是否真的在前进。
```

所以 RSI 第一轮不继续加关键词规则，也不继续手调一组固定 token 数。
它要补的是 **Outcome-aware policy**：让路由策略看到“动作是否产生有效进展”。

## 实验目标

用受控的自我改进闭环，打磨 aware-gateway 的智能路由策略：

- 保持任务完成质量，不牺牲 hidden verifier reward。
- 降低整条 trajectory 成本，而不是只降低单次调用价格。
- 减少无效循环：反复截断、反复验证、反复 premium recovery 但没有文件/测试进展。
- 让每一轮策略变化都可解释、可复现、可回滚。

## GQM

### Goal

从网关策略维护者的角度，验证 aware-gateway 是否能用自身 trace 和 outcome 数据，
持续改进 smart-router 的成本/质量表现。

### Questions

1. 新策略是否保持或提升 solved rate？
2. 新策略是否降低 solved task 的总成本？
3. 新策略是否减少没有进展的 agent 循环？
4. 新策略是否比上一版更少依赖 semantic judge？
5. 新策略是否能泛化到未参与策略生成的任务？
6. 每次策略变化是否能追溯到具体证据，而不是凭感觉改 prompt？

### Metrics

- `reward`
- `total_cost_usd`
- `agent_call_count`
- `decision_call_count`
- `judge_call_rate`
- `premium_call_count`
- `flash_call_count`
- `length_finish_count`
- `length_finish_rate`
- `episode_adjust_call_count`
- `file_modified_count`
- `test_failed_count`
- `test_passed_count`
- `verifier_reward`
- `no_progress_turn_count`
- `failure_frontier_size`
- `failure_frontier_reduced_count`
- `cost_per_attempt`
- `cost_per_success`
- `stopped_cost_usd`
- `provider_failure_cost_usd`

速度只作为诊断指标，不作为优化目标。

成本口径：

```text
cost_per_attempt = total_cost_of_all_runs / run_count
cost_per_success = total_cost_of_all_runs / successful_run_count
episode_summary.total_cost_usd = agent_cost_usd + decision_cost_usd
```

失败、手动停止和 provider failure 已经产生的成本都必须进入总成本。Provider 故障可以单独分层解释，
但不能从成本里静默删除。

## 实验假设

### H1: Outcome 事件能改善预算决策

如果 Episode state 不只记录 LLM 调用结果，还记录文件修改、测试结果和 verifier
结果，那么 router 能更早区分：

- 输出空间不够，需要更大预算；
- 当前方向错误，需要 premium recovery；
- 正在原地打转，需要停止 cheap/premium 循环；
- 已经接近交付，需要 final guardrail。

验收标准：

- 在匹配条件下，成本不高于新跑的 P0-current，且 reward 不低于 P0-current。
- 在 `shadow-relay` 上，不能明显差于 best-known 历史样本：A1 `$2.2468` / reward `1.0`
  和 A4 `$3.4808` / reward `1.0` 都作为解释锚点。
- `length_finish_rate` 低于 A4 的 `47.7%`。
- `episode_adjust` 触发后，后续 3 轮内应出现至少一个进展事件；否则记为 no-progress loop。

### H2: 离线 replay 能提前淘汰坏策略

同一批历史 router decision context，用不同策略重跑决策。如果新策略在 replay 中表现出
明显的 always-Opus、always-Flash、预算持续放大或恢复循环倾向，就不进入真实 Harbor。
Replay 样本必须只读取该次 decision 之前已经发生的事件：

```text
state_at_decision_t = reduce(events where event.timestamp < decision_t)
```

验收标准：

- `future_evidence_leakage = 0`。
- replay 后 premium share 不超过基准策略的 `+20%`，除非 reason 明确指向恢复或最终确认。
- `budget_action` 分布不退化成单一动作。
- 关键失败样本能给出和 `allowed_evidence_refs` 一致的 decision reason。

### H3: RSI 不能只在一个任务上过拟合

策略候选来自训练任务，但必须在 holdout 任务上保持质量和成本边界。

验收标准：

- 至少 3 类任务：协议/调试类、数据处理类、算法修复类。
- 用于生成策略的任务不参与最终 holdout 验收。
- holdout 不要求一次达到最优成本，但不能出现 A5 式成本失控。

## 基线设计

RSI R1 使用三类基线，避免候选策略只比一个弱版本好：

| 基线 | 作用 | 说明 |
|------|------|------|
| P0-current | 公平主对照 | 在同任务、同时间窗口、同 provider 条件下重跑当前 main 策略 |
| P-best-known | 历史质量/成本锚点 | 防止候选只优于 A4/A5，却低于 A1 这类已知更好轨迹 |
| Premium-only | 质量上限锚点 | 估计强模型在相同任务上的完成率、轨迹长度和成本范围 |

正式验收以 P0-current 为主对照，P-best-known 和 Premium-only 只用于解释边界，
不能替代匹配条件下的新基线。

## 实验分组

### P0: 当前策略

当前 `main` 的 smart-router：

- safe-control
- budgeted route
- decision history
- minimal episode
- recent length pressure

作用：作为 RSI 第一轮 baseline。

### C1 / P1: Outcome-aware Prompt Policy

只改决策提示词和 replay 脚本，不改运行时核心：

- prompt 输入增加 compact outcome state；
- decision schema 增加 `progress_state`；
- decision reason 必须引用具体 outcome evidence；
- budget action 选择必须说明是“需要更多空间”还是“需要换方向”。

作用：先验证 prompt 层是否能利用 outcome 信号。

### C2: No-progress Budget Freeze

只加入一个本地控制机制：

- length pressure 发生但没有 progress 时，不继续放大同类预算；
- 连续 no-progress 后强制进入 replan/recover，而不是重复执行；
- budget freeze 的 reason 必须引用 no-progress evidence。

作用：隔离验证“停止无效预算放大”是否有收益。

### C3: Repeated-failure Recovery

只加入一个本地恢复机制：

- failure fingerprint 重复但 failure frontier 不下降时，触发 premium recovery；
- recovery 后如果仍无进展，下一轮不能继续同样 recovery；
- 必须把 repeated failure 和新旧 failure frontier 写入 trace reason。

作用：隔离验证“重复失败恢复”是否有收益。

### P2: Outcome-aware State Controller

只有 C1/C2/C3 的 replay 和 screening 结果都可解释后，才组合成 P2：

- 连续 no-progress 后禁止继续同类 budget 放大；
- 文件没有变化但多次测试失败时，强制 premium recovery；
- 测试通过后进入 `completion_readiness`，再由 premium assess 判断证据是否足够；
- premium recovery 后若仍无进展，触发本地 stop gate，避免继续花上游调用把同一条坏轨迹跑长。

作用：验证事件驱动控制是否比纯 prompt 更稳。

原则：每个 candidate 只改变一个主要机制。若一次同时改 prompt、budget profile 和
controller threshold，即使结果变好也无法归因。

## 数据与事件模型

### 已有事件

```text
llm_call:
  status
  finish_reason
  outcome
  model
  budget_action
  cost
  tokens
  latency_ms
```

`llm_call.outcome` 的语义必须先归一化：

```text
finish_reason=stop     -> response_completed
finish_reason=length   -> length_truncated
2xx missing metadata   -> provider_incomplete
HTTP/ErrorKind error   -> error
task_completed         -> completion protocol + validation/verifier evidence
```

注意：`response_completed` 只说明一次模型响应正常结束，不代表 benchmark 任务完成。

### 本轮新增的最小 Outcome 事件

```text
file_modified:
  path_count
  changed_paths_summary

test_failed:
  command
  failing_count
  failure_fingerprint

test_passed:
  command
  passing_count

verifier_result:
  reward
  passed_count
  failed_count

no_progress:
  reason
  since_turn
```

事件可以先从 Harbor trajectory、terminal transcript、audit trace 和 verifier 输出中离线抽取。
第一轮不要求实时完美采集；先保证状态可以重建，策略可以 replay。

### 事件证据层级

所有事件必须保留来源和证据引用：

```json
{
  "event_id": "evt-123",
  "episode_id": "shadow-relay__run1",
  "timestamp": "2026-09-08T10:00:00Z",
  "kind": "test_failed",
  "source": "terminal_transcript",
  "observation": {
    "command": "go test ./...",
    "exit_code": 1,
    "failure_fingerprint": "assert relay id"
  },
  "evidence_refs": ["transcript:lines:170-190"],
  "certainty": "observed",
  "extractor_version": "outcome-extractor-v1"
}
```

| 层级 | 含义 | 示例 |
|------|------|------|
| observed | 系统直接观察到的事实 | 命令、exit code、diff、verifier reward |
| derived | 确定性 reducer 计算出的状态 | failure frontier 降低、no-progress streak |
| inferred | 模型或启发式推断 | 核心假设可能错误、可能接近完成 |

Router 可以读取 inferred 信息，但不能把 inferred 重新写成 observed fact。

### Activity 与 Progress

Activity 不等于 Progress。Progress 必须表示任务状态向完成条件单调接近。

| 信号 | 示例 | 是否直接算进展 |
|------|------|----------------|
| Activity | 读文件、搜索、重复运行同一测试 | 否 |
| Artifact change | diff 发生变化 | 仅为候选进展 |
| Failure frontier | 失败集合从 5 个降低到 3 个 | 是 |
| Validation | 此前失败的测试通过 | 是 |
| Delivery | verifier 得分提升或通过 | 是 |
| Churn | diff 来回变化且 error fingerprint 不变 | 否 |

确定性定义：

```text
progress =
  failure_set_reduced
  OR previously_failing_test_passed
  OR verifier_score_improved
  OR persistent_diff_created_and_new_validation_passed

no_progress =
  N turns without failure-frontier, validation, or verifier improvement
```

## RSI 闭环步骤

### Step 1: 冻结基线

- 基线提交：`c329c64`
- 基线报告：Phase 2 Safe Control Report
- 基线样本：
  - A1 pass: `$2.2468`, 22 agent
  - A3 pass: `$3.7369`, 24 agent
  - A4 pass: `$3.4808`, 44 agent
  - A5 stopped: `$7.0576`, 59 agent, 18 episode_adjust

### Step 2: 建立 outcome extractor

新增离线脚本 `scripts/extract_episode_outcomes.py`，把每个 trial 的轨迹转成
episode timeline：

```text
trace rows + terminal transcript + verifier output
=> episode-events.jsonl
=> episode-summary.json
=> replay-cutoff-check.json
```

第一版已经落地的范围：

- 能识别文件是否被修改；
- 能从 CTRF/verifier 输出识别测试失败/成功；
- 能识别 verifier reward；
- 能计算 length pressure 后仍无进展的 `no_progress`；
- 能区分 `response_completed`、`length_truncated`、`provider_incomplete` 和 `error`；
- 能为每个事件保存 `timestamp`、`evidence_refs`、`certainty` 和 `extractor_version`。
- 能为每次 router decision 生成只包含历史事件的 cutoff replay 样本。
- 当 gateway trace 不存在时，能从 Harbor `trajectory.json` 生成基础 `llm_call`，
  但 outcome 必须保持 `unknown`，不能拿来做质量判断。

最小运行命令：

```bash
python3 scripts/extract_episode_outcomes.py \
  --trial-dir /path/to/harbor/trial-or-job-dir \
  --traces-json /path/to/gateway-traces.json \
  --output-dir /path/to/rsi-output \
  --strict
```

当前已用两条真实历史轨迹做 smoke：

| 样本 | 事件数 | Decision 样本 | future evidence leakage | 关键结论 |
|------|--------|---------------|-------------------------|----------|
| A4 `shadow-relay` pass | 48 | 23 | 0 | 可复现 21/44 length truncation，reward `1.0` |
| A5 `shadow-relay` stopped | 60 | 29 | 0 | 可复现 18 次 episode_adjust，且没有 verifier outcome |

还没纳入第一版 extractor 的范围：

- terminal transcript 的通用命令解析；
- repeated failure frontier 的跨测试集归约；
- 多任务线 `continue/interrupt/resume` 的实时 Episode Runtime。

进入 replay 前必须人工抽查关键事件。Observed event 的关键字段抽取准确率必须是 100%；
无法判断的事件保留 `unknown`，不能强行归类。

### Step 3: 生成候选策略

候选策略只能修改三类内容：

- decision prompt
- budget profile
- state controller thresholds

候选策略必须附带：

- 改动说明；
- 来源证据；
- 预期影响；
- 风险；
- 回滚条件。

### Step 4: 离线 replay

用历史 decision context 和新增 outcome state 生成新的 decision log：

```text
P0 current decisions
P1 outcome-aware prompt decisions
P2 outcome-aware controller decisions
```

每条 replay 样本必须保存：

```text
decision_id
decision_timestamp
event_cutoff
state_before
allowed_evidence_refs
original_decision
candidate_decision
candidate_reason
future_evidence_leakage
```

replay 只用于筛掉明显坏策略，不宣称等价真实 benchmark。它可以检查策略是否合法、
是否退化、是否符合 guardrail、reason 是否引用真实证据；它不能可信预测新策略下的
后续 agent 输出、真实调用数、最终成本和 solved rate。

#### 2026-09-08 P1 outcome-aware replay checkpoint

实现脚本：`scripts/replay_episode_decisions.py`。

输入：

- A4 `shadow-relay` pass 的 `episode-events.jsonl` / `replay-cutoff-check.json`
- A5 `shadow-relay` stopped 的 `episode-events.jsonl` / `replay-cutoff-check.json`

运行命令：

```bash
python3 scripts/replay_episode_decisions.py \
  --episode-dir /mnt/data2/aware-gateway-runs/rsi-r1-outcome-extractor-smoke-a4 \
  --episode-dir /mnt/data2/aware-gateway-runs/rsi-r1-outcome-extractor-smoke-a5 \
  --output /mnt/data2/aware-gateway-runs/rsi-r1-outcome-replay-p1-20260908T0820Z/router-replay-rsi-p1.json \
  --prompt-id rsi-p1-outcome-aware-v1 \
  --model openai/gpt-5.6-sol \
  --resume
```

结果：

| 指标 | 结果 |
|------|------|
| replay decisions | 52 |
| valid candidate decisions | 52 |
| future evidence leakage | 0 |
| reason evidence coverage | 100% |
| original model mix | Flash 35 / Opus 16 / unpaired 1 |
| P1 candidate model mix | Flash 36 / Opus 16 |
| switched decisions | 18 |
| dominant candidate action | `freeze_or_replan` 41/52 |
| reported/estimated replay decision cost | `$0.87543` |

解释：

P1 outcome-aware prompt 没有明显降低 Opus 占比，但大量触发 `freeze_or_replan`。
这说明它能读到 no-progress 风险，却过度依赖第一版粗粒度 `no_progress` 事件。

当前 P1 结论：

```text
Decision: keep for replay only
Do not ship as live policy.
```

原因：

- A4 是成功轨迹，但 P1 仍在中后段频繁要求 freeze/replan；
- 第一版 extractor 只在末尾看到 patch/verifier，缺少中途 terminal/test/file-write 事件；
- `no_progress` 事件一旦出现，在后续大多数 decision state 里都会持续存在；
- 因此 P1 的问题不是模型选择比例，而是控制动作过度保守。

下一步不应该直接上线 P1 prompt。应先增强 progress projection：

- 采集真实 tool/file/test events，而不只依赖最终 `agent.patch`；
- 将 `no_progress` 从 sticky flag 改成 windowed/streak state；
- 区分 `freeze_budget_expansion`、`replan_with_flash` 和 `premium_recover`；
- 对 `freeze_or_replan` 增加可执行下一步要求，而不是只作为抽象动作。

#### 2026-09-08 P2 windowed-progress replay checkpoint

本轮实现了 P1 后面缺的两块：

- 从 Harbor `trajectory.json` 抽取真实 `tool_call`、`file_written`、`test_run`；
- 在 reducer 里加入最近窗口状态 `no_progress_window.severity`，把 `none/watch/stale/blocked`
  和历史累计 `no_progress_event_count` 分开。

输入：

- A4 `shadow-relay` pass：`/mnt/data2/aware-gateway-runs/phase2-windowed-progress-extractor-a4-20260908T1410Z`
- A5 `shadow-relay` stopped：`/mnt/data2/aware-gateway-runs/phase2-windowed-progress-extractor-a5-20260908T1410Z`

抽取结果：

| 轨迹 | reward | events | LLM calls | tool calls | file writes | test runs | candidate progress | no-progress window |
|------|--------|--------|-----------|------------|-------------|-----------|--------------------|--------------------|
| A4 pass | 1.0 | 96 | 44 | 41 | 5 | 2 passed | 4 | stale 16 / watch 4 / none 3 |
| A5 stopped | null | 133 | 59 | 61 | 12 | 0 | 0 | stale 13 / none 8 / watch 4 / blocked 4 |

运行命令：

```bash
python3 scripts/replay_episode_decisions.py \
  --episode-dir /mnt/data2/aware-gateway-runs/phase2-windowed-progress-extractor-a4-20260908T1410Z \
  --episode-dir /mnt/data2/aware-gateway-runs/phase2-windowed-progress-extractor-a5-20260908T1410Z \
  --output /mnt/data2/aware-gateway-runs/phase2-windowed-progress-replay-p2-20260908T1420Z/router-replay-rsi-p2.json \
  --prompt-id rsi-p2-windowed-progress-v1 \
  --model openai/gpt-5.6-sol \
  --resume
```

P2 replay 结果：

| 指标 | P1 | P2 |
|------|----|----|
| replay decisions | 52 | 52 |
| valid candidate decisions | 52 | 52 |
| future evidence leakage | 0 | 0 |
| reason evidence coverage | 100% | 100% |
| candidate model mix | Flash 36 / Opus 16 | Flash 38 / Opus 14 |
| switched decisions | 18 | 18 |
| `freeze_or_replan` | 41/52 | 25/52 |
| cheap actions | 3/52 | 17/52 |
| premium recovery/reason | 8/52 | 10/52 |
| replay decision cost | `$0.87543` | `$1.00909` |

解释：

P2 没有改变总的 Flash/Opus 比例，但明显改变了控制动作分布。P1 看到旧的
`no_progress` 后经常直接冻结；P2 会先看最近窗口，如果只是 `watch`，更倾向
cheap probe/execute；如果进入 `stale` 或 `blocked`，才更常触发 replan/recover。

这说明 P2 的状态表达更接近我们想要的方向：

```text
不是“历史上失败过，所以永远保守”
而是“最近是否仍然卡住；有没有新的文件、测试或交付证据”
```

当前 P2 结论：

```text
Decision: keep for replay and next small pilot
Do not call it accepted yet.
```

原因：

- replay 门禁通过：52/52 有效，未来证据泄漏为 0；
- A5 的 `blocked` 状态能被识别出来，和实际 stopped 结果一致；
- P2 明显降低了无差别 `freeze_or_replan`；
- 但它只在 `shadow-relay` 两条历史轨迹上验证，还没有证明真实 benchmark 成本/质量提升；
- evidence citation 已改成短 `event:<id>`，本轮 replay 覆盖率为 100%。

下一步进入小规模真实 pilot 前，只接受 P2 作为 candidate policy，不替换生产策略。

### Step 5: 小规模 Harbor pilot

只跑通过公开 leaderboard 或本地已知可解的任务。

建议首轮：

| 任务 | 类型 | 角色 |
|------|------|------|
| `shadow-relay` | 协议/调试 | 继续做主回归任务 |
| 1 个短数据/算法任务 | 泛化检查 | 防止只贴合 `shadow-relay` |

每个策略先只跑 1 次：

| 策略 | Trial |
|------|-------|
| P0 | 1 per task |
| P1 | 1 per task |
| P2 | 1 per task |

这是 screening，不是 acceptance。如果 P1 或 P2 明显成本失控，立即停止，不进入第二轮。

正式 acceptance 至少需要：

```text
每个候选策略 x 每个任务 >= 3 次
```

成本受限时，减少候选策略数量，而不是用单次随机结果接受策略。

### Step 6: 验收/回滚

接受策略需要同时满足：

- reward 不低于匹配条件下的 P0-current；
- cost_per_success 低于匹配条件下的 P0-current，且包含失败/停止成本；
- total cost 低于 P0-current，或在同成本下 agent_call_count 明显下降；
- no_progress_turn_count 下降；
- judge_call_rate 不明显上升；
- 没有 provider incomplete 混入质量结论；
- replay 与 pilot 的趋势一致。

不接受策略的情况：

- 出现 A5 式成本失控；
- premium recovery 连续触发但没有进展；
- Flash 被完全弃用，策略退化成 all-premium；
- Flash 被过度使用，策略推迟关键恢复；
- reason 无法对应到 outcome evidence。
- replay 与 pilot 方向相反；这种策略最多保留为 replay-only，不进入 canary。

### 进入真实 Pilot 前的门禁

- `future_evidence_leakage = 0`
- 关键 observed event 抽取准确率 `= 100%`
- 无法判断的事件保留 `unknown`
- 策略未退化成 all-premium 或 all-flash
- 不存在无上限预算增长
- decision reason 的 evidence coverage `= 100%`

## 止损线

每个 trial 必须有硬 stop gate：

```text
成本 > $4.00 且未接近 verifier: stop
agent 调用 > 40 且没有 delivery/test/verifier 级有效进展: stop
连续 3 次 length boost 后没有 file/test 进展: stop
length_pressure + no_progress: freeze budget expansion
premium_recover + no_progress: stop or replan gate
provider incomplete: stop and classify separately
```

当前 online runtime 已实现 5 条可执行 stop gate：

- `provider_incomplete`：上一轮 provider 返回 2xx 但缺少 finish/tokens 元数据，下一轮本地停止并分类为 `gateway_provider_incomplete_stop_gate`。
- `cost_without_verifier`：episode 成本超过 `stop_cost_usd`，但还没有 validation/verifier 近端证据，下一轮本地停止并分类为 `gateway_cost_stop_gate`。
- `agent_call_no_effective_progress`：agent 调用超过 `stop_agent_call_threshold`，但没有 delivery/test/verifier 级有效进展，下一轮本地停止并分类为 `gateway_no_progress_stop_gate`。
- `length_pressure_without_progress`：连续 length pressure 超过阈值，但还没有 file/test 进展，下一轮本地停止并分类为 `gateway_length_pressure_stop_gate`。
- `blocked_premium_recover_no_progress`：episode 已进入 `no_progress=blocked`，上一轮 route 是 `premium_recover`，且上一轮 route 仍处于 `pending` 或 `no_progress`，下一轮本地停止并分类为 `gateway_stop_gate`。

这些本地停止都会返回 HTTP `409`，不会请求上游模型。对应 trace 记录
`pool=local`、`route_budget_action=stop_trial`、具体 `error_kind` 和完整 episode evidence。
V4 runner 会在 Harbor 运行中轮询这些 trace：一旦看到 `stop_trial` 或
`gateway_*stop_gate`，就写入 `gateway-stop-gate.json`、中断 Harbor、刷新一次
episode watcher。后处理 analyzer 会把这类样本输出为对应的 `failure_kind`，
不再和 provider 5xx、wall-clock cap 或 verifier failed 混在一起。

这些 gate 不是为了省时间，而是为了避免把坏策略误跑成“长尾样本”。

Completion 也必须拆成两个状态，避免“测试通过一次就提交”：

```text
test_passed -> completion_readiness -> premium_assess -> completion_guardrail
```

## 预期产物

- `episode-events.jsonl`：由 `scripts/extract_episode_outcomes.py` 生成
- `episode-summary.json`：由 `scripts/extract_episode_outcomes.py` 生成
- `replay-cutoff-check.json`：由 `scripts/extract_episode_outcomes.py --strict` 生成并校验
- `docs/rsi/event-schema-v1.json`
- `docs/rsi/progress-rules-v1.yaml`
- `docs/rsi/candidate-manifest.template.json`
- `router-replay-rsi-p1.json`
- `router-replay-rsi-p2.json`
- `rsi-p2-screening-summary.json`
- `rsi-p2-screening-summary.csv`
- `rsi-pilot-summary.csv`
- `docs/smart-router-rsi-report.html`

报告只展示有效对照：

- P0 vs P1 vs P2
- reward
- cost
- no-progress loop
- decision distribution
- outcome evidence examples
- future evidence leakage check
- accepted/rejected decision

## 第一轮验收结论格式

```text
RSI round: R1
Baseline: c329c64
Candidate: P1 / P2
Decision: accept | reject | keep for replay only

Accepted because:
- ...

Rejected because:
- ...

Next policy change:
- ...
```

## 项目状态目标

R1 extractor checkpoint 已达到：

```text
Event Schema                      done
Progress Rules                    done
Offline Outcome Event Projection  done for trace/tool/file/test/patch/verifier
Replay Cutoff Guard               done
Fixture Test                      done
Real-history Smoke                done on A4/A5 shadow-relay
P1 Outcome-aware Replay           done, replay-only
P2 Windowed-progress Replay       done, candidate for next small pilot
Policy Acceptance Gate            done for matched summary/replay evaluation
V4 Pilot Artifact Builder         done for job/traces -> episode summaries -> policy gate
Online Episode ID                 done for explicit X-Episode-ID fallback to session/trial
Online Episode Resolver           done for header/body operation plus minimal interrupt/resume stack
Online Episode Session Query      done for GET /v1/episode-sessions and trace/event backfill
Online State Version Audit        done for state-before/state-after trace fields
Trace Query by Episode            done for /v1/traces?episode_id=...
Online Episode Event API          done for single/batch POST and GET /v1/episode-events
Online Event Store                done for audit SQLite episode_events table
Batch Sidecar Event Ingest        done for atomic validation before sink fan-out
Online Progress Projection        done for posted file/test/verifier/no_progress events
Online Command Runner Adapter     done for batched wrapped command/test/file-write events
Harbor Artifact Watcher           done for batched trajectory/patch/ctrf/result sidecar
Stateful No-progress Recovery     done for stale/blocked -> local premium_recover
No-progress Budget Freeze         done for explicit/stale/blocked no-progress state
Repeated-failure Recovery         done for non-improving repeated test failure frontier
Completion Readiness Projection   done for current delivery/test/verifier state
Completion Guardrail Evidence     done for state-aware task_complete routing reason
Stale Completion Invalidation     done for target writes after verifier success
Delivery State Floor              done for verifier pass/fail and validated delivery assessment without Judge
Route Outcome Linkage             done for route -> event/test/verifier windows
Online Route Outcome Projection   done for posted events after each LLM route
Implicit Pending Route Closure    done for pending route -> no_progress when next LLM call arrives without observable event
Recent Route Outcome History      done for compact route -> outcome memory in state/prompt
Next Minimum Capability Hint      done for state-derived router prompt guidance
Capability Floor Enforcement      done for hard recovery and post-delivery validation assess floors, advisory otherwise
Gateway Stop Gate                 done for provider_incomplete/cost/length_pressure/blocked_recovery local aborts
Gateway Stop Marker               done for V4 runner interrupt + analyzer failure_kind
Online Episode State Query        done for GET /v1/episode-state
State Backfill                    done for persisted traces/events -> online projection
Deterministic Runtime Probe       done for event ingest -> state query -> recovery route -> 4 local stop gates
```

RSI R1 完整结束后，aware-gateway 应达到：

```text
Prompt Router                     done
Safe-control Rule Layer           done
Budgeted Route Action             done
Minimal Episode Runtime           done
Session Episode Stack             done for deterministic continue/interrupt/resume/global/unknown
Session Stack Inspection          done for active task-line query and persisted trace/event rebuild
Outcome Event Projection          done for offline replay, online when events are posted
Outcome-aware Replay              done
Event-driven State Controller     partial for no-progress recovery
Route-to-Outcome Feedback         partial for extractor/replay windows and compact online route history
Next-step Capability Estimate     partial via deterministic state hint, not yet acceptance-tuned
Capability Floor Control          partial; hard verifier/no-progress and post-delivery validation assess floors enforced, delivery floors now local
Gateway Stop Gate                 partial; four local abort paths enforced, acceptance thresholds still need matched pilot tuning
Online State Inspection           done for current in-memory projection
Restart State Rebuild             partial for audit trace/event backfill
Runtime Probe Acceptance          done for deterministic local gateway/mocks
C2 No-progress Budget Freeze      implemented; needs matched Harbor pilot acceptance
C3 Repeated-failure Recovery      implemented; needs matched Harbor pilot acceptance
Delivery Feedback                 partial via completion readiness, stale-proof invalidation, guardrail evidence, and local delivery floor routing
Outcome-aware Screening Pilot     harness done for P2; needs matched Harbor pilot data
Outcome-aware Acceptance          gate implemented; still needs at least 3 runs per accepted task class
Budget Policy Effectiveness       accepted or explicitly rejected
Automatic Tool Event Capture      partial via Harbor artifact watcher; native hook not started
Command-level Event Adapter       done for local command wrapping
Issue #1                          remains open until native live events and acceptance gates close the loop
```

这轮的重点不是证明 aware-gateway 已经“智能完成”，而是建立一条能持续打磨的工程路径：
每次失败都能被归因，每次策略修改都能被 replay，每次上线前都能被小样本验证。
