# moshi-hooks（Go 移植）

PiG 的 moshi-hook 生命周期钩子扩展。它把 Pi 的会话事件归一化后，以**一条连接一条 JSON 行**的
协议写入 moshi-hook 守护进程的 Unix socket（Windows 为命名管道）；守护进程不在时连接失败被
静默吞掉，用户的回合绝不被打断。

- **模块**：`github.com/dorokuma/pi-extensions/go/moshi-hooks`（独立嵌套 module，见 `go/README.md`
  「组件嵌套 Module」；`go/go.work` 中有 `./moshi-hooks` 一行）
- **权威源（基线）**：`/root/.pi/agent/extensions/moshi-hooks.ts`，836 行，由
  `/root/.local/bin/moshi-hook` 生成。本仓没有它的 `src`（非 hosted 扩展）。
- **本 Go 版是本仓对它的逐函数移植**；TS 部署件保持原样（铁律①，仓库内不写 `/root/.pi`）。
- 移植范围：`moshi-hooks.ts` 的**全部 836 行**（`helperBinary` 常量到 `moshiPiHook` 导出）。
  反向自检（行号对照表见下）用于 reviewer 逐行核验。

## 1. 行为契约（与 TS 逐条一致）

| 契约 | 实现 |
|---|---|
| Socket 路径：`MOSHI_SOCKET_PATH` 非空直返 → darwin `~/Library/Application Support/Moshi/moshi-hook.sock` → windows `\\.\pipe\moshi-hook` → `XDG_RUNTIME_DIR/moshi-hook.sock` → `/tmp/moshi-hook.sock` | `ResolveSocketPath`（socket.go:40） |
| 单连接一消息：dial → 写紧凑单行 JSON + `\n` → `CloseWrite` 半关闭 → 读干守护进程 ack 到 EOF → `Close` | `deliverEnvelope`（socket.go:70） |
| 单次发送上限 250ms（连接超时 + `SetDeadline`） | `SendTimeout`（socket.go:23） |
| 所有连接/读写错误静默吞掉 | `deliverEnvelope` 无错误返回 |
| **严格保序**：单 worker 串行投递，避免"解决事件"先于"请求事件"到达 | `Sender`（socket.go:120）`run`（socket.go:145） |
| `FlushSends(timeout)`：等待队列排空或超时 | socket.go:182 / socket.go:232 |
| `session_shutdown` 传 1000ms | `ShutdownFlushTimeout`（socket.go:23）+ extension.go:172 注册处 |
| 终端上下文进程级解析一次（含测试重置） | `TerminalContextForProcess`（terminal.go:164）/ `ResetTerminalContextForTest`（terminal.go:176） |
| 子代理（lineage 子会话）不进收件箱；header 判定按路径缓存 | `sessionIsChild` / `transcriptHeaderIsChild`（state.go） |
| transcriptPath 四级回退链（见下） | `resolveTranscriptPath`（state.go） |
| stop 去重：完成态下 5s 内同签名只发一次 | `recordTaskCompletion`（state.go:106） |

envelope 字段顺序 = TS 插入顺序（`baseEnvelope` 的 21 个基础字段，然后按事件追加
`actionId`/`phase`/`category`/`toolName`/`title`/`subtitle`/`message`/`interactivePrompt`），
JSON 名逐字照抄 TS 属性名（`herdrWorkspaceId`、`herdrTabId`、`toolUseId`、`planFilePath` …）。
序列化用 `SetEscapeHTML(false)`，避免 Go 默认把 `<`/`>`/`&` 转成 `\u003c` 等与
`JSON.stringify` 不同的字节。

## 2. TS ↔ Go 对照表

行号为 `moshi-hooks.ts` 的实际行号（`grep -n` 核对过）；Go 侧列 `文件:行`。

