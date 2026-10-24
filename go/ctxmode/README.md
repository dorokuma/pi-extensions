# ctxmode — PiG Go stdio bridge

将原版 Pi 的 ctxmode 桥移植为 Go factory 扩展。桥启动现有 `ctxmode` 二进制，使用
JSON-RPC 2.0 / MCP over stdio，注册 `ctx_run`、`ctx_fs`、`ctx_git`、`ctx_kb`、
`ctx_bg`、`ctx_stats` 六个原生工具；**本目录不重写 ctxmode 服务端**。

## 来源与加载约定

- 源码：`/root/.pi/agent/extensions/ctxmode.ts`（960 行）。已用 `cmp` 验证其与
  `/root/workspace/ctxmode/integrations/pi/ctxmode.ts` 逐字节一致，两个位置均只读。
- 源码 SHA-256：`51fad3634b5ec5e7443a805f35f6e0d51340b8bea7606bbfde2724baf9c587eb`。
- 核心测试：`/root/workspace/ctxmode/integrations/pi/ctxmode.test.ts`，SHA-256
  `6b77295942a9a6e525907ae65287c5a3608c60f0315924dd1f03b4d33aa93d7e`。
- ANSI 测试：`/root/workspace/ctxmode/integrations/pi/stripansi.test.ts`，SHA-256
  `b245b703b45fd98a1685134620747761583b61f2510edc74412920bb62c6906d`。
- 行为说明：`/root/workspace/ctxmode/integrations/pi/README.md`。
- module：`github.com/dorokuma/pi-extensions/go/ctxmode`，`go 1.26`，仅标准库与
  `github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0`。
- 所有 Go 文件使用 `package ctxmode`，入口为 `func Extension() *sdk.Extension`。
  **没有 `main()` 或 `init()`**。与 `go/moshi-hooks` 的工厂约定一致，PiG 为其生成 runner。
- 本地 build/vet/test 通过 `go/go.work`（含 `./ctxmode`）使用 `go/pigsdk`；PiG 加载时使用 staged SDK。
  未修改生产扩展、宿主 ctxmode 仓库或启动器。

## 文件与实现对应

| TS 内容 | Go 实现（`文件:行` 为定义行，改动后按 `grep -n '^func '` 重算） |
|---|---|
| 环境配置、`stripANSI`、`clampUtf16Index`、`compressToolText` | `text.go:14` `envString` / `text.go:26` `envNumber` / `text.go:42` `envPositiveNumber` / `text.go:60` `StripANSI` / `text.go:67` `clampUtf16Index` / `text.go:84` `CompressToolText` |
| `READONLY_ROLES` / `resolveRoleEnv` | `text.go:114` `ResolveRoleEnv` / `text.go:144` `roleEnviron` |
| `diagLog`、降级提示、目录恢复、`.1/.2` 轮转、INFO/DEBUG 过滤 | `diag.go:28` `DiagLog` / `diag.go:77` `isInterestingStderrLine` |
| `CtxmodeClient`、握手与 MCP 数据结构 | `client.go:71` `CtxmodeClient` / `client.go:100` `Start` / `client.go:125` `doStart` |
| stdout 响应匹配、最近 20 行 stderr、跨 chunk carry、异常退出 dump | `client.go:224` `readOutput` / `client.go:240` `acceptResponseLocked` / `client.go:260` `readStderr` / `client.go:300` `dumpStderrBufferLocked` |
| `disposeProc` / `shutDownProc` / `cleanup` / `stop` | `client.go:340` `disposeProc` / `client.go:371` `shutDownProc` / `client.go:332` `cleanupLocked` / `client.go:390` `Stop`；`Stop` 为终态，旧进程以身份校验隔离 |
| 请求预算、超时取消、Esc 取消、disconnect 重连但不重放 | `client.go:578` `timeoutForTool` / `client.go:498` `sendRequest` / `client.go:562` `writeRequest` / `client.go:464`/`479` `cancelRequestLocked`/`cancelPendingLocked` / `client.go:610` `CallTool` / `client.go:672` `reconnect` |
| 三个事件、两个 slash command、延迟一次注册工具 | `extension.go:120` `Extension` / `extension.go:35` `extensionState` / `extension.go:56` `start` |
| 六个工具的全部元数据与 TypeBox schema | `tools.json`（从 TS 定义提取后嵌入）/ `extension.go:27` `definitions` / `extension.go:107` `registerTools` |
| 文本结果与取消 error 的区别 | `extension.go:87` `run` / `extension.go:84` `textResult` |

