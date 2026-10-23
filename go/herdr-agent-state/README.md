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
| `source` | 必须 | 必须 | 固定 `"herdr:pi"` |
| `agent` | 必须 | 必须 | 固定 `"pi"` |
| `seq` | 可选/递增 | 可选/递增 | 单调自增序列号，起始于 `time.Now().UnixMilli()*1000` |
| `state` | 必须 | - | `"idle"` \| `"working"` \| `"blocked"` |
| `message` | 可选 | - | 当 blocked 时为阻断原因/标签（如 `data.label`），非 blocked 为 null |
| `session_start_source` | - | 可选 | 来自 `session_start` 事件的 `reason`（如 startup/resume） |
| `agent_session_path` | 条件注入 | 条件注入 | 会话文件绝对路径（POSIX 或 Win32），优先级高于 `agent_session_id` |
| `agent_session_id` | 条件注入 | 条件注入 | 若无合法路径且存在会话 ID 时注入 |

---

## 3. TypeScript 原版与 Go 版逐项对照

| 功能模块 | 原版 TS 位置 (`herdr-agent-state.ts`) | Go 版位置与实现 | 行为一致性说明 |
|---|---|---|---|
| **环境变量读取** | L11-17 (`HERDR_ENV`, `socketPath`, `paneId`) | `extension.go: ConfigFromEnv()` | 读取 `HERDR_ENV`、`HERDR_SOCKET_PATH`、`HERDR_PANE_ID` |
| **平台支持与端点解析** | L13-14 (`\\\\.\\pipe\\` 拼装) | `transport.go: ResolveSocketEndpoint()` | 仅 Linux/macOS，Windows 未支持（标准库 `net` 无法在无三方依赖下原生支持 Windows Named Pipe） |
| **启用条件判定** | L18-20 (`enabled()`) | `extension.go: Enabled()` | `HERDR_ENV=="1" && socketPath!="" && paneId!=""`，不满足时完全 no-op |
| **单次发送尝试** | L22-48 (`sendRequestAttempt`) | `transport.go: Client.SendRequestAttempt()` | 建立 Unix 短连接，写 JSON + `\n`，读任意响应判定成功；单次 attempt 硬上限 = 500ms / 1500ms，与 TS 一致（dial 与 I/O 共享总超时 deadline） |
| **超时与重试** | L50-55 (`sendRequest`) | `transport.go: Client.SendRequest()` | 500ms 首次尝试超时，失败后进行 1500ms 第二次尝试重试；单次 attempt 硬上限 = 500ms / 1500ms，与 TS 一致 |
| **序列号生成** | L65-72 (`reportSeq`, `nextReportSeq`) | `transport.go: NextReportSeq()` | `atomic.Uint64` 原子自增，基数对齐 `time.Now().UnixMilli()*1000` |
| **会话路径与 ID 校验** | L74-92 (`updateSessionRef`) | `state.go: UpdateSessionRef()`, `transport.go: IsAbsolutePosixOrWin32()` | POSIX 与 Win32 绝对路径格式双重校验，优先 path 其次 id |
| **会话引用混入** | L94-112 (`withSessionRef`, `currentSessionRef`) | `transport.go: SendReportAgentSession()`, `SendReportAgent()` | 优先注入 `agent_session_path`，若无则注入 `agent_session_id` |
| **会话上报消息** | L114-132 (`reportSession`) | `transport.go: SendReportAgentSession()`, `state.go: ReportSession()`, `PrepareReportSession()` | 仅当存在 sessionRef 时发送；在事件处理中同步提取 sessionRef 并自增分配 seq，避免并发上报导致 seq 倒置 |
| **状态上报消息** | L134-147 (`sendState`) | `transport.go: SendReportAgent()` | 发送 `pane.report_agent`，带 seq 与 sessionRef |
| **串行化队列** | L149-177 (`sendInFlight`, `queuedState`, `drainStateQueue`) | `state.go: StateManager.PublishState()`, `drainLoop()` | 互斥排空循环，发送中新状态覆盖排队，保证单连接在飞且只发最新状态 |
| **目标状态计算** | L191-199 (`desiredState`) | `state.go: StateManager.desiredStateLocked()` | blockedCount > 0 → blocked(label)；agentActive → working；否则 idle |
| **状态发布防抖** | L201-209 (`publishState`) | `state.go: StateManager.PublishState()` | 状态与 message 均未变化时不重发；`force=true` 时无条件触发 |
| **进程内阻断总线** | L211-227 (`pi.events.on("herdr:blocked")`) | `state.go: HandleHerdrBlocked()` | 监听 `herdr:blocked`，非 rootSession 忽略，维护 blockedCount 与 label |
| **会话启动接线** | L229-241 (`session_start`) | `state.go: HandleSessionStart()` | 仅 `mode=="tui"` 激活 rootSession，上报 session，由 `!isIdle` 推导 active 并强制发布 |
| **Agent 启动接线** | L243-251 (`agent_start`) | `state.go: HandleAgentStart()` | 非 rootSession 忽略，在 handler 内部同步快照 sessionRef 并预分配 seq，后台 goroutine 发送，确保 `report_agent_session.seq < 随后 report_agent.seq` |
| **Agent 沉淀接线** | L253-260 (`agent_settled`) | `state.go: HandleAgentSettled()` | 非 rootSession 忽略，仅当 `isIdle==true` 时置 active=false 并发布 idle |