| TS 行 | 内容 | Go |
|---|---|---|
| L15-16 | `helperBinary`（仅调试/手工重放用，从不 spawn） | extension.go:24 |
| L18-19 | `agentSource="pi"` / `agentDisplayName="Pi"` | state.go:22 |
| L21-37 | `resolveSocketPath` | socket.go:40 |
| L39-53 | 保序队列说明 + `inFlightSends`/`sendTimeoutMs`/`sendQueue` | socket.go:23、socket.go:120 |
| L55-60 | `sendEnvelope` | socket.go:166、socket.go:228 |
| L62-91 | `deliverEnvelope` | socket.go:70 |
| L93-106 | `flushSends` | socket.go:182、socket.go:232 |
| L108-123 | `interface TerminalContext` | terminal.go:26 |
| L125-127 | `muxEnvKeys` | terminal.go:47（`MuxEnvKeys`） |
| L129-136 | `runForOutput` | terminal.go:74 |
| L138-142 | `tmuxSocketFromEnv` | terminal.go:60 |
| L144-200 | `resolveTerminalContext` | terminal.go:84 |
| L202 | `const terminalContext`（进程级缓存） | terminal.go:164 |
| L204-209 | `projectNameFromCwd` | terminal.go:185 |
| L211-213 | `projectNameForCwd` | terminal.go:199 |
| L215-220 | `firstString` | text.go:77 |
| L222-236 | `textFromContent` | text.go:87 |
| L238-246 | `textFromMessage` | text.go:118 |
| L248-258 | `modelLabel` | text.go:132（宿主取值：extension.go:66） |
| L260-275 | `sessionID` | extension.go:101 |
| L277-286（基线 TS）/ TS pig 的 `transcriptPath` 四级链 | `resolveTranscriptPath`（state.go）＋ `safeSessionFile`（extension.go） |
| L288-298 | `contextRemaining` | extension.go:77 |
| L300-308 | `newSessionID` | text.go:215 |
| L310-322 | 9 条 markdown 正则 | text.go:39-73 |
| L324-336 | `stripMarkdown` | text.go:142 |
| L338-342 | `truncateText` | text.go:160 |
| L344-346 | `plainTextClip` | text.go:174 |
| L348-363 | `displayUserPrompt` | text.go:178 |
| L365-368 | `formatUserPromptDetail` | text.go:194 |
| L370-387 | `interface SessionState` | state.go:48 |
| L389-408 | `stateFor` / `sessions` | state.go:72 |
| L410-412 | `nowSeconds` | state.go:42 |
| L414-420 | `promptSequenceForSubmit` | state.go:93 |
| L422-437 | `recordTaskCompletion`（5s 冷却） | state.go:106 |
| L439-462 | `sessionIsChild` + `transcriptChildCache` | state.go:176 |
| L464-485 | `transcriptHeaderIsChild` | state.go:202 |
| L487-496 | `interface HookPayload` | state.go:128 |
| L498-528 | `baseEnvelope` | state.go:227 |
| L530-696 | `handleEvent` | state.go:266 |
| L698-734 | `askQuestions` | state.go:477 |
| L736-758 | `planFileOnDisk` | state.go:535 |
| L760-767 | `planSummary` | text.go:204 |
| L769-772 | `agentKind` | extension.go:95 |
| L774-793 | `send` | extension.go:108 |
| L795-836 | `moshiPiHook`（6 个钩子） | extension.go:172 |

### 2.1 `handleEvent` 分支对照

| TS 行 | 事件 | Go 行 | 要点 |
|---|---|---|---|
| L537-554 | `SessionStart` | state.go:279 | 无 category 的静默状态载体；记录 model/transcript；修正已完成序列 |
| L555-563 | `SessionEnd` | state.go:301 | `type=session.closed`、`category=session_ended` |
| L564-579 | `UserPromptSubmit` | state.go:311 | 首次→`SessionStart`+`session_started`；后续→`UserPromptSubmit`+沿用上一条 title |
| L580-581 | `AgentStart` | state.go:336 | 不发送 |
| L582-603 | `PermissionRequest` | state.go:339 | `actionId`（缺 tool_use_id 时随机 32 hex）、`phase=waitingForApproval`、`subtitle=Answer in terminal` |
| L604-627 | `PlanReview` | state.go:364 | `toolName=exit_plan_mode`、`interactivePrompt.kind=plan`（带 `toolUseId`/`planFilePath`） |
| L628-649 | `QuestionAsked` | state.go:393 | `toolName=ask`、`interactivePrompt.kind=question`、message=首问句 |
| L650-653 | `PlanResolved` | state.go:419 | 无 category；守护进程撤卡 |
| L654-663 | `QuestionResolved` | state.go:423 | 无 category；`interactivePrompt={kind,toolUseId}` |
| L664-678 | `PermissionResolved` | state.go:433 | `category=session_started`、补 `actionId`、带出 `lastToolName` |
| L679-696 | `AgentEnd` / `TurnEnd` | state.go:450 | 去重后 `category=task_complete`，title=clip(assistant,80) |
| L693-695 | 默认分支 | state.go:469 | 基础 envelope，无 category |

