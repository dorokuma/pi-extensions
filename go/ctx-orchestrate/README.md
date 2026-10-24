# ctx-orchestrate（Go 版）

PiG 扩展组件，移植 `pi-extensions/extensions/ctx-orchestrate/src/ctx-orchestrate.ts`
（部署件 `/root/.pi/agent/extensions/ctx-orchestrate.ts`，md5 `0e33386a7178868cbefe31969f08720b`）。
只移植精简后版本：`ctx_search_digest` 组合工具 + codemode 3000 字回传治理钩子 + 注册诊断日志。
**不包含**已在本分支 `de148fc` 删除的 `ctx_symbol_read` / `ctx_test_digest`。

## 目录

| 文件 | 内容 |
|---|---|
| `go.mod` | `module github.com/dorokuma/pi-extensions/go/ctx-orchestrate`，`go 1.26`，只依赖 SDK（无 `replace`，只用标准库 + SDK） |
| `extension.go` | `func Extension() *sdk.Extension`：命名空间、工具注册、`tool_result` 钩子挂载、注册诊断 |
| `digest.go` | `ctx_search_digest`：rg 输出解析、限并发嵌套调用、跨模式去重、按文件聚合、摘要与结构化结果 |
| `govern.go` | 治理钩子 `CapCodemodeResult`、`TextCap` / `GovernEnabled`、环境变量读取 |
| `text.go` | JavaScript 语义助手：UTF-16 长度与安全截断、`oneLine`、`clampInt`、`optStr` |
| `diag.go` | TUI-safe 诊断日志（默认写文件，`CTX_ORCH_DEBUG=1` 才写 stderr） |
| `*_test.go` | 对照测试（94 个 PASS = 41 顶层 + 53 子测试；全部用假 rg，不跑外部二进制）。`diag_test.go` 专测诊断路径解析与 PIG_HOME 隔离 |

## TS → Go 对照

| TS（`ctx-orchestrate.ts`） | Go 位置 |
|---|---|
| `:44` `NAMESPACE_NAME` | `extension.go:35` `NamespaceName` |
| `:47` `DEFAULT_TEXT_CAP` | `govern.go:37` `DefaultTextCap` |
| `:50` `GOVERN_MIN_TOTAL` | `govern.go:41` `GovernMinTotal` |
| `:53` `EXCERPT_CAP` | `text.go:17` `ExcerptCap` |
| `:56` `HITS_DEFAULT` / `HITS_HARD` | `digest.go:22` `hitsDefault` / `digest.go:23` `hitsHard` |
| `:59` `CONCURRENCY` | `digest.go:25` `concurrency` |
| `:61` `envInt`（仅用于 `CTX_ORCH_TEXT_CAP`）/ `:68` `envFlag` | `govern.go:61` `parseTextCap`（总函数，见差异 6）/ `govern.go:78` `envFlag` |
| `:75` `clampUtf16Index` | `text.go:57` `truncateUTF16`（等价切割，见下） |
| `:83` `clampInt` | `text.go:119` `clampInt` |
| `:98` `oneLine` | `text.go:80` `oneLine` |
| `:109` `diagLog` | `diag.go:34` `diagLog` / `diag.go:68` `diagFile` / `diag.go:85` `resolveDiagDir` |
| `:141` `runNested` | `digest.go:470` `executeToolRunner`（返回 `nestedOutcome`） |
| `:177` `mapLimit` | `digest.go:223` `mapLimit`（泛型，chunk + 等齐） |
| `:219` `parseRgOutput` | `digest.go:122` `ParseRgOutput`（导出，同 TS `export`） |
| `:275` `searchDigestSchema` | `extension.go:55` `searchDigestSchema` |
| `:288` `searchDigestOutput` | `extension.go:80` `searchDigestOutputSchema` |
| `:300` `executeSearchDigest` | `digest.go:246` `executeSearchDigest` |
| `:460` `textCap` / `:464` `governEnabled` | `govern.go:79` `TextCap` / `govern.go:84` `GovernEnabled` |
| `:475` `capCodemodeResult` | `govern.go:97` `CapCodemodeResult`（导出，同 TS `export`） |
| `:556` `installGovernanceHook` | `extension.go:128` `handleToolResult` + `extension.go:163` `e.OnToolResult` |
| `:572` `export default function (pi)` | `extension.go:139` `Extension()` |

