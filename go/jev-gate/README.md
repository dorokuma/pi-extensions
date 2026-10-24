# jev-gate（Go 版，第 2 批）

Jev / Clef（**System One 分类器，非 LLM**）门禁与巡检扩展的 Go 移植。

- 权威源（TS）：`extensions/jev-gate/src/jev-gate.ts`，1005 行 / 48840 字节，md5
  `f4ff427bb51b6fa378fcfd356b81fa99`（与部署件 `/root/.pi/agent/extensions/jev-gate.ts`
  逐字节一致）。
- Go 版：本目录，module `github.com/dorokuma/pi-extensions/go/jev-gate`，Go 1.26，
  仅依赖标准库 + `github.com/MichaelKinsy/PiG/extensions/sdk`（本地经 `go/go.work` 解析到
  `go/pigsdk`，PiG 运行时由 staged SDK 注入；**无 `replace`**）。
- 判定阈值 **冻结不变**：契约 `0.35 / 0.80`、门禁 `0.35 / 0.80`、回传分类
  `0.95`（正常）/ `0.80`（异常）。五个任务的问句表逐字照搬（英文问句 + 中文 state）。

## 文件结构

| 文件 | 行数 | 内容 |
|---|---|---|
| `questions.go` | 320 | 任务/角色枚举、问句表（`CONTRACT_FIELDS` / 门禁 / 回传标签 / 出处 / 分歧）、有序 JSON 编码、`questionsFor` |
| `gate.go` | 557 | 常量与冻结阈值、环境配置（`ConfigFromEnv`，JS `Number()` 语义）、`probOf` / `choiceOf` / `triage3` / `verdictFor` / `fmtProbs`、工具结果形状 |
| `key.go` | 214 | 凭据三步解析（环境 → `settings.json` → 本地网关令牌）、`IsLocalGatewayURL`（回环判定，含 IPv4-mapped / translated IPv6） |
| `jev.go` | 316 | `POST {base}/systemone`、响应解包（顶层 / `result` / 双层 `result.result`）、`validateAnswers` / `validateAnswer` fail-closed、截断、有界响应读取（4 MiB）、usage 归一 |
| `classify.go` | 257 | `models.classify()` 的 `typesafe-system-one` 实现（PiG 缺口绕法，见下）+ 回传前用同一套 `validateAnswer` 校验答案结构 |
| `identity.go` | 591 | `SUBAGENT-IDENTITY` 块（S1–S4 fail-closed、`PIG_HARNESS_ARGV_FILE` 桥接的进程级临界区、真实 herdr socket 查询、/proc 祖先链） |
| `extension.go` | 357 | `executeCheck`、参数 schema、`providerConfig`、`toolDefinition`、`Extension()` 工厂 |
| `gate_test.go` / `key_test.go` / `jev_test.go` / `extension_test.go` / `identity_test.go` / `classify_test.go` | 499 / 143 / 363 / 352 / 464 / 208 | 见「测试」 |
| `go.mod` | 5 | 独立嵌套 module（PiG 只按 `go.mod` 识别扩展单元） |

`go/go.work` 仅新增一行 `./jev-gate`。

## TS → Go 行号对照

TS 行号取自 `extensions/jev-gate/src/jev-gate.ts`（1005 行）；Go 行号取自本目录现行文件。

