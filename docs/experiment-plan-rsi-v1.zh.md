# aware-gateway RSI 打磨实验 V1

## RSI 在本项目里的含义

这里的 RSI 取 Recursive Self-Improvement 的思想，但工程落点改成
**Router Self-Improvement**：

```text
真实运行数据 -> 找出策略问题 -> 生成候选策略 -> 离线重放 -> 小流量实跑 ->
验收/回滚 -> 写回下一轮策略
```

它不是让 agent 自动改代码并直接上线。aware-gateway 的改进必须经过固定门禁：
单元测试、确定性 probe、离线 replay、小规模 Harbor pilot、成本止损线和人工验收。

## 当前切入点

当前已经完成：

- Prompt Router
- Safe-control 本地规则
- Session 级决策历史
- Budgeted Route Action
- 最小 Episode 投影
- `finish_reason=length` 动态预算反馈

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
- `cost_per_success`

速度只作为诊断指标，不作为优化目标。

## 实验假设

### H1: Outcome 事件能改善预算决策

如果 Episode state 不只记录 LLM 调用结果，还记录文件修改、测试结果和 verifier
结果，那么 router 能更早区分：

- 输出空间不够，需要更大预算；
- 当前方向错误，需要 premium recovery；
- 正在原地打转，需要停止 cheap/premium 循环；
- 已经接近交付，需要 final guardrail。

验收标准：

- 在 `shadow-relay` 上，成本不高于 A4 的 `$3.4808`，且 reward 保持 `1.0`。
- `length_finish_rate` 低于 A4 的 `47.7%`。
- `episode_adjust` 触发后，后续 3 轮内应出现至少一个进展事件；否则记为 no-progress loop。

### H2: 离线 replay 能提前淘汰坏策略

同一批历史 router decision context，用不同策略重跑决策。如果新策略在 replay 中表现出
明显的 always-Opus、always-Flash、预算持续放大或恢复循环倾向，就不进入真实 Harbor。

验收标准：

- replay 后 premium share 不超过基准策略的 `+20%`，除非 reason 明确指向恢复或最终确认。
- `budget_action` 分布不退化成单一动作。
- 关键失败样本能给出和证据一致的 decision reason。

### H3: RSI 不能只在一个任务上过拟合

策略候选来自训练任务，但必须在 holdout 任务上保持质量和成本边界。

验收标准：

- 至少 3 类任务：协议/调试类、数据处理类、算法修复类。
- 用于生成策略的任务不参与最终 holdout 验收。
- holdout 不要求一次达到最优成本，但不能出现 A5 式成本失控。

## 实验分组

### P0: 当前策略

当前 `main` 的 smart-router：

- safe-control
- budgeted route
- decision history
- minimal episode
- recent length pressure

作用：作为 RSI 第一轮 baseline。

### P1: Outcome-aware Prompt Policy

只改决策提示词和 replay 脚本，不改运行时核心：

- prompt 输入增加 compact outcome state；
- decision schema 增加 `progress_state`；
- decision reason 必须引用具体 outcome evidence；
- budget action 选择必须说明是“需要更多空间”还是“需要换方向”。

作用：先验证 prompt 层是否能利用 outcome 信号。

### P2: Outcome-aware State Controller

在 P1 基础上加入轻量本地控制：

- 连续 no-progress 后禁止继续同类 budget 放大；
- 文件没有变化但多次测试失败时，强制 premium recovery；
- 测试通过后进入 completion guardrail；
- premium recovery 后若仍无进展，下一轮交还 semantic judge 并带上失败摘要。

作用：验证事件驱动控制是否比纯 prompt 更稳。

## 数据与事件模型

### 已有事件

```text
llm_call:
  status
  finish_reason
  model
  budget_action
  cost
  tokens
  latency_ms
```

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

新增一个离线脚本，把每个 trial 的轨迹转成 episode timeline：

```text
trace rows + terminal transcript + verifier output
=> episode-events.jsonl
=> episode-summary.json
```

第一版只需要做到：

- 能识别文件是否被修改；
- 能识别测试命令失败/成功；
- 能识别 repeated failure fingerprint；
- 能识别 verifier reward；
- 能计算 no-progress turn。

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

replay 只用于筛掉明显坏策略，不宣称等价真实 benchmark。

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

如果 P1 或 P2 明显成本失控，立即停止，不进入第二轮。

### Step 6: 验收/回滚

接受策略需要同时满足：

- reward 不低于 P0；
- total cost 低于 P0，或在同成本下 agent_call_count 明显下降；
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

## 止损线

每个 trial 必须有硬 stop gate：

```text
成本 > $4.00 且未接近 verifier: stop
agent 调用 > 50 且 no-progress 连续增加: stop
连续 3 次 length boost 后没有 file/test 进展: stop
provider incomplete: stop and classify separately
```

这些 gate 不是为了省时间，而是为了避免把坏策略误跑成“长尾样本”。

## 预期产物

- `episode-events.jsonl`
- `episode-summary.json`
- `router-replay-rsi-p1.json`
- `router-replay-rsi-p2.json`
- `rsi-pilot-summary.csv`
- `docs/smart-router-rsi-report.html`

报告只展示有效对照：

- P0 vs P1 vs P2
- reward
- cost
- no-progress loop
- decision distribution
- outcome evidence examples
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

RSI R1 结束后，aware-gateway 应达到：

```text
Prompt Router                     done
Safe-control Rule Layer           done
Budgeted Route Action             done
Minimal Episode Runtime           done
Outcome Event Projection          done for offline replay
Outcome-aware Replay              done
Outcome-aware Pilot               at least 2 tasks
Budget Policy Effectiveness       accepted or explicitly rejected
Issue #1                          remains open until realtime Episode/Outcome loop exists
```

这轮的重点不是证明 aware-gateway 已经“智能完成”，而是建立一条能持续打磨的工程路径：
每次失败都能被归因，每次策略修改都能被 replay，每次上线前都能被小样本验证。