## 行为对齐要点

- **参数 schema**：`patterns` 必填（string 数组），`path`/`glob`/`max_hits`/`include_hidden`/
  `ignore_case`/`context`/`literal` 可选；`max_hits` 默认 50、硬顶 500（`clampInt`）；`context`
  取 `min(floor(n), 5)`；`path`/`glob` 空串经 `optStr` 丢弃。
- **嵌套调用**：一律 `ctx.ExecuteTool("ctx_fs", args, nil)`，`action:"rg"`、`limit:maxHits`。
  `nil` 选项即 SDK 默认「沿用调用方请求的取消」，与 TS 把工具调用的 `AbortSignal` 传下去等价
  （SDK 另有 run 级的 `ctx.Signal()`，不是请求级，故意不用）。
- **并发**：`mapLimit` 按 4 个一块跑、块内并发、块间等待，结果按输入顺序回收，单次调用失败/panic
  都降级成条目，不会拖垮整批（TS 的 `Promise.allSettled` 语义）。
- **降级重试**：给了 `path` 且零命中且首次成功时，用 `include_hidden:true` + `glob:"**"` 重试一次；
  第二次有命中才替换，`truncated` 取或。没给 `path` 不重试。
- **去重聚合**：同一 `file:line` 被多个模式命中合并成一条，`patterns` 按模式输入顺序累加（不重复），
  `excerpt` 取 UTF-16 更长的那条；文件内按行号升序；文件按命中数降序、再按路径升序。
- **摘要与文本**：`patterns=N files=N hits=N`（`rg reported N` 仅在首行自报命中数时追加）、
  `; K pattern(s) degraded: ...`、`; rg output truncated — ...`；文本面最多 40 个文件、每文件 5 条，
  超出写 `... (+N more ...)`；`details` 只有 `{summary}`，结构化结果走 `StructuredContent`
  （`patterns/files[{path,hits[{line,excerpt,patterns}]}]/summary/errors?`）。
- **isError**：只有「全部模式都失败」才是错误结果；部分失败只落在 `errors` 里。
  空 `patterns` 直接返回 `isError:true` + `errors:[{pattern:"",error:"patterns is empty"}]`。
- **治理钩子**：`toolName=="codemode"` 且无 `parentToolCallId`（嵌套调用不治理）且文本总量 > 200
  且 > 上限（`TextCap()`，默认 3000）才动手；预算 = `cap-180`（原因见下「marker 预算」），
  超出部分丢弃并统计 `dropped`；marker 追加到最后一个文本块；`details`/`structuredContent`/
  `isError`/`usage` **显式回传**（丢 `details` 会没有 `fullOutputPath`，丢 `structuredContent`
  会被宿主清空）；`details.fullOutputPath` 存在时 marker 带上该路径。
- **marker 预算（`cap-180`，与 TS 一致，勿改）**：截断预算 = `max(0, cap-180)`，即先扣掉 180 个
  UTF-16 单位再装正文，剩余空间留给追加的中文警告 marker，保证「截断后总量 ≤ cap」。这个 180
  不是估算值，而是与 TS 版 `capCodemodeResult` 的 `Math.max(0, cap - 180)` **逐字对齐**的截断预算
  语义：两边对同一输入得到同样的 `dropped` 与同样的总长上界，同一套 `CTX_ORCH_TEXT_CAP` 在 TS/Go
  两版之间行为可互换。**刻意不改成「截到刚好 cap、marker 再外挂」**：那会让治理后的回传总量超过
  cap（TS 不会），也会让 `dropped` 与 TS 不一致，属跨版本回归。