### 生命周期与取消不变量

- 启动参数保持 `ctxmode -workdir <absolute workdir>`，注入 `ResolveRoleEnv`；
  `extensionState.start` 传 `filepath.Abs(workdir)`（`extension.go:62`）而 notify 文案
  用原始 `workdir` 字符串（`extension.go:132`），两条命令与 session_start 共用该闭包。
- 握手保持协议版本 `2024-11-05`、客户端 `pi-ctxmode` / `1.0.0`；记录服务端
  instructions，依次发送 initialized 通知与 tools/list。initialize/tools/list 失败
  **或超时**都在 `Start()` 返回错误前同步回收子进程（SIGTERM → `CTXMODE_DISPOSE_WAIT_MS`
  → SIGKILL → 等 `cmd.Wait` 收尸），不留孤儿或 zombie。
- `Stop()` 设置终态后，所有 `Start()` 拒绝；异常退出保留最多五次指数退避重连。
- 请求行先于取消通知写出。Esc 立即取消本地等待；超时先通知服务端再返回错误。
  已响应请求不补发取消；超时/取消不重连、不重放 tools/call。
- 写入与状态锁解耦：请求行在独立 goroutine 中写出（`writeRequest`），被写满的 stdin
  管道阻塞时 `ctx.Done()` 仍立即生效，调用方马上拿到取消错误，pending 条目照常清掉；
  取消通知等请求行落地后才发（写未完成则延后到写完，顺序仍是请求行在前）。
- 断连重连握手（`reconnect`）也在自己的 goroutine 中跑：握手期间按 Esc 立即返回
  取消错误，不阻塞两段 `CTXMODE_START_TIMEOUT_MS`。
- dispose 关闭 stdio，SIGTERM 等待窗口后才升级 SIGKILL。Go 版等待子进程被回收，
  不留下 zombie；旧 readers/waiter 不能修改替换连接的缓冲或 pending。
- before_agent_start 只追加 `## Ctxmode server instructions`，不拦截用户消息。

## 环境变量

均在使用时读取，按 TS `Number(process.env.X || DEFAULT)` 的语义解析：未设置或空串取
默认值，`"0"`（含 `" 0 "`）解析为 `0` 且**不回退**默认值，非数字或 `NaN`/`Infinity`
回退默认值。**例外**：两个超时旋钮走 `envPositiveNumber`，只在 `> 0` 时采用环境变量值，
`"0"` 或负数一律回退默认值（0ms 预算会让握手/请求在服务端应答前就失败）。

| 变量 | 默认 / 含义 |
|---|---|
| `CTXMODE_BIN` | `ctxmode` |
| `CTXMODE_WORKDIR` | 会话 `ctx.Cwd()`；客户端用 `filepath.Abs`，notify 文案用原值 |
| `CTXMODE_START_TIMEOUT_MS` | `15000`；仅 `> 0` 生效，`"0"`/负数回退默认值 |
| `CTXMODE_REQUEST_TIMEOUT_MS` | `120000`；长工具预算另加服务端缓冲，最大 `3660000`；仅 `> 0` 生效 |
| `CTXMODE_OUTPUT_CHARS` | `50000`，按 UTF-16 code unit 计算，不拆代理对 |
| `CTXMODE_OUTPUT_LINES` | `500`，按 TS 的最低 40 行头 / 15 行尾逻辑压缩 |
| `CTXMODE_DEBUG` | 非空时直接写 stderr，不写日志文件 |
| `CTXMODE_DIAG_STDERR` | 等于 `1` 时同时写 stderr 和日志文件 |
| `CTXMODE_DIAG_DIR` | `~/.pi/agent/logs`，文件名 `ctxmode.log` |
| `CTXMODE_DIAG_MAX_BYTES` | `5242880`；写前已有文件超过上限则轮转，最多两个历史文件 |
| `CTXMODE_DISPOSE_WAIT_MS` | `3000`，SIGTERM→SIGKILL 等待窗口 |
| `CTXMODE_ROLE` | 显式非空值优先；否则扫描当前进程 argv 的 `--name` |
| `CTXMODE_READONLY` | 推导到六种只读角色且未设置非空值时注入 `1` |

