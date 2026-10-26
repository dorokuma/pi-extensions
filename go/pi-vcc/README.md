# pi-vcc — PiG Go 移植

将 `@sting8k/pi-vcc@0.11.1`（pi 的算法式对话压缩扩展）及其在 eqi12 上随附的两个本地
补丁移植为 Go factory 扩展。压缩由 `session_before_compact` 产出的结构化摘要驱动，
`vcc_recall` 工具/命令提供压缩前后内容的召回。**本目录只重写 pi-vcc 自身逻辑的 Go 版本，
不改其对外契约。**

## 来源与加载约定

- 原版：npm `@sting8k/pi-vcc@0.11.1`，`src/` 37 个 `.ts` 加根 `index.ts`，逐一有 Go 对应物
  （完整对照清单见 `go/reports/batch-4-pi-vcc.md` §3）。0.11.1 相对 0.10.2 的上游变更已同步，
  摘要小节构成随之改变，见下文「摘要小节」；本轮同步的逐项落地对照（表 A–N）与实测证据见
  `go/reports/batch-4b-sync-0.11.1.md`。
- module：`github.com/dorokuma/pi-extensions/go/pi-vcc`，`go 1.26`，仅依赖标准库与
  `github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0`。
- 所有 Go 文件使用 `package pivcc`，入口为 `func Extension() *sdk.Extension`
  （`extension.go:116`）；**没有 `main()` 或 `init()`**。按 batch-2 确认的 factory 规则
  （非 `main` 包名 + 导出 `Extension()`），PiG 为其生成 runner。
- 本地 build/vet/test 经 `go/go.work`（含 `./pi-vcc`）解析到本地 `go/pigsdk`；PiG 加载时
  使用 staged SDK。未修改生产扩展目录或 TS 部署副本。

## 注册面（对外契约）

`Extension()` 复刻 `index.ts` 的五步：`scaffoldSettings()` → 注册压缩钩子 → 两个斜杠命令
→ recall 工具。命令名 / 工具名 / `details.compactor` / `details.version` 与 TS 完全一致。

| 类别 | 名称 | 注册点 |
|---|---|---|
| 工具 | `vcc_recall` | `extension.go:331` `RegisterRecallTool` |
| 斜杠命令 | `/pi-vcc` | `extension.go:220` `RegisterPiVccCommand` |
| 斜杠命令 | `/pi-vcc-recall` | `extension.go:313` `RegisterVccRecallCommand` |
| 事件 | `context` | `hook.go:26` |
| 事件 | `before_agent_start` | `hook.go:53` |
| 事件 | `session_before_compact` | `hook.go:58` |
| 事件 | `session_compact` | `hook.go:64` |

- 落库 compaction entry 的 `details.compactor` 恒为 `"pi-vcc"`、`details.version` 恒为 `2`
  （`extension.go` 顶部常量；`session_compact` 处理器据此判断是否弹 stats toast）。
- `context` 事件把 auto-continue 的不可见续跑标记（按 `customType` 匹配）从 LLM 载荷里剔除；
  `before_agent_start` 清 pending auto-continue 定时器。

## 摘要小节（0.11.1）

`formatSummary()` 按此顺序渲染，缺项整段省略：

| 小节 | 来源 |
|---|---|
| `[Early Session Requests]` | 本会话前 3 条用户消息（`extract/user-messages.ts` `EARLY_REQUESTS_KEPT`），压缩间固定不变 |
| `[Files And Changes]` | 按 Modified / Created / Read 分类，每类最新 10 个，超出记 `(+N earlier)` |
| `[Commits]` | 每个 commit 一行，最多 8 行，一条 commit 只出现一次 |
| `[Tracked Commands]` | 分类名来自 `trackCommands` 配置 |
| `[User Messages]` | 最新 15 条用户消息，最早在前 |

0.11.1 用「用户原话」取代了 0.10.2 的 `[Session Goal]` / `[User Preferences]` /
`[Outstanding Context]` 三段推断式小节：不再猜目标、偏好和阻塞，只记录用户写了什么
（`userMessages.go`）。`injected` 标记的消息（扩展 `custom_message`）只进 brief，不进上述两节
（`beforecompact.go`、`normalize.go`）。

## 两个原生合入的本地补丁

这两个补丁随 eqi12 上的 pi-vcc 一起分发，Go 侧原生实现（非事后外挂），并有 1:1 测试。

### 补丁 #1 `pi-vcc-0.9.0-ctx-run`（tracked-commands）

放宽 tracked-commands 的扫描门槛：主循环不再只扫 `bash` 工具，而是对**任何** tool_call
块调用 `commandTextsOf(args)`（`trackedCommands.go:223`）抽取 shell 类命令文本，支持三种形状——
`args.command`（string）、`args.argv`（数组，只收 string 元素后 `join(" ")`）、
`args.commands`（数组，元素为 string 或含 string `command` 字段的对象）；无这些字段的调用不贡献文本，
对非 shell 工具仍是 no-op。目的是让 `ctx_run`（bash 之外的执行通道）的命令也进入跨压缩记忆。
测试：`patchCtxRun_test.go`。

### 补丁 #2 `pi-vcc-0.10.0-recall-note`（`RECALL_NOTE`）