- **数值边界（`CTX_ORCH_TEXT_CAP`）**：`parseTextCap` 是总函数，以下输入一律回退默认 3000，
  而不是让非法浮点漏进 Go 的 int 转换：未设置 / 空串 / 无法解析（`abc`、`12px`）/ `NaN`、`nan` /
  `Inf`、`+Inf`、`-Inf`、`infinity`、`-infinity` / `<= 0`（`0`、`0.0`、`-1`、`-0.5`）/ 超出 int 范围
  （`1e19`、`9.3e18`、`1e308`，以及 `9223372036854775807`——它作为 float64 已进位到 2^63）。
  有限正数先 `floor`，再过 `MinTextCap=200` 地板（`250.7→250`、`199.9→200`、`3000.9→3000`）。
  为什么必须这样：Go 的 `int(NaN)` / `int(1e19)` 是实现定义的，amd64 上折成最小 int，再经
  `max(200, ·)` 就变成 **200** 硬顶——一个手误的环境变量会把每条 codemode 回传砍到 200 字符
  （实测旧实现：`3500 字符` 的回传被写成「超过硬顶 200，丢弃约 3480 字符」）。现在这类值等价于
  「没设」，治理按 3000 跑。要整体关闭治理用 `CTX_ORCH_GOVERN=0`（不靠把 cap 设成天文数字）。
- **UTF-16 语义**：JS 的 `String.length` 与 `slice` 都按 UTF-16 code unit 计，本包所有长度比较与截断
  都用 `utf16Len` / `truncateUTF16`，星平面字符计 2 单位且永不切断（等价于 TS 的
  `clampUtf16Index(..., towardStart=true)`）。例：cap=200 时 200 个 emoji 只留 99 个 + `…`（199 单位），
  与 JS 结果一致。
- **诊断**：注册时写一行 `registered ctx_search_digest (...) and codemode text cap governance (...)`；
  治理触发时写 `codemode result capped: total=... cap=... dropped=... blocks=...`。
  目录解析优先级：`CTX_ORCH_DIAG_DIR`（非空即用）→ 非空 `PIG_HOME` 时 `$PIG_HOME/agent/logs` →
  否则 `<home>/.pi/agent/logs`（无 home 时 `/tmp/.pi/agent/logs`），文件名固定
  `ctx-orchestrate.log`。写失败只降级一次并自报 `diagnostics degraded`，不影响扩展。

## 与 TS 的差异（逐条）

1. **事件承载类型**：TS 的 `tool_result` 事件是对象（`event.toolName` 等）；Go 侧 SDK 只给
   `map[string]any`（wire key：`toolName`/`parentToolCallId`/`content`/`details`/`structuredContent`/
   `isError`/`usage`），`CapCodemodeResult` 因此收 `map[string]any`。字段名与语义逐一对应。
2. **无改动时的返回值**：TS 返回 `undefined`；Go 返回 `nil`（宿主同样视为「不改写」）。
3. **成员存在性**：TS 无条件写 `details/structuredContent/isError/usage`（值为 `undefined` 时被
   `JSON.stringify` 丢掉）；Go 只在事件真的带该成员时才回传，序列化结果一致。
4. **排序 tie-break**：TS 用 `a.path.localeCompare(b.path)`（区域感知）；Go 用字节序 `<`。
   仅在非 ASCII / 大小写混淆路径上可能顺序不同（受测样例一致）。
5. **非字符串 pattern**：TS `String(p)` 对对象/数组得到 `[object Object]`/逗号串；Go 的
   `toJSString` 对对象/数组返回空串，因而被「空 pattern」过滤掉。实际参数是字符串，无影响。
6. **`CTX_ORCH_TEXT_CAP` 解析是总函数**：TS `envInt` 只挡 `Number.isFinite`（NaN/±Inf 回退 3000），
   其余交给 `Math.max(200, Math.floor(n))`；因此 TS 里 `0`/`-5` 会得到 **200** 硬顶，而 `1e19` 会原样
   当上限（治理实际失效），`NaN`→3000 与 Go 相同。Go 版把「不是可用正数上限」统一视为未设置：
   `<= 0` 与超出 int 范围都回退 3000（TS 分别是 200 与 1e19）。这是有意的安全性差异：200 会把回传
   砍到几乎不可用，1e19 则静默关掉治理；两者都远离调用方真实意图，而 `default` 是文档化的默认行为。
   有限正数、解析写法（接受 `1e3`，不接受前后空格/十六进制）与 TS 的差异不变。

