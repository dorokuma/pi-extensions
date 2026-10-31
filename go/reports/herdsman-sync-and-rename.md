# herdsman 同步修复 + 改名/归档报告

| 项 | 值 |
|---|---|
| 工作树 | `/root/workspace/.wt/pi-extensions/fix-herdsman-bridge` |
| 分支 | `fix/herdsman-bridge` |
| commit 1（修复） | `3eed329` `fix(herdsman-pi): skip daemon on one-shot runs and close superseded sockets` |
| commit 2（改名+归档） | 见文末「最终 commit 列表」 |
| 上游对照 commit | `7558d7f` `fix(herdsman-pi): close superseded sockets and skip one-shot sessions` |

分两部分写：先修复、后改名与归档，与两个 commit 的顺序一致。

---

## 第一部分：同步上游 `7558d7f` 的两处修复

上游这次提交做了三件事：一次性/无会话场景跳过 daemon client、重连/会话重置时
主动关闭被顶替的旧 socket 并取消未决重连定时器、以及给 socket/定时器 `unref`（Node
专有，无 Go 对应物）。Go 移植的判定如下。

### 1.1 一次性/无会话场景跳过 daemon client（**新增**）

上游 `shouldSkipDaemonClient(event, ctx, sessionRef)`：无 session file、`hasUI === false`、
或 `mode === "print" | "json"` 时根本不创建 daemon client。Go 端此前没有这层判定，
`session_start` 一律 `NewReconnectingDaemonClient`，`pi --print` 会挂在 `herdsman.sock` 上。

改动（commit `3eed329`，当时目录还叫 `go/herdsman-pi`，下同）：

- `go/herdsman-pi/extensionhost.go` — `Context` 接口新增 `HasUI() (hasUI, known bool)`
  与 `Mode() (mode string, known bool)`。两个 `known` 返回值对应 TS「字段缺席=未知、
  不当作非交互」的语义：TS 里 `hasUI`/`mode` 是可选字段，缺席时必须保持旧行为。
- `go/herdsman-pi/extensionlifecycle.go` — 新增 `shouldSkipDaemonClient`，并在
  `HandleSessionStart` 的 `launchIdentity` 判定之后、建 client 之前调用。命中时走与上游
  相同的清理序列：`stopKeepalive` → 关掉旧 client → `client = nil` → `connected=false`
  → `loseRole` → return。`HandleSessionStart` 签名加 `eventMode *string`（事件自带的
  mode，对应 TS `event.mode`；nil=事件没带，回落到 `ctx.Mode()`）。
- `go/herdsman-pi/extensionentry.go` — `EventSessionStart` 回调把 `data["mode"]`（非空
  字符串才取）作为 `eventMode` 传入；`sdkContext` 实现 `HasUI()`/`Mode()`。

SDK 判定面（以 `go/pigsdk` 实际定义为准，非凭空假设）：

- `Context.Mode()`（`context.go`）返回 ready 消息里的 `mode`，注释明确「Defaults to
  `"print"` when the host did not specify one」——即 SDK 把「未上报 mode」直接归一为
  `print`。这与 TS「mode 缺席=未知、不 skip」不同：在 SDK 语义下未上报 mode 的主就是
  非 UI 运行，正是要跳过的场景。所以 `sdkContext.Mode()` 永远 `known=true`。
- `Context.HasUI()`（`context.go`）读 ready/state 快照里的 `hasUI`，同样有明确语义
  （print/json 无 UI）。`sdkContext.HasUI()` 也永远 `known=true`。
- 偏差：TS 还能容忍「mode/hasUI 字段缺席」当作未知；SDK 把两者都归一化了（mode 缺席→
  `print`，hasUI 缺席→`false`）。等价信号下 `sdkContext` 不存在「未知」分支，但
  `Context` 接口保留了 `known` 返回值，让注入的测试/其它 Context 仍能表达「未知不
  skip」——`fakeContext` 用 `hasUI *bool`/`mode string` 复刻了 TS 的缺席语义。