| TS | 内容 | Go |
|---|---|---|
| `64-79` | `PROVIDER` / `DEFAULT_BASE_URL` / `DEFAULT_MODEL_ID` / `KEY_ENV_NAMES` / `LOCAL_GATEWAY_KEY` / `MAX_STATE_CHARS` / `TIMEOUT_MS` | `gate.go:23-49`（常量）、`gate.go:74-120`（`Config` + `ConfigFromEnv`） |
| `90-97` | 冻结阈值 `FIELD_MISSING_MAX` … `TRIAGE_ANOMALY_MIN` | `gate.go:53-66` |
| `99-103` | `TASKS` / `ROLES` | `questions.go:25-54` |
| `105-107` | `isLoopbackIpv4` | `key.go:30-37` |
| `109-139` | `isLocalGatewayUrl` | `key.go:79-114`（+ `embeddedLoopbackIPv4` `key.go:44-72`） |
| `141-168` | `keyLookupError` / `resolveApiKey` | `key.go:135-199`（`KeyResolution` + `ResolveAPIKey`）、`key.go:203-214`（`resolveKeyOrError` 的 fail-closed 文案） |
| `170-186` | `noul()` / `choice()` | `questions.go:92-99`（+ `MarshalJSON` `questions.go:104-150`） |
| `188-230` | `CONTRACT_FIELDS`（7 角色硬字段） | `questions.go:201-243` |
| `232-238` | `GATE_QUESTIONS`（g1–g4） | `questions.go:248-254` |
| `240-253` | `TRIAGE_CRITERIA`（8 标签） | `questions.go:258-267` |
| `255-258` | `oneLine` | `gate.go:169-176` |
| `260-262` | `isRecord` | `gate.go:179-182` |
| `264-271` | `isValidProb` | `gate.go:185-187` |
| `273-284` | `probOf` | `gate.go:236-269`（`probOf` / `probOfMap`） |
| `286-303` | `choiceOf` | `gate.go:283-327`（`choiceOf` / `choiceOfMap`） |
| `305-308` | `Line` | `gate.go:330-333`（+ `line` `gate.go:336-342`） |
| `310-317` | `fmtProbs` | `gate.go:374-384`（+ `sortedKeys` `gate.go:389-400`） |
| `320-330` | `JevCall` | `jev.go:55-72`（`Truncation` / `JevUsage` / `JevCall`） |
| `333-347` | `unwrapSystemOneBody` | `jev.go:116-136` |
| `349-381` | `validateAnswers` | `jev.go:141-199`（`validateAnswers` / `validateAnswer`） |
| `383-454` | `callJev` | `jev.go:216-299`（截断 `jev.go:100-112`、传输错误 `jev.go:204-212`、usage `jev.go:303-316`、有界响应体 `jev.go:43-52`） |
| `456-458` | `ADVISORY` | `gate.go:69-70` |
| `461-465` | `SOURCE_QUESTIONS` | `questions.go:270-274` |
| `467-470` | `DISAGREEMENT_QUESTIONS` | `questions.go:278-281` |
| `472-478` | `DISAGREEMENT_RELATION` | `questions.go:284-290` |
| `480-519` | `questionsFor` | `questions.go:293-320` |
| `522-528` | `triage3` | `gate.go:347-360` |
| `530-628` | `verdictFor` | `gate.go:410-547`（`Verdict` `gate.go:403-407`、`toolResult` `gate.go:550-557`） |
| `630-699` | `executeCheck` | `extension.go:99-223`（`ExecuteCheck` / `executeCheck` / `failedResult` / `errorResult`） |
| `701-702` | `BUILTIN_CLASSIFIER_PROVIDER = "typesafe"` | `gate.go:31` |
| `704-706` | `SUBAGENT-IDENTITY-BEGIN` 块头 | `identity.go:2-31`（块说明） |
| `734-738` | `HERDR_SOCKET_FILE` / `IDENTITY_QUERY_TIMEOUT_MS` / `IDENTITY_ANCESTOR_MAX_HOPS` | `identity.go:47-64` |
| `754-814` | `queryPaneShellPid` | `identity.go:75-141` |
| `816-837` | `readProcPpid` / `readProcCmdline` | `identity.go:144-179` |
| `839-854` | `readAncestorChain` | `identity.go:182-202` |
| `856-863` | `isAgentRuntimeCmdline` | `identity.go:208-221`（+ `agentRuntimePattern` `identity.go:204`） |
| `865-877` | `subagentLaunchMarker` | `identity.go:343-361` |
| `879-913` | `resolveAgentIdentity` | `identity.go:466-572`（`resolveAgentIdentityWithEnv`） |
| `915-921` | `isSubagentSession` | `identity.go:581-591` |
| `924-1005` | `export default async function (pi)`：子代理早退 + `registerProvider` + `registerTool` | `extension.go:323-357`（`Extension` / `newExtension`）、`extension.go:270-290`（provider config）、`extension.go:293-316`（tool definition）、`extension.go:236-266`（schema） |