7. **数字取值**：Pig 用 `encoding/json` 解码参数，整数是 `float64`；`clampInt`/`positiveInt` 同时
   接受 `float64`/`int`/`int64`/`json.Number`，便于单测与其它调用方。
8. **结构化结果类型**：TS 是对象字面量；Go 是 `SearchDigestOutput` 结构体。JSON 成员名与顺序与 TS
   一致（`patterns`→`files`→`summary`→`errors?`；hit 内 `line`→`excerpt`→`patterns`），
   所以 codemode 脚本按名取字段不受影响。
9. **诊断时间格式**：TS `new Date().toISOString()`；Go 用等价的
   `2006-01-02T15:04:05.000Z`（UTC，毫秒）。
10. **`oneLine` 默认参数**：TS 有默认 `cap=EXCERPT_CAP`；Go 显式传 `ExcerptCap`（同一常量），
    错误行传 `errLineCap=300`，与 TS 的 `oneLine(..., 300)` 相同。
11. **诊断默认目录尊重 `PIG_HOME`**：TS 只认 `os.homedir()`（`<home>/.pi/agent/logs`）；Go 版在
    `CTX_ORCH_DIAG_DIR` 未设置且 `PIG_HOME` 非空时改用 `$PIG_HOME/agent/logs`，否则仍回退 TS 位置。
    原因：`./bin/pig-test` 用 `PIG_HOME=/root/.pig-test` 隔离运行，默认路径若仍指向 `~/.pi/agent/logs`
    就会在隔离演练里写生产目录（正是本次要修的隔离缺口）。设了 `CTX_ORCH_DIAG_DIR` 时两版行为一致。
12. **marker 预算未改**：`cap-180` 与 TS 逐字一致（见「行为对齐要点·marker 预算」），属刻意的
    兼容性选择而非待办；改它会造成同一环境变量下 TS/Go 截断结果不同。

## SDK / 宿主缺口（实测，未改 PiG 源码）

**缺口 1：codemode exposure 的工具在 PiG 0.5.2 里「未激活就对脚本不可见」。**

- TS pi 文档与本 SDK 文档都写的是 `codemode` = 「注册即可调用、不主动声明给模型」
  （`pigsdk/tool_orchestration.go:18-30`），PiG 宿主 `coding/session_loadout.go:108-124`
  的 `callableEntriesLocked` 也把 `codemode`/`deferred` 一律算作 callable。
- 实测（隔离目录 `/root/.pig-test`，本组件已加载、诊断日志证明注册成功）：
  - `-t codemode` → 脚本报
    `TypeError: tools.ctx_search_digest does not exist. Available: .`；
  - `-t codemode,read,bash` → `Available: read, bash`（只有被激活的 direct 工具）；
  - `-t codemode,ctx_search_digest` → `{"digest":"function","all":["ctx_search_digest"]}`，可见可调。
- 控制实验排除「本实现的问题」：`/tmp/probe-codemode`（21 行、只注册一个 `exposure="codemode"`
  的 `probe_tool`，无 namespace、无 OutputSchema 之外的额外设置）同样报
  `tools.probe_tool does not exist. Available: .`。故这是 PiG 宿主侧行为/缺口，与 Go 移植无关。
- 规避（不改 PiG）：部署时把工具名放进 `--tools` 或 settings 的 `defaultTools`，例如
  `pig -e <组件> -t codemode,ctx_search_digest`。**不**把 `DefaultActive` 设成 `true` 来「修好」：
  那会让工具主动声明给模型、进入 loadout，偏离 TS 的设计（TS 从不主动声明）。

