# herdr-agent-state (Go 版)

PiG Go 原生组件：与 Herdr 守护进程进行状态同步与会话上报。

> **重要声明**：本组件实现绝对**未修改** Herdr 托管的原版 TypeScript 扩展文件（`/root/.pi/agent/extensions/herdr-agent-state.ts`）。原版 TypeScript 扩展由 Herdr 二进制全权托管与覆盖维护（遵守 `pi-extensions` 铁律①与铁律⑤），仓内新增的 Go 版与原版受托管文件安全并存，专用于 PiG 运行环境。

---

## 1. 调研结论：herdr 0.9.5 官方集成现状

在启动移植前，对 Herdr 官方集成目标进行了严格复跑与验证：

### 1.1 命令复跑与输出证据

1. **版本确认**：
   ```sh
   $ herdr --version
   herdr 0.9.5
   ```

2. **官方支持的目标集成**：
   ```sh
   $ herdr integration install --help
   Install an integration

   Usage: herdr integration install <TARGET>

   Arguments:
     <TARGET>
             [possible values: pi, omp, claude, codex, copilot, devin, droid, kimi, opencode, kilo, hermes, qodercli, qwen, cursor, mastracode, antigravity-cli, grok, letta]
   ```
   **结论**：`herdr integration install` 提供的官方 target 列表中**完全没有** `pig` 或 `PiG` 目标。

3. **现有集成安装状态**：
   ```sh
   $ herdr integration status
   pi: current (v9) (/root/.pi/agent/extensions/herdr-agent-state.ts)
   omp: not installed (/root/.omp/agent/extensions/herdr-omp-agent-state.ts)
   claude: not installed (/root/.claude/hooks/herdr-agent-state.sh)
   codex: not installed (/root/.codex/herdr-agent-state.sh)
   copilot: not installed (/root/.copilot/hooks/herdr-agent-state.sh)
   devin: not installed (/root/.config/devin/herdr-agent-state.sh)
   droid: not installed (/root/.factory/hooks/herdr-agent-state.sh)
   kimi: not installed (/root/.kimi-code/hooks/herdr-agent-state.sh)
   opencode: not installed (/root/.config/opencode/plugins/herdr-agent-state.js)
   kilo: not installed (/root/.config/kilo/plugin/herdr-agent-state.js)
   hermes: not installed (/root/.hermes/plugins/herdr-agent-state/__init__.py)
   qodercli: current (v4) (/root/.qoder/hooks/herdr-agent-state.sh)
   qwen: not installed (/root/.qwen/hooks/herdr-agent-session.sh)
   cursor: current (v2) (/root/.cursor/herdr-agent-state.sh)
   mastracode: not installed (/root/.mastracode/hooks/herdr-agent-state.sh)
   antigravity-cli: current (v4) (/root/.gemini/config/hooks/herdr-agent-state.sh)
   grok: current (v3) (/root/.grok/hooks/herdr-agent-state.sh)
   letta (experimental): not installed (/root/.letta/hooks/herdr-agent-session.sh)
   ```
   **结论**：现有 `pi` 目标仅面向 Node 原版 Pi（安装路径为 `/root/.pi/agent/extensions/herdr-agent-state.ts`），Herdr 尚未提供 PiG 的 Go 扩展安装目标。

4. **官方协议定义（`herdr api schema --json`）**：
   Herdr RPC API Schema 中针对 `pane.report_agent` 与 `pane.report_agent_session` 的参数定义如下：
   - `pane.report_agent` 参数定义：
     ```json
     {
       "properties": {
         "agent": { "type": "string" },
         "agent_session_id": { "type": ["string", "null"] },
         "agent_session_path": { "type": ["string", "null"] },
         "message": { "type": ["string", "null"] },
         "pane_id": { "type": "string" },
         "resume_argv": { "items": { "type": "string" }, "type": ["array", "null"] },
         "seq": { "format": "uint64", "minimum": 0, "type": ["integer", "null"] },
         "source": { "type": "string" },
         "state": { "enum": ["idle", "working", "blocked", "unknown"], "type": "string" }
       },
       "required": ["pane_id", "source", "agent", "state"],
       "type": "object"
     }
     ```
   - `pane.report_agent_session` 参数定义：
     ```json
     {
       "properties": {
         "agent": { "type": "string" },
         "agent_session_id": { "type": ["string", "null"] },
         "agent_session_path": { "type": ["string", "null"] },
         "pane_id": { "type": "string" },
         "resume_argv": { "items": { "type": "string" }, "type": ["array", "null"] },
         "seq": { "format": "uint64", "minimum": 0, "type": ["integer", "null"] },
         "session_start_source": { "type": ["string", "null"] },
         "source": { "type": "string" }
       },
       "required": ["pane_id", "source", "agent"],
       "type": "object"
     }
     ```

### 1.2 为什么必须自己写 Go 版

由于 Herdr 0.9.5 没有内置对 PiG 的扩展集成，且根据 `PLAN.md` 整体规划，eqi12 机器将逐步移除 Node 运行时切换至 PiG。因此必须在 PiG 扩展体系内自建与原版 TS 行为 100% 对齐的 Go 组件，向 Herdr 通信上报 agent 状态与会话生命周期。

---

## 2. 协议字段对照表

