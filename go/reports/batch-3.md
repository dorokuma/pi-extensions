# 第 3 批：cache-stats 与 pi-my-compact-ui 的 Go 移植收口报告（reports/batch-3.md）

| 项 | 值（均为本次实测/实读） |
|---|---|
| 分支 | `feature/go-batch1`（`git status --porcelain -b` 首行 `## feature/go-batch1`） |
| HEAD（写报告时） | `7c40f89` feat(go): port pi-my-compact-ui extension to Go (batch 3) |
| 报告采集起点 | `2026-10-25 21:51:19 +0800`（`date '+START %Y-%m-%d %H:%M:%S %z'`） |
| 工具链实测 | `go version go1.26.4 linux/amd64`（EXIT=0）；`pig --version` → `0.5.2+1.4.0`（EXIT=0） |
| 工作目录 | `/root/workspace/pi-extensions/go` |

证据范围声明：

- 本报告只写本次会话**亲自跑过**的命令输出与**亲眼读到**的文件内容；每条命令贴原文并显式捕获退出码（含 `make build/vet/test/sdk-check`、第 3 批 `cache-stats` 与 `pi-my-compact-ui` 的 `gofmt`/`build`/`vet`/`-count=1 -race -v` 及隔离 `-count=3 -race -v` 测试、PiG 隔离加载）。
- 按分工边界，本批范围覆盖第 3 批 `go/cache-stats` 与 `go/pi-my-compact-ui`（含 `go/go.work` 同步更新）；第 4 批（`pi-vcc`、`rpiv-ask-user-question`、`herdsman-pi`）与第 5 批（`herdsman` 守护进程 Go 化）由其它工作区并行推进，本会话未触碰。
- 全程未打印任何密钥，未改动 `/root/.pi/agent/extensions/*.ts`、`/root/.pi/agent/npm/node_modules/pi-my-compact-ui/dist/extension.ts` 生产部署件与各自外生宿主仓（`/root/workspace/pi-cache-stats`）。

---

## 1. 批次结论摘要

| 组件 | 档位 | commit | 提交文件数 | Go 实现 / 测试行数 | 测试状态（`-count=1 -race -v` 及隔离 `-count=3 -race -v`） |
|---|---|---|---|---|---|
| cache-stats | B 档 | `338c5f0` | 17 | 3788 / 3544（共 7332 行，比值 0.94） | 87 顶层 / 137 PASS（含子用例），连续 3 轮 `-race` 261 顶层 PASS，`DATA RACE`=0，`ok … 1.299s` |
| pi-my-compact-ui | B 档 | `7c40f89` | 26 | 7351 / 6300（共 13651 行，比值 0.86） | 137 顶层 / 137 PASS，连续 3 轮 `-race` 411 顶层 PASS，`DATA RACE`=0，`ok … 2.220s` |
| 骨架：`go/go.work` | —— | 随各组件同步更新 | 1 | 16 条 `use` 项（`.` + 14 组件 + `./pigsdk`） | 根模块与全部 14 组件 `make build/vet/test` 全绿（见 §7） |
| 骨架：`make sdk-check` | —— | —— | —— | 逐文件 md5 对比 staged SDK | `OK: pigsdk matches staged SDK (/root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef)`，EXIT=0 |

---

## 2. 提交清单（实测原文）

```console
$ git -C /root/workspace/pi-extensions log --oneline -4
7c40f89 feat(go): port pi-my-compact-ui extension to Go (batch 3)
338c5f0 feat(go): port cache-stats extension to Go (batch 3)
4a03b16 docs(go): add batch-2 migration report
f5104f8 feat(go): port ctxmode extension to Go (batch 2)
（EXIT=0）
```

逐提交 `git show --shortstat --oneline` 实测原文：

```console
338c5f0 feat(go): port cache-stats extension to Go (batch 3)
 17 files changed, 7587 insertions(+)
7c40f89 feat(go): port pi-my-compact-ui extension to Go (batch 3)
 26 files changed, 14192 insertions(+)
```

两个组件提交均在各自 commit 中同步纳入了 `go/go.work` 的 `./cache-stats` 与 `./pi-my-compact-ui` 条目，每个提交点均可独立检出并构建。

---

## 3. 逐组件报告

### 3.1 cache-stats（`338c5f0`）

