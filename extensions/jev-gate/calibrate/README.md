# jev-gate 校准（阶段 A：构造式）

## 这是什么
用**构造样本 + 人工真值**测 `jev_check` 的问句准确率与置信度可靠性，据此替换扩展里
三个占位阈值（`FIELD_PRESENT_MIN` / `GATE_FLAG_MIN` / `TRIAGE_NORMAL_MIN`）。

关键点：跑批读的是 **`../src/questions.json`** —— 与线上扩展同一份问句表，
所以「校准测的问句」与「线上发的问句」不会漂移。

## 文件
| 文件 | 作用 |
|---|---|
| `fixtures.json` | 样本与人工真值（`expect`）；`ambiguous: true` 的样本不计入准确率，只用于看置信度是否过高 |
| `run.mjs` | 跑批 + 出报告（Node 原生 fetch，无依赖） |

## 用法
```bash
cd /root/workspace/pi-extensions/extensions/jev-gate/calibrate
node run.mjs                                  # 报告打到 stdout
node run.mjs --out /tmp/report.md             # 或写文件
node run.mjs --target 0.95 --min 3            # 调整建议阈值的目标
```
凭据解析顺序与扩展一致：`WINDHUB_API_KEY`（进程环境）→ `<PI_CODING_AGENT_DIR>/settings.json` 的 `env` 块。
缺凭据、请求失败、样本缺失一律非零退出（fail-fast）。

## 报告怎么读
- **bool 问句表**：给出各阈值的准确率，并给出**建议阈值** —— 取满足
  `P(真值=true | p ≥ t) ≥ --target`（默认 0.95）的最小 `t`，且被预测为正的样本数 ≥ `--min`。
  之所以按精确率（而不是准确率）选阈值：本工具是门禁/巡检方向，**假「存在」漏掉缺陷，
  比假「缺失」多看一眼要贵得多**。
- **可靠性曲线**：把概率分桶，比较「平均预测概率」与「经验命中率」。两列接近 → 概率可当概率用；
  偏差普遍为正 → **过度自信**（阈值应整体上调）。
- **choice 问句表**：argmax 准确率 + 「要求 top ≥ 0.80 / 0.95 时的准确率与覆盖率」。
  覆盖率低说明该标签在正常阈值下拿不到高置信度，需要人工复核。

## 阶段 B：真实中文回传（工具已就绪，待人工标注）

阶段 A 测不了中文与真实分布。阶段 B 用**生产里真实的子代理回传**做同一套指标。

### 语料来源
`~/.pi/agent/sessions/**/*.jsonl` 里的 `herdsman.agent_event` 记录：
- 回传原文 —— `data.compactHistory.lastAssistantMessage.text`
- 产生角色 —— 优先取任务原文的 `角色：xxx`（与派发协议同源），退路是
  `data.compactHistory.historyRef.path` 里的 `role-<name>-<hash>`
- 当时要求 —— `data.compactHistory.lastUserMessage.text`（给标注者看上下文）

实测存量（2026-10-06，106 个会话文件）：**1898 条可用回传**，按角色分布
oracle 389 / worker 884 / reviewer 366 / scout 117 / planner 63 / researcher 29 / unknown 50。
**verifier 为 0** —— 该角色 2026-10-06 才建，无历史，故其契约问句在真实数据上暂时无法校准。

### 四步流程
```bash
cd /root/workspace/pi-extensions/extensions/jev-gate/calibrate

# ① 抽语料（默认每角色 6 条、抽样按长度分位取，避免全是大报告）
node extract-returns.mjs --per-role 6

# ② 看清单 / 看原文 / 打标
node label.mjs list                     # 待标注清单
node label.mjs show <id> --chars 4000   # 看原文（--full 看全文）
node label.mjs set <id> kind=normal_delivery has_locations=present has_relationships=absent

# ③ 进度与合并
node label.mjs status
node label.mjs merge                    # labels.json → fixtures-b.json

# ④ 跑批（限流 20 req/min，脚本默认 15 rpm）
node run.mjs --fixtures ../../../../.agents/notes/jev-gate-phase-b/fixtures-b.json \
             --out ../../../../.agents/notes/jev-gate-phase-b/report-b.md
```

### 标注口径
- **kind**（8 选 1）：这条回传属于哪一类。正常且完整的交付 = `normal_delivery`；
  报错 / 空回传 / 有文字但没产出 / 明显截断 / 执行超时 / 拒答 / 证据不全 按实际选。
- **契约字段**（present / absent）：只标**该回传产生角色**对应的字段；
  **拿不准就留空** —— 留空只是不进该字段统计，不算错。
- 中文从严：只标你亲眼在原文里看到的，不要按"大概应该是"。

### 输出位置（重要）
`candidates.json` / `labels.json` / `fixtures-b.json` / 报告一律落
`<repo>/.agents/notes/jev-gate-phase-b/`（`.gitignore:36` 已忽略）。
**语料含真实工程内容，禁止入库。** 脚本默认只写该目录。

### 阶段 B 的结论去向
跑完后把「中文阈值是否需要调整」写回 `../README.md` 的「校准」节；
若阈值变动，只改 `src/jev-gate.ts` 里那三个常量 + 一个阈值，然后 `bash install.sh jev-gate`。


## 实际使用观测（回答「能不能固定节点稳定用」）

离线校准只能证明「标签对不对」，证明不了「用起来有没有用」。观测脚本只读会话、零埋点：

```bash
cd /root/workspace/pi-extensions/extensions/jev-gate/calibrate
node observe-usage.mjs --out /root/workspace/pi-extensions/.agents/notes/jev-gate-phase-b/usage.md
```

它从 `~/.pi/agent/sessions/**/*.jsonl` 里读 `jev_check` 的每次调用（assistant 的
`toolCall` 块 + `toolResult`），输出：

| 指标 | 回答的问题 | 达标线 |
|---|---|---|
| ① 调用量与 task/角色分布、耗时、成本 | 有没有在被用 | — |
| ② 三档分布（present/uncertain/missing、flagged/uncertain/clear） | **有没有增量** | 被标记（uncertain+命中）≥ 20% |
| ③ 按问句的中间带(0.35–0.80)占比 | 问句有没有区分度 | uncertain 占比 > 60% 即说明它给不出判断 |
| ④ 调用后 6 条消息内的 pane/close/commit/git 类工具 | **有没有被误用** | 0 次 |
| ⑤ 调用率（分母 = 主代理收到的子代理更新条数，上限近似） | 覆盖率 | ≥ 50% |
| ⑥ 最近 10 次明细 | 抽查 | — |

`--census` 会额外列出会话里出现过的所有工具名与次数（用来验证解析器没瞎）。

**观察结论去向**：达标 → 再讨论要不要写进 `SYSTEM.md` §4 当门禁辅助；
不达标（调用率过低 / uncertain 占比过高 / 出现误用）→ 收窄用途或调阈值，别硬上。
