# cache-stats（Go / PiG 移植）

PiG 的**提示缓存观测扩展**：分别统计当前 run 与全会话的命中率，可选择显示 TUI
页脚（herdsman 状态、双命中率菱形、上下文占用、tokens/s、模型），对每个模型首次返回
400 时告警一次，并注册 `/cache-stats` 命令（`enable` / `disable` / `reset` / `prefix`）。
默认模式是**只读观测**：不重写 system prompt，不改 provider payload；默认开启的前缀
诊断只在本扩展自己的 `before_provider_request` hook 里比较 system/developer/
instructions/tools，不是命中率结论，不是最终 HTTP body，也不是服务端 token 前缀。

- Module: `github.com/dorokuma/pi-extensions/go/cache-stats`，Go 1.26。
- SDK: `github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0`；本地通过父级 `go.work`
  解析到 `go/pigsdk`，PiG 构建时用 staged SDK。组件内无本地 `replace`。
- 入口：`package cachestats`，导出 `func Extension() *sdk.Extension`。
  **无 `main()` / 无 `init()`**，与同仓 `ctxmode`、`codegraph-go`、`moshi-hooks`
  的 factory 加载方式一致。
- 权威源：`/root/.pi/agent/extensions/cache-stats.ts`，2110 行，81348 字节，
  md5 `9b5692bfd8f7488154bf297d9e38e0a6`，
  SHA-256 `99cfa65fa08bcae3995a68d004f722577ba51bc50eb776074eb433c8677fe939`。
  与 `/root/workspace/pi-cache-stats/extensions/cache-stats.ts` **逐字节一致**
  （`cmp` 无差异，两者 md5/SHA-256 相同）。移植期间两处权威源与
  `/root/workspace/pi-cache-stats/` 全仓均未修改。

## 行为契约

1. **只读观测**：任何事件 handler 都不返回 systemPrompt 覆写，也不改动
   `event.payload`。TS 的默认模式本身就是纯观测，Go 版逐条保持。
2. **命中率分母**：`input + cacheRead + cacheWrite`（Pi 的 `usage.input` 始终是
   各 API 的净未命中 input，不含 cacheRead/cacheWrite）。`aggregateHit` 在分母
   ≤ 0 时返回 null（`n/a`），绝不编造 0%。
3. **双菱形门控**：扫描窗口内从未出现缓存交互（`cacheRead > 0 || cacheWrite > 0`，
   统计 assistant / toolResult / compaction / branch_summary 全部条目）时，
   ◆（累计）与 ◇（最近）都显示 `n/a`；一旦窗口内出现过缓存交互，后续单轮未命中
   的 ◇ 显示 0%（该轮可测量），◆ 仍按累计口径。
4. **latest 语义**：最新的 assistant 消息无条件覆盖 ◇；有 usage 且分母 > 0 时为
   `aggregateHit(cacheRead, denom)`，缺 usage（中断/报错/注入）或分母为 0 时清回
   null，绝不残留上一轮的值。
5. **turn 报告有界**：`turnReports` 上限 50 条，超出裁掉最旧的；累计计数不受影响。
6. **agent_end 回填**：`liveRun` 四项全 0 且 `event.messages` 非空时，从 messages
   汇总 usage 并回填 snapshot，使当前 run 聚合能读到本轮数据。
7. **TPS 一次性消费**：`before_provider_request` 记录 t0，`turn_end` 通过
   `clearTurnTiming` 一次性消费全部时间戳；没有新请求的重复 `turn_end` 得到全 null，
   渲染 `n/a` 而不是把旧 t0 重新量出塌陷的 t/s。
8. **前缀诊断分 scope 隔离**：scope key = `sessionId + provider + api + model.id +
   hash(baseUrl)`，另有 16 字节随机 salt，跨实例/跨会话/跨模型/跨 endpoint 不共享
   比较状态；scope 字段缺失、非字符串、超长一律 skip，且从不用显示名当身份键。
9. **有界遍历**：depth 8、nodes 2000、单串 100 000 字符、总量 250 000 字符、
   tools 64、循环引用、非有限数——任一越界都导致 `skipped`，绝不当成 `stable`。
10. **通知清洗**：所有 `notify` 文本经 `escapeForNotify` 去掉 C0/C1/DEL/行分隔符
    并截断到 240 字符；身份键始终用原始值，清洗只作用于显示。

## 环境变量

读取时机与 TS 一致：factory（即 `Extension()`）执行时读取一次。

