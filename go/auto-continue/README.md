# auto-continue（Go 移植）

`auto-continue` 的 Go 版：一次瞬时 provider/中转/网络失败结束后，自动给模型发一条
用户消息「继续」，让它接着干。它不重试认证（401/403）、配额、上下文溢出、截断
（`stopReason: length`）和用户主动中止。

第二个续跑入口是「早衰 stop」：上游中转通道在任务未完成时提前返回
`finish_reason=stop`（只有 thinking、或正文中途断句），结构上没说完 + 两道闸都过时，
复用同一条续跑链路（含 streak/maxAttempts 上限）。

- 模块：`github.com/dorokuma/pi-extensions/go/auto-continue`（`go 1.26`，仅标准库 + PiG SDK）
- 工厂：`func Extension() *sdk.Extension`（`extension.go:68`）
- 权威源（TS）：`/root/workspace/pi-extensions/extensions/auto-continue/src/auto-continue.ts`
  与部署件 `/root/.pi/agent/extensions/auto-continue.ts` **md5 完全一致**
  （`8d5c33da9ffce60f400ed73f90df32e1`，本次实测），故不存在「部署件为准」的差异项。
- 命令：`/auto-continue [on|off|status|max N]`
- 环境变量：`PI_AUTO_CONTINUE=0|1`、`PI_AUTO_CONTINUE_MAX`、`PI_AUTO_CONTINUE_DELAY_MS`

## 文件结构

| 文件 | 职责 |
|---|---|
| `constants.go` | TS 的常量（LOG/STATUS_KEY/默认值/阈值/BARE_ESC） |
| `messages.go` | 消息/content 块读取、JS 语义 trim、截断文本判定、时间戳扫描 |
| `numbers.go` | JS `Number()` / `String()` / 模板数字的 Go 等价实现 |
| `errors.go` | HTTP 状态码表、瞬时错误 / SKIP 正则、`ExtractHTTPStatus`、`IsContinueWorthyError` |
| `premature.go` | `PrematureStopFacts`、`IsPrematureStop`（结构特征 + 两道闸）、提示文案表 |
| `config.go` | 偏好文件读写、路径解析、env 覆盖、JS `clampInt` |
| `state.go` | 状态机（streak/timer/generation/esc 锁/压缩 busy/早衰时间戳）+ 命令处理 |
| `extension.go` | 唯一的 SDK 接触面：`sdkHost` 适配器 + 事件/命令注册 |
| `*_test.go` | 对照测试（表驱动，注入 fake clock/scheduler/host，`t.TempDir()` 隔离） |

## TS → Go 对照

行号均指部署件/仓内源（两者一致）`auto-continue.ts`。事件名与 Pi 等价，注册在
`extension.go:68` 起。

### 纯函数与常量