### 2.2 钩子注册对照（TS L795-836）

| TS 事件 | Go 调用 | 备注 |
|---|---|---|
| `session_start` | extension.go:177 | `send("SessionStart")` |
| `before_agent_start` | extension.go:182 | `extra={prompt: event["prompt"]}` |
| `agent_start` | extension.go:187 | `send("AgentStart")`（归一化后不发送） |
| `agent_end` | extension.go:194 | 逆序找最后一条 `role=="assistant"`，只记不发 |
| `agent_settled` | extension.go:201 | `send("AgentEnd", extra={last_assistant_message})` 并清空 |
| `session_shutdown` | extension.go:209 | `send("SessionEnd")` + `FlushSends(1s)` |

### 2.3 pig 适配：transcriptPath 四级回退链

`extensions/moshi-hooks-pig/` 的 TS 版（已随本改动删除）把 pi 版「只问
sessionManager」的 `transcriptPath` 扩成四级回退链——因为 packed-go cell
里宿主可能不给 `getSessionFile`，而 MOSHI Chat View 必须落到**本 pig 会话**的
真实 jsonl（绝不能用 `~/.pi` 下的路径）。Go 版把该链原样吃进 `state.go`：

1. **`sessionFile`**（`safeSessionFile`，extension.go）—— sessionManager
   `getSessionFile()`，权威答案。
2. **event 字段**——`transcript_path` / `transcriptPath` / `sessionFile` /
   `session_file` / `sessionPath`（`eventTranscriptPath`）。
3. **`sessionTranscripts` 缓存**——仅当 stage 1/2 权威值缺失时兜底；
   权威值一旦出现立即覆盖缓存（「升级」而非固化），修正「stage 5 猜错后
   永久挡住权威路径」的场景。
4. **`findSessionFileByID(sessionID)`**——按 sessionID 扫 sessions root 下每个
   cwd 目录找 `*_<sessionId>.jsonl`；覆盖「pane cwd 与 session-dir cwd 不一致」
   （从 tab 根拉起但被派到 worktree 的 worker）。真实会话匹配，同样回写缓存。
5. **`newestJsonl(pigSessionDirForCwd(cwd))`**——本 cwd session 目录里最新的
   `.jsonl`（`session_start` 早于首次落盘，UserPromptSubmit 时文件已在）。
   纯猜测（文件可能属同 cwd 的另一会话），**不回写缓存**，后续权威值仍可纠正。

sessions root 解析（`pigSessionsRoot`）与 PiG node shim
（`runtime-node/shims/pig-config.mjs`）同序：`PIG_EXT_HOST_INFO` JSON 的
`agentDir`（直接解析 env，不走 sdk.AgentDir 全局缓存，便于 `t.Setenv` 隔离）→
`PIG_CODING_AGENT_DIR` → `PI_CODING_AGENT_DIR` → `PIG_HOME` →
`XDG_CONFIG_HOME/pig` → `~/.pig`；前两级拼 `/sessions`，后三级拼
`/agent/sessions`。解析结果中**权威来源（stage 1/2）与 stage 4 的 id 匹配**按 sessionID
缓存在 `sessionTranscripts`（`resolveTranscriptPath` 是唯一写方，
`handleEvent` 不再重复写入），后续不带路径的事件仍能定位 transcript；stage 5
的 cwd 猜测不缓存，避免把另一会话的文件误当本会话权威值——猜测随
`TranscriptPathGuessed` 标记返回，`sessionIsChild` 也不拿它做 lineage 判定
（旧文件可能带 `parentSession` 误抑制本会话）。packed-go cell 里源码改动由 PiG 自动重编，无需
手工 install。