### Go / SDK 适配差异与局限

1. 按任务要求，超时与输出上限运行时读取；TS 中部分上限原本在模块加载时读取。
   日志目录变更也能生效，不依赖重载。
2. Node readline/EventEmitter 替换为 goroutine、互斥状态锁和进程身份校验；
   cancellation 使用 SDK `Context.Done/Err` 的 context adapter。工具返回原始
   `{content:[{type:"text",text}], details:{}}` 形状，不依赖 SDK 的字符串 Content 捷径。
   写入路径单独用一把 `writeMu` 串行化、不持状态锁，故被塞死的 stdin 不会拖住取消。
3. SDK 在 before_agent_start 中将 `systemPrompt` 转为 `*sdk.SystemPrompt`，因此用
   `Get()` 读取；不是直接断言为 string。台架提供真实的 `systemPromptOptions`。
4. `ResolveRoleEnv` 的提取与不覆盖行为按原版测试验证。但 PiG 的 factory 可能在
   独立 runner 中运行，runner 的 `os.Args` 不一定包含宿主 `--name`；生产部署若依赖角色
   自动推导，应由调用方显式传递 `CTXMODE_ROLE` / `CTXMODE_READONLY`。本任务未增加
   父进程 argv 探测或修改宿主。这是加载形态的边界，不声称 smoke 已验证宿主角色传递。
5. 支持有效 Unicode 文本，压缩通过 UTF-16 编码定位，再解码完整代理对。未单独测试
   JSON 中孤立 surrogate 的 WTF-8 保留。数值配置：`envNumber` 只对空串与
   非数字/`NaN`/`Infinity` 回退默认值（TS 对后两者得到 `NaN`/`Infinity`，Go 的 int
   装不下，故作安全回退）；`"0"` 严格解析为 `0`，与 `Number("0")` 一致。
   `envPositiveNumber`（`text.go:42`）只服务两个超时旋钮：这两处的 `"0"`/负数会让握手或
   每次调用在服务端应答前就超时，故仅 `> 0` 采用，其余回退默认值（对 TS 的有意偏离）。
   `CTXMODE_OUTPUT_CHARS` / `CTXMODE_OUTPUT_LINES` / `CTXMODE_DIAG_MAX_BYTES` /
   `CTXMODE_DISPOSE_WAIT_MS` 仍按 TS 语义允许 `"0"`。
6. smoke 使用模拟 MCP 服务端，无真实模型请求、无真实服务端命令执行；不等同于
   ctxmode 服务端各工具的独立集成验收。

## 原版测试逐条对应

**权威文件实际是 27 条核心 + 12 条 ANSI，共 39 条，不是派发计数 20 + 11。**