### 1.2 重连/会话重置时关闭旧 socket + 取消未决重连定时器（**已具备，补回归测试**）

上游修法是 `#connect()`/`resetForSession()`/`close()` 先 `#clearReconnectTimer()` +
`#destroySocket()`，再让被顶替的 socket 在 `connect`/`data` 回调里自查 generation 自毁。

Go 移植时这条路径用了 generation 闸门实现，等价语义已就位：`beginConnectLocked`
在临界区内把上一个 `c.socket` 作为 `superseded` 取出并清空，`dialGeneration` 解锁后先
`superseded.Close()` 再拨新 socket；新 socket 落地时若 `generation` 已被更新（新一次
attempt 或 `Close`）则 `socket.Close()` 自毁——这正是上游在 `connect`/`data` 回调里补的
backstop。因此本部分**没有改 `daemonclient.go` 的生产代码**，只补了把该契约钉住的回归
测试：

- `daemonclient_test.go` — `TestClientResetForSessionLeavesNoOrphanConnection`（构造后
  立即 `ResetForSession`，断言只剩 1 条 live 连接、无孤儿）、
  `TestClientCloseAfterFailedDialLeavesNoOrphan`（拨号失败挂起 backoff，`Close` 后定时器
  被取消且不再拨号）、`TestClientSupersededDialIsClosedNotAdopted`（过期 generation 的
  `dialGeneration` 落地即销毁，不被收养）。
- 给测试用 `fakeDaemon` 加 `LiveConnections()`/`removeLive()`，按服务端实际存活连接
  计数，断言「无孤儿 ESTABLISHED」。
- `extension_test.go` — `TestExtensionSkipsDaemonClientOnOneShotRuns` 六个子用例：context
  mode=print / json、`hasUI=false`、事件 mode 优先、无 session file 且关掉旧 client、
  未知 mode 的交互会话仍连接。
- `harness_test.go` — `fakeContext` 加 `hasUI *bool`/`mode string` 及对应访问器；所有
  `HandleSessionStart` 调用点改传 `nil`（事件未带 mode，走 context）。

#### 第一部分验证

| 命令 | 退出码 | 说明 |
|---|---|---|
| `cd go && go build ./herdsman-pi/` | 0 | 改名前模块 |
| `cd go && go vet ./herdsman-pi/` | 0 | |
| `cd go && go test ./herdsman-pi/` | 0 | 15.7s，全部通过（含 3 个新 client 测试 + 1 个 6 子用例测试）|
| 新增用例单跑 `-run ...` | 0 | 9/9 PASS |

行为变化仅落在 session_start：一次性/无会话场景不再创建 client；daemon wire 协议、重连
退避序列、注册载荷均未动。

---

## 第二部分：改名 `herdsman-pi` → `herdsman` + 归档 TS 桥

理由：爸爸定名——Go 版就叫 `herdsman`，不带 `pi` 也不带 `bridge`。此前 `6add117`
把它改成 `herdsman-bridge` 属于改错了名，本次一并改正（历史经 rebase 重写，分支上只留
正确的改名轨迹）。

**module 路径冲突判定**：本扩展 Go module 定为
`github.com/dorokuma/pi-extensions/go/herdsman`；上游 herdsman 守护进程仓的 Go module 是
`github.com/dorokuma/herdsman/go`（`/root/workspace/herdsman/go/go.mod`）。两者前缀不同
（`pi-extensions/go/herdsman` vs `herdsman/go`）、互不包含，**不冲突**，可直接用该名。

### 改了什么（本任务的改名+归档范围）