| 变量 | 默认 / 作用 |
|---|---|
| `PI_CACHE_STATS_FOOTER` | 未设置即开启；设 `0`/`false`/`no`/`off`（去空白、小写）关闭页脚 |
| `PI_CACHE_STATS_PREFIX_DIAGNOSTICS` | 未设置即开启；设假值关闭前缀诊断（命令显示 `Prefix: disabled`） |
| `PI_CACHE_STATS_VERBOSE` | 真值：即使本轮无缓存交互也 `appendCustomEntry` |

## TS → Go 定位

TS 行号对 `cache-stats.ts`（2110 行）；Go 行号用 `grep -n '^func '` 精确核对。

| TS 行段 | 内容 | Go 定位 |
|---|---|---|
| 1–35 | 文件头、常量（LOG、两个 env、各 LIMIT） | `prefix.go:23` 常量块 |
| 40–58 | `CacheUsage` 类型、`clampNonNeg` | `usage.go:163` `clampNonNegValue` |
| 62–137 | `normalizeUsage` | `usage.go:43`；分槽读取 `usage.go:127` `pickNumber` / `:140` `nestedNumber`；`orZero` `usage.go:154` |
| 142–145 | `aggregateHit` | `usage.go:174` |
| 166–224 | `scanLiveHitRates` | `usage.go:233` |
| 226–317 | `computeLiveHitRates` | `usage.go:305` |
| 228–266 | `cacheHitDenom` / `cacheHitPct` | `usage.go:185` / `usage.go:209`（另有 `…Numbers` 数值重载 `:203` / `:216`） |
| 320–510 | 类型、`emptySnapshot`/`emptyLiveRun`、`randomSalt`、`createInstanceState`、`clearPrefixMemory`、`clearTurnTiming`、`resetRunState` | `state.go:150` `createInstanceState`；`:165` `clearPrefixMemory`（锁内体 `:171` `…Locked`）；`:178` `clearTurnTiming`（`:184` `…Locked`）；`:194` `resetRunState`（`:200` `…Locked`）；`state.go:138` `randomSalt`；`state.go:133`/`:135` `emptySnapshot`/`emptyLiveRun` |
| 512–533 | `FOOTER_ICON`、`isEnabled`、`modelKey`、`sheepMeat`、`formatHerdsmanStatus` | `footer.go:19` 图标常量；`extension.go:224` `isEnabled`；`tps.go:99` `sheepMeat`；`tps.go:107` `formatHerdsmanStatus` |
| 535–560 | `readFooterCtx` | `state.go:228`（入参 `footerReadInput` 由 `extension.go:565` `captureFooterInput` 从 `sdk.Context` 采集） |
| 562–571 | `refreshFooter` | `state.go:262` |
| 573–611 | `recordTurnUsage` | `state.go:275` |
| 613–703 | `uninstallFooter` / `installFooter`（含 `render()` 与 dirty-check 缓存） | `extension.go:887` / `extension.go:835`；渲染体 `footer.go:165` `footerRender`，快照 `footer.go:87` `captureFooterSnapshot`（缓存键 `footer.go:44` `hitCacheKey`）；`SetFooterRenderer` 注入钩 `extension.go:813` `footerRendererHook`（宿主输入安全读取 `extension.go:857` `footerUIInputs`） |
| 705–877 | 前缀类型、`emptyBudget`、`section`、`fingerprint`、`escapeForNotify`、读取与预算原语 | `prefix.go:188` `fingerprint`（`padHex8` `:219` / `padInt32Hex8` `:235`）；`:246` `escapeForNotify`；`:273` `chargeChars`；`:290` `chargeString`；`:294` `bumpNodes` |
| 879–1037 | `hashValue`、`writeValue`、`extractPlainText`、`textToSection` | `prefix.go:320` / `:331` / `:461` / `:544` |
| 1039–1198 | `collectRoleBag`、`detectShape`、`toolsSection`、标签扇描、extras | `prefix.go:578` / `:676` / `:711` / `:765` `scanUniqueTaggedBlock` / `:825` `extraFromSystemText` |
| 1200–1560 | `sectionFromExtract`、`scopeKey`、LRU、`decidePrefixStatus`、`summarizePayload` | `prefix.go:846` / `:899` / `:940`–`:977` / `:984` / `:1058` |
| 1562–1680 | `formatSection`、`prefixStatsLine`、`showPrefix` | `prefix.go:1166` / `:1192` / `:1205` `prefixLines` |
| 1682–2074 | `Extension()` 工厂与 **12 个事件订阅**、`/cache-stats` | `extension.go:54` `Extension`；`before_provider_request` `extension.go:304` `applyProviderRequestTiming` + `state.go:407` `observePrefixDiagnostics`；`agent_end` `extension.go:235` `applyAgentEnd`；`turn_end` `extension.go:315` `applyTurnEndEvent`；`message_update`/`message_end` `state.go:310`/`:352`；命令 `extension.go:333` `handleCommand` |
| 2060–2131 | `readSessionAggregate`、`showStats` | `extension.go:432` `sessionAggregateOf`；`extension.go:449` `statsLines` |
| 700–810 | `computeTurnTps`、`toStamp`、`extractAndNormalizeUsage`、`truncateFooter`、`formatTps`、`formatWindowSize` | `tps.go:199` / `:174` / `usage.go:336` / `tps.go:148` / `tps.go:67` / `tps.go:41` |
| 812–856 | `extractAndNormalizeUsage` | `usage.go:336`（回写在 `usage.go:379`） |