TS 侧无对应的 Go 加固：有界响应读取 `jev.go:43-52`（含常量/哨兵为 `33-52`）、分类器答案结构校验 `classify.go:164-186`（含辅助函数 `questionMember` / `criteriaLabels` 为 `158-218`）、argv 桥进程级临界区 `identity.go:282-323`（含注释为 `275-323`）。

## 与 TS 的行为差异（逐条）

1. **子代理身份判定（SUBAGENT-IDENTITY）采用家族已审 Go 语义**，与 TS 块有 4 点差异
   （与 `go/exclude-tools-guard` 一致，便于两个扩展共用同一判据）：
   - S1 **不再要求 pane 名以 `:p1` 结尾**，只要求 `HERDR_PANE_ID` 非空（pane 名只作查询键与
     证据串）。理由：pane 可被改名/重编号，命名从来不是防护面；伪造环境变量仍过不了 S2/S3/S4。
     副作用（改善）：主代理在改过名的 pane 里不会被误判子代理。
   - 宿主锚点由「固定 hop」改为「自身向上第一个 agent-runtime 祖先」（子进程形态）或「自身」
     （融合形态），并新增 `PIG_HARNESS_ARGV_FILE` + `ArgvCache` 桥接（PiG 的 Go 扩展子进程
     argv 为空，负向标记必须从该文件读，读失败即 fail-closed）。
   - 新增 `DetectSubprocessForm`（`PIG_EXT_SOCKET` / `PIG_EXT_SOCKET_<NAME>` /
     `PIG_EXT_PACKED_CELL` / `PIG_EXT_ACTIVE_MEMBERS`）：PiG 的 Go 扩展默认以 packed cell 成员
     运行，只看 `PIG_EXT_SOCKET` 会把真主代理误判成子代理。
   - 未确证时用 `sync.Once` 打一行 stderr 警告（PiG 会丢弃扩展 stderr，见 D56，属尽力信号）。
   - S2 的 1500ms 预算与「不重试、不延长」纪律保持不变；socket 路径仍是硬编码常量。
   - **残余面（与 `go/exclude-tools-guard/README.md` 对齐）**：若子代理未携带任何负向标记
     （`PI_SUBAGENT` / `PI_ROLE` / 三条 argv 标记都不可见），跑在非 `:p1`（甚至任意）pane，但进程链
     形态看起来就是「pane shell 直接起 agent」（宿主是 pi、pane shell 是宿主的严格祖先（含父进程）、无中间 agent
     runtime），S1–S4 会全部成立而被判 main，子代理分支的摘工具/清 addendum 不会发生；现网证据见
     `go/exclude-tools-guard/README.md`（`herdr agent start --kind pi` 不注入负向标记）。
2. **`models.classify()` 的 provider 注册方式不同（PiG 缺口，见下节）**：TS 只注册模型、依赖
   pi-ai 内置 transport；Go 版额外注册 `classifiers["typesafe-system-one"]` 回调，协议与
   `ai.ClassifyTypesafeSystemOne` 对齐（URL `<baseUrl>/systemone`、body `{model,state,questions}`）。
   `jev_check` 自身不用这条路径（仍直连 HTTP），因此五个任务的判定完全未变。
3. **choice 概率的展示顺序**：TS 用上游 JSON 的键序（`Object.entries`），Go 的 `map` 无序，故
   `probs: kind:*` 行与 `structuredContent.probabilities` 的键按升序输出。**数值、verdict 文本、
   阈值判定均不变**，仅展示顺序。
4. **字符计数口径**：payload 截断与 `oneLine` 截断按「字符」（Go rune）计，TS 按 UTF-16 码元计。
   BMP 文本（中文/英文）完全一致；仅星平面字符（emoji 等）边界略有差别。截断标记文本逐字一致。
5. **`JEV_MAX_STATE_CHARS` 为负数时**：TS 会 `slice(0, -n)` 从尾部砍掉 n 个字符（JS 怪癖）；
   Go 视为「关闭截断」。NaN / `Infinity` 两边都是关闭截断。属有意收敛，不改变默认行为。