| TS | 内容 | Go |
|---|---|---|
| 98-99 | `LOG` / `STATUS_KEY` | `constants.go`（`LogName` / `StatusKey`） |
| 101-102 | `DEFAULT_MAX` / `DEFAULT_DELAY_MS` | `constants.go`（`DefaultMax` / `DefaultDelayMs`） |
| 105 | `CONTINUE_PROMPT = "继续"` | `constants.go`（`ContinuePrompt`） |
| 112 | `BARE_ESC = "\u001b"` | `constants.go`（`BareEsc`） |
| 119 | `COMPACTION_BUSY_MAX_MS` | `constants.go`（`CompactionBusyMaxMs`） |
| 127 | `TOOL_USE_WINDOW_MS` | `constants.go`（`ToolUseWindowMs`） |
| 133 | `USER_PROGRESS_STALL_MS` | `constants.go`（`UserProgressStallMs`） |
| 143 | `RUN_ACTIVE_MIN_MS` | `constants.go`（`RunActiveMinMs`） |
| 146-171 | `CONTINUE_HTTP_STATUSES` | `errors.go:12` `ContinueHTTPStatuses` |
| 172-244 | `CONTINUE_ERROR_PATTERN` | `errors.go:53` `ContinueErrorPattern` |
| 246-283 | `SKIP_ERROR_PATTERN` | `errors.go:126` `SkipErrorPattern` |
| 285-330 | `AssistantLike` / `ContentBlockLike` / `PrematureStopLike` / `MessageLike` / `PrematureStopFacts` / `Verdict` | `map[string]any` + `Verdict`（`errors.go:182`）+ `PrematureStopFacts`（`premature.go:14`） |
| 333 | `asBlocks` | `messages.go:63` |
| 344 | `blockText` | `messages.go:73` |
| 352 | `hasThinkingBlock` | `messages.go:94` |
| 356 | `hasToolCallBlock` | `messages.go:108` |
| 361 | `userMessageText` | `messages.go:122` |
| 374-390 | `looksTruncatedText` | `messages.go:132` `LooksTruncatedText` |
| 392-408 | `lastToolUseTsFromMessages` | `messages.go:216` |
| 410-421 | `lastUserTsFromMessages` | `messages.go:238` |
| 423-426 | `positiveTs` | `messages.go:202` |
| 446-497 | `isPrematureStop` | `premature.go:48` `IsPrematureStop` |
| 501-505 | `PREMATURE_HINTS` | `premature.go:116` `PrematureHints` |
| 512-519 | `extractHttpStatus` | `errors.go:169` `ExtractHTTPStatus`（正则 `errors.go:163`） |
| 521-556 | `isContinueWorthyError` | `errors.go:214` `IsContinueWorthyError` |
| 558-575 | `PersistShape` / `defaultPersist` | `config.go:13` `Persist` / `config.go:20` `DefaultPersist` |
| 564-566 | `settingsPath` | `config.go:35` `SettingsPath`（见差异 D1） |
| 568-571 | `clampInt` | `config.go:47` |
| 577-591 | `loadPersist`（含解析失败回写默认值） | `config.go:65` `ParsePersist` + `config.go:101` `loadPersist` |
| 593-597 | `savePersist`（`JSON.stringify(x,null,2)+"\n"`） | `config.go:118`（`json.MarshalIndent` + `"\n"`） |
| 599-606 | `envEnabledOverride` | `config.go:131` |
| 607-613 | `envInt` | `config.go:153` |
| 615-621 | `lastAssistant` | `state.go:200` |
| 623-630 | `editorHasDraft` | `state.go:188` |

### 状态机与钩子

| TS | 内容 | Go |
|---|---|---|
| 632-637 | 模块初始化（读偏好 + env 覆盖） | `state.go:142` `NewState` |
| 639-641 | `persistNow` | `state.go:813` |
| 643-687 | 闭包状态变量 | `state.go:108` `State`（`sync.Mutex` 保护） |
| 689-695 | `cancelPending`（generation++ / 清 timer） | `state.go:211`/`217` |
| 697-703 | `clearStatus` | `state.go:179` |
| 712-718 | `judgePrematureStop` | `state.go:228` `judgeLocked` |
| 728-748 | `markUserAbort` | `state.go:244` |
| 751-756 | `clearCompactWatchdog` | `state.go:266` |
| 763-782 | `markCompactionStart` + 看门狗回调 | `state.go:276` + `291` |
| 784-787 | `markCompactionEnd` | `state.go:307` |
| 804-839 | `handleTerminalInput`（裸 ESC，R1/R2） | `state.go:322` `HandleTerminalInput` |
| 841-857 | `registerTerminalInput` | `state.go:367` |
| 859-868 | `unregisterTerminalInput` | `state.go:392` |
| 871-887 | `pi.on("session_start")` | `state.go:406` |
| 889-900 | `pi.on("session_shutdown")` | `state.go:429` |
| 902-904 | `pi.on("session_before_compact")` | `state.go:444` |
| 906-908 | `pi.on("session_compact")` | `state.go:449` |
| 910-923 | `pi.on("agent_start")` | `state.go:454` |
| 925-929 | `pi.on("after_provider_response")` | `state.go:470` |
| 933-939 | `tool_execution_start` / `tool_execution_end` | `state.go:481` / `488` |
| 941-943 | `pi.on("agent_end")` | `state.go:495` |
| 951-969 | `pi.on("input")`（extension vs interactive/rpc） | `state.go:507` |
| 971-977 | `pi.on("before_agent_start")` | `state.go:529` |
| 979-996 | `pi.on("turn_end")` | `state.go:541` |
| 998-1113 | `pi.on("agent_settled")` 主体 | `state.go:565` |
| 1078-1113 | `setTimeout` 回调（发「继续」） | `state.go:717` `fireContinue` |
| 1115-1155 | `pi.registerCommand("auto-continue")` | `state.go:823` `HandleCommand` |

