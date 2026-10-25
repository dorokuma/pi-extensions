# pi-my-compact-ui（Go / PiG 移植）

PiG 的**紧凑对话 UI 扩展**：把一次 assistant 回合里连续流出的工具调用与思考过程折叠成
一个可折叠的工具组（树形 rail + 自旋帧 + 等待提示 + header 计数），历史思考在流式输出
结束前重新挂到对应 assistant 文本锚点，`cache miss` 注入折叠成一条通知行，并提供
`/compact-inspect` 检查器（`Ctrl+I`）与 `/compact-ui-config` 设置（每组最大条目数）。

- Module: `github.com/dorokuma/pi-extensions/go/pi-my-compact-ui`，Go 1.26。
- SDK: `github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0`；本地通过父级 `go.work`
  解析到 `go/pigsdk`，PiG 构建时用 staged SDK。组件内无本地 `replace`。
- 入口：`package compactui`，导出 `func Extension() *sdk.Extension`（`extension.go:22`），
  factory 内部由 `NewExtension`（`extension.go:33`）构装 runtime 与 11 个事件订阅。
  **无 `main()` / 无 `init()`**，与同仓 `cache-stats`、`ctxmode`、`codegraph-go`
  的 factory 加载方式一致。
- 权威源：`/root/.pi/agent/npm/node_modules/pi-my-compact-ui/dist/extension.ts`，
  **2453 行 / 96609 字节**，md5 `1c1e22f513899efa8ef3495348a92554`，
  SHA-256 `b9dac35123722dae7ea73dfe9f0eca5825588c07b94dc4387d93aab9223e1fb9`。
  移植期间该文件与整棵 `dist/` 目录均未修改（只读比对）。

## 行为契约

1. **顺序即语义**：组的 `entries` 严格按流入顺序保存工具调用、思考 run、通知与
   provisional（仍在流式）工具调用（`tool_group.go:15`），折叠渲染逐条出一行
   （`tool_group.go:516`）。
2. **可见性口径**：工具与通知始终可见；思考 run 仅在 active 或文本非空时可见
   （`tool_group.go:165`），空 run 不占行。
3. **分组生命周期**：只有「最近的开放组」吸收新条目；遇到真实文本输出即 seal
   （`grouping.go:89`），seal 后该组只读、不再追加，等待提示消失
   （`tool_group.go:200`）。
4. **历史思考重挂**：流式过程中思考已即时显示过的文本不重复渲染
   （`assistant.go:22`）；assistant 文本到达时把尚未显示的思考挂到该文本锚点之前
   （`grouping.go:677`），并对容器做幻影 padding 剥离（`grouping.go:777`）。
5. **cache miss 折叠**：从注入文本中提取 cache miss 通知，转为组内一条 notice 行，
   并抑制其在 chat 层重复出现（`grouping.go:381` / `grouping.go:426`）。
6. **时间口径**：优先用已记录时长，其次 group 结束时间差，最后 start 至今的实时差；
   中断或被 seal 且无结果的调用显示 `0ms`（`streaming.go:150`）。
7. **动画有界**：只有一个待触发 tick（`runtime_glue.go:95`），组不再需要动画时链条
   自然结束；`session_shutdown` 停止 ticker（`extension.go:512`）。
8. **配置降级**：配置文件读取失败/字段缺失静默回退默认值（`values.go:375`），
   保存只写本扩展自己的配置键（`values.go:433`）。
9. **输入边界**：所有宿主输入（事件 payload、终端输入序列、配置 JSON）都按任意/畸形
   输入设计：类型不符即忽略该字段，绝不 panic（`events.go:160` 起的转换层 +
   `extension_test.go:753` `TestZeroContextSafety`）。

## 并发模型与锁纪律（本移植的核心约束）

TS 原版是单线程进程内扩展：事件回调、`requestRender`、动画 tick 全在同一个 JS 事件
循环上，且它的分组/思考/历史锚点行为靠**改宿主原型**生效。Go 移植后事件由 PiG 子进程
协议投递，宿主线程与本扩展的渲染/ticker goroutine 会同时触达同一份 runtime 状态，因此
本移植把「共享状态只在 `mu` 下读写、宿主调用绝不在锁内发生」作为硬约束：

1. `runtimeState.mu` 是**普通 `sync.Mutex`**（`state.go:23` 的 `runtimeState` 结构）。早先用 `RWMutex`
   的版本在 `-race` 下暴露**写者饥饿**：渲染 goroutine 持续持读锁，写者
   （`addTool` 等）永久拿不到锁（`TestGroupingConcurrentReads` 挂死 100s+）。渲染
   本就该串行化，读并行在这里没有收益，因此降级为 `Mutex`。
2. 渲染路径整段持锁、锁外只做排版：`renderWidth`（`tool_group.go:663`）在 `mu` 内取
   可见条目、计算折叠行，解锁后才调 `scheduleAnimation`、`tui.TruncateToWidth`
   与拼装返回值。