---

## 4. 与原版 TS 的主要差异

1. **运行时模型**：TS 运行在 Node.js 单线程事件循环，Go 运行在多 goroutine 并发环境。状态更新和排队使用 `sync.Mutex` 保护，保证线程安全。
2. **Socket 协议与预算控制 (S1)**：TS 基于 `node:net`，Go 基于标准库 `net.DialTimeout` 与 `SetDeadline`。在 dial 前计算剩余 deadline 并在 dial/write/read 共享统一预算，单次 attempt 硬上限 = 500ms / 1500ms，【已对齐】。
3. **平台支持与 Windows 命名管道 (S2)**：原版 TS 支持 Windows Named Pipe（`\\.\pipe\...`），Go 版本由于标准库 net 不原生支持 Windows 命名管道（需引入外部非标准依赖 `winio`），当前仅支持 Linux/macOS（Unix domain socket），Windows 未支持，【差异说明】。
4. **会话上报 seq 顺序 (S3)**：TS 在单线程执行栈中先执行 `void reportSession()` 同步分配 seq，再执行 `publishState()` 分配状态 seq。Go 版通过在 handler 内同步快照 `sessionRef` 并预分配 `seq`，由后台 goroutine 仅负责网络发送，严格保证 `report_agent_session.seq < 随后 report_agent.seq`，【已对齐】。
5. **测试可观测性**：Go 实现支持注入超时配置与排空完成通知 channel，使测试可以在真实 Unix Socket 假服务端上在毫秒级快速断言超时重试与排队合并，无需死等真实 2 秒。

---

## 5. 兼容性注意：不以 herdr 的 idle 分类作为完成信号

### 5.1 实测反例与背景

在真实多代理调度与长时间任务执行中，Herdr 服务端的空闲检测可能存在滞后或误判：
- **实测反例**：实测表明，herdr 会把**正在跑工具**的 agy 误判成 idle——`p12` 在 **21:45:02** 被判 `idle`，实际到 **21:49:42** 才结束（时差长达 4 分 40 秒）。
- **核心结论**：我们 Go 版的上报逻辑**不得**把 herdr 的 idle 分类当作完成信号。

### 5.2 状态判定依据：仅来自 PiG 生命周期与 ctx.IsIdle

本组件向 Herdr 上报的 `state` 绝不依赖 Herdr 的外部状态推断，严格由 PiG 内部事件与 `ctx.IsIdle()` 权威判定，且从**不回读** Herdr 返回的任何状态：
1. **状态唯一来源**（`state.go:152-165`）：
   `desiredStateLocked` 计算的目标状态仅有三条分支：
   - `sm.blockedCount > 0` → `"blocked"`（附带阻断原因标签）；
   - `sm.agentActive == true` → `"working"`；
   - 否则兜底为 `"idle"`。