## 与 TS 差异

**D1 偏好文件路径尊重 `PIG_HOME`（唯一的路径语义改动，为隔离红线）。**
TS 固定 `join(homedir(), ".pi", "agent", "auto-continue.json")`。Go 版在 `PIG_HOME`
存在时解析到 `$PIG_HOME/agent/auto-continue.json`，否则与 TS 完全相同。理由：Go
扩展跑在 PiG 里，本仓 `./bin/pig-test` 写死 `PIG_HOME=/root/.pig-test`；若仍解析到
生产路径，隔离冒烟就会读写 `/root/.pi/agent/auto-continue.json`，与迁移硬规矩
「不改 `/root/.pi/agent`」冲突。实测：冒烟后 `/root/.pi/agent/auto-continue.json`
mtime 保持 `2026-09-23 22:20`，而 `/root/.pig-test/agent/auto-continue.json` 由工厂创建。

**D2 并发模型与派发的线性化点。** TS 单线程闭包；Go SDK 的 handler 并发执行（`docs/extension-api-parity.md`
「The Go, Rust and Python SDKs run handlers concurrently」），因此 `State` 全部字段由
`sync.Mutex` 保护，host 调用一律在释放锁之后发出（避免阻塞或被宿主 handler 重入而死锁）。

计时器取消与发送的次序不靠「谁先跑到」，而由一个显式状态 `continuePhase` 钉住
（`continueIdle` / `continueArmed` / `continueChecking` / `continueCommitted`，见 D10）：

- `agent_settled` 在 `generation++` 的同一临界区置 `continueArmed`，之后才装 timer；
- `fireContinue` 入口在锁内校验 token，只有本代才 `timer = nil` 并转入 `continueChecking`，
  然后在锁外做 `IsIdle` / `HasPendingMessages` / `GetEditorText` 三次预检；
- 预检通过后再取锁做**派发线性化点**：token 仍是本代才置 `continueCommitted`、写
  `streak`/`selfSending`，出锁后才调 `SendUserMessage`；
- 裸 ESC 在**同一临界区**观察 `continueArmed`/`continueChecking` 并作废代号，因此「取消」
  与「发送」只能有一个赢；看到 `continueCommitted` 时不声称取消（改走运行中的中止路径）。

**D3 JS 语义等价实现。** Go `regexp` 无 `(?:...)`，TS 的非捕获组写成捕获组（只用
`MatchString`，捕获组无副作用）；JS 的 `\s` 与 `.` 用显式字符类展开（`jsSpaceClass`
/ `jsDotClass`），因为 Go 的 `\s` 不含 `\v`、NBSP、`\u2000-200a`、BOM 等，`.` 也不排除
`\r`/`\u2028`/`\u2029`。`jsNumber` 模拟 `Number()`（bool、null、字符串、单元素数组、
`0x/0b/0o`，对象 → NaN；Go 的十六进制浮点 `0x1p-2` 在 JS 是 NaN，这里同样拒绝）。

