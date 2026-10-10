# exclude-tools-guard

## 概况
全局工具排除守卫：在**所有**会话（交互 TUI、`--print`、`-p`、`--mode json`、RPC、恢复旧会话）
中把 `grep` / `find` / `ls` / `bash` 从活动工具里反复过滤掉，强制走 CodeGraph、`ctx_fs`、`ctx_run`。
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
- 部署值（2026-10-08 子代理身份加固后）：md5 `9f448d58bb6c056d2041441b61ba26ed`
- 字节数：`11704`（263 行）
- 与部署副本逐字节一致（2026-10-09 回仓时 `cmp` 校验）；`bash install.sh --audit` 该项 `MATCH`。
- 历史沿革：纳管初值 `5d4edb821255e3d4ad66e96ecac97633` / `968` 字节（迁移当日补位版）→
  2026-10-08 加固后为上值（新增 `SUBAGENT-IDENTITY` 块 + 子代理会话额外剔除
  `jev_check` / `ask_user_question`）。
- `SUBAGENT-IDENTITY` 块与 `extensions/jev-gate/src/jev-gate.ts` 中的同名块**逐字节一致**，校验命令：
  `diff <(sed -n '/SUBAGENT-IDENTITY-BEGIN/,/SUBAGENT-IDENTITY-END/p' extensions/jev-gate/src/jev-gate.ts) \
       <(sed -n '/SUBAGENT-IDENTITY-BEGIN/,/SUBAGENT-IDENTITY-END/p' extensions/exclude-tools-guard/src/exclude-tools-guard.ts)`
  （2026-10-09 实测 exit 0）。

## 出处与许可
- 自研（pi 托管安装迁移当日的补位扩展）。
- 与 `settings.json` 的关系：`defaultTools = ["+codemode", "-bash"]` 负责 bash；
  本扩展负责 grep/find/ls（settings 未覆盖的部分）+ 全部四者的**中途兜底**。两者互补，不重复。

## 改动流程（铁律）
1. 只在本仓 `extensions/exclude-tools-guard/src/exclude-tools-guard.ts` 改；**严禁**直接编辑部署副本。
2. `bash install.sh exclude-tools-guard` 同步后 `/reload` 或新会话生效。