## 3. 与 TS 的差异（逐条，附理由）

1. **`ctx.agent.kind` → 事件载荷 `agent_kind`**（state.go:176、extension.go:95）
   PiG Go SDK 没有 agent 访问器。本文件是「为 Pi 生成」的版本，`agentSource === "pi"`，
   `sessionIsChild` 的第一个分支（`agentSource === "omp"` 才读 `agent_kind`）**静态不可达**，
   因此线上行为与 TS 完全一致；`agent_kind` 仅在宿主主动放进事件载荷时才被读取。
2. **`modelName` 的来源**（extension.go:66）
   TS 读对象字段 `displayName`/`display_name`/`name`/`id`/`modelId`。Go 用
   `ctx.GetModelInfo()`（结构体只有 `Name`/`ID`）按 `Name → ID` 取值，宿主不答时回落到
   `ctx.Model()`。实跑取值 `智能路由`（见第 5 节连接 1），与 TS 的 displayName 语义一致。
3. **`planFileOnDisk` 的 `getArtifactsDir`**（state.go:535）
   PiG SDK 没有 `sessionManager.getArtifactsDir`，故 `PlanContext.ArtifactsDir()` 由调用方注入；
   运行期为空时走 TS 同款回落（transcript 去 `.jsonl` 后缀）。**该函数在 TS 里从未被调用**
   （`PlanReview` 分支直接用 `plan_file_path` 原值），Go 侧保留为对齐用的可测实现，不影响线上行为。
4. **Windows 命名管道**（socket.go:70）
   路径解析照抄（返回 `\\.\pipe\moshi-hook`），但投递在 `GOOS == "windows"` 时为文档化 no-op：
   纯 `net` 无法 dial 命名管道，而任何可阻塞的替代都会违反「hook 绝不打断回合」。目标平台
   Linux/macOS 走 Unix socket，功能完整。
5. **`session_shutdown` 的等待方式**（extension.go:209）
   TS 返回 `flushSends(1000)` promise 由宿主 await；Go 在 handler 内同步等待 ≤1s（宿主同样 await
   handler），语义等价。
6. **JS `\s` 字符集**（text.go:21）
   Go 的 `\s` 只有 ASCII。为与 JS 正则逐字节一致，所有 `\s` 位置都替换为 ECMA-262
   WhiteSpace + LineTerminator 全集（`jsSpaceClass`），`trim` 亦用同一集合（`jsTrim`）。
   唯一残留差异：Go regexp 是 RE2，无逆向引用——TS 的 9 条正则都不含逆向引用，pattern 等价。
7. **`muxEnv` 键序**（terminal.go:45）
   TS 按环境注入顺序写对象，Go 的 `map[string]string` 序列化按键名字典序。守护进程按键取值，
   语义无关；字段始终存在（`{}` 而非省略或 `null`）。
8. **进程级状态**（state.go:38、socket.go:211、terminal.go:157）
   TS 用模块级 `const`/`Map`；Go 用互斥保护的包级状态（事件可能并发到达），并额外提供
   `ResetSessionsForTest`/`SetSenderForTest`/`ResetSenderForTest`/`ResetTerminalContextForTest`
   供测试重置。这些是 TS 没有的测试面，不改变线上行为。
9. **测试专用注入缝**：`resolveTerminalContext(getenv, run)` 与 `newSender(sockPath, deliver, timeout)`
   把进程/环境/网络依赖注入化，便于无 socket 单测（TS 无对应物）。
10. **未移植**：无。TS 里被引用但未调用的 `helperBinary` 常量照留（extension.go:24）。

## 4. 文件布局