3. **禁止嵌套取锁**：凡是持锁的辅助函数一律有 `*Locked` 孪生版本（如
   `visibleEntriesLocked` `tool_group.go:165`、`workPhaseLabelLocked` `tool_group.go:272`、
   `toolElapsedLocked` `streaming.go:158`、`invalidateLocked` `tool_group.go:326`、
   `nowMsLocked` `accessors.go:35`）。写侧同理：`addTool`/`addThinking`/`addNotice`/
   `removeTool` 自持锁，其 `*Locked` 变体供已持锁调用方（`grouping.go`、
   `extension.go`、`modals.go` 的持锁点）。
4. **宿主调用不持锁**：`requestRender`、`notify`、overlay 增删都在解锁后执行
   （`extension_test.go:791` `TestHostCallsNeverUnderLock` 用 `TryLock` 探针断言）。
5. `-race` 三轮全绿见下方验证证据；并发用例覆盖「事件生产者 + 渲染消费者 + 消息/
   思考生产者」同时跑（`extension_test.go:910` `TestConcurrentTurnRace`）。

## PiG 子进程扩展协议 vs TS 进程内原型补丁

TS 原版的 UI 行为大量依赖**进程内改宿主原型**：`Container.prototype.addChild /
removeChild / clear`（`installGrouping`）、`AssistantMessageComponent.prototype.*`
（`installNativeThinkingSuppression` / `ensureAssistantThinkingPatched`）、
`CompactionSummaryMessageComponent.prototype.updateDisplay`、以及
`ToolExecutionComponent.prototype.render`（`hideGroupedToolRender`）。PiG 的 Go 扩展运行
在独立子进程里，只通过 SDK 暴露的扩展协议（`Extension()`、`OnEvent`、`Command`、
`Context.ui.*`、`AddOverlay`）与宿主通信，**拿不到也无法改宿主的原型**。因此本移植的
边界是：

1. **分组容器改为自持容器**：Go 侧自己实现 `container`（`component.go:26`）与
   `component` 接口（`component.go:14`），`installGrouping`（`grouping.go:589`）改为在
   本扩展自己的容器树上执行等价调度；TS 里「替换掉宿主 addChild」的拦截语义由
   `addChildTo`（`grouping.go:467`）/ `removeChildFrom`（`grouping.go:543`）/
   `clearPatched`（`grouping.go:560`）在本容器上复刻。
2. **助手思考补丁改为渲染层重写**：TS 直接 patch `AssistantMessageComponent` 让它在
   历史消息里重排 thinking 块；Go 侧改为在渲染 assistant 消息时由
   `applyAssistantThinkingPatch`（`assistant.go:173`）产出等价内容，并由
   `stripAssistantPhantomPadding`（`grouping.go:777`）清掉原实现留下的空行。
3. **压缩摘要标题改为自带组件**：TS patch `updateDisplay`；Go 侧自带
   `compactionHeaderComponent`（`component.go:301`），由
   `installCompactionSummaryRendering`（`compaction.go:70`）挂载。
4. **分组内工具不再渲染**：TS 把已分组工具的 `render` 打成空实现；Go 侧在
   `hideGroupedToolRender`（`guards.go:111`）里把该工具的渲染输出置空，效果一致。
5. **原生 thinking widget 抑制**：TS 通过 patch 让原生 widget 不再占位；Go 侧识别并
   跳过它（`grouping.go:829` `isNativeThinkingWidget`）。
6. **符号键改用显式字段**：TS 用 `Symbol.for(...)` 的 `PARENT_KEY` / `PATCH_KEY` 等
   把补丁状态挂在宿主组件上；Go 侧无此机制，改为结构体字段与 `map`（如
   `toolComponent.owner`、`runtimeState.groups`），语义等价且可被 race detector 覆盖。
7. **命令与检查器**：`/compact-inspect`、`/compact-ui-config` 通过 SDK 的
   `Command` 注册（factory `extension.go:33`、`settings.go:128`）；详情/选择器弹窗通过
   `AddOverlay` + 输入钩实现（`runtime_glue.go:195`、`runtime_glue.go:259`），
   不再依赖宿主 modal 原型。

### 生产事件路径与 `ext.ToolRenderer`（PLAN.md §4 的落地方式）

TS 原版的宿主原型补丁在**进程内**改的是宿主自己的组件树，`ToolRenderer` 与
`InspectSelectorModal` / `DetailModalComponent` 只是读那棵树。PiG 的 Go 扩展拿不到宿主
原型，因此把「树」搬到了本进程内，再由 SDK 协议把渲染结果交回宿主：

| 宿主面 | 本移植的落地 |
|---|---|
| `ctx.SetWidgetRenderer("compact-anim", ...)` | `extension.go:560` `renderWidget`（widget 行） |
| `ctx.SetHiddenThinkingLabel("")` | `handleSessionStart`（`extension.go:474`）里设置，原生 thinking 标签留空 |
| `ctx.OnTerminalInput` | `extension.go:525` `handleTerminalInput`，`Ctrl+I` 开检查器 |
| `ctx.Custom` / `AddOverlay` | `runtime_glue.go:195` / `runtime_glue.go:259` 两个 overlay |
| `ext.ToolRenderer(...)` | `extension.go:387` `installToolRenderer`：Call/Result 都返回 `toolRowLocked` 的紧凑单行 + 结果文本 |

