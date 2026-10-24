# codegraph-go（Go / PiG 移植）

PiG 的 **CodeGraph MCP stdio 桥接扩展**，不是索引服务器本身。它将 Pi 的单个
`codegraph` 工具转发到外部 `codegraph-go` 二进制，保留工作目录授权、按需启动、
参数预算与输出清洗。需要与工具面匹配的 codegraph-go ≥ 0.9.0。

- Module：`github.com/dorokuma/pi-extensions/go/codegraph-go`，Go 1.26。
- SDK：`github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0`；本地通过父级 `go.work`
  解析，PiG 构建时使用 staged SDK。组件不含本地 `replace`。
- 入口：`package codegraphgo`，导出 `func Extension() *sdk.Extension`。
  **无 `init()` / `main()`**，与同仓 `moshi-hooks`、`jev-gate`、`ctx-orchestrate` 的
  factory 加载方式一致。初始 standalone 入口被 PiG 判为父 module 的根路径不匹配；
  经主代理补充授权改用 factory 后，嵌套 module 加载验证通过。
- 权威参考：`/root/.pi/agent/extensions/codegraph-go.ts`，1237 行，与
  `/root/workspace/codegraph-go/integrations/pi/codegraph-go.ts` 逐字节一致。
  SHA-256：`2618db7c78059d3179bd0983701a0d2481a09e2c455dfc15e7a884c20f80f625`。
- 文档参考：宿主仓 Pi integration，本机绝对位置为
  `/root/workspace/codegraph-go/integrations/pi/README.md`；该宿主仓
  不属于本仓交付物。移植中未修改宿主仓或生产部署目录。

## 行为契约

1. `session_start` 只决议授权根、注册工具和通知，**不 spawn**。首次工具调用惰性启动；
   并发首次调用互斥。启动直接使用已授权的决议根，不重新读取 WORKDIR 决议（M4）。
2. workdir 优先级：强制 `CODEGRAPH_GO_WORKDIR` → config 第一个展开后非空的根 →
   git root → 项目标记根 → cwd。`/`、`/home`、`/Users` 默认拒绝；家目录可作索引根。
3. config 查找：`CODEGRAPH_CONFIG` → `./codegraph-config.yaml` →
   `~/.config/codegraph/config.yaml`；环境变量指向死路径时记录诊断并继续默认查找。
   轻量 workdirs 解析支持 block/flow；锚点、别名、多文档、独立块标量指示符及 flow
   mapping 整体回退。无可解析列表时授权根回退 HOME；列表有项但展开全部为空时
   **空授权根 fail closed**。最终根必须通过 canonical 路径段级授权判定。
4. 20 个项目标记与 TS 一致；家目录提示只列一级带标记的非隐藏目录，跳过 `go`、
   `node_modules`、`code_references`。
5. MCP 逐行 JSON-RPC：`initialize`（协议 `2024-11-05`，client
   `pi-codegraph-go/0.4.0`）→ `notifications/initialized` → `tools/list`。
   工具始终转发 `tools/call` 的 name=`codegraph`，保留完整参数，不按 action 拆工具。
6. `!initialized` 时先 `start()`；这一步在 TS 位于 `try` 之外，因此**首次启动失败
   （含 `initialize timed out`）直接返回该错误，不会落进重试分支再起一次进程**。
   之后 `tools/call` 报错匹配 `not running|disconnected|timed out` 时才调用 `start()`
   并重发**一次**。对齐 TS 的幂等 start：超时时若进程仍已初始化，不强制换进程，而在
   同一连接重试；真正断连后才重新 spawn。普通业务错误不重试；失败后的下次调用可重新
   惰性启动。
7. 默认输出预算超限才压缩：行数保头尾，字符按 UTF-16 code unit 计数，不切断代理对。
   再按 TS 顺序清洗 markdown、引用与 rg fallback。`node/read`、`skipCode=false` 或
   `includeCode=true` 保留代码内容与 `file:line:content`，但仍执行行内反引号清理。