| 核心 TS 测试（按源顺序） | Go 测试 |
|---|---|
| 字数超限保留头尾退出码 | `TestCompressToolTextHeadTail` |
| 日志降级仅报一次并恢复 | `TestDiagDegradedRecovery` |
| 最近 20 行环形缓冲 | `TestStderrRing` |
| 跨 chunk carry 与 flush | `TestStderrCarry` |
| force dump 回退末 5 行 | `TestDumpStderrFallback` |
| exit 0 不 dump | `TestExitZero` |
| 信号退出 code=null dump | `TestExitSignal` |
| 非零退出 dump | `TestExitNonzero` |
| dispose 关闭 stdio、及时退出不升级 | `TestDisposeStreams` |
| dispose SIGKILL 升级 | `TestDisposeEscalates` |
| dispose 后旧 stderr 不污染 | `TestDisposeOldStderrIsolated` |
| 两代诊断日志轮转 | `TestDiagRotation` |
| initialize 失败回收 | `TestStartInitializeFailureReaped` |
| disconnected / not running 不重放 | `TestDisconnectDoesNotReplay` |
| fetch / run_task 自定义 timeout | `TestTimeoutForToolFetch` |
| 角色推导且显式环境优先 | `TestResolveRoleEnvOriginal` |
| Esc 取消通知与 pending 清理 | `TestEscCancellation` |
| 客户端超时取消通知 | `TestClientTimeoutCancels` |
| 正常响应无取消 | `TestNormalResponseNoCancel` |
| 调用前已取消无请求 | `TestAlreadyCancelledNoWrites` |
| 响应后取消不补通知 | `TestLateAbortNoNotify` |
| 扩展 abort rejected 而非文本 | `TestExtensionAbortRejected` |
| ctx_stats 注册与请求透传 | `TestExtensionStatsRegistered` |
| ctx_fs resolve/related enum 与透传 | `TestExtensionFSResolveRelated` |
| stop 终态拒绝 start | `TestStopIsTerminal` |
| session_shutdown 不泄漏第二个进程 | `TestExtensionShutdownNoRespawn` |
| 崩溃自动重连不受 stop 守卫削弱 | `TestCrashAutoRestart` |

ANSI 原版 12 条在 `TestStripANSIOriginal` 中逐条为：`RIS`、`DECALN`、
`CharacterSet`、`PercentAndSaveRestore`、`DCS_ST`、`DCS_BEL`、`SOS_PM_APC`、
`DCS_Unterminated`、`OSC_BEL_ST`、`CSI`、`Plain`、`Mixed`。ST 显式写为 `"\x1b\\"`。

补充十八个审查/边界用例：`TestCompressionBoundaries`、`TestTimeoutAndRoleBoundaries`、
`TestDiagOptInsAndFiltering`、`TestHandshakeListFailureAndAbort`、`TestStopDuringStart`、
`TestSDKCancellationAndCommands`、`TestExtensionStartupFailureLevels`，
以及 R2 回归：`TestStartTimeoutReapsBeforeReturn`、`TestCallToolCancelUnblocksBlockedWrite`、
`TestReconnectHandshakeHonoursCancel`、`TestDisposeWaitZeroEscalates`、
`TestEnvNumberZeroAndFallbackSemantics`、`TestDiagMaxBytesZeroRotatesEveryWrite`、
`TestWorkdirNotifyKeepsRawValue`，以及 R3 回归：
`TestCallToolNormalCompletionNoGoroutineLeak`、`TestReconnectCancelDisposesHandshakeChild`、
`TestTimeoutKnobsIgnoreNonPositive`、`TestCompressToolTextZeroCaps`。

Go 总计 **57 个叶子行为用例**（46 个顶层测试名，其中 1 个容器下有 12 个子用例；
顶层 `--- PASS` 46 + 子用例 12 = 58 条 PASS 行），不是将 ANSI 容器本身再重复计数。
测试与台架 1538 行，生产 Go 1089 行另加元数据 JSON，未超过实现改动的三倍。
模拟服务端由测试二进制自身提供，所有 PID/请求/日志在临时目录，
SDK 台架通过真实长度前缀 IPC 和 `Extension().RunWithConn` 完成握手与晚注册。

## R2 审查修复与回归（P1×2 / P2×4）

