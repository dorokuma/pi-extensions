# pi-env (Go 移植版)

本扩展是原 TS 版 `pi-env`（npm 包 `pi-env@1.0.1`，源码位于 `~/.pi/agent/npm/node_modules/pi-env/extensions/env.ts`，本机路径示例：`/root/.pi/agent/npm/node_modules/pi-env/extensions/env.ts`，共 237 行）向 PiG Go 扩展体系的行为等价移植实现。

## 背景与设计初衷

在 Pi 生态中，`pi-env` 扩展用于从 `settings.json` 中读取 `"env"` 配置并注入到进程环境变量中：
- 遵循 Pi 的 settings 继承层级：项目级配置（`.pi/settings.json`）覆盖全局配置（`~/.pi/agent/settings.json`，本机路径示例：`/root/.pi/agent/settings.json`），两者合并生效。
- 支持 `$VAR` 和 `${VAR}` 变量插值引用已有环境变量，`$$` 转义为字面量 `$`。
- 仅支持标量值（string、number、boolean），自动转换为字符串；非法嵌套对象或数组将被忽略并产生警告。
- 注册 `customType="pi-env"` 的消息渲染器，将格式化的环境信息呈现到终端。
- 监听 `context` 事件，将 `customType="pi-env"` 类型的自定义消息从 LLM 上下文中剔除，防止敏感环境变量泄漏给大模型。
- 注册 `/env` 命令供用户随时查看当前生效的环境变量报告。
- 在 `session_start` 会话启动时，对非法标量、未解析变量或覆盖宿主原有变量发出告警。

## 与原 TS 版的行为对照

| 行为点 | 原 TS 版 (`env.ts` 行号) | Go 版 (`pi-env/extension.go`) | 说明 |
|---|---|---|---|
| **扩展入口** | L177: `export default function (pi: ExtensionAPI)` | `func Extension() *sdk.Extension` | 符合 PiG 扩展工厂契约 |
| **启动立即生效** | L179: `const { ... } = applyEnv(process.cwd());` | `Extension()` 初始化时调用 `ApplyEnv(cwd)` | 在扩展初始化时完成环境解析与子进程环境注入 |
| **Settings 读取与合并** | L71-L76: `SettingsManager.create(cwd)` 并合并 `globalEnvRaw` 与 `projectEnvRaw` | `LoadOrderedSettings(cwd)` + `ApplyEnvExplicit` | 严格模式只认顶层 "env" 键，无 env 零注入；项目级同名配置严格覆盖全局配置，保持声明顺序 |
| **键序保序性** | L78-L79, L100, L122, L159-L175: `Object.keys/entries` 保序 | `OrderedEnv` (L196-L270) + `ParseOrderedSettingsJSON` / `ParseOrderedEnvJSON` (L272-L336) | 保序解析与流式求值，不使用 map 字母排序 |
| **标量判定与类型转换** | L61-L63, L102, L124: `isScalar(value)` 与 `String(value)` | `IsScalar(value)` + `ScalarToString(value)` | 支持 string、bool、int/int64、float64、json.Number；对象与数组返回 false |
| **非标量非法配置告警** | L78, L218-L220: 收集 `nonScalarKeys` 并在 `session_start` 告警 | 收集 `NonScalarKeys` 并在 `session_start` 格式化告警 | 忽略非法结构体/数组，保留警告字段名（保持书写序） |
| **Reload 状态清理与追踪** | L82-L91, L145-L154: `_PI_EXT_ENV_KEYS` 与 `_PI_EXT_ENV_OVERRIDES` | `ApplyEnvExplicit` 读写对应 TrackerKey 与 OverridesKey | 清理上一次配置但本次被删除的环境变量；保持对系统自带变量被覆盖的持续跟踪 |
| **变量插值替换** | L43-L53: `resolveValue(value)` 正则 `/\$\$ \| \$\{([A-Za-z_][A-Za-z0-9_]*)\} \| \$([A-Za-z_][A-Za-z0-9_]*)/g` | `ResolveValue(value, lookup)` | 支持 `$VAR`、`${VAR}`、`$$` 转义；未定义变量解析为空并记入 `unresolvedVars` |
| **系统变量覆盖检测** | L106-L110, L127-L135: 判定原有变量或上期 overrides | `ApplyEnvExplicit` 中对 global 与 project 分别做精确判定 | 准确记录被本扩展改写的系统变量名 |
| **消息渲染器** | L182-L191: `pi.registerMessageRenderer("pi-env", ...)` | `e.MessageRenderer("pi-env", MessageRendererHandler)` | 解析 string 或多 part 文本并在终端按行切分输出 |
| **LLM Context 消息剔除** | L194-L198: `pi.on("context", async (event) => ...)` | `e.OnEvent(sdk.EventContext, ...)` | 严格过滤 `role=="custom" && customType=="pi-env"` 消息，保留其它正常消息 |
| **`/env` 命令注册** | L201-L211: `pi.registerCommand("env", ...)` | `e.Command("env", ...)` | 动态读取当前 `ctx.Cwd()` 重新计算并发送渲染后的环境变量报表（保持书写序） |
| **会话启动警告提示** | L214-L236: `pi.on("session_start", ...)` | `e.OnEvent(sdk.EventSessionStart, ...)` | 若 `ctx.HasUI()` 为 true 且存在警告，发送带主题色的告警消息，并调用 `ctx.Notify` |