- **权威源**：`/root/.pi/agent/extensions/cache-stats.ts`（2110 行，81348 字节，md5 `9b5692bfd8f7488154bf297d9e38e0a6`，SHA-256 `99cfa65fa08bcae3995a68d004f722577ba51bc50eb776074eb433c8677fe939`，与 `/root/workspace/pi-cache-stats/extensions/cache-stats.ts` 逐字节一致）。
- **实现规模**：7 个实现文件共 3788 行 + 7 个测试文件共 3544 行 = 14 个 `.go` 文件共 7332 行（连同 `go.mod`、`README.md`、`go/go.work` 共 17 个文件，测试/实现比 0.94）。
- **核心覆盖**：
  1. 只读提示缓存观测（不改写 `systemPrompt` 或 provider `payload`）；
  2. 命中率分母 `input + cacheRead + cacheWrite`、双菱形门控（◆ 累计 / ◇ 最近）、latest 语义与 50 条有界 `turnReports`；
  3. `agent_end` 空 `liveRun` 回填与 `before_provider_request` → `turn_end` 的 TPS 一次性消费；
  4. 前缀诊断分 scope 隔离（`sessionId + provider + api + model.id + hash(baseUrl)` + 16 字节随机 salt；每次观测实时查询 `ModelRegistry().GetAll()` 不缓存，缺失字段即 `skipped`）、有界遍历护栏（depth 8、nodes 2000、单串 100k、总量 250k、tools 64）、`padInt32Hex8` 带符号 16/17 字符指纹（与 Node 23 组固定向量 + 9 组 `padInt32Hex8` 向量逐字符对拍一致）；
  5. TUI 页脚渲染（herdsman 羊头状态、双命中率菱形、上下文占用、tokens/s、模型名）及 `/cache-stats` 命令族（`enable|disable|reset|prefix`）。
- **测试实测**：
  ```console
  $ cd /root/workspace/pi-extensions/go/cache-stats
  $ gofmt -l . ; go build ./... ; go vet ./... ; go test -count=1 -race -v ./...
  EXIT=0 CS_RUN=137 CS_TOP=87 CS_ALL=137 CS_FAIL=0 CS_RACE=0
  ok  	github.com/dorokuma/pi-extensions/go/cache-stats	1.299s
  ```

### 3.2 pi-my-compact-ui（`7c40f89`）

- **权威源**：`/root/.pi/agent/npm/node_modules/pi-my-compact-ui/dist/extension.ts`（2453 行，96609 字节，md5 `1c1e22f513899efa8ef3495348a92554`，SHA-256 `b9dac35123722dae7ea73dfe9f0eca5825588c07b94dc4387d93aab9223e1fb9`）。
- **实现规模**：16 个实现文件共 7351 行 + 7 个测试文件共 6300 行 = 23 个 `.go` 文件共 13651 行（连同 `go.mod`、`README.md`、`go/go.work` 共 26 个文件，测试/实现比 0.86）。
- **核心覆盖**：
  1. 生产事件流与 `ext.ToolRenderer` 挂接：`session_start` 重建历史分组并异步快照封口（`sealHistoricalGroupsFrom` 不误伤新回合分组）、`message_start(assistant)` 先重置后挂载 live assistant、`message_start(user)` 先 `finalizeActiveThinkingLocked` 再 `resetTurnLocked`、`message_update` 流式 thinking（含无索引→有索引分段与 `thinking_end` 保留增量文本）与流式 `toolcall_start/delta/end` 预备工具原地吸收转换（`absorbGroupedPreparingLocked`）、`tool_execution_start/update/end`、`agent_end` 中断结算；
  2. 树状折叠工具分组（`ToolGroupComponent`）：动态脉冲动画 ticker（`scheduleAnimation` / `stopAnimation` 持 `animTicks` 追踪并等待所有 in-flight tick 完成后返回，绝不在持锁下调用宿主）、思考摘要行（`thinkingTokenLabel` / `formatTokenK` 采用 `big.Rat.SetFloat64` 实现 IEEE-754 `(n/1000).toFixed(1)`，经 Node 在 `n=0..99000` 全量 99,001 个整数对拍零失配）、UTF-16 code unit 安全截断（`oneLine`）；
  3. 终端快捷键与双层弹窗（`Ctrl+I` 经 `pigsdk/tui` 官方 `IsKeyRelease` / `ParseKey` / `MatchesKey` 过滤 Kitty release 事件与修饰键、`/compact-inspect`、`/compact-ui-config`）：`InspectSelectorModal` 与 `DetailModalComponent` 渲染及轮询（`isModalEntryActive`）全程在 `runtimeState.mu` 锁内采集不可变快照；`sdkCustomAdapter.HandleInput` 严格区分按键消费与 `Done: true` 关闭，并在 `capturedTui == nil` 时通过 `customRoot` 在同一 `Custom` 会话内完成 selector ⇄ detail 视图切换。
- **测试实测**：
  ```console
  $ cd /root/workspace/pi-extensions/go/pi-my-compact-ui
  $ gofmt -l . ; go build ./... ; go vet ./... ; go test -count=1 -race -v ./...
  EXIT=0 CUI_RUN=137 CUI_TOP=137 CUI_ALL=137 CUI_FAIL=0 CUI_RACE=0
  ok  	github.com/dorokuma/pi-extensions/go/pi-my-compact-ui	2.220s
  ```