**D4 print / JSON 的末轮续跑不保证，且两端机制不同。** 上游 `docs/extension-api-parity.md:87`
（“Print and JSON prompt-end contract”）原文：Pi's print mode awaits each prompt and goes
on in the same continuation, to the next prompt or to `disposeRuntime()`, which emits
`session_shutdown` and then invalidates the runner；又：“A timer, immediate or I/O callback
an extension queues in the prompt's last call (its command, a handled `input` or
`agent_settled`) or in `session_shutdown` therefore runs only once the next prompt reaches
I/O, once a later extension's handler for the same event awaits a timer, I/O or a child
process …, or after the ctx is stale.” 以及 “When the host declares one of those two modes
(`Host.SetMode`, as `pig -p` and `--mode json` do), the Node runtime … waits in place until
the host's next step reaches its process”，“A host that declares no mode gets no wait”，
残余窗口：“after `invalidate`, the host may end the process before a remaining callback
runs.”

所以 TS 在 print/JSON 下**不是**「2500ms 计时一定执行」：invalidate 之后进程可能先退出，
末轮 timer 回调也可能不再运行（旧版 README 写的「TS 在 print 模式下由 Node 事件循环保活」
是误述，已删）。Go/Rust/Python SDK 的 handler 并发执行、“have no such wait”，Go 版因此
同样**不保证** `pig -p` / `--mode json` 末轮 2500ms 后「继续」发出；这是平台差异，不做
keepalive 也不宣称保活。RPC 与 TUI 按上游 “RPC and TUI wait for input after a prompt, as
Pi does” 在 prompt 后等待输入，该路径上计时器在用户下一次输入前到期即可发出；真实续跑
达标与否另列证据（见「本次验证」），**不把** RPC 里命令/UI 通道成功当成 print/JSON 续跑
成功的证据。

**D5 host 调用失败的映射。** TS 里 `isIdle()` / `hasPendingMessages()` 抛错会让
`agent_settled` handler 抛异常（净效果：不续跑），`getEditorText()` 抛错被 try/catch
吞掉（净效果：无草稿）。Go 版按同样净效果处理：前两者返回错误即「本次不调度」并
不注入额外文案，后者返回错误即「无草稿」；`handleTerminalInput` 里 `isIdle` 抛错时
按 idle 处理，与 TS 的 try/catch 一致。

**D6 `sendUserMessage` 无返回值语义。** TS 的 `pi.sendUserMessage` 是 fire-and-forget，
失败只以扩展错误事件出现；Go SDK 返回 error，本移植 `_ =` 忽略（`state.go:789`），
保持同一语义。

**D7 计时器实现。** `setTimeout` → `time.AfterFunc`（`Scheduler` 可注入，测试用假实现
避免 sleep）。`setTimeout` 的 token/generation 语义原样保留：`cancelPending` 递增
generation，已排出但已非当前代的回调直接返回。

**D8 有意保留的 TS 细节。** `session_shutdown` **不**清 streak/lastMessages（只
cancelPending），`turn_end` 里 `stopReason` 缺失即忽略，`max` 参数按 `Math.floor`
截断，`off` 会持久化 env 覆盖后的当前值——逐条与 TS 对齐，并各有测试钉住。

**D9 文案与阈值零改动。** notify/setStatus 文本逐字节一致（含「max 需要 1–20」的
en dash、两空格分隔的 status 行、`300000ms` 看门狗提示）。阈值常量值一致。

**D10 计时器取消竞态（F1/F2）与 dispatch 线性化点。** 这两个缺陷只存在于 Go 的并发模型，
已修并有确定性交错回归（`TestStaleTimerCallbackLeavesNewTimerAlone` /
`TestEscCancelsCallbackInHostPrechecks` / `TestEscVsDispatchClaimLinearization`）：