| 项 | 问题 | 修复 | 回归用例（先 FAIL 后 PASS） |
|---|---|---|---|
| P1-1 握手失败同步收尸 | 需要确认失败/超时路径在 `Start()` 返回前已回收子进程 | 无需改代码：`doStart` 的 `defer`（`client.go:164`）在返回错误前同步跑完 `shutDownProc`→`disposeProc`（SIGTERM→等待窗口→SIGKILL→等 `cmd.Wait`）。补超时路径用例钉住 | `TestStartTimeoutReapsBeforeReturn`（新，直接断言 `Start()` 返回后 pid 已 ESRCH） |
| P1-2 取消被写入阻塞 | `sendRequest` 原在状态锁内同步 `Write`（`client.go:498` 区），stdin 塞满时 `ctx.Done()` 永远看不到 | 请求行改到 `writeRequest` goroutine；`writeMu` 只包住 `Write`；取消通知等 `entry.written` 后再发（写未完成则延后），调用方立即返回取消错误 | `TestCallToolCancelUnblocksBlockedWrite`（新）：修复前 2.00s 超时失败 |
| P1-2 重连忽略取消 | 断连重连分支同步 `Start()`，Esc 后仍得等两段 `START_TIMEOUT_MS` | `reconnect`（`client.go:672`）把 `Start()` 放 goroutine，`ctx.Done()` 胜出即返回取消错误；无人接收的 start 错误仍写诊断日志 | `TestReconnectHandshakeHonoursCancel`（新）：修复前 1.996s 才返回 |
| P2-1 数值旋钮 `"0"` | 四个旋钮共用的 `envNumber` 需对齐 `Number(process.env.X \|\| DEFAULT)` | 空串回退默认值、`"0"` 解析为 0 不回退（原实现已对），并把 NaN/±Infinity 从归 0 改为回退默认值 | `TestEnvNumberZeroAndFallbackSemantics`、`TestDisposeWaitZeroEscalates`、`TestDiagMaxBytesZeroRotatesEveryWrite` |
| P2-2 空串 `CTXMODE_ROLE` | 需按 TS `if (baseEnv.CTXMODE_ROLE) return baseEnv`，空串视为未设置 | 无需改代码：`ResolveRoleEnv`（`text.go:114`）的 `!= ""` 判断已是该语义；补空串用例钉住 | `TestResolveRoleEnvOriginal` 新增空串分支 |
| P2-3 workdir 文案 | 需 `NewClient` 收绝对路径、notify 用原始字符串 | 无需改代码：`extension.go:62` 做 `filepath.Abs`，`extension.go:132` notify 用原始 `workdir`；补用例钉住两条路径 | `TestWorkdirNotifyKeepsRawValue` |

P1-2 的“取消优先”是对 TS 的**有意偏离**：TS 在重连分支会先 `await this.start()` 再抛原
错误，此时 Esc 只是不再等待（本地已 reject），而 Go 版在 `ctx.Done()` 胜出时直接返回取消
错误，避免宿主把取消当普通工具结果写进对话。其余取消/重放语义与 TS 保持一致（超时/取消
不重连、不重放；已响应不补取消）。

## R3 审查修复与回归（P1×2 / P2×2）

