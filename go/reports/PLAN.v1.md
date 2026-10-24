# pi 全面 Go 化方案（eqi12）

版本：2026-10-23 v1　负责人：爸爸（决策）/ Grok Bot（排期、验收）/ 开发机上的 pi（写代码）

## 0. 目标与边界

**终点**：eqi12 上 coding agent 这一套只剩 Go 二进制（外加 Rust 的 herdr、Google 的 agy 这两个外部工具），Node 彻底卸掉。

| 组件 | 现在 | 终点 |
|---|---|---|
| pi 主程序 | 原版 pi 1.4.0（TS，Node 26.7.0） | PiG 0.5.2+（Go 单二进制） |
| 自写扩展 12 个（9338 行） | TS | Go（PiG Go SDK / piglet） |
| npm 包 5 个（pi-tinyfish、pi-env、rpiv-ask-user-question、pi-vcc+2 补丁、pi-my-compact-ui） | TS | 按需求重写为 Go，自己维护 |
| pi-context-pruning（git 包，218 行） | TS | Go |
| herdsman-pi（扩展壳 14 行 + 源码 3889 行） | TS | Go |
| herdsman 守护进程 + CLI（15286 行） | TS，Node 22 | Go |
| codegraph-go、moshi-hook | Go | 不动 |
| herdr | Rust | 不动 |
| agy | Google 闭源 CLI | 不动；只保证 herdsman 钩子接口不变 |

**已放弃**：magpie-image（已移到 /root/.pi/agent/disabled/）。

**硬规矩（每一批都适用）**
1. 原版 pi 在全部 Go 版本验收前保持原样可用：不改 `/root/.pi/agent/` 下任何文件，不改 `~/.pi/agent/bin/pi` 启动器，不改 `/root/.gemini/config/hooks.json`，不重启正在跑的 herdsman 守护进程。
2. 所有新代码进 `/root/workspace/pi-go`（新 git 仓库，Go 1.26，单 module），每个组件一个子目录，一个组件一次提交。herdsman 的 Go 版在 herdsman 仓库新分支 `go-port` 的 `go/` 目录下。
3. PiG 只在隔离的配置目录里跑（见第 1 批），不碰原版 pi 的会话和设置。
4. 每个组件都要：`go build ./...`、`go vet ./...`、`go test ./...` 全绿；行为对照 TS 原版写测试（TS 原版有测试的，逐条翻成 Go 测试）；在 PiG 里实际加载跑通。
5. 每批结束写报告 `/root/workspace/pi-go/reports/batch-N.md`：做了什么、测试结果原文、和 TS 版的行为差异、遗留问题。不许写"应该可以"，没跑过的就写没跑。
6. 碰到 PiG 本身的缺口（已知 D78 provider 对象、D82、D83 isProxy）先记进报告，不要去改 PiG 源码；需要绕的写明绕法。

## 1. 第 0 批：并行环境（只配不写业务代码）

- 把 `/tmp/pigtest/pig`（0.5.2）装到 `/root/.local/opt/pig/0.5.2/pig`，软链 `/root/.local/bin/pig`。
- 克隆 `https://github.com/MichaelKinsy/PiG` 到 `/root/workspace/ref/PiG`，作为 Go SDK 的参考（读它的 SDK、piglet、parity 文档，不改它）。
- 查清 PiG 用哪个环境变量/参数指定配置目录，建隔离目录 `/root/.pig-test/agent`，settings 复制原版的 provider/model（magpie、group/smartass），扩展先一个都不加载。
- 冒烟：在隔离目录下 `pig -p "回复 OK"` 能正常出结果；记录启动耗时（`date +%s%N` 计时，跑 5 次）。
- 建 `/root/workspace/pi-go` 仓库骨架：`go.mod`、`README.md`、`reports/`、`Makefile`（build / test / vet / install-test 目标，install-test 只装到隔离目录）。
- 报告里写清：Go 扩展怎么注册、怎么打包成 piglet、怎么热加载 `/reload`，附最小 hello 扩展的实测输出。

## 2. 第 1 批：小件（约 2000 行 TS）

gemini-safety(49)、herdr-agent-state(261)、exclude-tools-guard(279)、no-tables(317)、pi-env(237)、pi-tinyfish(621，`@tiny-fish/sdk` 改成直接 HTTP 调用)、pi-context-pruning(218)。

（2026-10-24 注：gemini-safety 已弃用并删除）

注意：exclude-tools-guard 在 pi-extensions 仓库里有未提交改动，以 `/root/.pi/agent/extensions/` 里实际在用的版本为准，差异写进报告。

## 3. 第 2 批：中件（约 6300 行）

moshi-hooks(836)、ctxmode(960)、jev-gate(1005，含 registerProvider `typesafe`，对照 D78)、ctx-orchestrate(1115)、auto-continue(1156)、codegraph-go(1237，调用 `codegraph-go` 二进制，保持参数不变)。