**事件订阅数 = 12**（`grep -c OnEvent extension.go` → 12）：`before_provider_request`、
`after_provider_response`、`agent_start`、`agent_end`、`message_update`、`message_end`、
`turn_end`、`tool_execution_start`、`tool_execution_end`、`session_start`、
`model_select`、`thinking_level_select`。之前文档写的“13 个”是错的，源码与 TS 原版
均为 12。

## 测试与验证证据

原版 21 个 `.mjs` 用例按类翻成 Go 测试（`go test` 断言行为，不再需要 Node）：

| 原版文件 | Go 测试 |
|---|---|
| `test/hit-rate.mjs` §1–6、§8、§9、§10；§7 页脚格式化 | `usage_test.go`：`TestHitRateDenominatorAndPercent`、`TestAggregateHitOverTurns`、`TestNormalizeUsageOpenAIStyles`、`TestNormalizeUsageReasoningAndTotals`、`TestComputeLiveHitRates*`、`TestFormatTps`、`TestFormatWindowSize`、`TestFormatHerdsmanStatus` |
| `test/cache-stats.mjs` 命中率/turn/页脚/TPS 各段 | `lifecycle_test.go`（agent_end 记账、回填、有界报告、双 scope、reset 口径、400 列表）、`footer_test.go`（双菱形门控、watermark、dirty-check、上下文配色、段序、截断）、`tps_test.go`（`decodeOf` 差分 9 样本、四大护栏、一次性消费） |
| `test/prefix-diagnostics.mjs` | `prefix_test.go`：`TestPrefixBaselineStableChangedPerShape`、`TestPrefixShapeFromStructureNotVendor`、`TestPrefixUnknownAndMissingShapes`、`TestPrefixOutputNeverLeaksContent` |
| `test/prefix-r9-accessors.mjs`、`prefix-r9-budget.mjs`、`prefix-r9-endpoint.mjs` | `TestPrefixBudgetGuards`、`TestPrefixWalkerGuards`、`TestPrefixScopeIsolation`、`TestPrefixScopeMissingFieldsSkip`、`TestPrefixEvictionIsBounded` |
| `test/prefix-r11-*.mjs` | `TestPrefixExtrasRecognition`、`TestPrefixExtrasUnidentifiedForms`、`TestPrefixRoleBagStructureSensitivity`、`TestPrefixControlCharactersInStats` |
| `test/prefix-r12-*.mjs` | `TestPrefixBaselineStableChangedPerShape`、`TestPrefixMixedExclusiveFieldsAreUnknown`、`TestPrefixCrossShapeChange`、`TestPrefixExtrasUnidentifiedForms` |
| `test/prefix-r14-*.mjs` | `TestPrefixFingerprintLengthBoundaries`（65535/65536/65537/100000 边界）、`TestPrefixOversizeSkipsThenRebaselines` |
| `test/comparison.mjs`、`test/agent-session.mjs` | **未翻**：两者验证的是 Pi Node 侧 `DefaultResourceLoader` / `ModelRuntime` / `AgentSession` 的隔离与真实网络 benchmark 脚手架，不属于扩展行为本身；见下方"TS → Go 差异" |

实测（`/root/workspace/pi-extensions/go/cache-stats` 下）：

```
$ gofmt -l .
（无输出）

$ go build ./...
（退出码 0）

$ go vet ./...
（退出码 0）

$ go test -count=3 -race -v ./...
--- PASS（87 个顶层用例全过，另有 50 个子用例；连续 3 轮全绿）
PASS
ok  	github.com/dorokuma/pi-extensions/go/cache-stats	1.903s
（DATA RACE 计数：0）
```