| 文件 | 内容 |
|---|---|
| `envelope.go` | `Envelope`/`InteractivePrompt`/`Question`/`QuestionOption` 线格式 |
| `socket.go` | `ResolveSocketPath`、`deliverEnvelope`、`Sender`（保序队列 + `Flush`）、全局 sender |
| `terminal.go` | `TerminalContext` 探测（tmux/zellij/herdr/muxEnv）、项目名 |
| `text.go` | 文本清洗（markdown/截断/user_query）、`planSummary`、`newSessionID` |
| `state.go` | `SessionState`、lineage 判定、`baseEnvelope`、`handleEvent`、`askQuestions`、`planFileOnDisk` |
| `extension.go` | 宿主取值适配（`safeCwd` 等）、`Send`、`Extension()` 的 6 个钩子 |

## 5. 测试与实测

```bash
cd /root/workspace/pi-extensions/go/moshi-hooks
gofmt -s -l .                              # 期望无输出
go build ./...                             # EXIT 0
go vet ./...                               # EXIT 0
go test -count=1 -race -v ./...            # 全绿，无 DATA RACE
```

覆盖（59 个顶层测试，含 40 个子测试，合计 99 条用例，`-race` 下无 `DATA RACE`）：

- **文本**：`stripMarkdown`（代码块/行内码/链接/粗斜体/标题列表/空白折叠）、`truncateText`
  （含 rune 而非字节）、`plainTextClip`、`displayUserPrompt`（`<user_query>` 大小写不敏感、
  skill/image/system-reminder 剥离）、`formatUserPromptDetail`（200 rune + `You: ` 前缀）、
  `planSummary`、`firstString`、`textFromMessage`、`modelLabel`、`newSessionID`、`jsTrim`。
- **终端上下文**：tmux（探测命令与参数、`#S\t#I` 解析、失败回落）、herdr（含压过 zellij、
  缺 `HERDR_ENV` 时忽略 id、label 留空）、zellij（session/pane/`ZELLIJ` 单独命中）、`muxEnv`
  取非空键、`tmuxSocketFromEnv`（无逗号/逗号在首位）、`projectNameFromCwd`、
  `projectNameForCwd` 优先级、进程级缓存与重置。
- **socket**：路径优先级三档；保序投递；**同一时刻只有一个投递在跑**；`Flush` 等队列；
  `Flush` 超时上限；空队列立即返回。**真实 Unix socket 集成测试**：单连接恰好一行 JSON +
  `\n`、服务端观察到干净半关闭（EOF 用时 < 250ms）、字段顺序前缀、`muxEnv` 为对象；
  慢守护进程（600ms 后才 ack）在 250ms deadline 释放、不阻塞队列；无守护进程时立即静默返回；
  4 条消息经 socket 严格保序且每条连接只有一行；`FlushSends` 返回前 socket 端已收到全部 envelope。
- **状态机**：`SessionStart` 基础字段（含 `requestedAt` ISO 毫秒、`muxEnv` 为空对象、
  `category/title/message` 全部不存在）、`contextRemaining` 四舍五入与存在性、首问句
  `SessionStart`→`UserPromptSubmit` 的标题沿用、`AgentStart` 不发、`AgentEnd` 完成/冷却/
  换签名/过冷却后再发、无 assistant 文本时默认标题、`TurnEnd` 同完成、`SessionEnd`
  的 `session.closed`、**子会话静默**（`UserPromptSubmit`/`SessionEnd`/`AgentEnd` 全跳过而
  `SessionStart` 照发）、lineage 按路径缓存、不可读 transcript 不算子会话、`PermissionRequest`/
  `PlanReview`/`QuestionAsked`/`PlanResolved`/`QuestionResolved`/`PermissionResolved` 全字段、
  未知事件直通、缺 `session_id` 时随机 32 hex 且互不相同、`askQuestions` 归一化、
  `planFileOnDisk` 六种路径、`transcriptHeaderIsChild` 三态、`recordTaskCompletion` 冷却语义、
  `promptSequenceForSubmit` 四种状态。

### 5.1 PiG 隔离加载实测（真实输出）

本机确实有一份 moshi-hook 守护进程，为了不污染真收件箱，用一次性 capture 守护进程
（`/tmp/moshi-capture/main.go`，同样的单连接一行协议，把 envelope 追加到日志）监听
`/tmp/moshi-capture/moshi-hook.sock`，再用 `MOSHI_SOCKET_PATH` 指过去：