---

## 4. 门禁轮次与关键修复摘要

| 组件 | 门禁档位与角色 | 关键发现与修复 | 最终结论 |
|---|---|---|---|
| cache-stats | B 档（verifier R1–R4 + reviewer R1–R4） | R1–R3 修正 `padInt32Hex8` 负数 `padStart(8,"0")` 语义并补齐 Node 向量对拍、去掉 `(provider,id)` 模型 endpoint 永久缓存改为每次实时读取 `ModelRegistry().GetAll()`、将 `num()` 委托 `jsNumberString` 对齐 `1e21` 阈值；R4 切至 `magpie/group/glm-5.3` 完成 C1–C6 独立隔离验证并校准 `README.md` 全部行号 | R4 复验 0 缺陷通过 |
| pi-my-compact-ui | B 档（verifier R1–R4 + reviewer R1–R4） | R1/R2 接入生产 `ext.ToolRenderer` 与历史/流式容器驱动、修复 `sdkCustomAdapter.HandleInput` 导航键误关窗、修复 `preparing` 工具 nil 解引用、补齐渲染锁内快照与 `pigsdk/tui` 键位解析；R3 修复 `message_start(assistant)` 挂载顺序、`absorbGroupedPreparingLocked` 原地状态转换、详情弹窗 `isEntryActive` 持 `r.mu` 锁读取、`jsTenths` IEEE-754 二进制浮点舍入（99,001 整数 Node 对拍 0 失配）与 `customRoot` 真实 `Enter` 输入测试；R4 修复并发 `stopAnimation` 提前清空 `r.animTicks` 漏洞并补齐非 `--help` RPC 真实编译加载 + 坏语法对照与全部回归的独立隔离变异 FAIL→PASS 证据 | R4 复验 0 缺陷通过 |

---

## 5. TS → Go 关键架构与语义说明

1. **`cache-stats` 的 Go 值模型与键序处理**：
   - PiG SDK 传入的事件负载为解码后的 `map[string]any` / `[]any`，不存在 JS `Proxy` getter trap，故 TS 的 `readDataOwn` 四态在 Go 中收敛为 `data` / `missing` 二态，同时完整保留全部 depth / node / char / 循环引用 / 非有限数预算护栏；
   - 普通 `map[string]any` 按字典序稳定遍历，`sdk.OrderedObject` 保留插入序，同进程跨轮次指纹判定稳定。
2. **`pi-my-compact-ui` 的子进程 UI 适配**：
   - TS 原版在同进程内直接 monkey-patch 宿主 `Container.prototype` / `AssistantMessageComponent.prototype` / `CompactionSummaryMessageComponent.prototype`；
   - Go 版作为 PiG 独立子进程扩展，通过 `ext.ToolRenderer` + `ctx.SetHiddenThinkingLabel("")` + `ctx.OnTerminalInput` + `ctx.SetWidgetRenderer` + `ctx.Custom`（含 `customRoot` 单会话 selector/detail 切换）工作，并在扩展内存模型中完整维护 `chatContainer` / `AssistantMessageComponent` / `ToolGroupComponent` 树供 `ToolRenderer` 与双层弹窗消费。

---

## 6. 遗留与未跑（如实列）

1. **真实 TUI 交互式观感**：非交互模式（`-p` / `--mode rpc`）不驱动真实终端 TUI 帧渲染与按键交互，`/cache-stats`、`/compact-inspect`、`/compact-ui-config`、`Ctrl+I` 弹窗与折叠工具卡片的逻辑、快照及并发安全性已由高强度 `-race` 单测全覆盖，真实 TUI 视觉观感留待后续切流验收阶段人工确认。
2. **`pi-my-compact-ui` 源码注释中的历史 TS 行号**：Go 源码注释中保留的 157 处 `(L###-###)` 指向移植初期拆分稿，`go/pi-my-compact-ui/README.md` 已统一改用 `dist/extension.ts`（2453 行）与 Go 源码的实测精确行号（212 处引用 0 漂移）。
3. **第 4 批与第 5 批并行任务**：按分工由其它工作区在 `/root/workspace/.wt/` 各分支独立推进，不在本报告范围内。
4. **未跑项清单**：`make examples-smoke`（会发起真实模型请求，按规矩不跑）；生产切流（在爸爸批准前，线上仍跑 TS 版）。

---

## 7. 复现方式与隔离证据

### 7.1 必跑验证原文

