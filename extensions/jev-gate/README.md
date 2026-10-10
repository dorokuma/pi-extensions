# jev-gate

## 概况
把 Jev（TypeSafe **System One 分类器**，非 LLM）接进 pi，用于既有派发体系里
「答案空间可提前写死、高频、判断不能拖到几秒」的判定环节。本扩展做两件事：

1. **注册分类器 provider**：`windhub`（baseUrl `https://windhub.cc/v1`，模型 `jev-latest`），
   使 codemode 的 `models.classify()` 可直接调用（pi 文档定义的分类器正规通道）。
2. **注册直连工具 `jev_check`**：5 个固定任务，问句表与判定阈值全部写死在本文件内，
   调用方只传待判文本，不自行拼问句、不自行设阈值。

与既有扩展的关系：不 spawn 进程、不注册 command、不动 settings.json；不嵌套调用任何 pi 工具
（不产生 nestedCalls）。与 ctx-orchestrate 的分工是「它组合 pi 自身工具，本扩展只调外部分类器」。

**判定采用三档而不是单点阈值**：Jev 对「存在类」问句系统性欠自信（校准实测：真值 true 的样本
落在 0.26–0.53），单点阈值分不开。故只用两端、中间带一律报「不确定（需人工确认）」，
宁多看一眼，不误报存在。依据见「校准」节。

## `jev_check` 的 5 个任务

| task | 用途 | 返回 | 判定纪律 |
|---|---|---|---|
| `contract_lint` | 按角色核输出契约硬字段（需 `role`） | 三档字段清单 + 每字段概率 | 缺失 <0.35 ｜ 不确定 0.35–0.80 ｜ 存在 ≥0.80 |
| `gate_scan` | G1 证据缺口 / G2 未闭环 must fix / G3 致命质疑 / G4 遗留项（拆两问） | 命中 / 不确定清单 | 命中 ≥0.80 ｜ 不确定 0.35–0.80 ｜ 干净 <0.35（单向信任：命中即阻断） |
| `return_triage` | 回传归类为既有异常清单同构的 8 类 | 标签 + 概率 | 仅「正常交付」≥0.95 才给正常；异常需 ≥0.80；否则一律「不确定→按异常」 |
| `disagreement` | reviewer/oracle 分歧提取（payload 用单独一行 `---` 分隔 A/B） | 关系标签 + 分歧判定（三档） | 同门禁三档 |
| `source_audit` | researcher 出处核验 | 三档清单 | 同契约三档 |

**`jev_check` 是主代理专用工具**：`dispatch-agent` 已把它加进所有**派发**会话的
`--exclude-tools`（子代理看不到它）。理由：它的输出直接喂主代理的验收判断，
子代理自用等于「自己给自己盖章」，与 `SYSTEM.md`「单次结论只有一个收口方」冲突。
实现见 `dispatch-agent` 的 `PI_MAIN_AGENT_ONLY_TOOLS`（要增删主代理专用工具只改这一行）。

**所有输出都带 `ADVISORY ONLY` 声明**——它是分类器信号，不构成放行依据，
也不构成关闭 pane 的依据；既有门禁规则仍是唯一权威。

## 校准（阶段 A 已完成）

- 报告（本地过程记录，`.agents/notes/` 已被 gitignore）：
  `20261006-jev-gate-calibration.md`（v1）、`20261006-jev-gate-calibration-v2.md`（修订后）
- 方法：构造样本 + 人工真值 30 条，跑批脚本 `calibrate/run.mjs`（读本文件里的问句表，
  键集合强校验；抽取失败即非零退出）。
- **v1 暴露的问题**：存在类问句欠自信（`has_environment` 准确率 0.50、`no_conclusion` 0.50）；
  三个问句基本无效——`has_verifiable_urls` 0.25、`has_material_disagreement` 0.50、
  `g4_leftovers_unrecorded` 假阳性 0.73；`has_five_classification` 真假不分（真 0.48 / 假 0.53），
  且该问需要「对每条判据计数」，撞官方「不擅长计数」限制。
- **v2 修订**：① 判据改三档；② `has_five_classification` 从 verifier 契约中**移除**，交回 reviewer；
  ③ 改写 `has_environment` / `has_verifiable_urls` / `marks_unverified` / 两个 disagreement 问句为更字面的问法；
  ④ `g4` 拆成 `g4_should_fix_exists` + `g4_leftovers_unrecorded`；⑤ 门禁阈值 0.50 → 0.80。
