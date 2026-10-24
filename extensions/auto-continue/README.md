# auto-continue

## 概况
在上游 provider / 中转站 / 网络出现**瞬时**失败并沉降后，自动补发一次用户 "continue"，
让模型把任务重新捡起来。Pi 自带的 `settings.retry` 只覆盖 429 / 5xx / 超时；本扩展补其
遗漏——典型如中转站 405、Cloudflare 52x、空 HTML 响应体、内置重试耗尽。它**不**重试
401/403（鉴权）、配额、上下文溢出、截断（`stopReason: length`）或用户中止。

另有**早衰 stop（v3，静默停止）**分支：上游中转通道会在任务未完成时提前返回
`finish_reason=stop`（已观测 12/12 样本），形态两种——① 只有 thinking、没有任何 text
块（thinking-only）；② thinking + 明显中途断句的 text（以 `：/，/、/；` 结尾、未闭合
``` 代码块、或 markdown 标题/列表项刚起头就没下文）。`isContinueWorthyError` 只认
`stopReason === "error"`，这种 stop 会被当成「正常完成」直接放行，任务被静默丢掉。
`isPrematureStop()` 与它并列，命中后**复用同一条续跑链路**
（`agent_settled` → `delayMs` → `sendUserMessage("继续")`）与 `streak`/`maxAttempts`
上限，不另起炉灶。判定分支与原因码随既有 notify 一起打出来：

| 原因码 | 含义 |
|---|---|
| `stop:premature-thinking-only` | 命中：只有 thinking、没有正文 |
| `stop:premature-truncated-text` | 命中：正文中途断句 |
| `stop:premature-empty-content` | 命中：正文与 thinking 都是空的 |
| `stop:complete-text` | 不续：正文正常收尾 |
| `stop:has-tool-call` | 不续：这条 stop 还带着 toolCall |
| `premature:no-activity-evidence` | 不续：闸 A——既无 5 分钟内 toolUse 活动、本 run 时长也未超 90 秒（v3.1 起由 `premature:no-recent-tool-use` 更名，口径从「无近期 toolUse」扩展为「无任何活动证据」） |
| `premature:user-message-too-recent` | 不续：闸 B——距上一条真人 user 消息 ≤ 90 秒 |

**两道防误伤闸（必须同时满足）**：A) 活动证据（**满足其一即可**）——该 turn 此前
5 分钟（`TOOL_USE_WINDOW_MS`）内有 toolUse 活动，**或**本 run 从 `agent_start` 起已超过
90 秒（`RUN_ACTIVE_MIN_MS`，覆盖纯思考、无工具动作的长任务早衰）。相对 v3 是**放行集
扩大**：无工具活动的长 run 也过闸；**但仍要求至少一条活动证据**，两条皆无一律按
「不续」处理。B) 距上一条**真人**
user 消息已超过 90 秒（`USER_PROGRESS_STALL_MS`）仍无进展。时间戳优先用扩展自己跟踪的
`lastToolUseTs` / `lastHumanUserTs` / `runStartTs`（`tool_execution_start`/`_end`、非
extension 来源的 `input` 事件、`agent_start` 分别更新），取不到才回落到
`agent_end.messages` 扫描；两证据都无时按
「不续」处理。扩展自发的「继续」不算新的 user 输入——否则续跑链的第二发永远过不了
90s 闸。`runStartTs` 随 `agent_start` 重置（run 时长是「本 run 已跑多久」的口径，不能
跨 run 累计），只随 session 切换清零。

**与既有机制的边界**：疑似早衰的 stop **不算**「上一轮正常结束」，因此不会按 R4 自动
解除 esc 取消锁——esc 之后照样要用户手发消息（或 `/auto-continue on`）才恢复；压缩
busy（R3 看门狗）、输入框草稿、非 idle / 有排队消息这几道拦截对所有续跑分支一视同仁。
另外 `turn_end` 对疑似早衰的 stop **不**把 `streak` 归零，否则每轮清零会让
`maxAttempts` 上限永远不生效。

运行期内按 `esc` 中止的语义见源码顶部 v2 注释（`userCancelled` 锁的收敛范围与 pi 事件
模型的限制）。

**计时器取消的四相位（与 Go 版对齐）**：一轮「继续」的调度相位显式记为
`idle` / `armed` / `checking` / `committed`（源码 `ContinuePhase`，对应
`go/auto-continue/state.go` 的 `continueIdle` / `continueArmed` / `continueChecking` /
`continueCommitted`）。该 Go 路径是当前同工作树开发中的实现，仅作设计对照：Go 移植
独立提交，本 TS 扩展运行不依赖 Go 源码；这里只作纯文本路径引用，不建远端链接。

- `armed`：`agent_settled` 里代号递增后**先于 timer 装入**、在同一个同步段置位；
  `setStatus` / `notify` 这段宿主调用里重入的裸 ESC 因此仍能取消，之后才复查代号与
  相位决定是否装 timer。
- `checking`：本代 timer 已触发、正在跑宿主前置检查（`isIdle` /
  `hasPendingMessages` / `getEditorText`）。这段时间里本代保持可取消身份——ESC 的
  观察与 `cancelPending` 同一个同步段完成，取消先赢就不发；前置检查之后再查一次
  代号，宿主方法重入（ESC / `/auto-continue off` / 会话切换）造成的取消不会被无
  复查地越过。
- `committed`：发送认领。此刻之后的 ESC 不得声称「已取消本轮自动继续」——真实
  abort 仍走 `isIdle` 分支的 `markUserAbort`（「已按 esc 中止」）。
- 代号已不符的旧回调**什么都不碰**：不再 `timer = undefined`，因此不会清掉新一代的
  timer 引用（旧实现会，导致 ESC 看不到新一代、`delayMs` 后照发）。

单线程事实：TS 没有并行线程，这些相位不是锁，也不声称与 Go 版共享并发窗口；它把
两种**假设性/防御性**交错（已排出/已排队旧回调被台架强制重放、宿主方法调用内由同步
hook 重入 ESC）写成可判定状态，只固化与 Go 版一致的不变量——**不是** Pi 1.4.0 在线
Node 事件调度已发生或已证明可达的故障，也不声称所有潜在故障永久不可能（double-settled
路径尚未完整核清）。Go 版额外覆盖的 goroutine 并行窗口在 TS 里不存在，不做类比。

## 权威源 / 目标 / 来源
- 权威源（本仓，唯一）：`extensions/auto-continue/src/auto-continue.ts`
  （绝对路径 `/root/workspace/pi-extensions/extensions/auto-continue/src/auto-continue.ts`）
- 目标安装路径：`/root/.pi/agent/extensions/auto-continue.ts`
- 来源：自本机部署副本逐字纳管（`/root/.pi/agent/extensions/auto-continue.ts`），内容一字未改。

## 纳管时的一致性记录
- md5：`4341280ec027415bf7b9276d965a49e7`
- 字节数：`29714`
- 与部署副本 `cmp` 逐字节一致（2026-10-04 收纳时校验）。

## 改动记录
| 日期 | 改动 | md5（改后） | 字节数 | 校验 |
|---|---|---|---|---|
| 2026-10-04 | 收纳纳管（逐字副本） | `4341280ec027415bf7b9276d965a49e7` | `29714` | 与部署副本 `cmp` 一致 |
| 2026-10-04 | v3：新增早衰 stop 自动续跑分支（`isPrematureStop` + 两道闸） | `cb50578c13d98e3875e239b1fcc4b364` | `44035` | `install.sh auto-continue` 部署 + `--audit` MATCH（`cmp` 与部署副本逐字节一致） |
| 2026-10-04 | v3.1：闸 A 新增 run 时长并列条件（`RUN_ACTIVE_MIN_MS`，无工具活动的长任务早衰不再永久假阴性，理由码更名 `premature:no-activity-evidence`）+ `blockText` 字符串 content 防御兼容；措辞按双审口径改为「放行集扩大、仍要求至少一条活动证据」 | `8d5c33da9ffce60f400ed73f90df32e1` | `46766` | `install.sh auto-continue` 部署 + `--audit` MATCH（`cmp` 与部署副本逐字节一致） |
| 2026-10-24 | 计时器取消线性化（四相位 `idle`/`armed`/`checking`/`committed`，与 Go 版 `go/auto-continue/state.go` 对齐）：旧代号回调不再触碰新一代 timer；宿主前置检查期间保持可取消；发送前复查代号；commit 之后 ESC 不谎称取消。新增 `test/auto-continue-timer-race.test.mjs`（7 用例）。同日按主代理审查只改注释措辞（「真实可达」→ 台架强制重放的假设性交错；另校正 `selfSending` 与 input 时序说明），执行逻辑与测试断言零改动（AST / 去注释 token 比较 IDENTICAL） | `55775e1fda107b4da9704c007d33b7b8` | `52326` | `tsc --noEmit` exit 0；`node --test extensions/auto-continue/test/auto-continue-timer-race.test.mjs` 7/7 PASS exit 0；2026-10-24 经 `install.sh auto-continue` 同步部署（源/部署 md5 一致 `55775e1fda107b4da9704c007d33b7b8`、`cmp` 0、`52326` 字节），`--audit` MATCH；**未** `/reload`、**未**重启，当前 pi 仍用原已加载逻辑，新代码等下次 pi 启动加载；本TS修复按独立提交归档，不与Go移植混入同一提交 |

（`MATCH — unchanged` 是 `install.sh` 的同步校验记录：2026-10-24 本阶段已执行
`bash install.sh auto-continue`，源与部署副本 md5 同为 `55775e1fda107b4da9704c007d33b7b8`、
`cmp` 0、各 `52326` 字节，只读 `bash install.sh --audit` 亦打印 `MATCH — unchanged`。
**未** `/reload`、**未**重启任何 pi：当前运行中的 pi 仍使用它启动时加载的原逻辑，新代码
要等下次 pi 启动才加载；本TS修复按独立提交归档，不与Go移植混入同一提交。历史基线 md5
`8d5c33da9ffce60f400ed73f90df32e1` / `46766` 字节仍是备份
`auto-continue.ts.bak-20261024-pre-timer-race` 的当前内容——该备份按 AGENTS.md #6 原样保留，
**不等于**新版本，在 `--audit` 中属预期 `historical backup` WARN。）

## 出处与许可
- **作者声明为自研，未经第三方独立核实。** 本仓不做独立溯源，也不代其声明任何许可证
  归属；改动请回本仓 `extensions/auto-continue/src/`，类型检查通过后
  `bash install.sh auto-continue` 同步。

## 改动流程（铁律）
1. 只在本仓 `extensions/auto-continue/src/auto-continue.ts` 改；**严禁**直接编辑
   `/root/.pi/agent/extensions/auto-continue.ts`。
2. `pnpm setup && bash install.sh auto-continue` 后，`/reload` 或新会话生效。

## 自检（v3.1 早衰 stop 分支）

冒烟脚本（假消息对象 / 假 ExtensionAPI + 假时钟，均在 `/tmp`，未入仓）：
- `node /tmp/auto-continue-smoke.mjs`：32 组断言——thinking-only 命中、冒号/逗号/顿号/
  分号/未闭合代码块/markdown 标题/裸列表项/省略号命中、完整段落收尾不误伤、带 toolCall
  的 stop 不误伤、无近期 toolUse 活动不续、90s 闸（含边界与「继续」不算新 user 输入）、
  闸 A run 时长并列条件（>90s 续 / 恰好 90s 不续 / toolUse 过期但 run 够长兜底）、
  blockText 字符串 content 防御兼容（断句命中 truncated-text、完整收尾判 complete-text）、
  `looksTruncatedText` 单测。
- `node /tmp/auto-continue-smoke-ext.mjs`：9 组扩展级场景——早衰 stop 走完
  `delayMs` → `sendUserMessage("继续")` 且日志带原因码、正常完成不续、esc 锁不被绕过
  （且不误解锁）、压缩 busy 优先、草稿优先、连续早衰 stop 的 streak 累加到
  `maxAttempts` 上限停手、`PI_AUTO_CONTINUE=0` 关闭时不续；S9 无工具活动但 run > 90s
  的 thinking-only 早衰也续、S10 无工具活动且 run ≤ 90s（闸 B 已过）不续。
- `node node_modules/typescript/bin/tsc --noEmit -p tsconfig.json`：exit 0。

## 自检（计时器取消：四相位回归）
```bash
node --test extensions/auto-continue/test/auto-continue-timer-race.test.mjs
```
7 个用例全 PASS、exit 0：T1（已排出但被取消的旧回调不得动新一代 timer，重放后 ESC
仍能取消新代，最终 send 为 0）、T2（`isIdle` / `hasPendingMessages` / `getEditorText`
三个宿主重入点各自重入裸 ESC：checking 仍可取消、send 0、`streak` 不增、取消通知准确）、
T3a（armed 窗口内重入 ESC：取消先赢，timer 不被装上、重放也不发送）、T3b（commit 先赢：
之后的 ESC 不得谎称取消，也不补发）。

台架读**真实权威源** `src/auto-continue.ts`，用 `typescript.transpileModule` 编成 CJS
后在当前 realm 执行，只注入 ExtensionAPI / ExtensionContext polyfill、假 fs（配置写不
到真实 home）、假 env、假 scheduler，驱动 `session_start` / `agent_end` /
`agent_settled` / `onTerminalInput` 这些真实入口；不跑 pi、不读用户配置、不 sleep。

对照「修复前红」：`AUTO_CONTINUE_SRC=<修复前只读副本> node --test <同一条命令>`。
基线（md5 `8d5c33da9ffce60f400ed73f90df32e1`）上 T1 / T2×3 / T3a FAIL（实质断言失败，
非编译错误），T3b PASS——「发送已 commit 后 ESC 不谎称取消」在原实现本来就正确，
本次只是把它固化成不变量。