- **F1 旧回调误清新 timer。** 旧实现在 `fireContinue` 入口先 `s.timer = nil` 再比 token。
  若 T1 已触发但尚未进临界区，此时 `cancelPending` 递增代号并装好 T2，T1 恢复后会把
  `s.timer`（此刻是 T2）清掉再按过期退出；于是 ESC 看到「无 timer + idle」直接返回，
  T2 仍到点发送。现在入口先比 token，过期回调**不碰本代任何状态**
  （timer / continuePhase / streak）。
- **F2 ESC 穿透。** 旧实现 callback 进预检前就清 timer，跨宿主调用窗口里 ESC 读不到
  「待发送」；且 ESC 的「读 hasTimer」与 `cancelPending` 分两次取锁。现在回调在预检期间
  保持 `continueChecking`（本代身份仍可取消），ESC 在**同一临界区**观察并作废代号；
  dispatch 在锁内取 claim（`continueCommitted`）之后才出锁调 `SendUserMessage`。因此
  取消先赢 → 不发；发送先提交 → ESC 不谎称取消，改走「运行中按 esc」的既有语义。
- 边界：不把 `SendUserMessage` 或任何可能阻塞/重入的 UI 调用放进 `State.mu`；没有新增
  keepalive / 发送队列。`generation` 与去重、R1/R2 分工、ESC 不 Consume、压缩 busy、
  streak 上限、正常完成/人工输入语义逐条不变（旧用例全部保留并通过）。

## SDK 缺口 / 移植时绕法

1. **`sdk.Context` 是具体类型，无法在测试里伪造。** 绕法：`state.go:24` 定义最小
   `Host` 接口，`extension.go:10` 的 `sdkHost` 是唯一适配层；状态机与全部测试都不碰 SDK。
   代价：适配层只能由 `go build` 覆盖，没有单测（它是逐方法转发）。
2. **`OnTerminalInput` 在无 UI（RPC）时返回 no-op 退订。** 与 TS 的
   `ctx.ui.onTerminalInput?.()` 静默降级等价；`registerTerminalInput` 对 nil 退订/错误
   都不保留订阅。
3. **`GetEditorText` 是必选调用且可返回 error。** TS 是可选链；Go 版把 error 当
   「无草稿」（D5），因此 RPC 模式不会误挡续跑。
4. **没有 hook 注册表的内省接口**（`Extension.handlers` 未导出）。绕法：接线测试只断言
   `Name()` 与工厂可在隔离环境构造；逐钩子行为由状态机测试覆盖，实机行为由 RPC 记录佐证。
5. **无 Node 式 prompt-end 原地等待**（见 D4）。Go SDK 的 handler 并发执行、runtime 没有
   `Host.SetMode` 那种原地等待，`pig -p` / `--mode json` 的末轮 timer 不保证执行。这不是
   可绕的缺口而是平台差异：不宣称 print/JSON 末轮续跑，续跑验证放在 RPC/TUI 等待输入的
   路径上，且把「命令/UI 通道成功」与「真实续跑」分开记录。
6. **`ctx.isIdle()` 的错误面与 Pi 的 JS 异常不同**（见 D5）。

## 测试

```sh
cd /root/workspace/pi-extensions/go/auto-continue
gofmt -l .          # 期望无输出
go vet ./...
go test -count=1 -race -v ./...
```

隔离原则（测试不读写真实 `~/.pi` / `~/.pig-test`）：

- 每个测试用 `t.TempDir()` 建偏好文件，`NewState` 永远收到显式 `SettingsPath`；
- 时钟注入 `fakeClock`（`time.UnixMilli`），调度器注入 `fakeScheduler`，环境变量注入
  `LookupEnv` map —— 没有 `sleep`，没有真实计时器；
- 只有 `TestSettingsPathIsolation` / `TestExtensionFactoryIsolated` 会走真实路径解析，
  且用 `t.Setenv("HOME"|"PIG_HOME", t.TempDir())` 把落点关进临时目录。