| 字段名 | `pane.report_agent` | `pane.report_agent_session` | 语义与取值来源 |
|---|---|---|---|
| `pane_id` | 必须 | 必须 | `os.Getenv("HERDR_PANE_ID")` |
| `source` | 必须 | 必须 | 固定 `"pig"`（**不能**用 `"herdr:pi"`，见 §2.1） |
| `agent` | 必须 | 必须 | 固定 `"pi"`（moshi gateway 的 canonicalAgentName 白名单只认 22 个官方 agent 名，拒收 `"pig"`，见 §2.1） |
| `seq` | 可选/递增 | 可选/递增 | 单调自增序列号，起始于 `time.Now().UnixMilli()*1000` |
| `state` | 必须 | - | `"idle"` \| `"working"` \| `"blocked"` |
| `message` | 可选 | - | 当 blocked 时为阻断原因/标签（如 `data.label`），非 blocked 为 null |
| `session_start_source` | - | 可选 | 来自 `session_start` 事件的 `reason`（如 startup/resume） |
| `agent_session_path` | 条件注入 | 条件注入 | 会话文件绝对路径（POSIX 或 Win32），优先级高于 `agent_session_id` |
| `agent_session_id` | 条件注入 | 条件注入 | 若无合法路径且存在会话 ID 时注入 |
| `resume_argv` | 条件注入（有会话引用时） | 持窗后可选 | `["pig","--session",<真实 path 或 id>]`；`pane.report_agent` 由 `ResumeCommand(sessionRef)` 决定：有会话引用即注入（该包本身就是持窗包）；`pane.report_agent_session` 仅在已持窗后注入，否则 Herdr 以 `resume_not_accepted` 拒绝 |

### 2.1 `source` 为什么不能是 `herdr:pi`

Herdr 自带内置 `pi` 集成（`herdr integration install pi`，部署物
`~/.pi/agent/extensions/herdr-agent-state.ts`，头部 `HERDR_INTEGRATION_ID=pi`），其
`source` 就是 `"herdr:pi"`。向真实 Herdr server 发 `pane.report_agent` 且
`source="herdr:pi"` 时，Herdr **返回 `{"result":{"type":"ok"}}` 但完全不落库**：

- `herdr pane get` 仍是 `agent_status: "unknown"` 且无 `agent` 字段；
- 该请求连 `herdr-server.log` 都不记录（`pane.report_agent` 本来就不落日志，
  `pane.release_agent` 才落，所以"日志没有上报"不能证明"扩展没发包"）。

对照实验（真实 socket，pane `w3N:pE`，同一 `agent=pi`、同一 `seq` 递增）：

| `source` | Herdr 是否落库 |
|---|---|
| `herdr:pi` | 否（静默丢弃，仍答 ok） |
| `herdr:pig` / `pig` / `pi` / 任意非内置 id | 是 |

官方文档同样写明：`--source` 要 stable、unique，且 **不要以 `herdr:` 开头**，因为那是
Herdr 自己集成的命名空间。故本组件取 `source = "pig"`（不被 Herdr 内置集成占用的自有
source）、`agent = "pi"`（上报的 agent 标签，moshi gateway 的 canonicalAgentName 白
名单只认 22 个官方 agent 名、拒收 `"pig"`）；`herdr pane get` 显示 `agent: pi`。
注意这三个同名概念别混淆：上报的 `agent` 标签是 `"pi"`，集成 `source` 是 `"pig"`，
而 `resume_argv` 里的 `"pig"` 是 PiG 二进制进程名。

**Herdr 0.9.5 的 `is_official_agent_source` 过滤（回读侧副作用）**：Herdr 0.9.5 只把
「官方集成 source」（`is_official_agent_source` 为真，即 Herdr 自带集成登记在册的 source）
的会话信息回读给 `herdr pane get`。本组件的 `source = "pig"` 不在该名单内，因此即便
`pane.report_agent` 被正常接受（Herdr 回 ok，见上表对照实验），`herdr pane get` 的
`agent_session` 仍为 `null`。这是**回读侧的过滤**，不是上报被拒，也不影响 §2.3 的送达判定。
会话历史不依赖该回读：由 `herdsman` 扩展配合 herdsman 的 `~/.pig/agent/sessions`
白名单闭环。

### 2.2 `pane.release_agent`（退出释放）

进程退出时必须显式释放持窗，否则 Herdr 面板会一直挂着旧的 `agent` / `agent_status` /
resume 命令，直到 pane 退回空闲 shell 提示符。Go 版在 `session_shutdown` 事件
（`state.go: HandleSessionShutdown()`）发出 `pane.release_agent`，参数如下：

| 字段名 | `pane.release_agent` | 取值 |
|---|---|---|
| `pane_id` | 必须 | `os.Getenv("HERDR_PANE_ID")` |
| `source` | 必须 | 固定 `"pig"`（**不能**用 `"herdr:pi"`，见 §2.1） |
| `agent` | 必须 | 固定 `"pi"`（与状态上报同一 agent 标签，见 §2.1） |
| `seq` | 可选（推荐） | 单调自增序列号，保证 release 排在同 source 的最后一次状态上报之后 |

### 2.3 响应信封校验（送达判定）

原版 TS 的 `sendRequestAttempt` 只看「有没有字节回来」（`socket.on("data")`），任何字节
——包括 Herdr 的错误信封、甚至非 JSON 垃圾——都算送达。Go 版改为解析并校验官方响应信封
（`transport.go: validateResponse()`），三条同时满足才算**送达**：

- 响应体可解析为 JSON（空响应、非 JSON 字节一律判**未送达**）；
- `id` 与本次请求的 `id` 一致（`id` 不匹配判**未送达**）；
- `result` 非空且不含 `error` 字段（错误信封 `{"id":..,"error":{"code":..,"message":..}}` 判**未送达**）。

未送达即走既有超时重试路径（首次 500ms，失败后 1500ms 再试一次）。此校验只回答「这一包
Herdr 收下了吗」，**不构成对 Herdr 状态的回读**：上报的 `state` 仍只由 PiG 生命周期推导
（见 §5.2）。

**响应帧必须完整读取**（`transport.go: readResponseFrame()`，`transport.go:279`）