隔离加载（`/root/workspace/pi-extensions/go` 下，PiG 0.5.2）：

```
$ ./bin/pig-test -e ./cache-stats --help
exit=0，stderr 0 字节

$ ./bin/pig-test -e ./cache-stats --mode json -p "say OK"
exit=0，stderr 0 字节，末条事件 {"type":"agent_settled","aborted":false}
```

`--help` 只打印帮助、不编译扩展，因此另起一个**故意语法错误**的扩展做对照，证明
`-e` 在真正加载路径上确实会编译并加载：

```
$ ./bin/pig-test -e /tmp/brokenext --mode json -p "say OK"
exit=1
Error: Failed to load extension "/tmp/brokenext": ... expected ';', found produces
```

对照组失败、`./cache-stats` 在同一路径上 exit=0 → 本扩展确实被编译并加载，无
standalone module root 报错。说明：非交互模式下 `-p` 不会分发 `/cache-stats` 斜杠
命令，所以命令输出由上面的单测覆盖，真实 TUI 交互未在本次验证范围内跑（见遗留）。

## TS → Go 差异

按任务要求，`isProxy` / `isUntrustedProxy`（TS 约 10 处，D83）在 Go 中改为显式类型
判断。逐项列出与 TS 不完全同构的地方：

1. **Proxy / accessor 检测（D83）**：SDK 交给 handler 的是解码后的
   `map[string]any` / `[]any`，不存在 getter、Proxy trap、`Symbol.iterator` 或
   `toJSON`，因此 TS 的 `readDataOwn`（MISSING / DATA / ACCESSOR / UNSAFE 四态）在
   Go 里退化为"有键即 data"：`data` 与 `missing` 仍严格区分，`accessor` 与 `unsafe`
   两态不再可能出现。**这不是遗漏，而是 Go 值模型下该类攻击面不存在**；所有相关的
   budget / depth / node / 循环引用 / 非有限数护栏完整保留，因为它们限定的正是有界性。
2. **对象键序**：Pi 的 JS 对象保留插入顺序，`Object.keys` 因此有序；Go 的 map 无序。
   walker 对普通 map **按字典序**枚举，对 `sdk.OrderedObject` 保留真实插入序
   （`values.go:53` `asObject`）。因此 `prefix-r14` 依赖跨 key / 元素构造的
   "16 位长度别名"碰撞夹具在原版里是**可构造的**，在 Go 版本无法用同样输入构造——
   该差异已在该测试中改测"未加料的同长内容必须 changed"（真正的行为断言）与
   "同内容不同键序指纹不同"（键序敏感性断言），未声称复现碰撞攻击。
3. **UTF-16 长度**：TS 的 `string.length` 与 `charCodeAt` 都是 UTF-16 code unit。
   `fingerprint` 用 `utf16.Encode` 逐 unit 混入，`chargeString` / `extractPlainText`
   的 `chars=` 也用 `utf16Length`，与 TS 对齐（对 BMP 之外的字符才有差别）。
4. **`sdk.Context` 无 mock**：TS 用 `mockContext()` 造 ctx；Go 的 `sdk.Context` 是
   不透明句柄，因此把宿主角色的那部分（读取 entries、context usage、thinking level、
   模型信息、notify、appendEntry、页脚重渲染）抽到 `agentHost` 接口与
   `footerSources` / `footerReadInput` 结构后面，测试直接驱动这些结构，覆盖与 TS 的
   同一批代码路径。事件分发本身由隔离加载验证（见上）。
5. **force 渲染**：TS 的 `requestRender(force)` 有强制位；PiG 的
   `Context.SetFooterRenderer` 无 force 参数，`refreshFooter(state, force)` 保留
   force 形参与调用点，但 re-push 时忽略它（语义等价：都会重渲染）。
6. **model api / baseUrl 来源**：`GetModelInfo` 不暴露 `api` 与 `baseUrl`，Go 版从
   `ctx.ModelRegistry().GetAll()` 里按 `provider`+`id` 取。**每次观测都重新读注册表，
   不做任何缓存**（`extension.go:595` `captureScopeIdentity` → `:701`
   `lookupModelEndpoints` → `:655` `buildModelEndpointResolver` → `:672`
   `modelEndpointsFrom`）：早先按 (provider,id)
   永久缓存的版本会把「注册表尚未就绪时的空结果」和查询失败永久记住（整个会话都是
   `skipped`，`session_start` 也不清），并在同名模型切换 `baseUrl`/`api` 后继续用
   旧 endpoint 算 scope key，造成跨 endpoint 串扰。TS 原版也是每次直接读当前 model。
   注册表取不到时 scope 字段为 nil → 该次观测 `skipped`（fail closed，下次重试），
   不会退化成跨 endpoint 共享比较状态。