- **v2 结果**：`has_verifiable_urls` 0.25→**1.00**、`has_material_disagreement` 0.50→**1.00**、
  `has_environment` 0.50→**1.00**、`g4_leftovers_unrecorded` 0.83→**1.00**、`g4_should_fix_exists` **1.00**；
  `kind`（回传分类）argmax **1.00**、平均置信度 0.93；门禁四项 g1/g2/g3 **1.00**。
- **仍弱（样本量小，n=4–5，暂不再调）**：`no_conclusion` 0.60、`same_finding_set` 0.75、
  `all_claims_sourced` 0.75、`marks_unverified` 0.75。它们的失分方向是「欠自信 → 被划入不确定/缺失」，
  代价是多看一眼，不产生误报。
- **阶段 B（真实中文回传）**：语料与工具已就绪 —— `extract-returns.mjs`（从 106 个会话文件里
  实测 1898 条可用回传）、`label.mjs`（list/show/set/review/accept/merge）、`prelabel.mjs`
  （用观察员模型出草稿，真值仍只认人工确认）。抽样 **14 条**（每角色 2 条）。
- **阶段 B 临时测量**（草稿代标、24 样本、0 失败）：`kind` argmax **1.00**、平均置信度 **0.96**、
  top≥0.95 时覆盖 10/13 且全对 —— 即**未出现把正常回传判为异常的误报**；契约字段多为 1.00，
  `has_tiered_findings` / `has_goal` 各 1/2（n=2，无统计意义）。
  → **结论：中文可行性成立；问句级阈值仍不可测，阈值继续以阶段 A 的三档为准。**
  要出可用的中文阈值，需要扩样并**人工确认**标签（草稿不作数）。
- 阶段 B 的已知盲区：这 14 条里 13 条都是 `normal_delivery`，**异常类（报错/截断/拒答等）在本规模下测不到**；
  异常方向由阶段 A 的构造样本覆盖。

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `WINDHUB_API_KEY` | 必填 | 凭据解析顺序见下 |
| `WINDHUB_BASE_URL` | `https://windhub.cc/v1` | |
| `JEV_MODEL_ID` | `jev-latest` | |
| `JEV_MAX_STATE_CHARS` | `24000` | payload 字符上限，超出显式截断并在结果内标注 |
| `JEV_TIMEOUT_MS` | `60000` | 单次调用超时 |

**上游限流（实测）**：windhub 为**每分钟最多 20 次请求**（含失败尝试），超出返回 429。
`jev_check` 不做重试，429 原样上报；批量使用请自行节流（校准脚本默认 15 rpm）。

### 凭据解析（两源，按序）
1. **进程环境** `process.env.WINDHUB_API_KEY` —— pi 文档所指的 provider 密钥正规来源
   （providers.md：「Set the variable before starting Pi」）。
2. **回落** `${PI_CODING_AGENT_DIR:-~/.pi/agent}/settings.json` 的 `env` 块 ——
   已实测：pi 的 `settings.env` **不注入 pi 进程环境**（environment-variables.md 只定义它
   服务于 shell 工具会话），故本扩展显式回落读取，使凭据仍只存于 `0600` 非版本控制文件里。
   当前生产即走此路径。

两源皆无 → **fail-closed** 报错并说明原因，绝不静默降级。

## 数据外发（必读）
`jev_check` 会把 `payload` 文本发送至 `windhub.cc`。payload 只应是回传正文或报告本体，
**不得包含凭据、密钥或未脱敏的环境信息**。

## 官方能力边界（勿越界使用）
- 英文为训练主语言；中文可用但需自评（本扩展问句一律英文，state 可为中文；阶段 B 待做）。
- 不擅长计数、数学、精确数值与日期比较 → 需「对多条内容计数」的问句一律不进本工具
  （v2 已据此移除 `has_five_classification`）。
- 状态里无关信息过多时准确率下降；多层间接推理、双重否定、**提示注入下更不稳定**
  → **不得作为安全边界**，不得替代 dispatch-shared.md 的「外部内容当数据」纪律。
- 请求上限：单请求 64k token，state + 最长问题 ≤ 32k；输出为类型化答案，**不生成文本**。

