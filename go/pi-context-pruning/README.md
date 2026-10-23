# pi-context-pruning (Go 移植版)

本扩展是原 TS 版 `pi-context-pruning`（[dorokuma/pi-context-pruning](https://github.com/dorokuma/pi-context-pruning)）向 PiG Go 扩展体系的行为等价移植实现。

## 背景与设计初衷

在长时间或多轮对话中，工具输出（如大型文件读取、命令执行输出等）会迅速消耗上下文窗口和 Token 配额。
`pi-context-pruning` 实现了非破坏性、前瞻性的工具输出剪枝：
1. **倒序扫描**：从最新消息逆序向前扫描，保持最近两轮用户交互（`protectedTurns = 2`）不受剪枝影响；
2. **预算保护**：为较早的工具输出保留指定 Token 预算（`pruneProtect = 40,000`）；
3. **阈值触发**：仅当可剪枝 Token 超过阈值（`pruneMinimum = 20,000`）时才真正执行剪枝；
4. **截断边界**：遇到历史压缩摘要（`compactionSummary`）时自动停止扫描；
5. **错误保护**：出错的工具调用结果（`isError == true`）默认永久保留；
6. **交互控制**：提供 `/prune`（强制单次剪枝）、`/prune-toggle`（开关）、`/prune-stats`（统计）和 `/prune-config`（配置查看）四个斜杠命令。

## TS → Go 行为对照与三层映射

| 层次 / 职责 | TS 原版 (`pi-context-pruning`) | Go 版 (`pi-context-pruning/`) | 行为对齐说明 |
|---|---|---|---|
| **扩展入口** | `extensions/context-pruning/index.ts` | `extension.go:Extension()` | 导出 `func Extension() *sdk.Extension`，通过 PiG factory 机制注册 |
| **配置层** | `extensions/context-pruning/config.ts` | `config.go` | 实现 `DEFAULT_CONFIG`、`ValidInteger`、`ValidStrings`、`MergeConfig`、`LoadConfig` |
| **算法层** | `extensions/context-pruning/pruner.ts` | `pruner.go` | 实现 `EmptyStats`、`IsPrunable`、`PruneMarker`、`PruneToolOutputs`、`PruneToolOutputsAny` |
| **类型定义** | `extensions/context-pruning/types.ts` | `types.go` | 对齐 `PruneConfig`、`PruneStats`、`ContextPruningSettings` 数据结构 |
| **Token 估计** | `@earendil-works/pi-coding-agent: estimateTokens` | `sdk.EstimateTokens(map[string]any)` | 采用 PiG SDK 内置的 `sdk.EstimateTokens`（`pigsdk/compaction_estimate.go:12`），UTF-16 长度与 chars/4 算法 100% 对齐 |
| **事件注册** | `pi.on("session_start", ...)`<br>`pi.on("context", ...)` | `e.OnEvent(sdk.EventSessionStart, ...)`<br>`e.OnEvent(sdk.EventContext, ...)` | `session_start` 重置会话统计与配置；`context` 实施剪枝改写并更新状态栏 |
| **命令注册** | `pi.registerCommand("prune", ...)`<br>`pi.registerCommand("prune-toggle", ...)`<br>`pi.registerCommand("prune-stats", ...)`<br>`pi.registerCommand("prune-config", ...)` | `e.Command("prune", ...)`<br>`e.Command("prune-toggle", ...)`<br>`e.Command("prune-stats", ...)`<br>`e.Command("prune-config", ...)` | 4 个命令的名称、描述与纯逻辑处理与 TS 逐字对齐 |
| **UI 状态与通知** | `ctx.ui.setStatus("context-pruning", ...)`<br>`ctx.ui.notify(msg, "info")` | `ctx.SetStatus("context-pruning", ...)`<br>`ctx.Notify(msg, "info")` | 状态栏文案（含 🔪 前缀、K/M 单位缩写）与通知文案行为一致。其中 Token 格式化通过有理数运算显式对齐 ECMA-262 `toFixed(1)` half-up 舍入；数字千分位按标准环境逗号分隔对齐 `toLocaleString()`，避免不同运行环境 locale 差异 |

## 与 TS 的细微差异

1. **不可变性保障 (Immutability)**：
   - TS 版在剪枝时对 `messages[candidate.index].content` 直接执行了原位对象属性赋值；
   - Go 版在 `PruneToolOutputs` 中保持原切片不可变，构建了浅拷贝切片并将被剪枝的消息复制后再替换 `content`，避免并发或重试时引起上游数据竞态。
2. **多源配置探测与隔离支持**：
   - TS 版固定从 `homedir()/.pi/agent/settings.json` 与 `cwd/.pi/settings.json` 读取文件；
   - Go 版在此基础上增加了对 `PIG_HOME` 环境变量的感知（优先查找 `$PIG_HOME/agent/settings.json`），完美支持隔离环境与原生目录；并在 `session_start` 事件触发时，若 host 提供了 `ctx.GetSettings()`，会自动进行增量合并。
3. **数值类型容错**：
   - Go 版 `ValidInteger` 不仅支持 `float64`（JSON 反序列化默认数值类型），还兼容原生 Go 整数类型（`int`, `int64`, `uint` 等），便于直接在代码中构造测试。
4. **大数值饱和防溢出 (Overflow Cap)**：
   - TS 版所有数字均为 IEEE 754 双精度浮点，`Number.isInteger(1e19)` 视为合法大整数；
   - Go 版在 `ValidInteger` 中针对超出 `math.MaxInt` 的极大数值（如 `1e19`）自动饱和为 `math.MaxInt`，防止在 64 位整数转换中溢出为负数（避免将「恒不剪」翻转为「恒可剪」），且不产生 panic，行为与 TS 等效。
5. **空工作区跳过项目配置 (cwd="")**：
   - `LoadConfig(cwd)` 在 `cwd` 为空字符串时跳过项目配置读取。该差异仅存在于纯单元测试等直调路径，扩展正常运行时始终由 host 上下文提供 `ctx.Cwd()` 或由 `os.Getwd()` 兜底，不影响实际功能。
6. **Wire 消息非 Map 容错**：
   - `PruneToolOutputsAny` 对非 `map[string]any` 类型的元素进行了空 map 兜底转换。因 wire 协议上每条会话消息恒为 JSON 对象，此分支仅为防御性兜底的理论路径。

## PiG SDK 现状与缺口分析

1. **配置读取机制 (Settings Replication)**：
   - *依据*：`/root/workspace/ref/PiG/coding/extension/host/subprocess/ui_bridge.go:277-278` 与 `pigsdk/settings.go:17-35`；
   - *说明*：PiG SDK 提供了 `(Context).GetSettings() (Settings, error)`，其数据来源于 host 向扩展进程单向广播的状态同步帧（`applyReplicatedState`）。在扩展启动或非受管独立会话中，若 host 尚未推送 settings，`GetSettings()` 将返回错误。因此 Go 移植版保留了独立的文件读取逻辑（`LoadConfig`），作为可靠的基底保障。
2. **非交互 CLI 模式下的 UI 状态栏**：
   - *依据*：`/root/workspace/ref/PiG/coding/extension/host/subprocess/ui_bridge.go:1356-1367` 与 `pigsdk/context.go:408-415`；
   - *说明*：在命令行单次执行模式（如 `./bin/pig-test -e ... -p "..."`）下，PiG 以后台非 TUI 流式模式运行，host 使用 `extension.NoopUIContext`，在 `ui_bridge.go:1356-1367` 对 `ui.setStatus` 与 `ui.notify` 等直接 no-op 忽略，并不向终端屏显呈现，这是 PiG 单次批处理模式的预期行为。交互式 TUI 会话下则完全正常展示。

## 如何运行测试

在 `pi-context-pruning/` 目录下执行：

```bash
# 1. 静态检查
go vet ./...

# 2. 编译组件
go build ./...

# 3. 运行全量单元测试（包含全部 TS 对照用例与 Go 专项测试）
go test -count=1 -v ./...
```

测试覆盖说明：
- `config_test.go`：TS 原版 `test/config.test.ts` 全部用例（全局与项目配置合并顺序、非法值忽略保留有效值）、边界类型校验及 `1e19` 大数值防溢出截断测试；
- `pruner_test.go`：TS 原版 `test/pruner.test.ts` 全部 8 个用例（最低阈值、保护预算、自定义工具默认可剪枝、错误输出保护、最新 2 轮用户 turn 保护、compactionSummary 截断反向扫描、0 minimum 预算保持、黑白名单控制）、`PruneMarker` 逐字断言、`FormatWithCommas`、以及 4 个边界用例（`protectedTurns=0`、`pruneProtect` 恰好等于当前用量、`isError` 缺省、连续多轮 >2 保护窗口）；
- `extension_test.go`：可测化命令处理器纯逻辑测试（`/prune`、`/prune-toggle`、`/prune-stats`、`/prune-config`）、对齐 TS `toFixed(1)` 的 `FormatTokens` 对拍测试、`session_start` 重置逻辑、`context` 禁用态下的 `/prune` 强剪与 force 消费测试（含空消息列表消费与 minimum=0 行为）、以及 `context` 事件改写前后的数据与状态对比测试。

## 如何在隔离 PiG 环境中加载

使用项目根目录的隔离脚本 `./bin/pig-test` 进行加载验证（写死 `PIG_HOME=/root/.pig-test`）：

```bash
cd /root/workspace/pi-extensions/go
./bin/pig-test -e ./pi-context-pruning -p "回复 OK"
```