| 项 | 问题 | 修复 | 回归用例（先 FAIL 后 PASS） |
|---|---|---|---|
| P1-1 取消监听 goroutine 退出 | `sendRequest` 里监听 `ctx.Done()` 的 watcher 在调用自行结束（拿到响应或超时）时无人唤醒，会永久挂起 | watcher 同时等 `ctx.Done()` 与 `reqDone`：`reqDone`（`client.go:518`）由 `defer close(reqDone)`（`client.go:519`）在每条返回路径关闭，`ctxDone`（`client.go:517`）只由 watcher 关闭 | `TestCallToolNormalCompletionNoGoroutineLeak`（新）：去掉 `reqDone` 分支后 25 次正常完成调用留下 25 个 goroutine，6.13s 超时 FAIL；加回后 0.14s PASS |
| P1-2 重连取消留在途握手 | `reconnect` 在 Esc 胜出时只返回错误，正在握手的子进程要等 `CTXMODE_START_TIMEOUT_MS`（15s 级）才被回收 | `ctx.Done()` 分支先 `c.shutDownProc()`（`client.go:684`），握手 goroutine 回话失败再补一次（`client.go:688`）；`shutDownProc` 标 `intentional`、**不**置 `c.stopped`（那是 `/ctxmode-stop` 的语义），重连不再遗留僵尸握手进程 | `TestReconnectCancelDisposesHandshakeChild`（新）：取消后 1.5s 子进程仍在 → FAIL（1.52s）；修复后 0.02s 内 ESRCH → PASS |
| P2-1 输出上限 `0`/负数 | `CTXMODE_OUTPUT_CHARS=0` / `CTXMODE_OUTPUT_LINES=0` 必须按 TS 截断而不是跳过，负数不得越界切片 | `CompressToolText`（`text.go:84`）本就没有 `charCap <= 0` 跳过，与 TS 一致；把 head/tail 偏移显式夹到 `>= 0`（`text.go:107`/`108`），越界防护不再只依赖代理对钳位；marker 保留 TS 的原始 cap 值 | `TestCompressToolTextZeroCaps`（新）：char cap 0 只剩 marker、负 cap 不 panic 且 marker 照 TS 写原值、line cap 0 = 40 头 + 15 尾 |
| P2-2 超时旋钮 `0`/负数 | `CTXMODE_START_TIMEOUT_MS="0"` 让 `Start()` 立刻以 `initialize timed out after 0ms` 失败，`CTXMODE_REQUEST_TIMEOUT_MS="0"` 同理使每次调用立刻超时 | 新增 `envPositiveNumber`（`text.go:42`）：两个超时旋钮只在 `> 0` 时采用环境变量值，否则回退默认值；输出上限与 `DIAG_MAX_BYTES`/`DISPOSE_WAIT_MS` 仍允许 `"0"` | `TestTimeoutKnobsIgnoreNonPositive`（新）：改回 `envNumber` 即 `initialize timed out after 0ms` FAIL；修复后 PASS，且 `CTXMODE_REQUEST_TIMEOUT_MS="0"` 仍回退 `120000` |

R3 的两处**有意偏离**：

- 两个超时旋钮不再严格照 `Number(process.env.X || DEFAULT)`（TS 下 `"0"` 得 0 → 立即超时）；
  这是任务要求的健壮性修正，已在“环境变量”与“Go / SDK 适配差异”两节标注。
- 负数 `CTXMODE_OUTPUT_CHARS` 只夹偏移、不夹 cap 本身，因此 marker 仍打印 TS 的原始值
  （`-3`），与 TS 逐字节一致；Go 的切片安全改由显式 `>= 0` 钳位保证。

R3 对照源码的一处**偏差更正**（据实记录）：派发说明假定 `sendRequest` 已有一个“监听
`ctx.Done()` 的 goroutine”并要求给它加 `reqDone`。核对源码后，改动前的 `sendRequest` 是
直接在 `select` 里等 `<-ctx.Done()` 的（无 watcher goroutine，也就没有那个泄漏点）；
`CompressToolText` 也不含 `charCap <= 0` 跳过。按任务要求把“watcher + `reqDone`”显式
实现出来并补了 no-leak 用例（去掉 `reqDone` 即复现泄漏）；`charCap <= 0` 因本就不存在
故无需删除，`CTXMODE_OUTPUT_CHARS=0`/`LINES=0` 的行为本就与 TS 一致，补用例钉住。

## 验证证据

环境：`go version go1.26.4 linux/amd64`。

原版只读基线命令（cwd `/root/workspace/ctxmode`）：

```text
node --experimental-strip-types --test integrations/pi/ctxmode.test.ts integrations/pi/stripansi.test.ts
39 tests / 39 pass / 0 fail / 0 skipped / duration_ms 3000.684625
```

最终 Go 验证（cwd 本目录）：

```text
gofmt -w .                 # 原地格式化，不将 stdout 写回文件
gofmt -l .                 # 输出为空
go build ./...             # exit 0
go vet ./...               # exit 0
go test -count=1 -race -v ./...
PASS
ok github.com/dorokuma/pi-extensions/go/ctxmode 4.949s
```

