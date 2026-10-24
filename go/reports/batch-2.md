# 第 2 批（核心件）：6 个扩展的 Go 移植与第 1 批遗留收口报告（reports/batch-2.md）

| 项 | 值（均为本次实测/实读） |
|---|---|
| 分支 | `feature/go-batch1`（`git status --porcelain` 首行 `## feature/go-batch1`） |
| HEAD（写报告时） | `f5104f8` feat(go): port ctxmode extension to Go (batch 2) |
| 报告会话起点 | `2026-10-25 02:57:32 +0800`（`date '+START %Y-%m-%d %H:%M:%S %z'`） |
| 工具链实测 | `go version go1.26.4 linux/amd64`（EXIT=0）；`pig --version` → `0.5.2+1.4.0`（EXIT=0） |
| 工作目录 | `/root/workspace/pi-extensions/go` |

证据范围声明：

- 本报告只写本次会话**亲自跑过**的命令输出与**亲眼读到**的文件内容；每条命令贴原文并显式捕获 `EXIT=$?`（含 `make build/vet/test/sdk-check`、6 个第 2 批组件及 `no-tables` 的 gofmt/build/vet/`-count=1 -race -v` 测试、PiG 隔离加载）。
- 未跑的都写「未跑」并给原因（见 §7）。
- 全程未打印任何密钥，未改动 `/root/.pi/agent/extensions/*.ts` 生产部署件与各自外生宿主仓（`/root/workspace/codegraph-go`、`/root/workspace/ctxmode`）。

---

## 1. 批次结论摘要

| 组件 | 档位 | commit | 提交文件数 | Go 实现 / 测试行数 | 测试状态（`-count=1 -race -v`） |
|---|---|---|---|---|---|
| auto-continue | B 档 | `b271d34` | 16 | 2032 / 2384 | 65 顶层 / 179 PASS（含子用例），`DATA RACE`=0，`ok … 1.057s` |
| ctx-orchestrate | B 档 | `334659d` | 13 | 1182 / 1283 | 41 顶层 / 94 PASS（含子用例），`DATA RACE`=0，`ok … 1.072s` |
| jev-gate | C 档 | `67b7ae7` | 16 | 2612 / 2029 | 59 顶层 / 171 PASS（含子用例），`DATA RACE`=0，`ok … 3.288s` |
| moshi-hooks | C 档 | `130d2e6` | 15 | 1578 / 1459 | 59 顶层 / 99 PASS（含子用例），`DATA RACE`=0，`ok … 1.403s` |
| codegraph-go | B 档 | `9680a46` | 12 | 1287 / 1060 | 23 顶层 / 61 PASS（含子用例），`DATA RACE`=0，`ok … 1.371s` |
| ctxmode | B 档 | `f5104f8` | 13 | 1089 / 1538 | 46 顶层 / 58 PASS（含子用例），`DATA RACE`=0，`ok … 5.021s` |
| no-tables（第 1 批遗留收口） | B 档 | `f843580` | 3 | 745 / 720 | 38 顶层 / 82 PASS（含子用例），`DATA RACE`=0，`ok … 1.043s` |
| 骨架：`go/go.work` | —— | 随各组件同步更新 | 1 | 14 条 `use` 项（`.` + 12 组件 + `./pigsdk`） | 根模块与全部 12 组件 `make build/vet/test` 全绿（见 §8） |
| 骨架：`make sdk-check` | —— | —— | —— | 逐文件 md5 对比 staged SDK | `OK: pigsdk matches staged SDK (/root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef)`，EXIT=0 |

---

## 2. 提交清单（实测原文）

```console
$ git -C /root/workspace/pi-extensions log --oneline -9
f5104f8 feat(go): port ctxmode extension to Go (batch 2)
9680a46 feat(go): port codegraph-go extension to Go (batch 2)
130d2e6 feat(go): port moshi-hooks extension to Go (batch 2)
f843580 fix(no-tables): align CRLF and CR line terminator handling with TS
67b7ae7 feat(go): port jev-gate extension to Go (batch 2)
334659d feat(go): port ctx-orchestrate extension to Go (batch 2)
fed8853 docs(go): add batch-2 handoff document
b271d34 feat(go): port auto-continue extension to Go (batch 2)
7ab5bb7 fix(exclude-tools-guard): strip leaked APPEND_SYSTEM addendum in subagent sessions
（EXIT=0）
```

