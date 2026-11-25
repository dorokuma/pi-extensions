# exclude-tools-guard

## 概况
全局工具排除守卫：在**所有**会话（交互 TUI、`--print`、`-p`、`--mode json`、RPC、恢复旧会话）
中把 `grep` / `find` / `ls` / `bash` 从活动工具里反复过滤掉，强制走 CodeGraph、`ctx_fs`、`ctx_run`；
对子代理会话（`isSubagentSession() === true`）额外剔除 `jev_check` / `ask_user_question`，并在
`before_agent_start` 阶段清空主代理专用 `APPEND_SYSTEM.md`（清空 `appendSystemPrompt` 并从
先序扩展可能已写入的 `forceSystemPrompt` 中精确剥离对应的 `<addendum>` 段落）。
`session_start` 与 `before_agent_start` 两个时机各兜底一次——后者专门防「会话恢复中途被激活」
与「其他扩展把内置工具重新激活」。

## 为什么需要它（历史依据）
排除这件事有三层，任何一层都可能静默失效：
1. `settings.json` 的 `defaultTools`（当前 `["+codemode", "-bash"]`）——只管启动，且只移除了 bash；
2. `dispatch-agent` 的 `--exclude-tools`——只覆盖**派发**会话；
3. 本扩展——兜底主代理会话的 grep/find/ls，以及一切「中途被重新激活」的情况。
背景：pi 迁移到托管安装（1.0.4，2026-10-06）后，原 `/root/.pi/agent/bin/pi` 包装脚本被官方
launcher 取代，其「每个会话前置 --exclude-tools」的行为随之消失——本扩展即为此补位。
`dispatch-agent` 注释里也记录过同类历史问题（"the wrapper's grep/find/ls were coming back
for read-only roles"）。

## 权威源 / 目标 / 来源
- 权威源（本仓，唯一）：`extensions/exclude-tools-guard/src/exclude-tools-guard.ts`
  （绝对路径 `/root/workspace/pi-extensions/extensions/exclude-tools-guard/src/exclude-tools-guard.ts`）
- 目标安装路径：`/root/.pi/agent/extensions/exclude-tools-guard.ts`
- 来源：2026-10-06 16:05 手写于部署目录（迁移当日补位），同日纳管回本仓；内容一字未改。

## 纳管时的一致性记录
- **hc-only 迁移 + 严格整数加固（2026-11-25）**：`SUBAGENT-IDENTITY` 块随 go/jev-gate v0.4.0 与
  go/exclude-tools-guard v0.2.0 对齐迁移至 herdr-core 唯一正证明路径：移除经典 herdr
  （`HERDR_SOCKET_FILE`/`herdr.sock`/`HERDR_PANE_ID`/`pane.process_info`/`queryPaneShellPid`），
  改为向写死的 `/run/user/0/herdr-core/default.sock`（绝不读 `HC_TERMINAL_SOCKET`）发 JSON-RPC 2.0
  `terminal.get`；同日 R2 收口将 `pid` 校验由宽松 `Number()` 强转收紧为严格 JSON number 判定
  （`typeof record.pid !== "number" || !Number.isSafeInteger(record.pid) || record.pid <= 0` 即 fail-closed，
  杜绝 `"pid": true` 被强转为 PID 1 的绕过面），`"state" in record` / `!("parent" in record)` 改为
  `Object.prototype.hasOwnProperty.call(...)`（不穿透原型链）。迁移+加固后仓内值：md5
  `ad533f48efd988dd150612c969f869fd` / 15136 字节 / 326 行；块在两扩展间逐字节一致
  （本节下方校验命令实测 exit 0）。**按任务纪律本次未运行 `install.sh`、未同步宿主部署副本**
  （部署侧真实加载入口现为 PiG 原生 Go 扩展 `go/exclude-tools-guard`，TS 部署副本属历史快照；
  与部署副本的一致性记录以下方 2026-10-22 部署值为最后同步点，此后仓内源已领先于部署副本）。
- 部署值（2026-10-22 子代理 APPEND_SYSTEM.md 隔离加固后，**部署侧最后同步点**）：md5 `30722dbb05328b63a2cb87729072b7a0`
- 字节数：`12450`（279 行）
- 历史沿革：纳管初值 `5d4edb821255e3d4ad66e96ecac97633` / `968` 字节（迁移当日补位版）→
  2026-10-08 加固后为 `9f448d58bb6c056d2041441b61ba26ed` / `11704` 字节（新增 `SUBAGENT-IDENTITY` 块 + 子代理会话额外剔除
  `jev_check` / `ask_user_question`）→
  2026-10-22 加固后为上述部署值（子代理 `before_agent_start` 阶段清空 `appendSystemPrompt` 并精确剥离 `forceSystemPrompt` 中的 `<addendum>` 块）→
  2026-11-24 claude 家族加固后仓内值 279 行 / 12564 字节 / md5 `18807c4e0a6e5f97d96701f470700ab3`（未同步宿主）→
  2026-11-25 hc-only 迁移 + 严格整数加固后为最上方仓内值（未同步宿主）。
- `SUBAGENT-IDENTITY` 块与 `extensions/jev-gate/src/jev-gate.ts` 中的同名块**逐字节一致**，校验命令：
  `diff <(sed -n '/SUBAGENT-IDENTITY-BEGIN/,/SUBAGENT-IDENTITY-END/p' extensions/jev-gate/src/jev-gate.ts) \
       <(sed -n '/SUBAGENT-IDENTITY-BEGIN/,/SUBAGENT-IDENTITY-END/p' extensions/exclude-tools-guard/src/exclude-tools-guard.ts)`
  （2026-10-09 与 2026-11-25 hc-only+严格整数加固后实测均 exit 0；加固后块内 267 行 / 12634 字节 / md5 `55266bb9e0984fe80f13a76cd2afd196`）。

## 出处与许可
- 自研（pi 托管安装迁移当日的补位扩展）。
- 与 `settings.json` 的关系：`defaultTools = ["+codemode", "-bash"]` 负责 bash；
  本扩展负责 grep/find/ls（settings 未覆盖的部分）+ 全部四者的**中途兜底**。两者互补，不重复。

## 改动流程（铁律）
1. 只在本仓 `extensions/exclude-tools-guard/src/exclude-tools-guard.ts` 改；**严禁**直接编辑部署副本。
2. `bash install.sh exclude-tools-guard` 同步后 `/reload` 或新会话生效。