全部 57 个叶子用例 PASS，最终完整 race 输出 **0 DATA RACE**。

### R3 回归证据（先 FAIL 后 PASS）

```text
# P1-1：临时把取消 watcher 改成只等 ctx.Done()（去掉 reqDone 分支）
go test -count=1 -run TestCallToolNormalCompletionNoGoroutineLeak -v ./...
--- FAIL: TestCallToolNormalCompletionNoGoroutineLeak (6.13s)   # condition not met before deadline
# 恢复 reqDone 后
--- PASS: TestCallToolNormalCompletionNoGoroutineLeak (0.14s)

# P1-2：临时去掉 reconnect 取消分支的 shutDownProc
go test -count=1 -run TestReconnectCancelDisposesHandshakeChild -v ./...
--- FAIL: TestReconnectCancelDisposesHandshakeChild (1.52s)     # in-flight handshake child survived the cancel
# 恢复后
--- PASS: TestReconnectCancelDisposesHandshakeChild (0.02s)

# P2-2：临时把两个超时旋钮改回 envNumber
go test -count=1 -run TestTimeoutKnobsIgnoreNonPositive -v ./...
--- FAIL: TestTimeoutKnobsIgnoreNonPositive (0.00s)              # initialize timed out after 0ms
# 恢复后
--- PASS: TestTimeoutKnobsIgnoreNonPositive (0.00s)

# P2-1：钉住用例（改动前该行为已与 TS 一致，无 FAIL 可复现）
--- PASS: TestCompressToolTextZeroCaps (0.00s)
```

隔离加载（cwd `go/`）：`./bin/pig-test -e ./ctxmode --help` 正常退出，但 help 本身
不足以证明生命周期。另实际执行无模型 RPC smoke：先用 `go test -c -race -o <tmp>/ctxmode-test`
构建测试服务器，再通过下列命令启动，`CTXMODE_BIN=<tmp>/ctxmode-test`、
`CTXMODE_TEST_HELPER=1`、接收/PID/诊断路径均设置在同一临时目录，dispose 等待 50ms。

```text
./bin/pig-test --offline --no-extensions --no-mcp --no-skills --no-prompt-templates \
  --no-themes --no-context-files --no-session -e ./ctxmode --mode rpc
stdin: {"id":"ctxmode-load","type":"get_commands"}
```

退出 0。关键输出：

```text
notify: ctxmode 已启动 (workdir: /root/workspace/pi-extensions/go), info
response: id=ctxmode-load, command=get_commands, success=true
commands: ctxmode-start (启动 ctxmode), ctxmode-stop (停止 ctxmode)
```

模拟服务端记录了 `initialize` → `notifications/initialized` → `tools/list`，诊断记录
`started ... tools: ctx_run, out≤50000c/500L`。退出后检查 smoke 子进程已被回收（ESRCH）。
没有模型请求，不读取/打印凭据，不改生产 Pi 配置。

### 前序失败与修正记录（不隐去失败）

- 首次格式化误将工具压缩后的 stdout 写回 `client.go`，导致 `... (省略 80 行...)` 入源码。
  build 退出 1 后停手；获授权从会话原始完整写入恢复，改用 `gofmt -w .`，后续 build 通过。
- 首次 race 测试两处台架失败：握手同步条件把 `notifications/initialized` 当第二个 initialize；
  before_agent_start 缺少 SDK 必需的 `systemPromptOptions`，未检查错误便断言结果引发 panic。
  获授权后改为精确 method 匹配、补齐事件输入并先检查错误；两项之后完整 PASS。
- 一次 RPC smoke 在启动前被工具 env 白名单拒绝 `GORACE`。获继续授权后不再注入该键，
  只使用允许的 `CTXMODE_*`，实际 RPC 加载通过。

未提交、未部署。以上为 worker 验证，仍需 reviewer / verifier 独立核验。