7. **`Object.freeze` 防御**：TS 需 try/catch 才能回写冻结的 usage 对象；Go 的
   map 无冻结概念，`writeBackUsage` 无需 try/catch（行为等价：都只在字段缺失时写入）。
8. **`process.env` 读取时机**：与 TS 一致，在 `Extension()`（factory）里读一次，
   不是每次事件读。

## 测试规模说明

`wc -l *.go`（在全部改动并 `gofmt -w .` 之后实测，含注释与空行）：

| 分类 | 文件 | 行数 |
|---|---|---|
| 实现 | `extension.go` | 898 |
| 实现 | `footer.go` | 241 |
| 实现 | `prefix.go` | 1250 |
| 实现 | `state.go` | 456 |
| 实现 | `tps.go` | 372 |
| 实现 | `usage.go` | 470 |
| 实现 | `values.go` | 101 |
| **实现小计** | 7 个文件 | **3788** |
| 测试 | `concurrency_test.go` | 702 |
| 测试 | `footer_test.go` | 250 |
| 测试 | `harness_test.go` | 227 |
| 测试 | `lifecycle_test.go` | 352 |
| 测试 | `prefix_test.go` | 967 |
| 测试 | `tps_test.go` | 569 |
| 测试 | `usage_test.go` | 477 |
| **测试小计** | 7 个文件 | **3544** |
| **合计** | 14 个文件 | **7332** |

测试/实现比约 3544 / 3788 ≈ 0.94。

## 遗留

1. 非交互模式（`-p`）不分发 `/cache-stats` 斜杠命令，命令文案与页脚的真实 TUI 观感
   未在本轮跑；命令输出逻辑已由 `statsLines` / `prefixLines` 单测全覆盖。
2. `test/comparison.mjs`（Pi Node 侧 `DefaultResourceLoader` / `ModelRuntime` 隔离
   自检 + 可选真实网络 benchmark）与 `test/agent-session.mjs`（Pi `AgentSession`
   的 `setActiveToolsByName` 重建 system prompt）验证的是宿主 Node 行为，不属于扩展
   逻辑，未翻成 Go；PiG 对应能力需在宿主仓验证。
3. **前缀诊断的键序语义差异（第 2 条）并不意味着指纹可以忽略**：指纹**参与**
   `decidePrefixStatus` 的 `stable` / `changed` 判定（`prefix.go:984`），不是纯显示值。
   Go 版对普通 `map[string]any` 按**字典序**稳定遍历，所以同进程内同一份 map 跨轮次
   仍然稳定 `stable`；对 `sdk.OrderedObject` 保留插入序。因此本条差异只影响
   「用键序构造指纹碰撞」这类输入在 Go 与 TS 之间的**跨语言等价性**，不影响 Go 版自身
   的稳定性判定。
4. **`h2` 的有符号十六进制**：TS 的 `h2 ^= imul(...)` 结果是 int32，`toString(16)`
   因此带负号，而 JS 的 `padStart(8, "0")` 是给**整个带符号字符串**左补 `'0'` 至总长 8：
   短负数补零在**符号之后**（`(-1)` → `"000000-1"`，`(-0x1234)` → `"000-1234"`），
   只有有效位已占满 8 位的负数才多出第九个符号字符（如 `-0x7fffffff` → `"-7fffffff"`，
   故整条指纹 16 或 17 字符）。Go 版严格按此实现（`prefix.go:235` `padInt32Hex8`），
   并用 Node 实测向量对拍：指纹 23 组固定向量
   （`TestFingerprintMatchesTSFixedVectors`）+ 9 组 `padInt32Hex8` 向量
   （`TestPadInt32Hex8MatchesJSVectors`）+ 形状规则
   （`TestPadInt32Hex8IsPadStartNotZeroExtend`），逐字符一致。
5. **`num()` 与 JS `String(v)`**：统计行的计数器渲染必须与模板字面量的 Number→String
   一致。旧实现为了让计数器整个整数域走普通记数法而偏离了 JS 在 `1e21` 处的阈值
   （JS 是 `"1e+21"`）。现直接委托 `jsNumberString`（`extension.go:544` `num`）。