## 与 TS 版本的差异分析

1. **键序（书写顺序保持）与兄弟变量引用**：
   - **TS 原版**：TS 版使用 V8 的 `Object.keys` 和 `Object.entries`，严格保持 `settings.json` 中键的书写声明顺序（insertion order）。
   - **Go 移植初版问题**：由于 Go 原生 `map` 无序，初版移植曾使用 `sort.Strings` 对键进行字母序重排。这不仅导致 `/env` 报告行序和 `session_start` 告警行序与 TS 原版不同，而且在**同 scope 内兄弟键互相 `$VAR` 引用**时会导致严重求值偏差。例如配置 `{"ZPATH":"$ABASE/x","ABASE":"/opt"}`，在 TS 中先解析 `ZPATH`（此时 `ABASE` 尚未定义，求得 `"/x"` 并告警 `ABASE` 未解析），而在字母序排序后先求 `ABASE`，导致 `ZPATH` 错误求得 `"/opt/x"`。
   - **重构修法**：本组件现已通过 `OrderedEnv` 结构（L196-L270）与 `json.Decoder` 逐 token 流式有序解析（`ParseOrderedSettingsJSON` / `ParseOrderedEnvJSON`，L272-L336），完全保留了 settings.json 的键书写顺序，消除了由于排序引入的语义偏差。同时严格限制只有 settings.json 中明确存在顶层 `"env"` 键时才进行提取，缺失 `"env"` 时零注入。
2. **Project 段覆盖判定语义（存在性 vs Falsy）**：
   - **TS 原版**：TS 源码（`env.ts:131`）在判定 project 段是否覆盖了宿主原有变量时，写为 `!globalEnvRaw?.[key]`。由于 JS 可选链与 falsy 特性，若全局配置了 `""`、`false` 或 `0` 等 falsy值，TS 会误将其判定为“未在 global 中配置”，从而误判为覆盖了原有环境变量并触发虚假警告。
   - **Go 移植版**：Go 版采用明确的键存在性判定 `_, inGlobal := globalEnv.Get(key)`。这是经过考量的防御性修正，避免合法的零值或空串全局配置在被项目级覆盖时触发误告警。
3. **Context 消息过滤处理（非 map 元素）**：
   - **TS 原版**：TS 的 `filter` 保留不符合条件的任意类型消息（即非对象也保留）。
   - **Go 移植版**：Go 版在类型断言 `m, ok := item.(map[string]any)` 失败时执行 `continue` 跳过。由于在 PiG 的 JSON-RPC 消息协议 wire 上，消息项恒为 JSON 对象，此边界差异在实际运行时等价。
4. **消息渲染器契约**：
   - **TS 原版**：TS 版使用 `pi.registerMessageRenderer` 返回 `new Text(text, 0, 0)`（pi-tui Component 实例）。
   - **Go 移植版**：PiG Go SDK 的 `e.MessageRenderer` 接受 `RendererFunc`，返回终端按行切分的 `[]string`，Go 版将提取到的纯文本通过 `strings.Split(text, "\n")` 适配输出。