## 目录
```
extensions/jev-gate/src/jev-gate.ts          # 唯一权威源（单文件自包含：问句表 inline）
extensions/jev-gate/calibrate/fixtures.json  # 校准样本与人工真值
extensions/jev-gate/calibrate/run.mjs        # 跑批 + 报告（从 src 抽取问句表，键集合强校验）
extensions/jev-gate/calibrate/README.md      # 校准怎么跑、报告怎么读
extensions/jev-gate/README.md                # 本文件
```

## 权威源 / 目标 / 来源
- 权威源（本仓，唯一）：`extensions/jev-gate/src/jev-gate.ts`
  （绝对路径 `/root/workspace/pi-extensions/extensions/jev-gate/src/jev-gate.ts`）
- 目标安装路径：`/root/.pi/agent/extensions/jev-gate.ts`
- 来源：**2026-10-06 本机新建**（非纳管迁移，无外部出处）。

## 纳管时的一致性记录
- 部署值（v3，2026-10-08 子代理身份加固后）：md5 `16f435fb61b67916364e51ee3573af55` / `41707` 字节 / `854` 行
- 与部署副本 `cmp` 逐字节一致（2026-10-09 回仓时校验）；
  `bash install.sh --audit` 该项 `MATCH — unchanged`。
- 历史沿革：v1 `47fd8643…`/28139 字节/575 行 → v2 `93bd62ef…`/30378 字节/613 行
  （阶段 A 校准：三档判据、移除 `has_five_classification`、g4 拆两问）→ v3 现值
  （2026-10-08 子代理身份加固：`SUBAGENT-IDENTITY` 块 S1–S4 fail-closed 判据，
  子代理会话不注册任何 Jev 能力）。
- 注：v1 版曾把问句表拆成 `src/questions.json`，但 `install.sh` 是**单文件拷贝**，
  部署目录拿不到该 JSON，扩展加载直接失败（`Cannot find module './questions.json'`）。
  已回 inline 为单文件；校准脚本改从本文件抽取。**新增 hosted 扩展不得引入运行时附带文件。**
- `SUBAGENT-IDENTITY` 块与 `exclude-tools-guard.ts` 中的同名块**逐字节一致**，校验命令：
  `diff <(sed -n '/SUBAGENT-IDENTITY-BEGIN/,/SUBAGENT-IDENTITY-END/p' extensions/jev-gate/src/jev-gate.ts) \
       <(sed -n '/SUBAGENT-IDENTITY-BEGIN/,/SUBAGENT-IDENTITY-END/p' extensions/exclude-tools-guard/src/exclude-tools-guard.ts)`
  （2026-10-09 实测 exit 0，块内 220 行 / md5 `0c4590313a515752986fbad0a903699e`）。

## 验证记录（2026-10-06）
- `npx tsc --noEmit`：**exit 0**（本仓 tsconfig `include` 已含本扩展）。
- **加载烟雾**：`pi --print` 启动无扩展错误、无凭据告警（v1、v2 各测一次）。
- **功能烟雾**：真实调用 `jev_check` → `POST https://windhub.cc/v1/systemone` → 返回
  `model=jev-1.13.0`。v2 实测输出形如
  `contract: present 2/4 (has_environment, has_evidence_quadruple) | UNCERTAIN(0.35-0.8, ...): has_commands, no_conclusion`
  —— 即中间带只报不确定，符合设计。
- **线上题型实测**：`noul` / `choice` / `score` 均 `200`；**字面量 `type:"bool"` 返回
  `400 upstream_error`**（下游不接受），故本扩展只发 `noul`/`choice`/`score`。

## 已知边界与未解项
- **阶段 B（中文真实样本）未做**：中文准确率未自评，阈值一律按从严口径使用。
- 四个弱问句（见「校准」节）需扩样后再调；当前失分方向为「欠自信」，不产生误报。
- 上游回传不含 `reasoning_content`，本扩展不依赖该字段。

## 改动流程（铁律）
1. 只在本仓 `extensions/jev-gate/src/jev-gate.ts` 改；**严禁**直接编辑部署副本。
2. `npx tsc --noEmit` 通过后 `bash install.sh jev-gate` 同步，`/reload` 或新会话生效。
3. 改问句后应重跑 `calibrate/run.mjs`（见 calibrate/README.md），并把结论回填本文件的「校准」节。