6. **`JEV_TIMEOUT_MS` 非数值/0/溢出**：复现 Node `setTimeout` 的钳制（一律 1ms），因此超时消息
   会是 `timeout after NaNms` 之类（TS 同样会在 1ms 后以 abort reason 报错）。数值解析复现
   `Number()`（空白、`Infinity`、`0x` 十六进制、NaN）。
7. **`IsLocalGatewayURL` 不做 WHATWG IPv4 数字简写归一**：`http://127.1/` 这类简写在浏览器/Node
   里会规范化成 `127.0.0.1`，Go 版判为远端（fail-closed，不静默放宽）。完整写法、`localhost.`、
   `::1`、IPv4-mapped（`[::ffff:7f00:1]`）与 IPv4-translated（`[::ffff:0:127.0.0.1]`、展开形态）
   的回环判定与 TS 等价（有表驱动测试）。
8. **未注册命令与事件钩子**：TS 权威源文件头明确「不 spawn 进程、不注册 command、不动 settings.json」，
   实现里也只有 `registerProvider` + `registerTool`。任务书提到的「命令 `/jev`」「事件钩子」在
   权威源中不存在，按**源码事实优先**未实现（见「任务书偏差」）。
9. 工具结果形状用 `sdk.ToolResult`（`Content` 为字符串），`Details` / `StructuredContent` 字段名与
   TS 一致；`isError` 分支的 `structuredContent` 与 TS 完全相同（前三类错误只有 `error`，
   catch 分支带 `task/error/advisory`）。
10. 启动告警文案与 TS 同形（`missing MAGPIE_API_KEY/WINDHUB_API_KEY: ...`），写 stderr。
11. 子代理分支清空内置分类器 provider 用 `registerProvider("typesafe", {models: []})`：已核对 PiG
    `composeNativeProvider`（`definitions != nil` 时按定义列表重建），空列表即清空，与 TS 的
    「提供 models 即整体替换」语义一致。
12. **响应体上限与分类器答案校验（TS 侧无对应加固）**：`jev.go` / `classify.go` 的 HTTP 响应改为
    有界读取（`maxResponseBodyBytes = 4 MiB`，`io.LimitReader(..., +1)`，超出即
    `response body too large` fail-closed，不再随上游无限缓冲）；`classifyWith` 在回传前用与
    `validateAnswers` **同一套** `validateAnswer` 校验每个答案（`noul` 概率有限且在 `[0,1]`、
    `choice` 的 `probabilities` 对象与 `confidence` 均在 `[0,1]` 且标签属于 `criteria`、`score`
    为有限数且在 `[0, len(criteria)-1]`、未知 question `type` 一律拒绝），畸形答案返回 `error`
    而不是 `stopReason: "stop"`。`PIG_HARNESS_ARGV_FILE` 桥接改为一把锁包住整段临界区（cache 命中
    直接返回），并发调用不会各自重读或退回空 argv。

## PiG SDK / D78 缺口与绕法

- **D78（provider 对象跨进程载体）**：本次未触发。jev-gate 只用 `registerProvider(name, config)`
  这条「无函数配置」通道；`GetProvider`/`GetRegisteredNativeProvider` 的 Provider 对象载体未被使用。
- **新发现并绕过的缺口（classifier 解析是按 provider 的）**：TS 依赖 pi-ai 的**全局** API transport，
  只要 model 声明 `api: "typesafe-system-one"` 就能分类；PiG 把 classifier 实现放在每个 provider 的
  `Classifiers` 映射里（`coding/extension/model_registry.go` 的 `ProviderConfigInput.Classifiers`，
  以及 `internal/codingagent/native_provider_composer.go` 的 `base.Classify`/`extension.Classifiers`）。
  自定义 provider id `magpie-jev` 没有内置条目，若不注册实现，`models.classify("magpie-jev",...)`
  会返回 `Provider magpie-jev does not support classification`（`ai/classifier.go:231`）。
  **绕法（按 PLAN 硬规矩 6，不改 PiG 源码）**：用 SDK 支持的 `classifiers` 回调注册同一线协议
  （`ProviderClassifyFunc`），实现放在 `classify.go`，README 本节记录缺口。这是 SDK 允许的**最大
  等价面**：`jev_check` 不依赖它，`models.classify()` 与 TS 行为对齐（同样的 URL/ body / answers /
  usage 归一）。