5. **SessionStart 通知机制**：
   - **TS 原版**：仅当 `ctx.hasUI` 为真时，构造带主题色文本并通过 `pi.sendMessage` 注入会话列表展示给用户。
   - **Go 移植版**：不仅完整还原了主题色告警并使用 `ctx.SendMessage` 发送至界面，还调用了 PiG Go SDK 的 `ctx.Notify`（level 设为 `"warning"`），同时兼容会话流消息与弹窗/状态通知。

## PiG 架构缺口定论（关键限制与运行模式界定）

在 Pi 原生架构中，扩展作为主 Node.js 进程的一部分在同进程内执行，`process.env[key] = resolved` 会直接改写 Node.js 主进程环境，从而直接传递给之后派生的一切子进程（如 bash 工具、各种 CLI 子命令）。

而在 PiG 架构中，需根据扩展的加载方式区分：

1. **以独立子进程方式加载（subprocess 模式，默认）**：
   - **独立子进程沙箱**：Go 扩展作为独立子进程被宿主派生执行，扩展内调用的 `os.Setenv` 受操作系统进程隔离保护，**仅能影响扩展进程自身**，完全无法修改父进程（PiG 宿主）的环境变量。
   - **Host-Call RPC 缺失**：审阅 PiG 宿主 RPC 路由全表（源码见 `coding/extension/host/subprocess/host_calls.go:427-535`），宿主目前向扩展开放的方法覆盖了工具注册、MCP、事件总线、UI 弹窗、VirtualModel 等能力，**全表无任何允许扩展修改宿主进程环境变量的 RPC 接口**。
   - **工具执行环境来源**：PiG 宿主派生 bash 工具及其他子进程时，使用的是宿主自身的 `os.Environ()`，扩展子进程注入的环境变量无法渗透回宿主。在此模式下，本组件对宿主以及 bash 工具的环境变量注入在物理上不生效。
2. **以同进程融合方式加载（fused / in-process 模式，D31）**：
   - 审阅 PiG 宿主加载源码（见 `coding/extension/host/subprocess/host.go:3106-3113`，`connectExt`），PiG 设计中支持通过内存管道直接服务的同进程融合扩展模式。
   - 源码注释载明：`"A fused extension's factory runs in this process, so this process's environment is the extension process's: it gets PIG_EXT_HOST_INFO as a spawned extension process does, before the factory runs. Later children of this process inherit the entry."`
   - 在 fused 模式下，扩展直接运行在宿主进程空间内，扩展执行 `os.Setenv` 会直接改写宿主进程环境，此时宿主后续派生的子进程（包括 bash 工具）即可正常继承这些注入的环境变量。

**定论**：上述「环境变量无法渗透宿主与工具」的结论**仅对以独立子进程方式加载时成立**；在 fused 同进程模式下 `os.Setenv` 会直接影响宿主。无论在何种加载模式下，本组件均具备完整的核心功能价值：
- settings.json 中 `"env"` 的统一严格解析（无 `"env"` 时零注入）、验证与变量插值展开；
- 通过 `/env` 斜杠命令在终端呈现保持书写序的格式化环境变量报表；
- 拦截并防止敏感配置泄露给 LLM 的 `context` 消息安全剔除；
- 在 `session_start` 会话启动时，对非法标量、缺失引用或变量覆盖发出终端告警提示。

## Module 形态

- Module 路径：`github.com/dorokuma/pi-extensions/go/pi-env`
- Go 版本：`go 1.26`
- 依赖声明：
  ```go
  require github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0
  ```
- 严格禁止在 `go.mod` 中使用 `replace` 指令。本地开发与分析通过根目录 `go/go.work` 解析至 `go/pigsdk`。

## 本地测试与覆盖情况

在 `/root/workspace/pi-extensions/go/pi-env` 目录下执行：

```bash
# 编译检查
go build ./...

# 静态分析
go vet ./...

# 运行对照测试（带详细输出）
go test -count=1 -v ./...
```

### 测试覆盖范围与已知局限说明