8. `before_agent_start` 保留原 system prompt，追加工作目录、预算和可选 server instructions；
   未授权则追加不可用说明。SDK 的 `*sdk.SystemPrompt` 通过 `Get()` 读取。
9. shutdown、手动 stop 关闭客户端；stop 保留决议，后续调用可以再次惰性启动。
   3 个命令为 `codegraph-start`、`codegraph-stop`、`codegraph-info`，文案按 TS 保留。
10. stderr 持续 drain；非 DEBUG 保留最近 20 行，异常退出过滤 INFO/DEBUG 写诊断，
    过滤后为空且 force 时回退最近 5 行；正常/预期关闭不 dump。

统一工具定义包含 TS 的 Label、Description、PromptSnippet、10 条 PromptGuidelines、
29 个参数属性（含 action）及全部 12 个 action：`search`、`files`、`explore`、`callees`、
`callers`、`impact`、`node`、`status`、`affected`、`communities`、`store_fact`、`search_facts`。

## 环境变量

每次函数执行时读取，便于测试与运行时覆盖，不在包初始化时固化。

| 变量 | 默认值 / 作用 |
|---|---|
| `CODEGRAPH_GO_BIN` | `codegraph-go` |
| `CODEGRAPH_GO_START_TIMEOUT_MS` | `30000` |
| `CODEGRAPH_GO_REQUEST_TIMEOUT_MS` | `120000` |
| `CODEGRAPH_GO_OUTPUT_CHARS` | `18000`；非正值压缩时回退 `14000` |
| `CODEGRAPH_GO_OUTPUT_LINES` | `260`；非正值压缩时回退 `220` |
| `CODEGRAPH_GO_SEARCH_MAX` / `_SEARCH_HARD` | `70` / `120` |
| `CODEGRAPH_GO_FILES_MAX` / `_FILES_HARD` | `100` / `150` |
| `CODEGRAPH_GO_SYMBOL_MAX` / `_SYMBOL_HARD` | `40` / `80` |
| `CODEGRAPH_GO_EXPLORE_MAX` / `_EXPLORE_HARD` | `30` / `60` |
| `CODEGRAPH_GO_WORKDIR` | 强制根，仍需授权校验 |
| `CODEGRAPH_GO_ALLOW_BROAD` | `1` / `true` / `yes` / `on` 允许系统根 |
| `CODEGRAPH_CONFIG` | config 首选路径，死路径跳过 |
| `CODEGRAPH_GO_DEBUG` | 非空诊断到 stderr；否则 `~/.pi/agent/logs/codegraph-go.log`，HOME 不可解析时按 TS `os.homedir() \|\| "/tmp"` 回退 `/tmp/.pi/agent/logs/codegraph-go.log` |

测试均将 HOME 指向临时目录，绝不写生产日志路径；唯一例外是 HOME 不可解析的回退用例，
它按实现写 `/tmp/.pi/agent/logs/codegraph-go.log`，并在结束时清理自己创建的文件与空目录。
真实加载演练亦覆盖 HOME，PiG 自身通过 `../bin/pig-test` 隔离到 `/root/.pig-test`。
一般运行本扩展时，非 DEBUG 诊断路径仍按 TS 使用 HOME 下 `.pi/agent/logs`，**不改成
PIG_HOME**。

## TS → Go 定位

| TS 行段 | 内容 | Go 定位 |
|---|---|---|
| 24–90 | 超时、诊断、stderr 过滤、预算 | `client.go:23` diagLog；`text.go:19` envNumber / `:32` budget |
| 92–196 | 项目标记、环境标记、路径、git/marker 根 | `workdir.go:14` projectMarkers；`workdir.go:23–101` |
| 198–467 | workdir、config、授权决议 | `workdir.go:102–284`；`:128` configItem（YAML 项清洗） |
| 469–643 | 压缩、保代码、清洗、默认参数 | `text.go:83` withToolDefaults；`:138` wantsKeepCode；`:159` compressToolText；`:211` formatCleanText；`:41` numberValue / `:70` clampInt |
| 647–923 | MCP 客户端 | `client.go:94` CodeGraphClient；`:112` start；`:134` doStart；`:369` callTool |
| 925–960 | 已决议根启动、项目提示 | `client.go:445` startClientAt；`workdir.go:286` listProjects |
| 962–1074 | 惰性获取与生命周期 | `extension.go:29` getClient；`:52` sessionStart；`:83` beforeAgentStart；`:110` shutdown |
| 1075–1145 | 三个命令 | `extension.go:118` commandStart；`:140` commandStop；`:151` commandInfo |
| 1148–1237 | 工具返回与完整定义 | `extension.go:189` execute；`:200` Extension；`schema.go:5` toolDefinition |