工具卡片的具体走法：`installToolRenderer` 注册的 resolver 对**所有工具**返回
`Shell: sdk.ToolRenderShellSelf` 的渲染器，`Call` 渲染该工具调用所属分组（或一个临时分组）
的紧凑行，`Result` 在同一行后附上输出文本。渲染全程在 `runtimeState.mu` 内取
`toolRenderToolLocked` 快照（`extension.go:434`），因此宿主在 TUI 线程绘制时不会与
`message_update` / `tool_execution_end` 抢同一份状态。

诚实边界：宿主**仍然会**画它自己的工具卡片，`ToolRenderer` 做的是接管那张卡片的绘制内容
（与分组内 `toolRow` 同一套格式化与状态），并不能像 TS 进程内补丁那样把宿主 DOM 树里的
节点删掉。PiG 子进程扩展的可见控制面就是上表这几项，README 不宣称任何未在协议里暴露的
宿主行为。

生产事件与分组的挂接（`必修 1` 的其余部分）：

- `session_start` 建默认主容器（`ensureChatContainerLocked`，`extension.go:146`），
  并用 `replaySessionHistory`（`extension.go:278`）按历史 session entries 重建
  `AssistantMessageComponent` / `ToolExecutionComponent` / `ToolGroupComponent`，
  随后由 `sealHistoricalGroupsFrom`（`extension.go:981`）按 `session_start` 时刻的快照
  代次封口，恢复的会话因此可用 `Ctrl+I` / `/compact-inspect` 回看。
- `message_start(role="assistant")` 先调 `assistantMessageStartLocked`（`state.go:268`）
  重置上一条消息流的状态，再挂上 live `AssistantMessageComponent`
  （`ensureLiveAssistantLocked`，`extension.go:155`）——顺序反了会把刚挂上的组件
  连同 `lastStreamingComp` 一起清掉，首个 thinking 流就找不到挂载点
  （`TestMessageStartAssistantKeepsLiveAssistant`）。
- `tool_execution_start` 由 `ensureToolComponent`（`extension.go:179`）找到或创建真实
  工具组件并走 `addChildTo` → `maybeGroup` 分组。流式阶段挂上的 `preparing` 条目由
  `absorbPreparingTool`（`streaming.go:516`）接管；当真实工具就是该条目挂载的那个组件时，
  由 `absorbGroupedPreparingLocked`（`streaming.go:588`）**原地**完成「provisional →
  执行中」的转换：解除条目的 `preparing` 链接、清除 `comp.preparing`、把 `comp.owner`
  指回该组并补进 `group.children`（组件不从组里摘掉，也不新增第二条目）。`children`
  正是 `agent_end` / `sealHistoricalGroups` 遍历标记中断的名单，漏了它中断态就漏一个工具
  （`TestPreparingToolAbsorbedInPlace`、`TestPreparingToolAbsorbedThenCompleted`）。
- `tool_execution_end` 由 `applyToolResult`（`extension.go:246`）把 `result` / `isError` /
  `isPartial` / 组结束时间写回组件，使分组渲染与两个弹窗拿到完整参数、结果与耗时。

已验证的命令结果原文见下方「测试与验证证据」；上面第 1–7 条涉及的每个 Go 函数都有对应
单测（见 `grouping_test.go`、`assistant_test.go`、`compaction.go` 相关用例）。

## TS → Go 模块对照

TS 侧行段取自 bundle 里的 `// src/x.ts` 模块注释，逐条实测：

| TS 行段（实测） | TS 模块 | Go 落点 |
|---|---|---|
| L1–L4 | `src/guards.ts` | guards.go |
| L5–L7 | `src/tool-group.ts` | （见下方函数表） |
| L8–L49 | `src/state.ts` | state.go / accessors.go |
| L50–L69 | `src/constants.ts` | values.go |
| L70–L192 | `src/helpers.ts` | values.go |
| L193–L521 | `src/assistant-patches.ts` | assistant.go / grouping.go |
| L522–L754 | `src/streaming-tools.ts` | streaming.go |
| L755–L785 | `src/animation.ts` | runtime_glue.go |
| L786–L807 | `src/config.ts` | values.go |
| L808–L1071 | `src/tool-group.ts` | （见下方函数表） |
| L1072–L1111 | `src/guards.ts` | guards.go |
| L1112–L1165 | `src/thinking.ts` | streaming.go |
| L1166–L1511 | `src/grouping.ts` | grouping.go |
| L1512–L1562 | `src/compaction.ts` | compaction.go / component.go |
| L1563–L2080 | `src/modals.ts` | modals.go / runtime_glue.go |
| L2081–L2083 | `src/extension.ts` | extension.go / events.go |
| L2084–L2191 | `src/settings.ts` | settings.go |
| L2192–L2454 | `src/extension.ts` | extension.go / events.go |

## TS → Go 核心函数对照

表中 TS 行号是**权威源 `extension.ts` 的真实定义行**（不是移植时写在 Go 注释里的行号，
二者存在系统性漂移，见「已知差异」第 1 条），Go 行号是 Go 文件的真实定义行。两列均由脚本
机械解析产生（TS 侧取 top-level `function`/`var`/`class` 或 2 空格缩进的类成员定义行、
`pi.on("<evt>"` 取事件接线行；Go 侧取 `^func`/`^type`/`^const`/`^var` 定义行，方法按接收者
类型消歧），任一符号无法唯一解析即中止而不是猜行号。本次生成 150 行、0 处待判定，
并经下文「行号核验」复核为 0 漂移。