Unix 流上的一次 `Read()` 只会拿到任意一段字节，不保证是一帧。原实现只读一次并把这一段
当作完整响应：Herdr 的响应被拆成多个报文段时，拿到的是**半帧**，JSON 解析失败，一次本来
成功的送达被误判为未送达并触发**重复重试**（同一请求发两遍）。Go 版改为按换行帧完整读取：

- 读到帧尾换行为止（与写请求共用**同一个 attempt deadline**，不额外放宽预算）；
- 设明确上限 `maxResponseFrameBytes = 64KiB`：超长帧直接判**未送达**，不无限缓冲；
- **空帧**（空行，或对端关闭且一个字节都没给）判**未送达**；
- **半帧**（对端只发一半且不补齐）判**未送达**；
- 对端在完整 JSON 之后直接关闭连接（EOF、无换行）时按完整载荷接受，兼容既有协议。

`id` / `result` / `error` 校验与 500ms / 1500ms 重试预算均不变（见本节前述）。

该判定结果不再只是内部口径，而是**回灌状态机**：`transport.go: SendRequest()` 与
`SendReportAgent()` 返回 `bool`（任一 attempt 通过 `validateResponse()` 即为 `true`），
`state.go: drainLoop()` 据此在**送达之后**才置持窗标志 `agentStateReported`，
`HandleSessionShutdown()` 据此保证释放包排在最后一个状态包之后（见 §2.4）。

### 2.4 持窗判定、退出串行化与会话世代隔离（S1 / S2 / S3 已闭合）

Herdr 对 `pane.report_agent_session` 的 `resume_argv` 有前置条件：source 必须**已经持窗**
（Herdr 侧 `hook_authority` 已建立）否则回 `resume_not_accepted`。因此「持窗」标志的置位
时机与「退出释放」的时序必须落在协议语义上，不能只按本地入队顺序推断。

**S1：持窗标志只在送达后置位**（`state.go: PublishState()` / `drainLoop()`）

- `PublishState()` 只把状态写入 `sm.queuedState` 并分配 seq，**不再**在入队时置
  `sm.agentStateReported`；
- `drainLoop()` 在锁外调用 `sm.client.SendReportAgent(...)`，其 `bool` 返回值表示
  「至少一次 attempt 通过 `validateResponse()`」（见 §2.3）；只有返回 `true` 时，才在持
  `sm.mu` 的临界区内置 `sm.agentStateReported = true`；
- 于是「入队」不再等于「持窗」：Herdr 未确认（错误信封 / `id` 不匹配 / 非 JSON / 超时）时
  标志保持 `false`，后续 `PrepareReportSession()` 不会附带 `resume_argv`，也就不会触发
  `resume_not_accepted`。

**S2：退出释放与排空循环串行化**（`state.go: HandleSessionShutdown()`）

- `HandleSessionShutdown()` 持锁判断 `rootSession`，随后**复位整个状态机**：
  `rootSession=false`、`queuedState=nil`、`agentStateReported=false`、`lastState=nil`、
  `lastMessage=nil`、`agentActive=false`、`blockedCount=0`、`blockedMessage=""`；
- 若此刻有 `drainLoop` 迭代在飞（`sendInFlight == true`），handler 取出 `inFlightDone`
  通道并在**锁外**等待：该通道由 `startDrainLocked()` 启动排空循环前创建，由
  `finishDrainLocked()` 在排空循环退出前 `close`，而 `close` 发生在在飞的
  `SendReportAgent()` **完全返回**之后（含重试窗口）；
- `drainLoop()` **取队列前**与 `SendReportAgent()` 返回后都会用世代号复核
  （`drainStaleLocked()`）：世代已变或已离开 rootSession 时**不再取出队列、也不再发送**，
  此时**不置** `sm.agentStateReported`，并立即退出排空循环；
- 在飞请求结束后，handler 才分配新的 `NextReportSeq()` 并**同步**发送
  `pane.release_agent`，由此保证 `release_agent.seq >` 任何已发送的 `report_agent.seq`，
  且不会有在飞的 `report_agent` 在 `release_agent` 之后抵达 Herdr（否则会把已退出的窗格
  重新锁上）。

**S3：会话世代号隔离（闭合 S1/S2 之后仍留的两个窗口）**（`state.go: sessionEpoch`）

S1/S2 闭合了「持窗标志置位时机」与「退出释放串行化」，但仍留两个窗口：

- **Handler 锁拆分窗口**：handler 把「状态变更」与「状态发布」拆成两次加锁（先改
  `agentActive` / `blockedCount`，解锁后再 `PublishState()`），两次加锁之间若发生
  `session_shutdown`，滞后的 handler 会在 `pane.release_agent` **之后**再补发一包
  `pane.report_agent`；
- **旧 Shutdown 与新 Session 交叉污染**：shutdown 等待在飞排空迭代期间若已有新会话接管
  同一 `StateManager`，过期的 `pane.release_agent` 会抹掉新会话刚建立的持窗。

闭合手段是 `StateManager.sessionEpoch uint64`（受 `sm.mu` 保护）：

- `handleSessionStartCore()` 持锁 `sessionEpoch++`（`state.go:784`）并把状态机**干净
  初始化**：`rootSession=true`、`queuedState=nil`、`agentStateReported=false`、
  `lastState=nil`、`lastMessage=nil`、`agentActive=false`、`blockedCount=0`、
  `blockedMessage=""`；
- `HandleSessionShutdown()` 同样 `sessionEpoch++`（`state.go:878`）并复位状态机
  （`rootSession=false`），于是「关闭世代」与「开启世代」互不干扰；