交错用例的同步方式：`gatedHost` 的 channel 门（`hostGate`）只挡**第一次**宿主调用，把 timer
回调停在指定预检里；回调跑在真实 goroutine，测试用 channel 与它会合后驱动 ESC，再用 channel
释放并 join。无 `sleep`、无 `t.Parallel`，断言只看行为（发送次数 / streak / timer 引用 /
通知文案），不看内部时序。

**证据边界。** 上述用例证明状态机与 timer/ESC 的次序；RPC 观测量（见下节）里的
`/auto-continue status` 片段只证明命令与 UI 通道接线。真实续跑另有一条记录：隔离 PiG +
本地假 provider 触发 premature-stop 分支后，假 provider 收到第二次请求且 user 消息为「继续」
（见「本次验证（实跑）」）。`pig -p` / `--mode json` 的末轮续跑按 D4 仍不保证。

覆盖面对应 TS 的每条分支：

| 领域 | 测试 |
|---|---|
| 配置默认值 / 非法值 / clamp / 回写 | `TestParsePersist`（14 例）、`TestLoadPersistWritesDefaultsWhenMissing`、`TestLoadPersistRewritesUnparseableFile`、`TestSavePersistRoundTrip`、`TestDefaultPersist` |
| env 覆盖（含 `0/1/on/off/yes/no`、非法值） | `TestEnvEnabledOverride`、`TestEnvInt`、`TestNewStateAppliesEnvOverrides` |
| 路径解析与隔离 | `TestSettingsPathIsolation`、`TestExtensionFactoryIsolated` |
| HTTP 状态码与错误分类 | `TestContinueHTTPStatuses`、`TestExtractHTTPStatus`、`TestIsContinueWorthyError`（21 例）、`TestContinueErrorPatternTransients`（61 例）、`TestContinueErrorPatternExcludes`、`TestSkipErrorPattern` |
| 截断文本判定 | `TestLooksTruncatedText`（截断 19 例 / 正常 10 例） |
| 早衰 stop 结构与阈值边界 | `TestIsPrematureStopShape`、`TestIsPrematureStopGateA`（5min 与 90s 的等于/越界）、`TestIsPrematureStopGateB`（90s 的等于/越界）、`TestIsPrematureStopMessageFallbacks`、`TestLastTimestampHelpers`、`TestPositiveTs` |
| 调度 / 去重 / 上限 | `TestAgentSettledSchedulesContinueOnTransientError`、`TestTimerGenerationDedup`、`TestMaxAttemptsCap`、`TestAgentSettledBlockers`（非 idle/排队/草稿/editor 不可读）、`TestAgentSettledSkipsWhenDisabled` |
| esc 锁 R1/R2/R4 | `TestAbortedTurnLatchesUserCancelled`、`TestTurnEndAbortedLatchesImmediately`、`TestNormalStopReleasesEscLock`、`TestPrematureStopDoesNotReleaseEscLock`、`TestTerminalInputDuringTimerCancelsRoundOnly`、`TestTerminalInputDuringRunLatchesOnce`、`TestTerminalInputIgnoresSequences`、`TestTerminalInputNeedsSessionStart`、`TestTerminalInputTreatsIdleErrorAsIdle` |
| timer / ESC 交错（F1/F2，D10） | `TestStaleTimerCallbackLeavesNewTimerAlone`、`TestEscCancelsCallbackInHostPrechecks`（3 形态：timer 已装 / 回调停在 `IsIdle` / 停在 `GetEditorText`）、`TestEscVsDispatchClaimLinearization`（取消先赢 / 发送先提交） |
| 压缩 busy R3 + 看门狗 | `TestCompactionBlocksAndWatchdogUnlocks`、`TestCompactionEndStopsWatchdog`、`TestCompactionStartCancelsTimerAndIsIdempotent`、`TestAgentStartClearsCompaction` |
| 会话生命周期 | `TestSessionStartResetsAndRegisters`、`TestSessionShutdownKeepsStreak`、`TestSessionStartWithoutTerminalCapability` |
| input/before_agent_start 来源区分 | `TestInputSourceDistinguishesExtensionFromHuman`、`TestBeforeAgentStartHumanReleasesLock`、`TestBeforeAgentStartWithoutPrompt` |
| provider status / 工具活动 / turn_end | `TestAfterProviderResponseFeedsStatus`、`TestAfterProviderResponseIgnoresBadStatuses`、`TestToolExecutionTimestamps`、`TestTurnEndStreakHandling`（4 例） |
| 命令各分支 | `TestCommandOff`（4 种写法）、`TestCommandOn`、`TestCommandMax`（8 例）、`TestCommandStatus`（3 例）、`TestCommandMaxPersistsAcrossRestart`、`TestCommandOffPersistsAcrossRestart` |
| 早衰续跑全链路 | `TestPrematureStopContinues`（settled → timer → 发「继续」） |
| 接线 | `TestExtensionIdentity`、`TestMessageMaps`、`TestTerminalInputVerdictIsNeverConsumed` |