**缺口 2：`tool_result` 事件没有类型化结构。** PiG 内部有
`coding/extension/events.go:1144` 的 `ToolResultEventResult`，但 Go SDK 只暴露
`OnToolResult(EventFunc)`（`func(ctx, map[string]any) (any, error)`）与一组
`IsXxxToolResult` 助手，没有「构造治理结果」的类型。本组件按 wire key 手写结果 map，
语义与宿主结构一致；若 SDK 后续提供类型化结果，`CapCodemodeResult` 可直接切换。

**说明（不是缺口）**：`ExecuteToolOptions.Signal` 需要 `context.Context`；SDK 的
`Context.Signal()` 是 run 级取消（`run_signal.go:54`），而 TS 传的是请求级 `AbortSignal`，
因此这里传 `nil`，让 SDK 用默认的「调用方请求取消」——语义相同。

## 跑测试

```bash
cd /root/workspace/pi-extensions/go/ctx-orchestrate
gofmt -l .              # 无输出
go vet ./...
go test -count=1 -race -v ./...
```

- **不跑真的 rg、不依赖外部二进制**：`executeSearchDigest(params, run nestedRunner)` 的第二个参数是
  嵌套调用接缝。生产是 `executeToolRunner(ctx,"ctx_fs")`（包 `ctx.ExecuteTool`，含 panic/错误降级），
  测试是 `fakeRunner`（可编排每个模式的输出、记录参数、测并发峰值）。
- 诊断日志在测试里被 `TestMain` 指到临时目录（`CTX_ORCH_DIAG_DIR`），不会写 `~/.pi/agent`。
- 覆盖：分组输出与裸 `file:line:content` 两种 rg 形态（TS 的 `parseRgOutput` 只认这两种文本形态，
  ctx_fs rg 没有 `--json` 输出形态，因此没有 JSON 解析分支要移植；`--json` 参数会由 `ctx_fs` 自己校验，
  本工具仍按文本解析）、`matches=`/`truncated=true`/`capture truncated`
  三种信号、`maxPerFile` 截断、excerpt 压平与 200 单位截断、跨模式去重与更长 excerpt 取胜、
  文件排序、空 patterns、参数转发与 `max_hits` 钳制、隐藏目录重试与「无 path 不重试」、
  部分失败/全部失败、truncated 摘要、文本上限（40 文件/5 条）、并发峰值 ≤ 4 且结果按输入顺序；
  治理钩子：3500 字压到 ≤3000 并带 marker/`fullOutputPath`、无路径 fallback 文案、
  `details`/`structuredContent`/`isError`/`usage` 透传、嵌套调用与非 codemode 工具不动、
  阈值边界（200 / 3000）、`CTX_ORCH_TEXT_CAP` 覆盖与 200 下限、`CTX_ORCH_GOVERN=0`、
  多块（图片保留、marker 落在最后文本块、丢弃计数）、尾部块被整体丢弃、UTF-16 计数与不切断、
  块其它成员（如 `cacheControl`）保留且不原地改事件数据；另加 `Extension()` 注册冒烟与 schema 形状。
- 本次新增的边界/隔离用例（bug 回归，先 FAIL 后 PASS，见下）：
  `TestTextCapRejectsUnusableValues`（19 个子用例：空/空白/不可解析/`NaN`/`Inf`/`-Inf`/`0`/负数/
  `1e19`/`9.3e18`/`1e308`/`math.MaxInt`）、`TestTextCapAcceptsFinitePositiveValues`（5 例：
  `1000`、`3000.9`、`250.7`、`199.9→200`、`2^63-1024`）、
  `TestCapCodemodeResultWithUnusableCapFallsBack`（3 例：`NaN`/`1e19`/`0` 下 
  marker 必须写硬顶 3000 而不是 200）、`TestResolveDiagDir`（5 例优先级与 `/tmp` 回退）、
  `TestDiagDefaultsToPigHome`（PIG_HOME 隔离：诊断行落 `$PIG_HOME/agent/logs`、`$HOME/.pi` 不被创建）。
  删掉了随 `envInt` 一起退役的 `TestEnvInt`，换成了 `TestEnvFlag`（`envFlag` 仍在用）。