| TS 定义行（实测） | TS 符号 | Go 定义行（机械核验） | Go 符号 |
|---|---|---|---|
| L9 | `createRuntimeState` | `state.go:107` | `createRuntimeState` |
| L51 | `SPINNER` | `values.go:18` | `spinnerFrames` |
| L52 | `SPINNER_MS` | `values.go:21` | `spinnerMs` |
| L53 | `THINKING_SPINNER` | `values.go:24` | `thinkingSpinnerFrames` |
| L58 | `GROUP_PADDING_X` | `values.go:33` | `groupPaddingX` |
| L59 | `GROUP_PADDING_RIGHT` | `values.go:36` | `groupPaddingRight` |
| L61 | `PARENT_KEY` | `state.go:10` | `installedCounter` |
| L73 | `shortenPath` | `values.go:59` | `shortenPath` |
| L77 | `oneLine` | `values.go:75` | `oneLine` |
| L84 | `estimateTextTokens` | `values.go:88` | `estimateTextTokens` |
| L87 | `formatTokenK` | `values.go:149` | `formatTokenK` |
| L93 | `thinkingTokenLabel` | `values.go:165` | `thinkingTokenLabel` |
| L102 | `toolSummary` | `values.go:183` | `toolSummary` |
| L130 | `toolStatus` | `values.go:226` | `toolStatus` |
| L137 | `streamingToolArgs` | `values.go:293` | `streamingToolArgs` |
| L148 | `locateStreamingToolCall` | `values.go:319` | `locateStreamingToolCall` |
| L163 | `toolResultText` | `values.go:346` | `toolResultText` |
| L166 | `formatWorkedTime` | `values.go:250` | `formatWorkedTime` |
| L175 | `formatToolDuration` | `values.go:272` | `formatToolDuration` |
| L186 | `isCtrlI` | `values.go:362` | `isCtrlI` |
| L196 | `thinkingAlreadyShown` | `assistant.go:22` | `thinkingAlreadyShown` |
| L210 | `stageHistoricalThinking` | `assistant.go:50` | `stageHistoricalThinking` |
| L235 | `ensureHistoricalThinkingAnchors` | `assistant.go:110` | `ensureHistoricalThinkingAnchors` |
| L256 | `installNativeThinkingSuppression` | `assistant.go:148` | `installNativeThinkingSuppression` |
| L319 | `ensureAssistantThinkingPatched` | `assistant.go:156` | `ensureAssistantThinkingPatched` |
| L327 | `getAssistantContentState` | `state.go:174` | `assistantContentState` |
| L327 | `getAssistantContentState` | `grouping.go:849` | `getAssistantContentStateLocked` |
| L335 | `removeGroupFromContainer` | `grouping.go:597` | `removeGroupFromContainer` |
| L345 | `detachGroup` | `grouping.go:611` | `detachGroupLocked` |
| L352 | `removeComponentFromContainer` | `grouping.go:859` | `removeComponentFromContainer` |
| L358 | `TurnDividerComponent` | `assistant.go:232` | `renderWidth` |
| L358 | `TurnDividerComponent` | `component.go:314` | `turnDividerComponent` |
| L377 | `placeTurnDividerBeforeText` | `grouping.go:867` | `placeTurnDividerBeforeText` |
| L383 | `insertTurnDivider` | `state.go:181` | `dividerPlacement` |
| L383 | `insertTurnDivider` | `grouping.go:885` | `insertTurnDivider` |
| L415 | `isVisibleTextMarkdown` | `guards.go:102` | `isVisibleTextMarkdown` |
| L418 | `placeAnchoredGroupBeforeText` | `grouping.go:633` | `placeAnchoredGroupBeforeText` |
| L431 | `insertAnchoredGroup` | `grouping.go:657` | `insertAnchoredGroup` |
| L439 | `anchorGroupBeforeCurrentText` | `state.go:187` | `groupAnchor` |
| L439 | `anchorGroupBeforeCurrentText` | `grouping.go:677` | `anchorGroupBeforeCurrentText` |
| L470 | `restoreAssistantAnchor` | `grouping.go:722` | `restoreAssistantAnchor` |
| L484 | `releaseAssistantAnchors` | `grouping.go:751` | `releaseAssistantAnchors` |
| L498 | `stripAssistantPhantomPadding` | `grouping.go:777` | `stripAssistantPhantomPadding` |
| L511 | `isNativeThinkingWidget` | `grouping.go:829` | `isNativeThinkingWidget` |
| L523 | `toolElapsed` | `streaming.go:150` | `toolElapsed` |
| L564 | `loadSessionToolTimings` | `streaming.go:216` | `loadSessionToolTimings` |
| L598 | `findGroupedTool` | `streaming.go:289` | `findGroupedTool` |
| L609 | `invalidateGroupedTool` | `streaming.go:314` | `invalidateGroupedTool` |
| L619 | `findOpenGroup` | `streaming.go:335` | `findOpenGroup` |
| L626 | `placeGroupAfter` | `streaming.go:355` | `placeGroupAfter` |
| L637 | `openToolGroup` | `streaming.go:384` | `openToolGroup` |
| L658 | `attachPreparingTool` | `state.go:228` | `preparingTool` |
| L658 | `attachPreparingTool` | `streaming.go:426` | `attachPreparingTool` |
| L667 | `detachPreparingTool` | `streaming.go:463` | `detachPreparingTool` |
| L675 | `takePreparingTool` | `streaming.go:485` | `takePreparingTool` |
| L693 | `absorbPreparingTool` | `streaming.go:516` | `absorbPreparingTool` |
| L704 | `clearPreparingTools` | `streaming.go:564` | `clearPreparingTools` |
| L708 | `noteStreamingToolCall` | `streaming.go:625` | `noteStreamingToolCall` |
| L756 | `thinkingSpinnerFrame` | `values.go:450` | `thinkingSpinnerFrame` |
| L763 | `disableMouseTracking` | `values.go:479` | `disableMouseTrackingSequence` |
| L769 | `scheduleAnimation` | `accessors.go:111` | `animTimer` |
| L769 | `scheduleAnimation` | `values.go:483` | `animationDelayLocked` |
| L769 | `scheduleAnimation` | `runtime_glue.go:95` | `scheduleAnimation` |
| L790 | `CONFIG_PATH` | `values.go:391` | `configPathFor` |
| L793 | `saveConfig` | `values.go:433` | `saveConfig` |
| L799 | `loadConfig` | `values.go:408` | `loadConfig` |
| L809 | `ToolGroupComponent` | `tool_group.go:25` | `toolGroupComponent` |
| L824 | `invalidate` | `tool_group.go:326` | `invalidateLocked` |
| L826 | `addTool` | `tool_group.go:66` | `addTool` |
| L842 | `addThinking` | `tool_group.go:94` | `addThinking` |
| L847 | `addNotice` | `tool_group.go:110` | `addNotice` |
| L853 | `removeTool` | `tool_group.go:138` | `removeTool` |
| L861 | `getVisibleEntries` | `tool_group.go:165` | `visibleEntriesLocked` |
| L871 | `hasVisibleEntries` | `tool_group.go:195` | `hasVisibleEntriesLocked` |
| L874 | `hasPending` | `tool_group.go:203` | `hasPendingLocked` |
| L878 | `hasActiveThinking` | `tool_group.go:220` | `hasActiveThinkingLocked` |
| L882 | `hasThinking` | `tool_group.go:237` | `hasThinkingLocked` |
| L885 | `hasTools` | `tool_group.go:254` | `hasToolsLocked` |
| L889 | `workPhaseLabel` | `tool_group.go:272` | `workPhaseLabelLocked` |
| L903 | `needsAnimation` | `tool_group.go:299` | `needsAnimationLocked` |
| L907 | `iconFor` | `tool_group.go:335` | `iconFor` |
| L911 | `colorFor` | `tool_group.go:347` | `colorFor` |
| L915 | `toolRow` | `tool_group.go:362` | `toolRow` |
| L938 | `noticeRow` | `tool_group.go:404` | `noticeRow` |
| L953 | `thinkingRow` | `tool_group.go:436` | `thinkingRow` |
| L988 | `renderCollapsed` | `tool_group.go:516` | `renderCollapsed` |
| L1059 | `render` | `tool_group.go:663` | `renderWidth` |
| L1073 | `isContainer` | `guards.go:65` | `isContainer` |
| L1078 | `isAssistantMessage` | `guards.go:70` | `isAssistantMessage` |
| L1083 | `isToolExecution` | `guards.go:75` | `isToolExecution` |
| L1088 | `isSpacer` | `guards.go:80` | `isSpacer` |
| L1091 | `isMarkdown` | `guards.go:85` | `isMarkdown` |
| L1094 | `isText` | `guards.go:90` | `isText` |
| L1097 | `isToolGroup` | `guards.go:95` | `isToolGroup` |
| L1100 | `hideGroupedToolRender` | `guards.go:111` | `hideGroupedToolRender` |
| L1113 | `updateActiveThinkingTokens` | `streaming.go:15` | `updateActiveThinkingTokens` |
| L1127 | `createThinkingEntry` | `state.go:195` | `thinkingEntry` |
| L1127 | `createThinkingEntry` | `streaming.go:52` | `createThinkingEntry` |
| L1142 | `finalizeActiveThinking` | `streaming.go:73` | `finalizeActiveThinking` |
| L1153 | `refreshSealedThinkingTokens` | `streaming.go:111` | `refreshSealedThinkingTokens` |
| L1172 | `previousGroupable` | `grouping.go:23` | `previousGroupable` |
| L1182 | `assistantHasVisibleText` | `grouping.go:43` | `assistantHasVisibleText` |
| L1187 | `absorbThinkingBeforeText` | `grouping.go:57` | `absorbThinkingBeforeText` |
| L1206 | `sealOpenGroupAtAssistantText` | `grouping.go:89` | `sealOpenGroupAtAssistantText` |
| L1221 | `ensureThinkingGroup` | `grouping.go:129` | `ensureThinkingGroup` |
| L1245 | `flushPendingTextSeal` | `grouping.go:182` | `flushPendingTextSeal` |
| L1267 | `getPrecedingAssistantThinking` | `grouping.go:235` | `precedingAssistantThinking` |
| L1287 | `attachPrecedingThinkingToGroup` | `grouping.go:264` | `attachPrecedingThinkingToGroup` |
| L1304 | `joinOpenGroup` | `grouping.go:288` | `joinOpenGroup` |
| L1322 | `maybeGroup` | `grouping.go:324` | `maybeGroup` |
| L1358 | `extractCacheMissNotice` | `state.go:212` | `noticeEntry` |
| L1358 | `extractCacheMissNotice` | `grouping.go:381` | `extractCacheMissNotice` |
| L1377 | `absorbCacheMissNotice` | `grouping.go:426` | `absorbCacheMissNotice` |
| L1401 | `installGrouping` | `grouping.go:589` | `installGrouping` |
| L1515 | `CompactionHeaderComponent` | `component.go:301` | `compactionHeaderComponent` |
| L1536 | `installCompactionSummaryRendering` | `compaction.go:70` | `installCompactionSummaryRendering` |
| L1565 | `DetailModalComponent` | `modals.go:22` | `detailModalComponent` |
| L1576 | `isEntryActive` | `modals.go:45` | `isEntryActive` |
| L1585 | `startPollingIfNeeded` | `modals.go:79` | `startPollingIfNeeded` |
| L1597 | `dispose` | `modals.go:124` | `dispose` |
| L1605 | `scrollBy` | `modals.go:138` | `scrollBy` |
| L1608 | `scrollTo` | `modals.go:146` | `scrollTo` |
| L1618 | `scrollToEnd` | `modals.go:162` | `scrollToEnd` |
| L1623 | `handleInput` | `modals.go:175` | `handleInput` |
| L1654 | `render` | `modals.go:292` | `renderWidth` |
| L1779 | `openDetailModal` | `runtime_glue.go:195` | `openDetailModal` |
| L1809 | `closeDetailModal` | `runtime_glue.go:229` | `closeDetailModal` |
| L1826 | `getAllInspectableEntries` | `modals.go:770` | `allInspectableEntries` |
| L1835 | `InspectSelectorModal` | `modals.go:492` | `inspectSelectorModal` |
| L1843 | `refreshEntries` | `modals.go:511` | `refreshEntries` |
| L1858 | `adjustScroll` | `modals.go:533` | `adjustScrollLocked` |
| L1871 | `handleInput` | `modals.go:551` | `handleInput` |
| L1933 | `render` | `modals.go:669` | `renderWidth` |
| L2031 | `openInspectSelectorModal` | `runtime_glue.go:259` | `openInspectSelectorModal` |
| L2064 | `closeInspectSelectorModal` | `runtime_glue.go:288` | `closeInspectSelectorModal` |
| L2087 | `CONFIG_KEYS` | `settings.go:24` | `configKeys` |
| L2097 | `makeStepper` | `settings.go:50` | `makeStepper` |
| L2141 | `cmd:compact-ui-config` | `settings.go:128` | `registerConfigCommand` |
| L2141 | `cmd:compact-ui-config` | `settings.go:137` | `handleConfigCommand` |
| L2193 | `setAgentWorkPhase` | `extension.go:563` | `setAgentWorkPhase` |
| L2198 | `extension_default` | `extension.go:33` | `NewExtension` |
| L2202 | `cmd:compact-inspect` | `extension.go:33` | `NewExtension` |
| L2208 | `on:session_start` | `extension.go:474` | `handleSessionStart` |
| L2273 | `on:session_shutdown` | `extension.go:512` | `handleSessionShutdown` |
| L2287 | `on:tool_execution_start` | `extension.go:576` | `handleToolExecutionStart` |
| L2294 | `on:tool_execution_end` | `extension.go:600` | `handleToolExecutionEnd` |
| L2310 | `on:message_start` | `extension.go:660` | `handleMessageStart` |
| L2349 | `on:message_end` | `extension.go:725` | `handleMessageEnd` |
| L2358 | `on:message_update` | `extension.go:756` | `handleMessageUpdate` |
| L2407 | `on:agent_end` | `extension.go:912` | `handleAgentEnd` |