## 隔离加载（PiG）

```sh
cd /root/workspace/pi-extensions/go
./bin/pig-test -e ./auto-continue -p "回复 OK"
```

`bin/pig-test` 写死 `PIG_HOME=/root/.pig-test`，因此本扩展的偏好文件落在
`/root/.pig-test/agent/auto-continue.json`（D1），生产 `/root/.pi` 不被读写。
`-e` 让 PiG 现场按扩展规范编译（`GOWORK=off` + staged SDK），无需手工构建产物。

命令在 RPC 模式可直接观察（`--mode rpc`，stdin 一行 JSON）：

```sh
(printf '%s\n' '{"id":"1","type":"prompt","message":"/auto-continue status"}'; sleep 4) \
  | ./bin/pig-test -e ./auto-continue --mode rpc --no-session
# → extension_ui_request method=setStatus statusKey=auto-continue   (session_start)
# → extension_ui_request method=notify "[auto-continue] on  streak=0/20  delay=2500ms"
# → response command=prompt disposition=handled
# → extension_ui_request method=setStatus statusKey=auto-continue   (session_shutdown)
```

这段输出只证明命令接线与 UI 通道（setStatus / notify / response），**不是**续跑证据，更不是
print/JSON 末轮续跑的证据（D4）。

## 本次验证（实跑）

| 命令 | 结果 |
|---|---|
| `gofmt -l .` | 无输出，EXIT=0 |
| `go build ./...` | EXIT=0 |
| `go vet ./...` | EXIT=0 |
| `go test -count=1 -race -v ./...` | 65 顶层 + 114 子 = 179 PASS（`^--- PASS` 65 / `^    --- PASS` 114），FAIL=0，`DATA RACE` 命中 0，`ok … 1.085s` |
| `go test -count=20 -race -run '<三个交错用例>' ./...` | EXIT=0，FAIL=0，`DATA RACE` 0，`ok … 1.217s`（预注册稳定性，不是失败重试） |
| 同一测试文件先 FAIL 后 PASS | 旧实现副本（`state.go` sha256=`ad54685e…`）EXIT=1：T1 「the stale callback replaced the current timer (state.timer = <nil>)」、T2 两形态「the cancelled continue sent 1 message(s)」、T3 「cancel wins」「esc missed the pending continue」；同一文件在修后实现上 EXIT=0 全 PASS |
| 真实入口续跑演练（隔离 PiG + 本地假 provider） | EXIT=0；假 provider 收到**第二次**请求且 `user_messages=["请按计划推进任务。","继续"]`，RPC 侧 notify `stop:premature-truncated-text…200ms 后自动继续（1/3）`（原文见下） |

**真实入口续跑演练（隔离）。** 命令与退出码：