2. **`agentActive` 生命周期置位与复位**：
   - **`session_start` 事件**（`state.go:321-324`）：通过 `isIdle, err := provider.IsIdle()` 初始化 `sm.agentActive = (err == nil && !isIdle)`，会话启动时若非空闲直接置为 working。
   - **`agent_start` 事件**（`state.go:353-355`）：Agent 启动执行，无条件置 `sm.agentActive = true`，进入 working 状态。
   - **`agent_settled` 事件**（`state.go:375-382`）：仅在 `provider.IsIdle()` 返回 `true` 且无错误时，才置 `sm.agentActive = false` 并触发发布 idle；若 `ctx.IsIdle()` 为 false，保持 working，绝不提前置为 idle。
3. **单向通信从不回读**：
   与 Herdr 守护进程的交互完全是单向的上报调用（`pane.report_agent` 与 `pane.report_agent_session`，见 `transport.go:214-273`），底层只判断是否有字节返回（`n > 0` 即视作交付成功，连 JSON 都不解析），绝不解析或采信 Herdr 服务端返回的 agent 状态。到达线序不保证（`report_agent` 可能先于 `report_agent_session` 到达），但 seq 分配序保证 session < agent；下游应按 seq 而不是到达序处理。

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
  - `TestTransport_NonJSONGarbage`：服务端返回非 JSON 垃圾字节时，断言 client 判定发送成功（与 TS socket.on('data') 语义对齐）。
  - `TestTransport_SingleAttemptHardTimeoutBudget`：服务端只 listen 不应答，断言单次 attempt 总耗时严格受单一 timeout 预算约束（≤ 超时），绝不翻倍。
  - `TestAgentStart_SessionSeqOrdering`：在 agent_start 触发后，真实 socket 截获上报消息，断言 `report_agent_session.seq < 随后 report_agent.seq`。
  - `TestSessionStart_AsyncNonBlockingAndSeqOrdering`：服务端不应答时断言 `session_start` handler 立即返回（< 50ms 预算，不阻塞 host），且 session 请求最终异步送达、`report_agent_session.seq < 随后 report_agent.seq`。
  - `TestStateMachine_Transitions`：完整验证 `idle -> working -> blocked -> working -> idle` 状态转移序列，验证去重与 force 逻辑。
  - `TestQueue_CoalescingAndSerialization`：高并发快速推入 3 个状态，验证中间状态被自动合并仅发最新状态，且连接峰值并发严格为 1。
  - `TestLifecycle_AgentSettledGate`：使用真实 Unix Socket 假服务端断言非 rootSession 忽略、isIdle 非 true 时绝对零发送且不置 idle、isIdle=true 成功置 idle 并发送上报。
- **内存 Dummy / Seam 判定用例**：
  - `TestEventHandlers_Logic`：使用 `dummy.sock` 验证事件总线阻断计数累加与递减逻辑。
  - `TestLifecycle_SessionStartGate`：使用 `dummy.sock` 与 Mock Provider 断言非 tui 模式不启用、tui 模式正确设置 rootSession 与推导 active 状态。
  - `TestLifecycle_AgentStartGate`：使用 `dummy.sock` 与 Mock Provider 断言非 rootSession 忽略、rootSession 激活后置 active=true 并发布 working。
  - `TestLifecycle_BlockedPriorityAndMessages`：使用 `dummy.sock` 验证阻断计数与消息优先级（blocked 优先于 working/idle，最新 label 优先）。
  - `TestExtension_EventWiring`：使用 `dummy.sock` 验证 `Extension()` 启用时各事件正确接线无 panic。
  - `TestPathValidation_IsAbsolutePosixOrWin32`：纯函数路径校验，验证 POSIX、Windows 盘符路径、UNC 路径及相对路径识别。

---

## 7. 加载与运行

在 PiG 启动时通过 `-e` 载入组件目录：

```sh
./bin/pig-test -e ./herdr-agent-state -p "回复 OK"
```

当环境变量未配置 `HERDR_ENV=1`、`HERDR_SOCKET_PATH`、`HERDR_PANE_ID` 时，扩展自动保持静默 no-op，无任何副作用。