```console
$ cd /root/workspace/pi-extensions/go && make build vet test sdk-check ; echo EXIT=$?
==> [root module] go build ./...
==> [component: auto-continue] go build ./...
==> [component: cache-stats] go build ./...
==> [component: codegraph-go] go build ./...
==> [component: ctxmode] go build ./...
==> [component: ctx-orchestrate] go build ./...
==> [component: exclude-tools-guard] go build ./...
==> [component: herdr-agent-state] go build ./...
==> [component: jev-gate] go build ./...
==> [component: moshi-hooks] go build ./...
==> [component: no-tables] go build ./...
==> [component: pi-context-pruning] go build ./...
==> [component: pi-env] go build ./...
==> [component: pi-my-compact-ui] go build ./...
==> [component: pi-tinyfish] go build ./...
==> [root module] go vet ./...
==> [component: auto-continue] go vet ./...
==> [component: cache-stats] go vet ./...
==> [component: codegraph-go] go vet ./...
==> [component: ctxmode] go vet ./...
==> [component: ctx-orchestrate] go vet ./...
==> [component: exclude-tools-guard] go vet ./...
==> [component: herdr-agent-state] go vet ./...
==> [component: jev-gate] go vet ./...
==> [component: moshi-hooks] go vet ./...
==> [component: no-tables] go vet ./...
==> [component: pi-context-pruning] go vet ./...
==> [component: pi-env] go vet ./...
==> [component: pi-my-compact-ui] go vet ./...
==> [component: pi-tinyfish] go vet ./...
==> [root module] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/internal/version	(cached)
==> [component: auto-continue] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/auto-continue	(cached)
==> [component: cache-stats] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/cache-stats	(cached)
==> [component: codegraph-go] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/codegraph-go	0.217s
==> [component: ctxmode] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/ctxmode	(cached)
==> [component: ctx-orchestrate] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/ctx-orchestrate	(cached)
==> [component: exclude-tools-guard] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/exclude-tools-guard	0.209s
==> [component: herdr-agent-state] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/herdr-agent-state	0.428s
==> [component: jev-gate] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/jev-gate	(cached)
==> [component: moshi-hooks] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/moshi-hooks	0.347s
==> [component: no-tables] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/no-tables	(cached)
==> [component: pi-context-pruning] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-context-pruning	(cached)
==> [component: pi-env] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-env	(cached)
==> [component: pi-my-compact-ui] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-my-compact-ui	1.059s
==> [component: pi-tinyfish] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-tinyfish	(cached)
Checking pigsdk against staged SDK: /root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef
OK: pigsdk matches staged SDK (/root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef)
EXIT=0
```

PiG 隔离加载（`./bin/pig-test -e ./<comp> --help` 及 `--mode rpc` 真实编译加载）实测：

```console
PIG_HELP_cache-stats=0
PIG_HELP_pi-my-compact-ui=0
PIG_RPC_pi-my-compact-ui=0
```

（此外在隔离副本中向 `pi-my-compact-ui/extension.go` 末尾追加语法错误 `func broken(` 后运行 `./bin/pig-test -e ./pi-my-compact-ui --mode rpc </dev/null` 实测 `EXIT=1` 并报 `extension.go:1048:14: expected ')', found 'EOF'`，还原后 `EXIT=0`，证明真实编译并加载了该扩展。）

### 7.2 生产扩展零触碰证据（md5 实测）

```console
$ md5sum /root/.pi/agent/extensions/*.ts /root/.pi/agent/npm/node_modules/pi-my-compact-ui/dist/extension.ts | sort -k2
55775e1fda107b4da9704c007d33b7b8  /root/.pi/agent/extensions/auto-continue.ts
9b5692bfd8f7488154bf297d9e38e0a6  /root/.pi/agent/extensions/cache-stats.ts
fc35f79118e4b655f07a4fadf793f5dd  /root/.pi/agent/extensions/codegraph-go.ts
5f925d4921788c30dda994192d29c15a  /root/.pi/agent/extensions/ctxmode.ts
0e33386a7178868cbefe31969f08720b  /root/.pi/agent/extensions/ctx-orchestrate.ts
30722dbb05328b63a2cb87729072b7a0  /root/.pi/agent/extensions/exclude-tools-guard.ts
c5e0582adbba7fba3bcba83b08274a79  /root/.pi/agent/extensions/herdr-agent-state.ts
3d909e1a87dd28a8d21e7e9b00668bad  /root/.pi/agent/extensions/herdsman-pi.ts
f4ff427bb51b6fa378fcfd356b81fa99  /root/.pi/agent/extensions/jev-gate.ts
3f8ec8443daf30e582d57eec99df383c  /root/.pi/agent/extensions/moshi-hooks.ts
a1f215b9843f38943cf86df38b4205f5  /root/.pi/agent/extensions/no-tables.ts
1c1e22f513899efa8ef3495348a92554  /root/.pi/agent/npm/node_modules/pi-my-compact-ui/dist/extension.ts
```

BATCH-3 DONE