- 每个 handler 在**离开锁之前**记下 `epoch := sm.sessionEpoch`，在**重新持锁之后**复核
  `sm.rootSession && sm.sessionEpoch == epoch`：
  - `updateSessionRefCoreForEpoch()`（provider 查询在**锁外**）——世代不符则不写会话引用；
  - `prepareSessionReportForEpoch()`——世代不符则不分配 seq、不发会话上报；
  - `applyAgentActivity()`——**置位与发布在同一次加锁内完成**，世代不符则
    连状态都不改（消除 Handler 锁拆分窗口）；
- `HandleHerdrBlocked()` 的阻断计数更新与状态发布同样在**同一次加锁**内完成（消除
  「解锁 → PublishState」之间的窗口）；
- `drainLoop(done, epoch)` 只处理本世代的队列：世代已变或已离开 rootSession 时**不再取出
  队列、也不再发送**（此前是「先发后查」，滞后的状态仍会打到 Herdr）；
- `PublishState()` / `PrepareReportSession()` 在 `!sm.rootSession && sm.sessionEpoch > 0`
  时一律拒绝；世代 0 表示「尚未跑过会话生命周期」，保留纯状态机单测直驱的兼容性；
- `HandleSessionShutdown()` 在等待在飞请求结束后**再次持锁复核**世代：若期间已有新会话
  接管（`sessionEpoch` 已变或 `rootSession` 已为 `true`），则**丢弃**这份过期的
  `pane.release_agent`，不抹除新会话的持窗（本轮起该复核发生在释放包**出队时**，见 S4）。

**S4：统一发送生命周期与序号分配（会话上报 / 状态上报 / 退出释放同一条队列）**

S1/S2/S3 把「状态机」这一层理顺了，但**发送层**仍然是三条各自为政的路子：会话上报由
`go sm.client.SendRequest(req)` 这样的**裸 goroutine** 直接发出，状态上报由排空循环自己写
socket，退出释放包由 shutdown handler 同步发出。由此留下三个窗口：

- 会话上报的 `seq` 在 handler 里分配，但**真正写 socket 的时刻**由 goroutine 调度决定 →
  **到达序可能与 seq 序不一致**（Herdr 看到的顺序与 seq 表达的顺序不是一回事）；
- shutdown 只等待**排空循环**（状态上报）在飞结束，**不等待在飞会话上报** →
  `pane.release_agent` 可能**反超**一条仍在路上的会话上报；
- 「检查世代 → 解锁 → 再发送」不是原子的：世代在解锁与发送之间变化时，仍会打出一条
  过期请求。

闭合手段是 `StateManager` 内的**统一发送生命周期**（outbox + 单一 dispatcher）：

- 所有请求——会话上报、状态上报、退出释放——都先在**持 `sm.mu`** 的临界区内快照载荷、
  **分配 seq**、再入队到 `sm.outbox`；`seq` 分配与入队在**同一次加锁**内完成，因此
  **上线路序 = seq 序**；
- 唯一发送者 `dispatchLoop()`（`state.go:512`）按队序**逐个**写 socket：不再有未跟踪的裸
  goroutine 发送 `report_agent_session`，也不再出现两个并发连接；
- `dispatchItem()`（`state.go:528`）在**锁外**做网络 I/O，`sm.mu` **不跨 2 秒网络等待**
  （不会与 provider 互锁）；发送前在锁内复核世代/会话状态，过期请求直接丢弃；
- `HandleSessionShutdown()` 复位状态机并 `sessionEpoch++`（**阻止新上报**），再等在飞排空
  迭代结束，然后把 `pane.release_agent` **入队到所有在飞请求之后**
  （`enqueueReleaseForEpoch()`，`state.go:650`）→ 释放包严格排在最后一条会话/状态上报之后；
- 释放包在**出队时**（而不是「检查后解锁再发送」）复核世代：期间已有新会话接管时**丢弃**
  这份过期释放包，不抹除新会话的持窗（与 S3 的世代隔离一致）。

**S5：活动序号（过期的 idle 观察不得覆盖最新的 working）**

`agent_settled` / `session_start` 都在**锁外**读 `ctx.IsIdle()`。若在这段窗口里到达一条更晚
的 `agent_start`（置 `agentActive=true` 并发布 working），先前的 settled handler 带着**过期
的 idle 观察**回来后会把状态改回 idle，把最新的 working **覆盖掉**。

闭合手段是最小化的**活动序号**（`activityTickets` / `activityApplied`，受 `sm.mu` 保护）：

- 每个活动事件（`session_start` / `agent_start` / `agent_settled`）在 handler **开始**时领一张
  **递增票号**；
- `applyAgentActivity()`（`state.go:673`）在锁内应用决策时复核：若**更新的活动事件已经应用
  过**（票号小于已应用的票号），这份过期观察直接丢弃——既不改 `agentActive`，也不发布；
- 票号**只覆盖 agent 活动**：`blockedCount` / label / blocked 优先级的记账完全独立，不会被
  这套机制丢弃（阻断更新不会因此少计或错计）；
- `force` / 去重语义不变：`session_start` 仍 `force=true`，`agent_start` / `agent_settled` 仍
  `force=false`，`desiredStateLocked()` 的三分支优先级不变。

---

## 3. TypeScript 原版与 Go 版逐项对照