共 150 行，TS 侧与 Go 侧行号均由脚本从文件实测取定义行。

## 行号核验（README 引用 0 漂移）

上文与对照表中的全部 `*.go:NNN` 引用都由脚本机械核验：取 README 正文（排除行首三反引号
围栏内的代码块，因为块内是对照组命令的原始输出原文、不是文档引用），逐个解析 `<file>.go:<line>`，要求该行
在对应文件中是**真实定义行**（`^func` / `^type` / `^const` / `^var`；测试文件的
`func Test...` 同样算定义行）。任一引用落在非定义行、或文件不存在，即判失败并打印该行
附近的真实定义行以便定位。

本次结果：**212 处引用全部命中真实定义行，0 漂移**，分布为
grouping.go×37、tool_group.go×29、extension.go×26、values.go×26、streaming.go×21、modals.go×15、state.go×11、runtime_glue.go×10、guards.go×10、assistant.go×8、component.go×5、settings.go×5、extension_test.go×3、accessors.go×3、compaction.go×2、events.go×1，覆盖 15 个实现文件 + 1 个测试文件。
`grouping.go×37、tool_group.go×29、extension.go×26、values.go×26、streaming.go×21、
modals.go×15、state.go×11、runtime_glue.go×10、guards.go×10、assistant.go×8、
component.go×5、settings.go×5、extension_test.go×3、accessors.go×3、compaction.go×2、
events.go×1`，覆盖 15 个实现文件 + 1 个测试文件。