- **D56（扩展 stderr 被丢弃）**：身份未确证的 warning 与凭据缺失告警都只能「尽力」送达，已在
  README/代码注释标注，不据此做判定。

## 任务书偏差（需 reviewer 注意）

任务书写「命令 `/jev`」「事件钩子」，但权威源 `jev-gate.ts` **既没有 command 也没有 `pi.on(...)`**
（文件头第 8–10 行：`不 spawn 任何进程、不注册 command、不动 settings.json；只做 registerProvider ×1
+ registerTool ×1`）。按「源码事实优先、不擅自扩大目标」的纪律，Go 版同样**不注册命令、不挂事件钩子**；
如需新增 `/jev`，属目标变更，应由主代理拍板后另立任务（并会引入与 TS 的行为差异）。

## 测试

全部测试零外网、零生产路径：`httptest.Server` 模拟 `/v1/systemone`（含 200 三种包裹形态、429/500、
畸形 JSON、`null`、数组、空 answers、缺键、非法概率、超时、调用方取消、截断、usage 别名、请求形状），
临时 Unix socket 模拟 herdr `shell_pid`（含 pane 不匹配、坏 JSON、零 pid、小数 pid、无 socket、静默超时），
`/proc` 合成祖先链覆盖 S1–S4 与负向标记、`PIG_HARNESS_ARGV_FILE` 桥接（成功/缺文件/坏 JSON/marker），
以及 `go.work` 下的并发安全（`-race`）。**不触碰**生产 `/root/.pi`、生产 herdr socket 与本地 Magpie：

```
$ gofmt -l .                     # 无输出
$ go build ./...                 # exit 0
$ go vet ./...                   # exit 0
$ go test -count=1 -race -v ./... # exit 0，171 个测试条目（59 顶层 + 112 子测试）全部 PASS，DATA RACE 0
ok  github.com/dorokuma/pi-extensions/go/jev-gate  3.3s
```

覆盖矩阵（与 TS 行为对照）：

| 面 | 测试 |
|---|---|
| 冻结阈值 | `TestFrozenThresholds`（逐个断言 0.35/0.8/0.95 数值） |
| 三档边界 | `TestTriage3Boundaries`（0.34 / 0.35 / 0.79 / 0.80 / 0.99 / 缺失） |
| 5 类任务结论 | `TestVerdictContractLint` / `…GateScan` / `…ReturnTriage`（0.94/0.95、0.79/0.80）/ `…Disagreement` / `…SourceAudit` / `TestVerdictUnknownTask` |
| 问句表 | `TestQuestionsFor`（逐任务键序与类型）、`TestQuestionWireShape`（逐字节 JSON） |
| 概率/标签解析 | `TestProbOf`（15 例）、`TestChoiceOf`（9 例非法形态） |
| 配置语义 | `TestConfigFromEnv`（默认、`MAGPIE_BASE_URL` 优先、`WINDHUB` 回落、NaN、0、十六进制、Infinity）、`TestTruncatePayload`、`TestOneLine`、`TestFormatHelpers` |
| 凭据/回环 | `TestIsLocalGatewayURL`（25 例）、`TestResolveAPIKeyOrder`、`TestResolveAPIKeyAgentDir`、`TestResolveKeyOrError` |
| HTTP 契约 | `TestCallJevAcceptedEnvelopes`（3 形态）、`TestCallJevUsageAliases`（6 例）、`TestCallJevRequestShape`、`TestCallJevFailures`（10 例）、`TestCallJevRateLimitedBody`、`TestCallJevTimeout`、`TestCallJevCallerCancellation`、`TestCallJevMissingCredential`、`TestCallJevTruncatesPayload`、`TestOversizedResponseBodyFailsClosed`（>4 MiB 响应体：`Call` / `classifyWith` 两路均 fail-closed） |
| 响应校验 | `TestUnwrapSystemOneBody`（6 例）、`TestValidateAnswers`（含 score 档位越界、bool 不支持） |
| 工具/注册 | `TestExecuteCheckInvalidTask` / `…EmptyPayload` / `…ContractLintRole` / `…Success` / `…ContractLintSuccess` / `…Truncation` / `…HTTPError` / `…RequestBodyIsSmall`、`TestJevCheckSchema`、`TestToolDefinition`、`TestProviderConfig`、`TestNewExtensionBranches` |
| 身份判定 | `TestResolveAgentIdentityNegativeMarkers`（5 例）、`TestResolveAgentIdentityProofChain`（10 子例：S1/S2/S3×2/S4/主代理/打包单元/融合/融合未知/pane 非 `:p1`）、`TestResolveAgentIdentityUnreadableAncestry`、`TestDetectSubprocessForm`、`TestLoadEffectiveArgs`（6 子例，含「缓存优先于重新 export 的变量」）、`TestLoadEffectiveArgsConcurrent`（8 goroutine × 200 轮并发，`-race`）、`TestQueryPaneShellPid`（7 子例 + 无 socket）、`TestQueryPaneShellPidTimeout`、`TestReadProcSelf` |
| 分类器回调 | `TestClassifyTypesafeSystemOne`、`…Failures`、`TestClassifyWithoutUsage`、`TestClassifyRejectsMalformedAnswers`（10 例畸形答案 + 1 例带 `criteria` 的合法答案正对照） |
| 并发 | `TestExecuteCheckConcurrent`（8 goroutine × 8 次，`-race`） |