| 功能模块 | 原版 TS 位置 (`herdr-agent-state.ts`) | Go 版位置与实现 | 行为一致性说明 |
|---|---|---|---|
| **环境变量读取** | L11-17 (`HERDR_ENV`, `socketPath`, `paneId`) | `extension.go: ConfigFromEnv()` | 读取 `HERDR_ENV`、`HERDR_SOCKET_PATH`、`HERDR_PANE_ID` |
| **平台支持与端点解析** | L13-14 (`\\\\.\\pipe\\` 拼装) | `transport.go: ResolveSocketEndpoint()` | 仅 Linux/macOS，Windows 未支持（标准库 `net` 无法在无三方依赖下原生支持 Windows Named Pipe） |
| **启用条件判定** | L18-20 (`enabled()`) | `extension.go: Enabled()` | `HERDR_ENV=="1" && socketPath!="" && paneId!=""`，不满足时完全 no-op |
| **单次发送尝试** | L22-48 (`sendRequestAttempt`) | `transport.go: Client.SendRequestAttempt()` | 建立 Unix 短连接，写 JSON + `\n`，**按换行帧完整读取**响应（≤ 64KiB；半帧/空帧/超长帧判未送达）后解析并校验 `id`/`result`/`error` 信封判定送达；单次 attempt 硬上限 = 500ms / 1500ms（dial 与 I/O 共享总超时 deadline） |
| **送达结果回灌** | 未实现（TS 无回执概念） | `transport.go: Client.SendRequest()`, `SendReportAgent()` | **新增**：返回 `bool`（任一 attempt 通过 `validateResponse()` 即为 `true`），供状态机判定「真持窗」（S1）与退出释放时序（S2），见 §2.3 / §2.4 |
| **source 身份** | L16 `source = "herdr:pi"` | `transport.go: SourcePi = "pig"` / `AgentPi = "pi"` | **偏离**：`herdr:pi` 被 Herdr 内置 pi 集成占用，上报被静默丢弃（见 §2.1）；`agent` 上报 `"pi"`（moshi 白名单拒收 `"pig"`） |
| **resume 命令** | 未实现 | `transport.go: ResumeCommand()` + `state.go` 持窗门禁 | **新增**：`["pig","--session",<path|id>]`，仅在 Herdr 已接受过一次状态上报（真持窗）后附带（见 §2.4 S1） |
| **退出释放** | 未实现 | `state.go: HandleSessionShutdown()` | **新增**：`session_shutdown` 时发 `pane.release_agent`（pane_id/source/agent/seq），并先复位状态机、等待在飞排空迭代结束，再把释放包**入队到所有在飞请求之后**（见 §2.4 S2 / S4）；释放包出队时复核世代，被新会话接管时丢弃这份过期释放包（见 §2.4 S3） |
| **超时与重试** | L50-55 (`sendRequest`) | `transport.go: Client.SendRequest()` | 500ms 首次尝试超时，失败后进行 1500ms 第二次尝试重试；单次 attempt 硬上限 = 500ms / 1500ms，与 TS 一致 |
| **序列号生成** | L65-72 (`reportSeq`, `nextReportSeq`) | `transport.go: NextReportSeq()` | `atomic.Uint64` 原子自增，基数对齐 `time.Now().UnixMilli()*1000` |
| **会话路径与 ID 校验** | L74-92 (`updateSessionRef`) | `state.go: UpdateSessionRef()`, `transport.go: IsAbsolutePosixOrWin32()` | POSIX 与 Win32 绝对路径格式双重校验，优先 path 其次 id |
| **会话引用混入** | L94-112 (`withSessionRef`, `currentSessionRef`) | `transport.go: SendReportAgentSession()`, `SendReportAgent()` | 优先注入 `agent_session_path`，若无则注入 `agent_session_id` |
| **会话上报消息** | L114-132 (`reportSession`) | `transport.go: SendReportAgentSession()`, `state.go: ReportSession()`, `PrepareReportSession()` | 仅当存在 sessionRef 时发送；载荷快照与 seq 分配在**同一次加锁**内完成；会话/状态/释放三类请求统一进入**同一发送生命周期**（outbox + 单一 dispatcher），不再由裸 goroutine 直接发送，上线路序与 seq 序一致（见 §2.4 S4） |
| **状态上报消息** | L134-147 (`sendState`) | `transport.go: SendReportAgent()` | 发送 `pane.report_agent`，带 seq 与 sessionRef |
| **串行化队列** | L149-177 (`sendInFlight`, `queuedState`, `drainStateQueue`) | `state.go: StateManager.PublishState()`, `drainLoop()` | 互斥排空循环，发送中新状态覆盖排队，保证单连接在飞且只发最新状态；排空循环不再自己写 socket，而是把报告交给**统一发送生命周期**并等待送达结果（见 §2.4 S4）；排空循环绑定会话世代，世代切换即停止取队列与发送（见 §2.4 S3） |
| **目标状态计算** | L191-199 (`desiredState`) | `state.go: StateManager.desiredStateLocked()` | blockedCount > 0 → blocked(label)；agentActive → working；否则 idle |
| **状态发布防抖** | L201-209 (`publishState`) | `state.go: StateManager.PublishState()`, `publishStateLocked()` | 状态与 message 均未变化时不重发；`force=true` 时无条件触发；发布在锁内完成并带世代校验，过期世代直接丢弃 |
| **进程内阻断总线** | L211-227 (`pi.events.on("herdr:blocked")`) | `state.go: HandleHerdrBlocked()` | 监听 `herdr:blocked`，非 rootSession 忽略，维护 blockedCount 与 label；计数更新与状态发布在同一次加锁内完成 |
| **会话启动接线** | L229-241 (`session_start`) | `state.go: HandleSessionStart()` | 仅 `mode=="tui"` 激活 rootSession（并开启新世代、干净初始化状态机），上报 session，由 `!isIdle` 推导 active 并强制发布 |
| **Agent 启动接线** | L243-251 (`agent_start`) | `state.go: HandleAgentStart()` | 非 rootSession 忽略，在 handler 内部同步快照 sessionRef 并把会话上报**入队**到统一发送生命周期（不再由后台 goroutine 直接发送），保证 `report_agent_session.seq < 随后 report_agent.seq`；全过程以世代号复核，过期 handler 直接丢弃 |
| **Agent 沉淀接线** | L253-260 (`agent_settled`) | `state.go: HandleAgentSettled()` | 非 rootSession 忽略，仅当 `isIdle==true` 时置 active=false 并发布 idle；锁外查询 `IsIdle()` 期间的世代变更会让该次发布作废（见 §2.4 S3）；锁外查询期间若到达更新的 `agent_start`，该次过期 idle 观察按**活动序号**丢弃，不会覆盖最新的 working（见 §2.4 S5） |

