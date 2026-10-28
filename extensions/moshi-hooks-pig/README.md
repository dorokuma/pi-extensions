# moshi-hooks-pig — pig（PiG）的 MOSHI 生命周期钩子扩展

让 MOSHI 守护进程（`moshi-hook`）把 **pig 会话当作 pi 会话处理**：审批推送、Live
Activity、Chat View 全部照常工作。

- **类型**：hosted 之外的 pig 专用 TS 扩展。权威源在本仓
  `extensions/moshi-hooks-pig/src/moshi-hooks.ts`；**不由 `install.sh` 管理**
  （`install.sh` 只管 pi 的 11 个扩展），部署方式见 §3。
- **基线**：`/root/.pi/agent/extensions/moshi-hooks.ts`（836 行，moshi-hook 自动生成）。
  本扩展是它的 pig 适配版：协议层逐条照抄，pig 差异集中在 §2 三点。
- **同仓还有一份 Go 移植**：`go/moshi-hooks/`（PiG Go SDK 版，已由
  `~/.pig/agent/extensions/moshi-hooks` 软链加载）。本 TS 版与它**并存**：Go 版是
  "pi 生成版"的 Go 化，本 TS 版是同一协议的 Node 实现，供需要在 Node 进程里
  加载/替换 Go 版时使用。两者信封 schema 完全一致。

## 1. 为什么 `source` 写 `"pi"`（伪装原因，勿改）

MOSHI 手机端 Chat View 的 parser 只认**已注册的 agent 名单**，`pi` 在册、`pig` 不在。
因此：

- `agentSource = "pi"`（`src/moshi-hooks.ts` 常量区，带注释）；
- `agentDisplayName = "Pi"`（守护进程与 App 原样渲染）；
- `transcriptPath` 与 herdr pane 字段**不伪装**，指向 pig 会话真实状态。

若 MOSHI 将来注册 `pig`，只需改这两个常量；届时本文件顶部注释同步更新。

## 2. 相对 pi 版的适配点（全部改动）

| # | 适配点 | pi 版 | 本版 |
|---|---|---|---|
| 1 | `transcriptPath` 取值 | 只读 `ctx.sessionManager.getSessionFile()` | ① `ctx.sessionManager.getSessionFile()`（PiG Node runtime 提供，见 ref/PiG `runtime-node/runtime.mjs:552-553`）→ ② ctx 上的 `sessionFile`/`session_file`/`sessionPath` 字段 → ③ **按 sessionId 在 sessions 根下跨目录定位** `<ts>_<sessionId>.jsonl` → ④ cwd 目录下最新 jsonl。回退路径保证 Chat View 拿到 pig 真实 transcript |
| 2 | pig 会话目录解析 | 无（pi 自己的路径由宿主给） | `pigSessionsRoot()`：`PIG_EXT_HOST_INFO.agentDir`（宿主注入的 JSON）→ `PIG_CODING_AGENT_DIR`/`PI_CODING_AGENT_DIR` → `PIG_HOME`/`XDG_CONFIG_HOME/pig`/`~/.pig` + `/agent/sessions`；目录名编码与 pi `session-manager.js` 逐字一致：`` `--${cwd.replace(/^[/\\]/,"").replace(/[/\\:]/g,"-")}--` `` |
| 3 | `sessionID` / `ctx` 读取 | 假定 pi 形状 | `sessionID` 增补 `ctx.getSessionId()` 回退；`modelLabel`/`contextRemaining`/`agentKind` 逻辑不变（PiG Node shim 与 pi 同形状） |
| — | 协议层（socket 寻址、信封 schema、250ms 超时、静默失败、单 worker 串行队列、`flushSends(1000)`） | — | **逐字节照抄**，无行为差异 |
| — | 事件映射（6 个钩子：`session_start`/`before_agent_start`/`agent_start`/`agent_end`/`agent_settled`/`session_shutdown`） | — | 事件名与 pig 宿主一致（PiG Go SDK `pigsdk/events.go` 与 pi 同名），逐条照抄 |

`sessionIsChild` 去掉了 pi 版的 `agentSource === "omp"` 静态不可达分支（本版
`agentSource` 恒为 `"pi"`），其余（lineage header 缓存、5s stop 冷却）不变。
`planFileOnDisk` 保留为 parity 用（pi 版同样未调用）。

**实测确认的 pig 会话 jsonl 路径**：`~/.pig/agent/sessions/<encoded-cwd>/<UTC时间戳>_<sessionId>.jsonl`。
例：cwd `/root/workspace` → `/root/.pig/agent/sessions/--root-workspace--/2026-10-23T16-56-50-553Z_01a169b1-….jsonl`。
注意：会话目录按**宿主进程启动 cwd** 解析；当 dispatch 从别处启动 pig 而 ctx.cwd 指向
worktree 时，按 sessionId 的跨目录定位（适配点 1③）仍能找到真实文件。

## 3. 部署（不在本任务范围，记录备查）

PiG 的 Node runtime 加载**目录**的 `index.ts`（`ref/PiG` `builder_node.go:52`
的入口名列表含 `index.ts`/`extension.ts`；`package.json` 的 `pi.extensions` 优先）。
因此部署 = 把本文件放到 `~/.pig/agent/extensions/moshi-hooks-pig/index.ts`
（新目录，避免动现有指向 Go 版的 `moshi-hooks` 软链），`/reload` 或新会话生效。
**注意**：与 Go 版**同名同装会双投**，只能启用其一。

## 4. 自检

```bash
# 类型检查（已纳入仓库 tsconfig include 与 pnpm check）
pnpm check

# 冒烟回归台架（真实扩展源码 transpile 后执行 + 一次性 Unix socket capture）
node --test extensions/moshi-hooks-pig/test/moshi-hooks-pig.test.mjs
```

台架断言（2 条用例，全绿）：① 四条 envelope 的完整 wire 格式——`source:"pi"`、
`transcriptPath` 落在 pig agentDir 下且不含 `/.pi/`、herdsman pane 三字段、
`contextRemaining`、`<user_query>` 剥离后的 `You: 回复 OK`、`session.closed`；
② 守护进程缺席时发送静默失败、回合不被打断。

台架与 auto-continue 台架同口径：依赖 `pnpm setup` 安装的 `node_modules`
（`typescript` 用于 transpile）；worktree 未安装依赖时先 `pnpm setup`。