```console
$ /tmp/moshi-capture/capture /tmp/moshi-capture/moshi-hook.sock /tmp/moshi-capture/envelopes.log &
$ cd /root/workspace/pi-extensions/go
$ MOSHI_SOCKET_PATH=/tmp/moshi-capture/moshi-hook.sock ./bin/pig-test -e ./moshi-hooks \
    --mode json -p "回复 OK"
   … 完整事件流：session → agent_start → turn_start → message_start/update/end →
   turn_end → agent_end → agent_settled；无 stderr 报错，命令正常结束。
```

capture 端收到的 4 条连接（原文，仅换行折行）：

```text
--- connection 1 (678 bytes) ---
{"type":"session.update","source":"pi","sessionId":"01a16e92-5d69-…","eventName":"SessionStart",
 "cwd":"/root/workspace/pi-extensions/go","projectName":"go","terminalKind":"herdr","muxEnv":{},
 "tmuxSession":"","tmuxWindow":"","tmuxPane":"","tmuxSocket":"","zellijSession":"","zellijPane":"",
 "herdrSession":"","herdrPane":"w37:p2D","herdrWorkspaceId":"w37","herdrWorkspace":"",
 "herdrTabId":"w37:t2D","herdrTab":"","modelName":"智能路由",
 "transcriptPath":"/root/.pig-test/agent/sessions/…/2026-10-24T15-40-48-362Z_01a16e92-….jsonl",
 "requestedAt":"2026-10-24T15:40:48.366Z","contextRemaining":100}
--- connection 2 (763 bytes) ---
{…同上基础字段…,"eventName":"SessionStart","category":"session_started",
 "title":"Pi session started","message":"You: 回复 OK"}
--- connection 3 (741 bytes) ---
{…,"eventName":"AgentEnd","category":"task_complete","title":"OK","message":"You: 回复 OK"}
--- connection 4 (730 bytes) ---
{"type":"session.closed",…,"eventName":"SessionEnd","category":"session_ended",
 "title":"Pi session ended"}
```

对照 TS 语义逐条成立：

- 连接 1 是 `session_start` 的**静默状态载体**（无 `category/title/message`，只有基础字段）；
- 连接 2 是首个 `before_agent_start`：`alreadyPushed === false` → `eventName` 仍是
  `SessionStart`，带 `session_started` + `You: 回复 OK`；
- 连接 3 来自 `agent_settled`（`agent_end` 只记录最后一条 assistant 文本）→ `AgentEnd` +
  `task_complete`，title 为 `OK`；
- 连接 4 是 `session_shutdown` → `session.closed` + `session_ended`，且在进程退出前送达
  （`FlushSends` 生效，否则这条会丢）；
- `terminalKind:"herdr"` 与 `herdrPane/herdrWorkspaceId/herdrTabId` 来自当前 pane 的真实环境，
  `muxEnv` 为空对象（本 pane 无 tern/TUIOS 变量），与 TS 的字段形态一致。

capture 进程与日志在实测后已清理（`/tmp/moshi-capture/`），不留在仓库里。

## 6. 遗留与注意

1. **未部署**：本组件只在隔离目录 `/root/.pig-test` 里验证过；未安装到 `/root/.pi/agent/extensions/`，
   未 `/reload`、未重启任何 pi。切换属后续批次决定。
2. **Windows 命名管道投递**是文档化缺口（见差异 4）；若将来要支持，需要引入平台专用依赖或在
   宿主侧换协议，二者都要主代理拍板。
3. **`ctx.GetModelInfo()` 每次事件一次宿主往返**（`agent_end` 不发 envelope，故只发生在
   `session_start`/`before_agent_start`/`agent_start`/`agent_settled`/`session_shutdown`）。
   实测 4 次事件未观测到可感延迟；若日后 PiG 暴露更廉价的模型访问器，`modelLabelForCtx` 是唯一改动点。
4. **`agent_kind`**：若将来 PiG SDK 提供 agent 访问器，`agentKind`（extension.go:95）应改为直接读它，
   以便与 OMP 一族的 `sessionIsChild` 首分支对齐。