- **先 FAIL 后 PASS 证据（2026-10-24）**：把旧语义副本（`TextCap` 走旧 `envInt`；`diagFile` 无
  PIG_HOME 分支）临时放回包内，用 `env HOME=/tmp/ctxorch-failfirst/home GOCACHE=… GOPATH=…`
  隔离跑（HOME 指到 `/tmp`，旧代码即使写 `$HOME/.pi` 也落不到 `/root/.pi`），EXIT=1：
  `TextCap(CTX_ORCH_TEXT_CAP="NaN") = 200, want the 3000 default`（`Inf`/`-Inf`/`0`/`1e19` 同形），
  `resolveDiagDir("", "/pig", "/home/u") = "/home/u/.pi/agent/logs", want "/pig/agent/logs"`，
  且旧代码下 3500 字回传的 marker 写的是「超过硬顶 200，丢弃约 3480 字符」；换回修复版后同一组
  用例 EXIT=0 全 PASS。

## 在隔离目录加载验证

```bash
cd /root/workspace/pi-extensions/go

# 1) 冒烟：加载 + 一次模型调用（PIG_HOME=/root/.pig-test，由 bin/pig-test 强制）
CTX_ORCH_DIAG_DIR=/tmp/ctxorch-diag ./bin/pig-test -e ./ctx-orchestrate -p "回复 OK"

# 2) 治理钩子在真实会话里触发（codemode 需要显式激活：model-only + defaultActive=false）
CTX_ORCH_DIAG_DIR=/tmp/ctxorch-diag ./bin/pig-test -e ./ctx-orchestrate --mode json \
  -t codemode -p '调用 codemode，脚本：text("x".repeat(3500))'

# 3) 让工具真正可达（见「缺口 1」）：显式激活
CTX_ORCH_DIAG_DIR=/tmp/ctxorch-diag ./bin/pig-test -e ./ctx-orchestrate --mode json \
  -t codemode,ctx_search_digest -p '调用 codemode：text(JSON.stringify(ALL_TOOLS.map(t=>t.name)))'
```

下面分两部分：本次的假 provider 真实入口演练（新证据）与上一轮的 print 模式记录（未复跑，仅存栏）。

### 本次：真实入口演练（本地回环假 provider，治理链路实测）

命令（`cd /root/workspace/pi-extensions/go`，**不设** `CTX_ORCH_DIAG_DIR`，让默认路径走 PIG_HOME 分支）：

```sh
# 假 provider：仅 /tmp、仅监听 127.0.0.1:3477；首轮回 codemode 工具调用，次轮回 DONE
python3 /tmp/ctxorch-drill/fake_provider.py /tmp/ctxorch-drill/requests.log 3477 &
python3 /tmp/ctxorch-drill/add_provider.py /root/.pig-test/agent/models.json 3477

(printf '%s\n' '{"id":"1","type":"prompt","message":"请用 codemode 跑 text(\"x\".repeat(3500))"}'; sleep 45) \
  | ./bin/pig-test -e ./ctx-orchestrate --provider fakeorch --model fake-1 --mode rpc --no-session -t codemode \
  > /tmp/ctxorch-drill/rpc.log 2>&1
# EXIT=0
```

假 provider 请求日志原文（`/tmp/ctxorch-drill/requests.log`）：

```
{"seq": 1, "path": "/v1/chat/completions", "model": "fake-1", "stream": true, "n_messages": 2, "roles": ["system", "user"], "tool_result_lens": [], "tool_result_has_governance_marker": false}
{"seq": 2, "path": "/v1/chat/completions", "model": "fake-1", "stream": true, "n_messages": 4, "roles": ["system", "user", "assistant", "tool"], "tool_result_lens": [2937], "tool_result_has_governance_marker": true, "tool_result_tail": ["xxxx…\n[…ctx-orchestrate 治理] codemode 回传 3547 字符超过硬顶 3000，已物理截断（丢弃约 727 字符）。请在脚本内自己收敛：只回摘要/计数/关键路径，需要全文就写入文件后用 read 精确取片段。"]}
```