| 项 | 文件 | 改动 |
|---|---|---|
| Go 目录 | `go/herdsman-pi/` → `go/herdsman/` | `git mv`，38 个文件历史保留（`6add117` 期间曾短暂叫 `go/herdsman-bridge`） |
| module 路径 | `go/herdsman/go.mod` | `.../go/herdsman-pi` → `.../go/herdsman` |
| workspace | `go/go.work` | `./herdsman-pi` → `./herdsman` |
| 包名 | 全部 `*.go` | `package herdsmanpi` → `package herdsman` |
| 扩展身份 | `agentdisplay.go` | `sdkLogName "herdsman-pi"` → `"herdsman"`（`sdk.New` 的注册名）|
| 错误文案 | `extensionentry.go` | `errNoHostContext` 前缀 `herdsman-pi:` → `herdsman:` |
| 日志面 | `agentdisplay.go` 等 | `logs/herdsman-pi-*.log` 文件名与 `[herdsman-pi]` 日志标签 → `herdsman`：它们是这个扩展自己的身份标识，不在 daemon wire 协议里 |
| TS 桥归档 | `extensions/herdsman-bridge/` → `archive/herdsman-bridge-ts/` | `git mv`（2 个文件：README.md + src/herdsman-bridge.ts）；并打 tag `archive/herdsman-bridge-ts-20261031` 指向 `d94bec5`（归档前 main 上该 TS 的最后状态/合并基点）|
| registry | `federated/registry.json` | 删除 `herdsman-bridge` extension 条目（hosted 6 个，全机 10 个）；`_comment` 计数同步 |
| install.sh | 头部注释、usage、guard | 移除只针对该 TS 桥的 herdsman 双加载防护（`guard_herdsman_doubleload` 整函数 + `install_hosted` 中的调用点 + `PI_EXT_SETTINGS_JSON` 环境变量登记 + 头注说明）；audit 差集注释里的 herdsman-bridge→herdsman-pi.ts 异名例子改为 pi-cache-stats→cache-stats.ts；计数 11→10、hosted 7→6 |
| 根 README | 矩阵/环境变量/已知事项/目录布局/计数 | 删 herdsman-bridge 矩阵行与异名注解、`PI_EXT_SETTINGS_JSON` 行、双加载防护保护范围段；tsc 段 herdsman-bridge 说明改为「已归档至 archive/」；计数 11→10 / 7→6 |
| go/PLAN.md | 3 处 | 「herdsman-pi（扩展壳…）」行、「herdsman-pi(3889…)」、「TS 版 herdsman-pi 和 Go 版 herdsman-pi」→ herdsman（现行口径）|
| go/README.md | 1 处 | 第 4 批条目 `herdsman-pi 扩展侧` → `herdsman 扩展侧` |
| go/herdr-agent-state/README.md | 1 处 | `herdsman-pi` 扩展 → `herdsman` 扩展（现行口径）|

### 没改什么（刻意保留，含留存理由）

- **上游 TS 引用注释**（`go/herdsman/` 内 port-of 注释）：`packages/herdsman-pi/src/...`、
  `test/unit|integration/herdsman-pi-*.test.ts`、`@dorokuma/herdsman-pi`、`TS herdsman-pi
  extension`、`createHerdsmanPiExtension`/`logHerdsmanPi`/`HerdsmanPiLogLevel` 等镜像上游
  命名的标识符——它们指向上游真实文件/包名/符号，改名会让 port-of 注释指错路径。
- **`go/reports/`**：其余历史报告（含 `batch-4-herdsman-pi.md`、`PLAN.v1.md`）按任务要求
  不动；本报告自身由 `herdsman-bridge-sync-and-rename.md` `git mv` 改名而来。
- **`archive/herdsman-bridge-ts/`**：归档内容原样保留（含 README 里的 herdsman-pi 事实
  引用），不做改写。
- **`AGENTS.md`**：顶部规则文件仍写 `herdsman-bridge`，其更新属主代理职责（规则文件修改
  例外），worker 不动。
- **install.sh 的 npm 包名匹配串**：`herdsman-pi` npm 包名是外部名（`@dorokuma/herdsman-pi`），
  不随本仓改名；不过该防护整段已随 TS 桥一起移除（见上表），不存在残留匹配串。

### 验证