node:net 用法（exclude-tools-guard、herdr-agent-state、jev-gate、moshi-hooks）改用 Go `net`，socket 路径和协议保持一致。

## 4. 第 3 批：界面类（约 4600 行）

cache-stats(2110，`isProxy`/`isUntrustedProxy` 10 处，D83 风险，Go 里改为显式类型判断)、pi-my-compact-ui(2453)。setFooter、onTerminalInput、registerToolRenderer 对应 SDK 的 SetFooterRenderer / ToolRenderer / setStatus。

## 5. 第 4 批：大件（约 15600 行）

pi-vcc(5529，两个本地补丁 `pi-vcc-0.10.0-recall-note`、`pi-vcc-0.9.0-ctx-run` 直接合进 Go 版)、rpiv-ask-user-question(6212)、herdsman-pi(3889，这批只做扩展侧，通过现有 RPC 和 TS 守护进程通信，协议不变)。

## 6. 第 5 批：herdsman 守护进程 + CLI Go 化（15286 行）

- 位置：herdsman 仓库 `go-port` 分支 `go/`。
- **对 agy 的兼容是硬要求**：
  - `herdsman agy-hook PreInvocation|Stop|WakeWaiter` 的参数、stdin JSON、stdout JSON、退出码与 TS 版逐字节一致；`test/unit/agy-hook-and-cli.test.ts` 的用例全部翻成 Go 测试并通过。
  - WakeWaiter 由 Node 自我重启改为 Go 二进制 exec 自己，环境变量 `HERDSMAN_AGY_SESSION_ID / HERDSMAN_AGY_STATE_DIR / HERDSMAN_AGY_WAITER_EPOCH` 保持。
  - agy 会话库 `~/.gemini/antigravity-cli/conversations/<id>.db`（SQLite + protobuf）只读解析，对照 `antigravity-proto.ts` / `antigravity-reader.ts` 及其单测；额外 home `AGY_EXTRA_HOMES`（默认 `/root/.agy-pro-home`）保留。
- 数据库：现在用 drizzle（见 `drizzle/` 迁移），Go 版沿用同一个库文件和表结构，不做迁移；用纯 Go SQLite 驱动（modernc.org/sqlite），不引 cgo。
- RPC：observability RPC 的方法名和消息格式不变，Go 版守护进程要能被 TS 版 herdsman-pi 和 Go 版 herdsman-pi 同时连。
- test/integration 下的用例尽量翻；翻不了的写进报告。
- 切换方式：`/root/.local/bin/herdsman` 包装脚本加开关 `HERDSMAN_IMPL=go|ts`，默认仍 ts；切换和回滚都只改这一处。**切换要爸爸点头。**

## 7. 第 6 批：切换与卸 Node（要爸爸点头）

1. 隔离目录里 PiG + 全部 Go 扩展连续实用 3 天无回退。
2. 启动器 `~/.pi/agent/bin/pi` 改为调 PiG（保留旧文件备份），herdsman 切到 Go。
   - 配置方式定为 PiG 直接复用 pi 的配置目录：启动器设 `PIG_USE_PI_DIRS=1`（此时 PiG 认 `PI_CODING_AGENT_DIR`，默认 `~/.pi/agent`），不另建 `~/.pig/agent`，也不写自己的喜鹊 Agent 插件。这样喜鹊内置的 Pi 适配继续管 models.json/settings.json：原生协议（magpie-anthropic 走 anthropic-messages）、价格、默认模型、思考档位都现成。
   - 依据（2026-10-23 在复制目录 `/root/.pig-pidirs-test/agent` 实测，PiG 0.5.2+1.4.0）：默认模型 magpie/group/smartass、人设、思考档位生效；usage.cost 有值；magpie-anthropic 的 api 为 anthropic-messages；PiG 起 node 扩展宿主并真正运行 TS 扩展（ctxmode 子进程被拉起）；跑完 settings/models/auth/mcp/trust/models-store 均未被改写。
   - 切换前还要补测：交互界面（TUI）、moshi-hooks 与 herdsman-pi 两个扩展（测试时移出未测）。
   - 切换后删掉隔离测试用的社区 agent-pig 插件（先 `magpie pig default` 再移除）和 `/root/.pig-test`、`/root/.pig-pidirs-test`。
3. 再观察 3 天，然后卸载 Node（nvm 下的 v22 和 v26），删 `node_modules`。
4. 卸之前跑 `rg -l "node|npx|pnpm" /root/.local/bin /root/.pi /root/.gemini/config` 确认没有残留调用。

## 8. 分工与验收流程

- Grok Bot：每批开工前把这份方案和该批说明交给开发机上的 pi；收工后读报告、亲自跑 `go test`、在隔离目录里加载验证，不过就打回。
- 每批完成给爸爸一句话汇报：完成了哪些、测试几条全过、遗留什么。
- 任何会影响原版 pi、正在用的 herdsman、agy 钩子的操作，都先停下来问爸爸。