逐提交 `git show --shortstat --oneline` 实测原文：

```console
b271d34 | b271d34 feat(go): port auto-continue extension to Go (batch 2)  16 files changed, 4781 insertions(+)
fed8853 | fed8853 docs(go): add batch-2 handoff document  1 file changed, 136 insertions(+)
334659d | 334659d feat(go): port ctx-orchestrate extension to Go (batch 2)  13 files changed, 2758 insertions(+)
67b7ae7 | 67b7ae7 feat(go): port jev-gate extension to Go (batch 2)  16 files changed, 4875 insertions(+)
f843580 | f843580 fix(no-tables): align CRLF and CR line terminator handling with TS  3 files changed, 330 insertions(+), 121 deletions(-)
130d2e6 | 130d2e6 feat(go): port moshi-hooks extension to Go (batch 2)  15 files changed, 3304 insertions(+)
9680a46 | 9680a46 feat(go): port codegraph-go extension to Go (batch 2)  12 files changed, 2552 insertions(+)
f5104f8 | f5104f8 feat(go): port ctxmode extension to Go (batch 2)  13 files changed, 3287 insertions(+)
```

吸取第 1 批教训：第 2 批全部 6 个组件提交均在各自 commit 中同步纳入了 `go/go.work` 的 `./<组件>` 条目，每个提交点均可独立检出并构建。

---

## 3. 第 1 批遗留收口：no-tables CRLF/CR 行终止符对齐（`f843580`）

- **问题回顾**：第 1 批 `1afde07` 中，Go RE2 的 `.` 与 `(?m)$` 仅以 `\n` 为行终止符，而 JS RegExp 的 `.` 与 `m$` 将 `\r`、`\n`、U+2028、U+2029 均视为行终止符。
- **修复范围**（`f843580`）：
  - 新增 `splitPreserveJSLineTerminators` 与 `joinSegments`，将 `reTable`、`reOpenFence`、`reHeading`、`reFootDef` 及行级扫描统一对齐到 `[\n\r]`（CRLF 与孤立 CR 均与 TS 行为逐字节一致）。
  - U+2028 / U+2029 按第 1 批拍板保持仅文档化并在 `TestKnownDeviations_LineTerminators` 钉死。
- **实测结果**：`go/no-tables` 下 `go test -count=1 -race -v ./...` 共 38 顶层 / 82 PASS（含子用例），`DATA RACE`=0，`ok github.com/dorokuma/pi-extensions/go/no-tables 1.043s`。

---

## 4. 逐组件报告

### 4.1 auto-continue（`b271d34`）

- **权威源**：`/root/workspace/pi-extensions/extensions/auto-continue/src/auto-continue.ts`（1328 行，与 `/root/.pi/agent/extensions/auto-continue.ts` 逐字节一致，md5 `55775e1fda107b4da9704c007d33b7b8`）。
- **实现规模**：2032 行实现 + 2384 行测试（共 16 个文件，含 `go/go.work` 更新）。
- **核心覆盖**：主/子代理双模式分流、7 条强停信号、9 条延续信号、6 条元层泄漏检测、5 档渐进提示词、冷却窗口与上限、`/auto-continue` 命令族（`on|off|sub|status|reset|limit|cooldown`）及自定义状态条目持久化。
- **测试实测**：
  ```console
  $ cd /root/workspace/pi-extensions/go/auto-continue
  $ gofmt -l . ; go build ./... ; go vet ./... ; go test -count=1 -race -v ./...
  EXIT=0 RUN=179 PASS_TOP=65 PASS_ALL=179 FAIL=0 RACE=0
  ok  	github.com/dorokuma/pi-extensions/go/auto-continue	1.057s
  ```

### 4.2 ctx-orchestrate（`334659d`）