| 命令 | 退出码 | 说明 |
|---|---|---|
| `cd go && go build ./...` | 0 | 全工作区（含改名后所有模块）|
| `cd go && go vet ./herdsman/` | 0 | |
| `cd go && go test -count=1 ./herdsman/` | 0 | 全绿 |
| `rg 'pi-extensions/go/herdsman-pi' go/` | — | 除 `go/reports/` 历史报告外无命中 |
| `rg 'herdsman-bridge\|herdsman-pi'` 全仓 | — | 残留仅 `go/reports/`、`archive/`、本报告、上游 port-of 注释/npm 外部名 |

两 commit 顺序正确（先修复后改名）、都在 `fix/herdsman-bridge` 分支，未 push、未合并
main、未碰 `/root/.pig` 与 `/root/.pi` 生产目录、未动上游 `/root/workspace/herdsman`。

---

## 部署清单（**只列不改**，本任务未执行任何部署动作）

TS 桥 `herdsman-bridge` 已从 registry 移除，`install.sh` 不再管它。以下生产部署件仍可能
残留 `herdsman-pi` 字样，是否随改名清理需要在部署阶段统一决策，本报告只列清单：

- `federated/registry.json` 中**已删除**的 `herdsman-bridge` 条目原有
  `destFile: herdsman-pi.ts`、`targetPath: /root/.pig/agent/extensions/herdsman-pi.ts`——
  这是 TS 桥的部署文件名，现部署目录里若还有该文件属「registry 移除后尚未清理的残留副本」，
  `--audit` 会以 `other *.ts` 未登记条目 WARN 点名。
- `/root/.pig/agent/extensions/` 下按 `herdsman-pi` 命名的符号链接/部署文件、pig
  `settings.json` 里若有 `@dorokuma/herdsman-pi` npm 包引用。
- Go 版若部署，其注册名为 `herdsman`（`sdkLogName`），日志写
  `<herdsman home>/logs/herdsman-<YYYYMMDD>.log`、`[herdsman]` 标签。

**回退方法**：若需回退本次改名，`git revert` 改名+归档 commit 即可恢复
`go/herdsman-bridge` 与 `extensions/herdsman-bridge`；若还要继续退到 `herdsman-pi`，再
revert `6add117`。TS 桥归档可用 tag `archive/herdsman-bridge-ts-20261031`（指向 `d94bec5`）
恢复 main 上的最后状态。部署目录现有 `.ts` 文件不受本 commit 影响（本任务零部署动作）。

## 偏差与遗留

- **SDK 判定面差异**：TS 把缺席的 `mode`/`hasUI` 当「未知、不 skip」；SDK 把未上报的
  mode 归一为 `print`、hasUI 归一为 `false`（`context.go` 注释明确）。等价信号下 Go 版
  在 SDK 语境里不会有「未知 mode 的 print 进程被误留」的问题——SDK 语境下未上报就是
  非交互。`Context` 接口仍保留 `known` 返回值，让测试能复刻 TS 的「未知」语义。
- **`daemonclient.go` 未改**：上游修的两处问题（孤儿 socket、悬挂定时器）在 Go 移植的
  generation 闸门里已是等价实现，本部分只补回归测试钉契约，属有意不改生产代码。
- **Node 专有项**：上游 `socket.unref()`/`timer.unref()` 无 Go 对应物（进程退出语义不
  同），未移植，报告明示。
- **双加载防护整体移除**：该防护只针对「registry 里的 TS 桥 + settings.json 的同名 npm
  包同时装载」这一双实例形态；TS 桥出 registry 后没有触发面，函数整段移除（若日后 Go 版
  进 registry 且仍有 npm 同名包风险，可在部署阶段按同一 fail-closed 口径恢复）。
- **AGENTS.md 未改**：仍写 `herdsman-bridge`/7 hosted/11 扩展，属规则文件修改例外、留给
  主代理统一更新。
- **6add117 已收敛**：原「herdsman-pi → herdsman-bridge」改名经 rebase 重写为
  「herdsman-pi → herdsman」并并入本次归档改动，分支历史不留错误改名轨迹。

## 最终 commit 列表

（见交付报告 / `git log --oneline`，本文件入库时 HEAD 即改名+归档 commit。）