---

## 4. 与原版 TS 的主要差异

1. **运行时模型**：TS 运行在 Node.js 单线程事件循环，Go 运行在多 goroutine 并发环境。状态更新和排队使用 `sync.Mutex` 保护，保证线程安全。
2. **Socket 协议与预算控制 (S1)**：TS 基于 `node:net`，Go 基于标准库 `net.DialTimeout` 与 `SetDeadline`。在 dial 前计算剩余 deadline 并在 dial/write/read 共享统一预算，单次 attempt 硬上限 = 500ms / 1500ms，【已对齐】。
3. **平台支持与 Windows 命名管道 (S2)**：原版 TS 支持 Windows Named Pipe（`\\.\pipe\...`），Go 版本由于标准库 net 不原生支持 Windows 命名管道（需引入外部非标准依赖 `winio`），当前仅支持 Linux/macOS（Unix domain socket），Windows 未支持，【差异说明】。
4. **会话上报 seq 顺序 (S3)**：TS 在单线程执行栈中先执行 `void reportSession()` 同步分配 seq，再执行 `publishState()` 分配状态 seq。Go 版在 handler 内**同一次加锁**内快照 `sessionRef` 并分配 `seq`，再把请求交给**统一发送生命周期**（单一 dispatcher 逐个写 socket），严格保证 `report_agent_session.seq < 随后 report_agent.seq` 且**上线路序与 seq 序一致**，【已对齐】（见 §2.4 S4）。
5. **测试可观测性**：Go 实现支持注入超时配置与排空完成通知 channel，使测试可以在真实 Unix Socket 假服务端上在毫秒级快速断言超时重试与排队合并，无需死等真实 2 秒。
6. **source 身份偏离**：TS 原版固定 `source = "herdr:pi"`，而那正是 Herdr 内置 `pi` 集成占用的 source，上报会被静默丢弃；Go 版取 `source = "pig"`（自有集成 source）、`agent = "pi"`（上报 agent 标签，moshi 白名单只认官方 agent 名）；`resume_argv` 中的 `"pig"` 是恢复进程的 PiG 二进制名（见 §2.1）。
7. **resume 命令（`resume_argv`）**：TS 原版未实现。Go 版新增 `["pig","--session",<真实 path 或 id>]`（`transport.go: ResumeCommand()`），且仅在 Herdr 已接受过一次状态上报（真持窗）后附带，避免 Herdr 以 `resume_not_accepted` 拒绝（见 §2 表 `resume_argv` 行、§2.4 S1、§3）。
8. **退出释放（`pane.release_agent`）**：TS 原版未实现。Go 版在 `session_shutdown` 时显式释放持窗，让 Herdr 立即清掉 agent 名、状态与 resume 命令；释放前先复位状态机并等待在飞状态上报结束，保证 release 排在最后一个 report 之后（见 §2.2、§2.4 S2、§3）。
9. **响应信封校验**：TS 原版「收到任意字节即算送达」，会吞掉 Herdr 的错误信封；Go 版解析并校验 `id` / `result` / `error`，非 JSON、错误信封、`id` 不匹配一律判未送达并触发重试（见 §2.3）。
10. **持窗判定与退出时序（Go 并发模型所需）**：TS 单线程事件循环下不存在此类竞态。Go 版把持窗标志从「入队即置位」改为「`drainLoop` 送达后才置位」（S1），并让 `HandleSessionShutdown()` 复位状态机、等待在飞排空迭代结束后再发 `pane.release_agent`（S2），避免 `resume_not_accepted` 与退出后反锁窗格（见 §2.4）。
11. **会话世代隔离（Go 并发模型所需）**：TS 单线程事件循环下 handler 不会被打断，Go 版必须显式建模「handler 执行期间会话可能已被关闭或替换」。Go 版引入 `StateManager.sessionEpoch`：每次 `session_start` / `session_shutdown` 都开启/关闭一个世代，handler 在锁外耗时操作（`IsIdle()`、会话引用查询）之后必须复核世代，过期即丢弃；状态变更与状态发布合并到同一次加锁，退出释放也会复核世代以免抹除新会话的持窗（S3，见 §2.4）。
12. **统一发送生命周期（Go 并发模型所需）**：TS 单线程事件循环下 `reportSession()` 与 `publishState()` 天然按调用序执行。Go 版必须显式建模「谁先写 socket」：会话上报、状态上报、退出释放统一入队到同一 outbox，由单一 dispatcher 按队序逐个写出，`seq` 与入队在同一次加锁内完成，上线路序与 seq 序一致；shutdown 在释放前等待所有在飞请求，释放包出队时复核世代（S4，见 §2.4）。
13. **活动序号（Go 并发模型所需）**：TS 单线程事件循环里 `agent_settled` 的 `IsIdle()` 观察不会被插队。Go 版必须显式建模「锁外观察可能过期」：每个 agent 活动事件领一张递增票号，过期票号的 idle 观察直接丢弃，不会把最新的 working 覆盖回 idle；该机制只作用于 agent 活动，不影响 blocked 计数与优先级（S5，见 §2.4）。

---