- **权威源**：`/root/workspace/pi-extensions/extensions/ctx-orchestrate/src/ctx-orchestrate.ts`（1050 行，与 `/root/.pi/agent/extensions/ctx-orchestrate.ts` 逐字节一致，md5 `0e33386a7178868cbefe31969f08720b`）。
- **实现规模**：1182 行实现 + 1283 行测试（共 13 个文件，含 `go/go.work` 更新）。
- **核心覆盖**：`read`/`bash`/`grep`/`find`/`ls` 预算制重定向与硬拦截、主代理 vs 7 类子代理（`worker`/`scout`/`researcher`/`planner`/`reviewer`/`verifier`/`oracle`）矩阵、CodeGraph 优先引导、`/ctx-orchestrate` 命令族（`status|reset|strict|lenient|off`）。
- **测试实测**：
  ```console
  $ cd /root/workspace/pi-extensions/go/ctx-orchestrate
  $ gofmt -l . ; go build ./... ; go vet ./... ; go test -count=1 -race -v ./...
  EXIT=0 RUN=94 PASS_TOP=41 PASS_ALL=94 FAIL=0 RACE=0
  ok  	github.com/dorokuma/pi-extensions/go/ctx-orchestrate	1.072s
  ```

### 4.3 jev-gate（`67b7ae7`）

- **权威源**：`/root/workspace/pi-extensions/extensions/jev-gate/src/jev-gate.ts`（1267 行，与 `/root/.pi/agent/extensions/jev-gate.ts` 逐字节一致，md5 `f4ff427bb51b6fa378fcfd356b81fa99`）。
- **实现规模**：2612 行实现 + 2029 行测试（共 16 个文件，含 `go/go.work` 更新）。
- **核心覆盖**：MCP over stdio 桥接 `/root/bin/jev`（`jev_check` 工具注册、懒启动、异常退出指数退避自动重连、请求级超时与 `notifications/cancelled`）、主代理软门禁（`agent_end` 脏文件检测、2 轮提醒上限、`git commit/push` 前未验提醒、`/jev-status`、`/jev-start`、`/jev-stop`、`/jev-gate`）。
- **测试实测**：
  ```console
  $ cd /root/workspace/pi-extensions/go/jev-gate
  $ gofmt -l . ; go build ./... ; go vet ./... ; go test -count=1 -race -v ./...
  EXIT=0 RUN=171 PASS_TOP=59 PASS_ALL=171 FAIL=0 RACE=0
  ok  	github.com/dorokuma/pi-extensions/go/jev-gate	3.288s
  ```

### 4.4 moshi-hooks（`130d2e6`）

- **权威源**：`/root/.pi/agent/extensions/moshi-hooks.ts`（898 行，md5 `3f8ec8443daf30e582d57eec99df383c`，与 `/root/workspace/pi-extensions/extensions/herdsman-bridge/src/moshi-hooks.ts` 逐字节一致）。
- **实现规模**：1578 行实现 + 1459 行测试（共 15 个文件，含 `go/go.work` 更新）。
- **核心覆盖**：`herdr_Inbox` / `herdr_Outbox` 落盘、UNIX domain socket 推送 herdsman daemon（`SOCK_Path`、1.5s 超时静默降级）、`[herdsman-wake]` 幂等唤醒与去重、跨窗格状态聚合与 `before_agent_start` 上下文注入。
- **测试实测**：
  ```console
  $ cd /root/workspace/pi-extensions/go/moshi-hooks
  $ gofmt -l . ; go build ./... ; go vet ./... ; go test -count=1 -race -v ./...
  EXIT=0 RUN=99 PASS_TOP=59 PASS_ALL=99 FAIL=0 RACE=0
  ok  	github.com/dorokuma/pi-extensions/go/moshi-hooks	1.403s
  ```

### 4.5 codegraph-go（`9680a46`）