测试用例对 TS 原版的核心功能分支进行了系统性验证，但在如下边界上存在已知局限与差异设计：
1. **键序保持**：本轮已补全报告行序、告警行序、兄弟键声明引用反例及 global/project 双段顺序测试。
2. **跨引用插值**：当前插值实现遵循 TS 原版的单次正则替换语义，未对复杂图状相互引用的循环依赖进行拓扑展开。
3. **Project 段覆盖判定**：TS 的 `!globalEnvRaw?.[key]` 会将 falsy 值判定为未定义，Go 版采用键存在性检查以避免误告警（已知有意差异）。
4. **Context 消息过滤**：Go 版在消息非 `map[string]any` 时直接跳过（wire 协议恒为对象）。

测试用例列表：
1. `TestResolveValue`：测试 `$VAR`、`${VAR}`、`$$` 转义、未定义变量替换为空并记录缺失、多变量、已定义空变量、非标识符字符保留等。
2. `TestResolveValue_SelfReference`：测试变量自身引用（已存在取旧值，不存在记缺失）。
3. `TestExtractEnv`：测试各种合法及非法 `settings.json` 中 `"env"` 的形态提取。
4. `TestIsScalarAndScalarToString`：覆盖字符串、布尔、整型、浮点型、json.Number 等标量，以及非标量对象的过滤与字符串转换。
5. `TestApplyEnvExplicit_MergePriority`：覆盖项目配置覆盖全局同名配置、各自独立配置的合并保留。
6. `TestApplyEnvExplicit_NonScalarIgnored`：覆盖非标量嵌套对象、数组的忽略与告警字段收集。
7. `TestApplyEnvExplicit_ReloadStaleKeyCleanup`：覆盖 reload 场景下通过 `_PI_EXT_ENV_KEYS` 清理已删除配置的环境变量。
8. `TestApplyEnvExplicit_OverridesTracking`：覆盖原有系统环境变量被覆盖时的追踪及 `_PI_EXT_ENV_OVERRIDES` 的维护。
9. `TestFormatReport`：测试 `/env` 命令在有配置与无配置时的文本报表渲染。
10. `TestBuildSessionStartWarnings`：测试 session_start 告警文本的构建与去重。
11. `TestFilterContextMessages`：测试混合消息列表中，仅精准剔除 `role=custom && customType=pi-env` 的消息，保留常规对话。
12. `TestRenderMessageText`：测试消息渲染器对普通字符串与结构化 text part 的提取。
13. `TestApplyEnv_RealEnvRollback`：使用 `t.Setenv` 验证真实环境隔离与读取，确保无全局环境变量污染。
14. `TestExtensionSmoke`：验证 `Extension()` 工厂函数的正常实例化（采用 hermetic 隔离目录）。
15. `TestKeyOrder_ReportFormatOrder`：验证 `/env` 报表严格保持 key 的书写声明行序。
16. `TestKeyOrder_WarningOrder`：验证告警项严格保持 key 的书写声明行序。
17. `TestKeyOrder_SiblingReferenceCounterexample`：验证同 scope 内兄弟变量引用的求值顺序，反例证明 ZPATH 在 ABASE 前时取得空值并告警，而非字母排序后的覆盖值。
18. `TestKeyOrder_GlobalAndProjectSections`：验证 global 与 project 两段各自保持书写序，且两段式覆盖语义正确。
19. `TestStrictSettings_NoEnvKeyZeroInjection`：验证 settings.json 完全无 env 键时，严格模式下进程 env 零变化、报告为空且无非标量告警。
20. `TestStrictSettings_EmptyAndNullEnvKeys`：验证 `"env":{}` 与 `"env":null` 时均返回 nil 并保持零注入。
21. `TestStrictSettings_OnlyEnvInjectedThemeExcluded`：验证仅注入 env 下键值，顶层非 env 配置（如 theme）绝不注入。
22. `TestStrictSettings_ExistingBehaviorPreserved`：验证包含兄弟变量引用与端口号的保序解析不受严格模式影响。

## 如何在隔离 PiG 环境中加载

使用根目录下的隔离测试启动器 `./bin/pig-test`（隔离运行于 `$PIG_TEST_HOME`，本机路径示例：`/root/.pig-test`）：

```bash
cd /root/workspace/pi-extensions/go
./bin/pig-test -e ./pi-env -p "回复 OK"
```