## 5. 兼容性注意：不以 herdr 的 idle 分类作为完成信号

### 5.1 实测反例与背景

在真实多代理调度与长时间任务执行中，Herdr 服务端的空闲检测可能存在滞后或误判：
- **实测反例**：实测表明，herdr 会把**正在跑工具**的 agy 误判成 idle——`p12` 在 **21:45:02** 被判 `idle`，实际到 **21:49:42** 才结束（时差长达 4 分 40 秒）。
- **核心结论**：我们 Go 版的上报逻辑**不得**把 herdr 的 idle 分类当作完成信号。

### 5.2 状态判定依据：仅来自 PiG 生命周期与 ctx.IsIdle

本组件向 Herdr 上报的 `state` 绝不依赖 Herdr 的外部状态推断，严格由 PiG 内部事件与 `ctx.IsIdle()` 权威判定，且从**不回读** Herdr 返回的任何状态：
1. **状态唯一来源**（`state.go:293`）：
   `desiredStateLocked` 计算的目标状态仅有三条分支：
   - `sm.blockedCount > 0` → `"blocked"`（附带阻断原因标签）；
   - `sm.agentActive == true` → `"working"`；
   - 否则兜底为 `"idle"`。
2. **`agentActive` 生命周期置位与复位**：
   - **`session_start` 事件**（`state.go:812-813`）：通过 `isIdle, err := provider.IsIdle()` 初始化 `sm.agentActive = (err == nil && !isIdle)`，会话启动时若非空闲直接置为 working（置位与发布在同一次加锁内完成，见 §2.4 S3 / S5）。
   - **`agent_start` 事件**（`state.go:843`）：Agent 启动执行，无条件置 `sm.agentActive = true`，进入 working 状态。
   - **`agent_settled` 事件**（`state.go:923-932`）：仅在 `provider.IsIdle()` 返回 `true` 且无错误时，才置 `sm.agentActive = false` 并触发发布 idle；若 `ctx.IsIdle()` 为 false，保持 working，绝不提前置为 idle；`IsIdle()` 在锁外查询，其间若发生会话世代切换，该次置位与发布作废（见 §2.4 S3）；其间若到达更新的 `agent_start`，该次过期 idle 观察按活动序号丢弃，不会覆盖最新的 working（见 §2.4 S5）。
3. **单向通信：不回读 Herdr 状态，但会校验响应信封**：
   与 Herdr 守护进程的交互仍然只是单向的上报调用（`pane.report_agent`、`pane.report_agent_session`，进程退出时补发 `pane.release_agent`，实现见 `transport.go`），**绝不解析或采信** Herdr 服务端返回的 agent 状态——本组件上报的 `state` 永远只由 PiG 生命周期推导（见本节第 1、2 点）。
   但**送达判定**已从「只看有没有字节返回（`n > 0` 即视作交付成功，连 JSON 都不解析）」改为解析并校验 Herdr 响应信封（`transport.go: validateResponse()`，见 §2.3）：`id` 匹配、`result` 非空、无 `error` 才算送达；错误信封、`id` 不匹配、空响应或非 JSON 响应一律判为**未送达**并走既有超时重试路径。这只回答「这一包 Herdr 收下了吗」，与回读 Herdr 的 agent 状态无关。到达线序由**统一发送生命周期**保证与 seq 序一致（见 §2.4 S4）：会话上报一定先于随后的状态上报抵达 Herdr；下游仍建议按 seq 而非到达序处理。

### 5.3 对上游与下游调度的建议

- **严禁依赖 Herdr 面板状态判定结束**：上游编排层、下游消费端或自动化检查器切勿将 Herdr 面板或状态接口的 idle 分类当作任务完成或空闲的信号。
- **权威完成判定准则**：判断 Agent 执行完成必须以 PiG 宿主派发的 `agent_settled` 事件且 `ctx.IsIdle() == true`，或底层工具进程实际执行退出为准。

---

## 6. 测试与验证

本组件包含完备的单元测试，根据验证目标分别采用真实 Unix Socket 假服务端或内存 Dummy / Seam 接口断言，**绝对不依赖外部 Herdr 守护进程**：

```sh
# 运行单元测试
cd /root/workspace/pi-extensions/go/herdr-agent-state
go test -count=1 -race -v ./...
```