在 `RECALL_NOTE` 常量尾部追加一句，提示模型每次压缩后先调 `vcc_recall` 拉回本任务的原始指令，
原句一字未改。Go 侧 `format.go:150` `RecallNote` 逐字节等于补丁后常量；`summarize.go:436`
`buildRecallNoteRe()` 由该常量动态生成正则、`summarize.go:451` `StripRecallNote()` 在合并前剥离旧尾注，
故尾注不随压缩叠加。测试：`patchRecallNote_test.go`。

## 与 TS 0.11.1 的行为差异

按"可观测行为"口径列；以下偏差均在代码注释或测试里钉住。完整差异清单见
`go/reports/batch-4-pi-vcc.md` §10；同步到 0.11.1 后的差异复核与遗留项见
`go/reports/batch-4b-sync-0.11.1.md`。

### D1 配置路径解析（`settings.go:79`）

TS 固定读 `homedir()/.pi/agent/pi-vcc-config.json`；Go 侧 `AgentDir()`（`settings.go`）按
`PIG_EXT_HOST_INFO.agentDir` → `$PI_CODING_AGENT_DIR` → `$PIG_CODING_AGENT_DIR` → `$PIG_HOME/agent` → `~/.pi/agent` 解析，
且 `SettingsPath()` 额外认 `$PI_VCC_CONFIG_PATH` 显式覆盖。目的是隔离 PiG 运行时绝不读写
生产 `~/.pi/agent` 配置（迁移硬规则），也是测试围栏的基础（`extension_test.go` 的 `TestMain`
把 `PI_VCC_CONFIG_PATH` 指向临时文件）。

### D2 每扩展独立状态（`extstate.go`）

TS 把 `lastStats` / `lastCompactWasPiVcc` / `pendingFollowUpPrompt` / auto-continue 定时器放在
模块作用域（一个扩展加载一个模块实例）。Go 一个子进程可服务多个扩展实例，故这些可变状态收进
`extensionStates` 映射、按 `*sdk.Extension` 指针取用（`extstate.go:34` `extensionStates` 映射、`extstate.go:37` `stateFor`）。
由 `extstate_test.go:17` `TestExtState_IsKeyedPerExtension` 钉住"两个 `Extension()` 不共享状态"的边界。

### D3 用户消息词数（`userMessages.go`）

TS 用 `new Intl.Segmenter(undefined, { granularity: "word" })` 数字，Go 无 ICU 与词典数据，
故按 UAX#29 字符类近似：字母/数字连续段算一个词（跨撇号、数字间的 `.`/`,` 合并），每个 Han
连续段算 `ceil(n/2)` 词，平假名/片假名一字一词。该校准表连同 node 实测值钉在
`extract_test.go` 的 `TestUserMessageWordCount_IntlSegmenterCalibration`。已知分歧仅两处短中文
（ICU 词典把「走吧/是的/没事」读作两个词，此处读作一个）与一处长句（ICU 多读一个词）；词数只
决定「是否满两个词」，故不影响任何实质消息的去留。

### 其它偏差

- **`sortedKeys`（`content.go:294`）**：TS 用 `Object.keys` 取插入序，Go map 无插入序，故改为排序键；
  仅在 TS 顺序本身"看似未指定"的输出（工具参数名）上可见，属对有意偏差的记录。
- **JS 语义适配层（`jslite.go:12` 等）**：字符串按 UTF-16 码位取长/切片（`jslite.go`）、
  `toFixed(1)` 用 `math/big` 做 half-away-from-zero（`jsFixed1`）、RegExp 在 RE2 上复刻 JS 语义
  （`.` 不吃行终止符、`\s` 含 Unicode 分隔符 + U+FEFF、`i` 用 Unicode simple case folding、
  `\uXXXX` 展开、JS `\b`，见 `jsregex.go`）。这些是"看起来相同"的必要成本。
- **`setTimeout(..., 0)` → `time.AfterFunc` + `recover`**：auto-continue 的不可见续跑改由 Go 定时器
  触发，回调里 `recover()` 兜住 detached Context 下的 panic（`extstate.go`）。
- **JSON 数值到 `any` 得 `float64`**：断言/格式化前统一走 `jsNumberString` / `jsFixed1`，避免 `1`
  被渲染成 `1.0`。

未发现"Go 侧行为与 TS 不一致"的实质偏差；如发现，按 `go/reports/batch-4-pi-vcc.md` §5 的用例逐条对照定位，
0.11.1 同步后的对照结果记在 `go/reports/batch-4b-sync-0.11.1.md`。

## 验证

在本目录实跑（环境 `go version go1.26.4 linux/amd64`）：

```text
gofmt -w .                 # 原地格式化（消除结构体 tag 对齐差异）
gofmt -l .                 # 输出为空
go build ./...             # exit 0
go vet ./...               # exit 0
go test -race -count=1 ./...
PASS
ok github.com/dorokuma/pi-extensions/go/pi-vcc
```

完整竞态输出 0 DATA RACE；测试与行为差异的逐条对照见 `go/reports/batch-4-pi-vcc.md`。
隔离加载实测（`go/bin/pig-test -e ./pi-vcc`，`PIG_HOME=/root/.pig-test`）通过，`vcc_recall`
工具在隔离会话中被实际调用并返回真实结果（见 batch-4 报告 §8；0.11.1 同步后的复测输出原文
与退出码见 `go/reports/batch-4b-sync-0.11.1.md`）。

未提交、未部署。以上为 worker 验证，仍需 reviewer / verifier 独立核验。