## 隔离 PiG 实测记录（2026-10-24）

均经 `go/bin/pig-test`（写死 `PIG_HOME=/root/.pig-test`，清除可绕过变量），生产 `/root/.pi` 零改动：

1. **加载冒烟**

   ```
   $ ./bin/pig-test -e ./jev-gate -p "回复 OK"
   EXIT=0
   OK
   ```

2. **真实入口（工具可见性）**：`--mode json -p 'Call the jev_check tool …'` → `EXIT=0`，无 panic、
   无扩展加载错误；但模型回「No jev_check tool」，请求里工具只有 `read/bash/edit/write`。
   **原因（非缺陷）**：本次运行发生在派发 pane `HERDR_PANE_ID=w37:p8` 内，进程链为
   `扩展 → pig → sh → pi(当前 agent) → pane shell`，S4 发现「宿主与 pane shell 之间存在另一个 agent
   runtime」→ 按设计 **fail-closed 判子代理** → 子代理分支不注册 `jev_check`/`magpie-jev`，只清空
   `typesafe`。这与 TS 判据方向一致（TS 还会额外因 pane 名不含 `:p1` 判子代理）。
   主代理分支的注册内容由 `TestToolDefinition` / `TestProviderConfig` / `TestNewExtensionBranches`
   静态覆盖；**在真正的 p1 主代理里看到 `jev_check` 仍需 verifier 在非嵌套会话复跑一次**（未做，
   如实记录）。

## 未做 / 遗留

- 未在真实主代理 pane 里做 `jev_check` 端到端调用（原因见上，嵌套环境下被判子代理）；未向本地
  Magpie `/v1/systemone` 发真实分类请求（避免消耗配额；HTTP 路径由 httptest 全量覆盖）。
- `calibrate/`（TS 侧校准脚本与 fixtures）不移植：它属于 TS 源仓的校准工具，读 TS 源文件抽表；
  Go 版问句表已逐字对齐并有 `TestQuestionWireShape` 钉住，改问句时仍应回 TS 侧重跑校准。
- 展示顺序（choice 概率升序）与字符计数口径差异见「与 TS 的行为差异」3、4。
- 发现但**未修改** TS 侧的健壮性隐患（按任务要求只报告）：`JEV_MAX_STATE_CHARS` /
  `JEV_TIMEOUT_MS` 用 `Number(env || default)`，拼错时得到 NaN —— 超时被 Node 钳到 1ms（每次调用
  立即 abort），截断则完全关闭；建议后续在 TS 侧对非有限值显式告警。负值 `JEV_MAX_STATE_CHARS`
  会触发 `slice(0,-n)` 的「砍尾」怪癖（Go 版已收敛为关闭截断）。分类器阈值语义本身未发现错误。
- 未提交：按任务要求待 C 档 verifier + reviewer + oracle 门禁后再 `git commit`（提交时须同时带上
  `go/go.work` 的 `./jev-gate` 一行）。