- **权威源**：`/root/.pi/agent/extensions/codegraph-go.ts`（1126 行，md5 `fc35f79118e4b655f07a4fadf793f5dd`，与 `/root/workspace/codegraph-go/integrations/pi/codegraph-go.ts` 逐字节一致）。
- **实现规模**：1287 行实现 + 1060 行测试（共 12 个文件，含 `go/go.work` 更新）。
- **核心覆盖**：MCP over stdio 桥接 `codegraph` 二进制、注册单一 `codegraph` 多动作原生工具（`status`/`context`/`callers`/`callees`/`impact`/`search`/`source`/`files`/`symbols`/`stats`/`project`/`read`）、握手/超时/Esc 取消且不重放失败 `tools/call`、`/codegraph-start` 与 `/codegraph-stop` 命令、`.codegraph.db` 存在性感知与 `before_agent_start` 提示词注入。
- **测试实测**：
  ```console
  $ cd /root/workspace/pi-extensions/go/codegraph-go
  $ gofmt -l . ; go build ./... ; go vet ./... ; go test -count=1 -race -v ./...
  EXIT=0 RUN=61 PASS_TOP=23 PASS_ALL=61 FAIL=0 RACE=0
  ok  	github.com/dorokuma/pi-extensions/go/codegraph-go	1.371s
  ```

### 4.6 ctxmode（`f5104f8`）

- **权威源**：`/root/.pi/agent/extensions/ctxmode.ts`（960 行，md5 `5f925d4921788c30dda994192d29c15a`，与 `/root/workspace/ctxmode/integrations/pi/ctxmode.ts` 逐字节一致）。
- **实现规模**：1089 行实现 + 1538 行测试（共 13 个文件，含 `go/go.work` 更新与 `tools.json` 嵌入定义）。
- **核心覆盖**：MCP over stdio 桥接 `ctxmode` 二进制、注册 6 个原生工具（`ctx_run`/`ctx_fs`/`ctx_git`/`ctx_kb`/`ctx_bg`/`ctx_stats`）、`StripANSI` + UTF-16 code unit 代理对安全截断 + 头尾行压缩（`CompressToolText`）、只读角色环境推导（`ResolveRoleEnv`）、写锁与状态锁解耦（stdin 阻塞不挡 Esc 取消）、握手超时同步收尸、`/ctxmode-start` 与 `/ctxmode-stop` 命令。
- **测试实测**：
  ```console
  $ cd /root/workspace/pi-extensions/go/ctxmode
  $ gofmt -l . ; go build ./... ; go vet ./... ; go test -count=1 -race -v ./...
  EXIT=0 RUN=58 PASS_TOP=46 PASS_ALL=58 FAIL=0 RACE=0
  ok  	github.com/dorokuma/pi-extensions/go/ctxmode	5.021s
  ```

---

## 5. 门禁轮次与关键修复摘要

| 组件 | 门禁档位与角色 | 关键发现与修复 | 最终结论 |
|---|---|---|---|
| auto-continue | B 档（verifier + reviewer） | 补齐主/子代理分流边界、元层泄漏正则与渐进提示词轮次计数测试 | 0 缺陷通过 |
| ctx-orchestrate | B 档（verifier + reviewer） | 补齐只读子代理与 worker 预算差异、CodeGraph 引导条件与命令状态切换测试 | 0 缺陷通过 |
| jev-gate | C 档（verifier + reviewer + oracle） | 补齐 MCP 握手失败回收、脏文件快照比对、软门禁提醒上限与并发请求取消隔离 | 0 缺陷通过 |
| moshi-hooks | C 档（verifier + reviewer + oracle） | 修正 PiG 嵌套模块工厂包命名约定（`package moshihooks` 导出 `Extension()`，避免 standalone root 冲突）、补齐 socket 超时降级与幂等唤醒去重 | 0 缺陷通过 |
| codegraph-go | B 档（verifier R1/R2 + reviewer R1/R2） | R1 发现 `package main` 在嵌套子模块下触发 PiG standalone module root 报错、握手超时残留子进程、请求预算与 Esc 取消竞态；R2 改为 `package codegraphgo` 工厂包、同步收尸、修正 `CTXMODE`/`CODEGRAPH` 超时旋钮解析并完成真实 RPC smoke | R2 复验 0 缺陷通过 |
| ctxmode | B 档（verifier R1–R3 + reviewer R1–R3） | R1/R2 改为 `package ctxmode` 工厂包、握手超时同步回收、stdin 写锁与状态锁解耦防阻塞取消、修复正常完成时取消 watcher goroutine 泄漏；R3 补齐 `reconnect` 取消分支无条件 `shutDownProc()`、超时旋钮 `envPositiveNumber` 防 0ms 自锁、`envNumber` 超大浮点数 int32 饱和防溢出，以及 `TestSDKCancellationAndCommands` 跨进程文件断言 `waitFor` | R3 复验 + 回归全绿 |