## 对齐细节与明确差异

- 经主代理授权，入口由任务初版 `package main/init()` 修订为 factory 包；不属于 TS
  功能遗漏。PiG 的 standalone 与 factory module 发现逻辑不同。
- Go 并发通过互斥与每个进程的身份保护状态，避免旧进程退出清理新进程；MCP pending
  用单次响应 channel。惰性获取持锁等待启动，同一轮成功启动只获取一次。
- 非法、NaN、Inf 或超出 int 范围的环境数值安全回退默认；有限小数向下取整。TS 直接
  `Number()` 的特殊数值/小数计时与预算行为不完全同构，此处有意采用 Go 整数安全读取。
- `clampInt` 只把数值当数值：`int`/`int64`/`float32`/`float64` 与 `json.Number` 这类
  有限数值接受，字符串（含 `"42"`）从不解析成数字，非有限值回退默认。夹逼在浮点域先做
  再转 `int`，因此 `1e30` 饱和到 `hard`，不会让 `int(math.Floor(1e30))` 溢出翻负（先转
  int 再夹的实现会把 `1e30` 退成默认值）。`explore` 的 `max=0` 判定共用同一「数值」定义，
  `json.Number("0")` 也与 `float64(0)` 一样按「服务端自决」删掉 max。
- YAML 项清洗保留 TS 的空白集与操作顺序：`item.trim()` 用 JS 空白（含 BOM，不含
  `U+0085`），随后先剥首尾引号、再剥行尾注释。因此 `'/a' # c`、`"/root" # comment` 按
  TS 得到 `/a'`、`/root"`（而非理想化的 `/a`、`/root`），`"/root"` 才得到 `/root`；
  尾引号按整项末尾判定，注释里以引号结尾也会被先剥掉（`"a' # c"` → `a'`）。轻量解析
  不是完整 YAML，真正安全校验仍在服务器。
- 显式 `skipCode:null` / `includeCode:null` 保留，不等同缺字段；与 TS 的 undefined
  默认分支一致。explore 的 `max=0` 不强塞默认，由服务器按体量决定。
- 引用清理 `^\s*>\s?` 可吞引用前空行；BOM 可阻止早期标题匹配，最终 trim 才去掉。
  keepCode 也仍去行内反引号；小 line cap 的最低头尾可能重叠。这些均照 TS 保留。
- Go 项目名按字典序排列（任务指定）；TS 使用 localeCompare。MCP JSON 对象键序与
  HTML 转义可能不同，服务器按 JSON 字段解码，协议语义相同。
- stdio 单行读取上限为 64 MiB（Go scanner 的防护边界）；超出结束读取。
- 当前进程实现使用 Unix SIGTERM，验证平台为 Linux；不宣称 Windows 支持。

## 测试与验证

在本目录执行：

```sh
gofmt -l .
go build ./...
go vet ./...
go test -count=1 -race -v ./...
```

本轮 worker 结果：前三项退出 0，gofmt 输出为空；race 测试退出 0，**1.376s**，
**23 个顶层 + 38 个子测试全部 PASS，0 DATA RACE**。额外覆盖率检查
`go test -count=1 -coverprofile=/tmp/codegraph-go-worker.cover ./...` 退出 0，
0.232s，**94.7% statements**；不宣称 100% 语句覆盖。未覆盖主要是底层 pipe/写入/日志
失败、部分系统路径与 SDK 错误边界，不为凑覆盖率增加故障注入用例。