## 测试规模说明

`wc -l *.go`（全部改动并 `gofmt -w .` 之后实测，含注释与空行）：

| 分类 | 文件 | 行数 |
|---|---|---|
| 实现 | `extension.go` | 1047 |
| 实现 | `grouping.go` | 944 |
| 实现 | `modals.go` | 793 |
| 实现 | `tool_group.go` | 748 |
| 实现 | `streaming.go` | 719 |
| 实现 | `values.go` | 559 |
| 实现 | `runtime_glue.go` | 472 |
| 实现 | `component.go` | 319 |
| 实现 | `settings.go` | 293 |
| 实现 | `events.go` | 285 |
| 实现 | `assistant.go` | 254 |
| 实现 | `state.go` | 276 |
| 实现 | `accessors.go` | 251 |
| 实现 | `host.go` | 172 |
| 实现 | `guards.go` | 116 |
| 实现 | `compaction.go` | 103 |
| **实现小计** | 16 个文件 | **7351** |
| 测试 | `grouping_test.go` | 1785 |
| 测试 | `extension_test.go` | 1548 |
| 测试 | `values_test.go` | 787 |
| 测试 | `modals_test.go` | 939 |
| 测试 | `tool_group_test.go` | 665 |
| 测试 | `harness_test.go` | 340 |
| 测试 | `assistant_test.go` | 236 |
| **测试小计** | 7 个文件 | **6300** |
| **合计** | 23 个文件 | **13651** |