RPC 事件流原文片段（`/tmp/ctxorch-drill/rpc.log`，长行截短）：

```
{"type":"tool_execution_start","toolName":"codemode","args":{"code":"text(\"x\".repeat(3500))"}}
{"type":"tool_execution_end","toolName":"codemode","result":{"content":[{"type":"text","text":"Script completed\nWall time 0.0 seconds\nOutput:\n"},{"type":"text","text":"xxxx…\n[…ctx-orchestrate 治理] …"}],"details":{"calls":[]}},"isError":false,"durationMs":35}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"DONE"}],"stopReason":"stop"}}
{"type":"agent_end","willRetry":false}
```

诊断日志原文（默认路径生效：`PIG_HOME=/root/.pig-test` →
`/root/.pig-test/agent/logs/ctx-orchestrate.log`，**未**写 `/root/.pi/agent/logs`）：

```
2026-10-24T07:19:21.318Z [ctx-orchestrate] registered ctx_search_digest (exposure=codemode, namespace=ctxmode) and codemode text cap governance (cap=3000, enabled=true)
2026-10-24T07:19:21.387Z [ctx-orchestrate] codemode result capped: total=3547 cap=3000 dropped=727 blocks=2
```

结论与边界：

- 注册诊断、codemode 工具结果治理**都发生在真实宿主链路上**：codemode 原始回传 3547 字符，
  治理后交给模型的 tool 结果 2937 字符（≤ 3000）且带治理 marker，下一次 provider 请求看到的
  就是这个已治理结果；脚本按 `cap-180` 预算留位，与 TS 一致（2937 = 2820 正文 + 换行 + marker）。
- `-t codemode` 让 codemode 成为可调用工具（缺口 1 的行为不变）；该次无 `fullOutputPath`，
  marker 用 fallback 文案。
- 隔离边界：`/root/.pig-test/agent/models.json` 演练期间临时多一条 `fakeorch`（127.0.0.1 回环），
  演练后从备份还原，md5 = `7ef998265f4515f58cbe201c117f7b36`（已核）；假 provider / 请求日志
  只在 `/tmp/ctxorch-drill/`，未入库；演练产生的 `/root/.pig-test/agent/logs/` 在取证后清除
  （副本 `/tmp/ctxorch-drill/diag.log`）；全程未写 `/root/.pi`、未 `/reload`、未重启任何 pi。

### 上一轮记录（print 模式，未复跑，仅存栏）

- 冒烟 EXIT=0，助手回复 `OK`。`toolsAdded` 里**没有** `ctx_search_digest`——它 `exposure="codemode"`、
  不是默认激活项（`defaultActive` 未设，按 exposure 默认 false），与 TS 设计一致；
  给 `-t ctx_search_digest` 时它出现在系统提示的工具清单里（declaration + promptSnippet +
  guidelines 都可见），说明注册链路正常。
- 治理钩子当场触发过一次，结果是
  `[…ctx-orchestrate 治理] codemode 回传 3547 字符超过硬顶 3000，已物理截断（丢弃约 727 字符）。…`，
  诊断日志同时写 `codemode result capped: total=3547 cap=3000 dropped=727 blocks=2`。
- 注册诊断（每次会话一行）：
  `[ctx-orchestrate] registered ctx_search_digest (exposure=codemode, namespace=ctxmode) and codemode text cap governance (cap=3000, enabled=true)`。

## 未做 / 后续

- 未带回 `ctx_symbol_read` / `ctx_test_digest`（`de148fc` 已删除）。
- 未实现 TS 版没有的部分：不注册 command、不改 settings、不 spawn 进程（与 TS 一致）。
- 缺口 1 的根治在 PiG 宿主（callable 判定或 codemode 列出未激活的 codemode 工具），按 PLAN 硬规矩 6
  只记录、不改 PiG 源码。