测试覆盖全部 action 默认分支、`json.Number` 数值夹逼与 `1e30` 饱和（含 `int(math.Floor(1e30))`
溢出的回归）、路径与 config 决议、js-trim 的 YAML 项清洗（BOM 与引号/注释顺序）、
授权拒绝、UTF-16 截断、清洗、握手、多请求并发、16 路启动单 spawn、20 路惰性获取、
M4 决议冻结、断连恢复、超时/业务错误/一次重试、首次启动失败不重试与随后恢复、
stderr carry 与环缓冲、HOME 不可解析的 `/tmp` 回退、事件及 3 个命令。

MCP 台架为真实测试子进程（`TestMCPProcess`），使用实际 stdin/stdout/stderr/timer；
无需真实索引服务器或 provider。SDK 台架调用实际 factory 注册的事件/命令，通过
**4 字节大端长度 + JSON** 帧交互，不与 MCP 的 newline JSON 混用。

初次测试失败及修复证据：
- `TestSDKLifecycleRegistration` 初次报 `invalid character '\x00' looking for beginning
  of value`：测试误用逐行 JSON 读取 SDK length-prefix。改台架后 PASS。
- `TestCleanText` 初次报 `JS trim/order`：引用正则吞空行，断言误保留空行。对照 TS 修正
  预期，不改清洗实现，后续 PASS。
- 初次 PiG 加载退出 1，`Go standalone ... must select its exact module root .../go`；
  授权改 factory 后成功，未改父级 go.mod/go.work。

R2 两项行为回归（先 FAIL 后 PASS）：
- `TestDefaults`：`clampInt` 旧实现不认 `json.Number`，把它当非数值退回默认。补断言后先
  FAIL（`clampInt(json.Number("4.9")) = 7 want 4`、`(json.Number("42")) = 7 want 12`、
  `(json.Number("1e30")) = 7 want 12`；`files max=json.Number("300")` 得 `100`；
  `explore max=json.Number("0")` 未删 max），改 `numberValue` 后 PASS。
- `TestConfigParser/js_trim_before_quotes`：旧实现用 Go `TrimSpace`，前导 BOM 未被剥，
  项成 `\ufeff"/root"`。换成 JS 空白集后先 FAIL（`got ["\ufeff\"/root"] want ["/root"]`）
  后 PASS；已有 `quotes before comments` / 新增 `quoted with comment`、`quoted only`
  钉住 TS 的「先引号后注释」顺序（`'/a' # c` → `/a'`、`"/root" # comment` → `/root"`、
  `"/root"` → `/root`）。

### PiG 实际加载验证

从 `go/` 使用 `./bin/pig-test -e ./codegraph-go`，附加
`--no-extensions --no-mcp --no-skills --no-prompt-templates --no-themes
--no-context-files --no-session --offline --mode rpc`；stdin 发送
`{"type":"get_commands","id":"cg-load"}` 后 EOF。临时 config 授权本组件，
`CODEGRAPH_GO_WORKDIR` 指向本组件，`CODEGRAPH_GO_BIN` 指向不存在的临时路径。
不请求模型、不启动真实 MCP 服务，不修改生产配置。

实测退出 **0**，耗时 **6.787s**：

```text
codegraph-go 可用（按需）：/root/workspace/pi-extensions/go/codegraph-go（CODEGRAPH_GO_WORKDIR）
get_commands success=true
codegraph-start — 启动 codegraph-go
codegraph-stop  — 停止 codegraph-go
codegraph-info  — 查看 codegraph 工作目录与上限
```

证明 factory 编译加载、session_start 回调、命令注册与不立即 spawn。真实 server 查询、
模型生成工具调用不在本次加载演练范围；由真实 MCP subprocess 单测验证桥接与重试。
worker 证据不替代 verifier/reviewer 独立验收。

## 交付边界

本组件实现 1287 行、测试 1060 行（不计 README/go.mod），测试未超过实现 3 倍。
构建残留二进制已删除，factory `go build ./...` 不再在本目录输出 standalone 文件。
未部署、未 commit/push；生产 TS 与宿主 codegraph-go 保持不变。