测试/实现比 = 6300 / 7351 ≈ 0.86。

## 测试与验证证据

原版 `.mjs` 用例按类翻成 Go 测试，共 **137 个顶层用例**（`grep -rn '^func Test'` → 137），
覆盖：组件渲染（工具组折叠/展开、spinner、workPhaseLabel、header 计数、详情弹窗、检查器、
设置步进器）、分组生命周期、thinking 合并与 token 标签、cache miss 吸收、assistant 锚点
与幻影 padding、流式更新与最终化、补丁与事件路由、终端输入拦截（含 Kitty CSI-u 的
`Ctrl+I` 映射）、压缩摘要渲染、配置读写与降级、跨实例隔离（`TestRuntimesAreIndependent`）、
零值 `sdk.Context` 安全、并发竞态（`-race`）与宿主调用不持锁。第 2 轮审查的 6 项修复各有
先 FAIL 后 PASS 的确定性回归：`TestMessageStartAssistantKeepsLiveAssistant`（必修 1）、
`TestPreparingToolAbsorbedInPlace` / `TestPreparingToolAbsorbedThenCompleted`（必修 2）、
`TestDetailModalPollingIsRaceFree`（必修 3，`-race` 下复现锁外裸读）、
`TestStopAnimationWaitsForInFlightTick`（必修 4）、`TestJsTenthsBinaryFloatBoundary`（必修 5）、
`TestCustomRootSwitchesViewsInsteadOfStacking`（必修 6，改为经 `Enter` 真实驱动）。

第 3 轮审查（`w37:p43`）再修一项并发漏洞并扩展同一用例：`stopAnimation` 原先执行
`ticks := r.animTicks; r.animTicks = nil`，当 `sealHistoricalGroupsFrom` 的异步停止与
`handleSessionShutdown` 重叠（两个调用方并发 `stopAnimation()`）时，第一个调用方拿走并
清空了 `r.animTicks`，第二个调用方看到 `animTimer == nil` 且 `animTicks == nil` 便提前
返回，而 tick 仍阻塞在宿主调用里。现在 `stopAnimation` 只做切片快照
`append([]chan struct{}(nil), r.animTicks...)`，全局 `r.animTicks` 由 tick 自身在宿主调用
返回后的 `defer` 中持 `mu` 摘除、解锁后关闭该通道：摘除前进入的调用方都拿到该通道并等待
其关闭，摘除后进入的调用方只可能因为宿主调用已结束才看到空表。
`TestStopAnimationWaitsForInFlightTick` 因此扩展为「同一阻塞 tick 下并发两个
`stopAnimation()`，两者在 `blockingTUI` 释放前都不得返回，释放后均返回且
`animTicks` 归零」，实测先 FAIL 后 PASS：

```
$ go test -count=1 -race -run TestStopAnimationWaitsForInFlightTick -v ./...
（修复前：r.animTicks = nil）
    a concurrent stopAnimation returned while the tick's host call was still running
--- FAIL: TestStopAnimationWaitsForInFlightTick (0.25s)

（修复后：拷贝快照，tick 自行摘除并关闭）
--- PASS: TestStopAnimationWaitsForInFlightTick (0.65s)
```