测试用例清单与实现方式：
- **真实 Unix Socket 假服务端用例**：
  - `TestExtension_Disabled`：验证未启用（`HERDR_ENV!=1`、缺少 socket/pane_id）时 0 连接、完全 no-op。
  - `TestTransport_RealUnixSocket_Protocol`：真实 Unix Socket 验证 JSON-RPC 请求结构、换行符结尾、seq 递增及会话引用优先级。
  - `TestTransport_RetryAndTimeout`：服务端延迟不应答时，断言 2 次超时重试与耗时。
  - `TestTransport_ImmediateClose_Retries`：服务端连接立即关闭时，断言触发第 2 次重试。
  - `TestTransport_NonJSONGarbage`：服务端返回非 JSON 垃圾字节时，断言 client 判定**未送达**（`SendRequestAttempt` 返回 false）——送达必须由 Herdr 响应信封校验得出，不能再由「收到任意字节」推断（TS 版 `socket.on('data')` 的字节数语义已被取代）。
  - `TestTransport_JSONErrorResponseIsNotSuccess`：真实 Unix Socket 假服务端返回 Herdr 错误信封 `{"id":..,"error":{"code":..,"message":..}}` 时，断言 `SendRequestAttempt` 返回 false（错误信封不算送达）。
  - `TestTransport_IDMismatchIsNotSuccess`：服务端返回结构合法但 `id` 属于别的请求的成功信封时，断言 `SendRequestAttempt` 返回 false（`id` 不匹配不算送达）。
  - `TestTransport_SingleAttemptHardTimeoutBudget`：服务端只 listen 不应答，断言单次 attempt 总耗时严格受单一 timeout 预算约束（≤ 超时），绝不翻倍。
  - `TestTransport_FragmentedResponseFrame`：服务端把同一条成功信封拆成两个报文段（中间留空档）发送，断言 `SendRequestAttempt` 能把帧**完整读完后**判定送达（半帧不算送达）；另一路服务端只发半帧且不补齐，断言同样判**未送达**（见 §2.3 响应帧完整读取）。
  - `TestShutdown_WaitsForInFlightSessionReport`：真实 Unix Socket 假服务端对会话上报**扣住回包** 300ms，断言同一会话的 `report_agent_session.seq < report_agent.seq`；随后在第二条会话上报仍在飞时触发 `session_shutdown`，断言 `pane.release_agent` **排在仍在飞的会话上报之后**抵达（不被反超）、且 `release_agent.seq > session_report.seq`（见 §2.4 S4）。
  - `TestAgentStart_SessionSeqOrdering`：在 agent_start 触发后，真实 socket 截获上报消息，断言 `report_agent_session.seq < 随后 report_agent.seq`。
  - `TestSessionStart_AsyncNonBlockingAndSeqOrdering`：服务端不应答时断言 `session_start` handler 立即返回（< 50ms 预算，不阻塞 host），且 session 请求最终异步送达、`report_agent_session.seq < 随后 report_agent.seq`。
  - `TestStateMachine_Transitions`：完整验证 `idle -> working -> blocked -> working -> idle` 状态转移序列，验证去重与 force 逻辑。
  - `TestQueue_CoalescingAndSerialization`：高并发快速推入 3 个状态，验证中间状态被自动合并仅发最新状态，且连接峰值并发严格为 1。
  - `TestLifecycle_AgentSettledGate`：使用真实 Unix Socket 假服务端断言非 rootSession 忽略、isIdle 非 true 时绝对零发送且不置 idle、isIdle=true 成功置 idle 并发送上报。
  - `TestRelease_AndResumeArgv`：真实 Unix Socket 截获请求，断言状态上报 `pane.report_agent` 携带 `resume_argv = ["pig","--session",<path>]`；假服务端对状态上报延迟 150ms 才回包，使该上报在飞期间触发 `session_shutdown`，断言 handler 复位整个状态机（`rootSession`/`queuedState`/`agentStateReported`/`lastState`/`lastMessage`/`agentActive`/`blockedCount`/`blockedMessage` 全清、在飞排空结束）、`pane.report_agent` **先于** `pane.release_agent` 抵达（释放包不被在飞上报反超），且 `pane.release_agent` 含 `pane_id`/`source`/`agent`/`seq`、身份字段与 `source=pig`、`agent=pi` 一致、`release_agent.seq > report_agent.seq`；同一用例再追加**会话世代隔离**断言：第二次 `session_start` 后让一个 handler 卡在锁外的 `IsIdle()` 查询上，并发触发 `session_shutdown`，断言该滞后的 handler 不会在 `pane.release_agent` **之后**再发出新的 `pane.report_agent`（见 §2.4 S3）。
  - `TestResume_NotOnSessionReportBeforePaneHeld`：真实 Unix Socket 假服务端下断言 source 尚未持窗时首次 `pane.report_agent_session` **不带** `resume_argv`（否则 Herdr 以 `resume_not_accepted` 拒绝）；经一次**被 Herdr 接受的状态上报**后才置位 `agentStateReported`（入队不置位），此后会话上报携带 `["pig","--session",<path>]`。
- **内存 Dummy / Seam 判定用例**：
  - `TestEventHandlers_Logic`：使用 `dummy.sock` 验证事件总线阻断计数累加与递减逻辑。
  - `TestLifecycle_SessionStartGate`：使用 `dummy.sock` 与 Mock Provider 断言非 tui 模式不启用、tui 模式正确设置 rootSession 与推导 active 状态。
  - `TestLifecycle_AgentStartGate`：使用 `dummy.sock` 与 Mock Provider 断言非 rootSession 忽略、rootSession 激活后置 active=true 并发布 working。
  - `TestAgentActivity_NewerStartSupersedesStaleIdle`：使用 `dummy.sock` 与 Mock Provider（`idleHook` 把 `IsIdle()` 卡在锁外），断言 `agent_settled` 读锁外 idle 期间到达的 `agent_start` 是更新的事件，该次过期 idle 观察被丢弃、状态保持 `working`（见 §2.4 S5）。
  - `TestLifecycle_BlockedPriorityAndMessages`：使用 `dummy.sock` 验证阻断计数与消息优先级（blocked 优先于 working/idle，最新 label 优先）。
  - `TestExtension_EventWiring`：使用 `dummy.sock` 验证 `Extension()` 启用时各事件正确接线无 panic。
  - `TestPathValidation_IsAbsolutePosixOrWin32`：纯函数路径校验，验证 POSIX、Windows 盘符路径、UNC 路径及相对路径识别。
  - `TestSource_NotReservedByHerdr`：断言 `agent == "pi"`、`source == "pig"` 且不以 `herdr:` 开头、也不等于 `herdr:pi`（Herdr 为自己的集成保留该命名空间），并断言 session / release 两类上报携带同一个 `source`。

---

## 7. 加载与运行

在 PiG 启动时通过 `-e` 载入组件目录：

```sh
./bin/pig-test -e ./herdr-agent-state -p "回复 OK"
```

当环境变量未配置 `HERDR_ENV=1`、`HERDR_SOCKET_PATH`、`HERDR_PANE_ID` 时，扩展自动保持静默 no-op，无任何副作用。