```sh
cd /root/workspace/pi-extensions/go
export PI_AUTO_CONTINUE=1 PI_AUTO_CONTINUE_DELAY_MS=200 PI_AUTO_CONTINUE_MAX=3
(printf '%s\n' '{"id":"1","type":"prompt","message":"请按计划推进任务。"}'; sleep 150) \
  | ./bin/pig-test -e ./auto-continue --provider fakeac --model fake-1 --mode rpc --no-session -nt
# EXIT=0
```

假 provider 是 `/tmp` 里的本地回环 HTTP 服务（OpenAI 兼容 + SSE），`~/.pig-test/agent/models.json`
仅在演练期间临时加一条指向 `127.0.0.1` 的 `fakeac` provider，演练后按原 md5
（`7ef998265f4515f58cbe201c117f7b36`）还原；未发起任何真实模型请求，未读写 `/root/.pi`。
首轮回复是中途断句正文 `好的，先看第一段。接下来要处理的是，`（`stopReason=stop`、无 tool call、
无 thinking、`durationMs=105002` > 90s，两道闸都过），扩展 notify 后 200ms 发出「继续」。
假 provider 请求日志（原文，`/tmp/ac-drill/requests.log`，只去掉 body_head 字段）：

```
{"seq": 1, "method": "POST", "path": "/v1/chat/completions", "model": "fake-1", "stream": true, "n_messages": 2, "user_messages": ["请按计划推进任务。"], "continue_detected": false}
{"seq": 2, "method": "POST", "path": "/v1/chat/completions", "model": "fake-1", "stream": true, "n_messages": 4, "user_messages": ["请按计划推进任务。", "继续"], "continue_detected": true}
```

RPC 输出对应片段（原文，`id` 截短）：

```
{"type":"extension_ui_request","id":"d40e5a56-…","method":"setStatus","statusKey":"auto-continue","statusText":"auto-continue 1/3"}
{"type":"extension_ui_request","id":"e9ac09ee-…","method":"notify","message":"[auto-continue] stop:premature-truncated-text（疑似早衰 stop：正文中途断句），200ms 后自动继续（1/3）","notifyType":"info"}
{"type":"agent_settled","aborted":false}
{"type":"agent_start"}
{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":"继续"}],"timestamp":1792825028107}}
```

边界：这是 RPC 模式（prompt 后等待输入）下的续跑证据，触发的是 premature-stop 分支；print/JSON
末轮续跑仍按 D4 **不保证**，未做 keepalive；生产 pi 未重启，**不**宣称「已在线生效」。

`make build`（go/ 根全组件）与 `./bin/pig-test -e ./auto-continue -p "回复 OK"` 冒烟是
上一批的记录；本次只改状态机、测试与本文，未复跑那两项，也不把旧结果当本次证据。

未达标 / 未验证（不伪造闭环）：

- 上一轮 RPC 假 provider 的首轮 502 被 PiG 内建 `auto_retry` 吃掉：两次请求都只有 fixture run、
  没有「继续」，那次演练未达标；本轮改用 200 + 中途断句（premature-stop）路径已达标（见上）。
  旧的 502 记录既不作为 F1/F2 的证据，也不用来反证 auto-continue 失效。
- 单测仍只用临时 `SettingsPath` / 假 clock / 假 Host；假 provider 脚本只放在 `/tmp`，未入库、
  不算测试文件；全程没有发起任何真实模型请求。
- print / JSON 末轮续跑按 D4 **不保证**，未做 keepalive，也未宣称保活。

`make build` 覆盖的组件段：root、auto-continue、ctx-orchestrate、exclude-tools-guard、
herdr-agent-state、no-tables、pi-context-pruning、pi-env、pi-tinyfish。

## 备注

- `go/go.work` 只由本组件追加了 `./auto-continue` 一行。提交前的 `git diff -- go/go.work`
  里另有一行 `./ctx-orchestrate`，来自**并行推进的兄弟组件**（其目录与 `go.mod` 早于本组件
  创建，非本组件改动），本组件不得删除。
- TS 原版没有测试文件；这里的对照测试是按 TS 行为逐分支新写的（TS 无既有用例可翻）。