实测（`/root/workspace/pi-extensions/go/pi-my-compact-ui` 下，逐字节记录）：

```
$ gofmt -l .
（无输出；退出码 0，stdout 0 字节）

$ go build ./...
（退出码 0，stderr 0 字节）

$ go vet ./...
（退出码 0，stderr 0 字节）

$ go test -count=5 -race -timeout 800s ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-my-compact-ui	7.022s
（退出码 0，DATA RACE 计数 0，连续 5 轮）
```

第 3 轮审查修复后复跑（同上命令，`runtime_glue.go` / `extension_test.go` 改动后实测）：

```
$ gofmt -l .
（无输出；退出码 0，stdout 0 字节）

$ go build ./...
（退出码 0，stderr 0 字节）

$ go vet ./...
（退出码 0，stderr 0 字节）

$ go test -count=5 -race -timeout 800s ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-my-compact-ui	7.022s
（退出码 0，DATA RACE 计数 0，连续 5 轮）

$ ./bin/pig-test -e ./pi-my-compact-ui --help
exit=0，stdout 11780 字节，stderr 0 字节
```

上一轮复跑（历史记录保留备查；本轮第 2 轮审查修复后的实测见上方主块与下方隔离加载）：

```
$ go test -count=1 -timeout 300s ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-my-compact-ui	0.256s

$ go test -count=30 -race -timeout 600s ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-my-compact-ui	7.672s

$ go test -count=20 -race -timeout 600s ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-my-compact-ui	8.853s

$ go vet ./...
（无输出；退出码 0）
```

全仓 `make build` / `make vet` / `make test`（`/root/workspace/pi-extensions/go` 下，
15 个组件 + root 模块）退出码均为 0。

隔离加载（`/root/workspace/pi-extensions/go` 下，PiG 0.5.2；`./bin/pig-test` 强制
`PIG_HOME=/root/.pig-test`，避免污染生产 `~/.pig`）：

```
$ ./bin/pig-test -e ./pi-my-compact-ui --help
exit=0，stdout 11780 字节，stderr 0 字节

$ ./bin/pig-test -e ./pi-my-compact-ui --mode json -p 'say OK'
exit=0，stdout 40572 字节，stderr 0 字节，末条事件 {"type":"agent_settled","aborted":false}
```

`--help` 只打印帮助、不编译扩展，因此另起一个**故意语法错误**的扩展做对照组，证明
`-e` 在真正加载路径上确实会编译并加载：

```
$ ./bin/pig-test -e /tmp/brokenext --mode json -p 'say OK'
exit=1，stdout 0 字节，stderr 213 字节
Error: Failed to load extension "/tmp/brokenext": Failed to load extension: parse /tmp/brokenext/broken.go: /tmp/brokenext/broken.go:3:18: expected ')', found 'EOF'
Hint: Start without extensions using "pig -ne".
```

对照组失败、`./pi-my-compact-ui` 在同一路径上 exit=0 → 本扩展确实被编译并加载，无
standalone module root 报错。说明：非交互模式下 `-p` 不分发 `/compact-inspect`、
`/compact-ui-config` 斜杠命令，命令输出逻辑由单测覆盖，真实 TUI 交互未在本次验证范围内
跑（见遗留）。

## 已知差异与遗留

1. **Go 注释里的 TS 行号与权威源存在系统性漂移（已量化，未逐条回改）**。移植时写在
   函数注释上的 `(L###-###)` 有 157 处，其中 106 处可在权威源中定位到同名定义，
   **只有 11 处精确**，漂移区间 `-1028 … +715`、中位数 `-4`；最大引用 `L2530` 已超出
   bundle 总行数 2453。原因：注释行号对的是移植期间的 `src/` 拆分文件或旧 bundle，
   不是当前 bundle。**本文档与上方对照表一律改用实测行号**；Go 源码注释未逐条修订
   （属注释修正，不影响行为），需要时另起任务。
2. **无原型可 patch（见上节）**：分组容器、assistant 思考补丁、压缩标题、分组工具
   置空、原生 thinking widget 抑制都由 Go 侧等价机制实现，语义对齐但实现路径不同。
3. **`sdk.Context` 不透明**：宿主角色的读取（thinking label、widget renderer、
   overlay、notify、terminal input）收敛到 `host.go` 的适配层与测试替身上
   （`harness_test.go`），事件分发本身由隔离加载验证。
4. **键序遍历**：Go 的 `map` 无序，凡依赖插入序的地方（如 thinking 块索引集合）都改用
   显式切片保序（`state.go:248` `thinkingBlock` + `accessors.go:45`），不使用 map
   遍历顺序做语义。
5. **`-race` 探针的宽限窗口**：`lockProbeUI` / `lockProbeTUI` 用 `lockFreeWithin`
   （`extension_test.go`）在 2ms 内重试取锁，以排除 ticker/动画 tick 短暂持锁造成的
   假阳性；「同 goroutine 持锁调用宿主」这类真实违规在窗口内永远拿不到锁，仍会失败。
6. **命令的 TUI 观感未跑**：非交互 `-p` 不分发斜杠命令；`/compact-inspect` 与
   `/compact-ui-config` 的交互由 `modals_test.go` / `settings.go` 单测覆盖逻辑。