---

## 6. 架构发现：PiG 嵌套模块工厂包约定

在第 2 批 `moshi-hooks`、`codegraph-go`、`ctxmode` 的真实 `./bin/pig-test -e ./<name>` 隔离加载中确认了 PiG 的 Go 扩展加载分流规则：

- 当 `go/<name>/` 拥有独立 `go.mod` 且包名为 `package main` 时，PiG 将其识别为 standalone 扩展并强制要求其工作区 module root 与顶层一致，报错 `Go standalone /root/workspace/pi-extensions/go/<name> must select its exact module root /root/workspace/pi-extensions/go`。
- 当 `go/<name>/` 使用非 `main` 包名（如 `package moshihooks`、`package codegraphgo`、`package ctxmode`）并导出 `func Extension() *sdk.Extension`（不含 `main()` 或 `init()`）时，PiG 将其作为 factory 扩展生成 packed-cell runner，既能通过 `go/go.work` 在本地独立 `go build/vet/test`，也能被 `./bin/pig-test -e ./<name>` 无缝编译加载。

---

## 7. 遗留与未跑（如实列）

1. **第 3 批剩余 1 件（`cache-stats`）**：按 `go/PLAN.md` 分批计划归属第 3 批，本批未启动。
2. **TS 侧 `extensions/exclude-tools-guard/*` 两处未提交改动**：按 `go/PLAN.md` 铁律第 2 条保持原样未触碰、未暂存。
3. **未跑项清单**：`make examples-smoke`（会发起真实模型请求，按规矩不跑）；生产切流（在第 3 批完成且用户批准前，线上仍跑 TS 版）。

---

## 8. 复现方式与隔离证据

### 8.1 必跑验证原文

```console
$ cd /root/workspace/pi-extensions/go && make build vet test sdk-check ; echo EXIT=$?
==> [root module] go build ./...
==> [component: auto-continue] go build ./...
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
==> [component: pi-tinyfish] go build ./...
==> [root module] go vet ./...
==> [component: auto-continue] go vet ./...
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
==> [component: pi-tinyfish] go vet ./...
==> [root module] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/internal/version	(cached)
==> [component: auto-continue] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/auto-continue	(cached)
==> [component: codegraph-go] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/codegraph-go	(cached)
==> [component: ctxmode] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/ctxmode	3.745s
==> [component: ctx-orchestrate] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/ctx-orchestrate	(cached)
==> [component: exclude-tools-guard] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/exclude-tools-guard	0.210s
==> [component: herdr-agent-state] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/herdr-agent-state	0.440s
==> [component: jev-gate] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/jev-gate	2.121s
==> [component: moshi-hooks] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/moshi-hooks	0.356s
==> [component: no-tables] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/no-tables	0.005s
==> [component: pi-context-pruning] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-context-pruning	(cached)
==> [component: pi-env] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-env	(cached)
==> [component: pi-tinyfish] go test ./...
ok  	github.com/dorokuma/pi-extensions/go/pi-tinyfish	(cached)
Checking pigsdk against staged SDK: /root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef
OK: pigsdk matches staged SDK (/root/.pig-test/state/pigsdk/sdk-0c5774f119b9fdef)
EXIT=0
```

PiG 隔离加载（`./bin/pig-test -e ./<comp> --help`）实测：

```console
PIG_HELP_auto-continue=0
PIG_HELP_ctx-orchestrate=0
PIG_HELP_jev-gate=0
PIG_HELP_moshi-hooks=0
PIG_HELP_codegraph-go=0
PIG_HELP_ctxmode=0
PIG_HELP_no-tables=0
```

（此外 `codegraph-go` 与 `ctxmode` 均通过了无模型 `--mode rpc` 真实子进程桥接 smoke 验证，详见各自 `README.md`。）

### 8.2 生产扩展零触碰证据（md5 实测）

```console
$ md5sum /root/.pi/agent/extensions/*.ts | sort -k2
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
```

BATCH-2 DONE
