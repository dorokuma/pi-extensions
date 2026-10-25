# ctx-orchestrate

## 概况
codemode 灰度的**终态设施**：把高频多步的 `ctx_fs` 检索流程固化为组合工具，并给 codemode 的
`text()` 回传加一道物理硬顶。本扩展**不 spawn 任何进程、不注册 command、不读
settings.json**——只 `registerTool` ×1 + `pi.on("tool_result")` ×1。

唯一组合工具（`exposure: "codemode"`，不主动声明给模型，仅 codemode 脚本内
`tools.*` / `searchTools()` 可达；`namespace: ctxmode`，`describeNamespace("ctxmode")`
读 instructions）：

| 工具（codemode 内调用形式，非顶层） | 嵌套调用 | 输出（outputSchema → structuredContent） |
|---|---|---|
| `tools.ctx_search_digest({ patterns: ["foo", "bar"], max_hits: 50 })`（对象参数；**只在 codemode 脚本内调用**，它不是模型顶层工具，不能顶层调用，也不要输出裸 `<tool_call>` 文本） | 并发多个 `ctx_fs` action=rg（有界并发 4） | `{patterns, files:[{path,hits:[{line,excerpt,patterns}]}], summary, errors?}` |

该工具沿用既有纪律：`annotations.readOnlyHint=true`、`outputSchema` + `structuredContent`
与 text 双通道返回、`ctx.executeTool()` 嵌套调用的失败防御（`Promise.allSettled`
+ 部分失败降级：失败落在 `structuredContent.errors` 字段，
整体失败才 `isError:true`，绝不因单步失败抛异常）。

## 治理钩子：codemode text() 回传硬顶
`pi.on("tool_result")` 拦截**模型直接发出的 `codemode` 调用**（无 `parentToolCallId`）：
文本总字符超过 `CTX_ORCH_TEXT_CAP`（默认 3000）时物理截断并附中文警告；图片块原样保留；
**警告 marker 携带恢复路径**：`event.details.fullOutputPath` 存在时，marker 附上该路径并指引
「用 read 精确取片段（无需重跑）」——被截断的模型看不到 `details`，把执行器已落盘的完整
输出路径带进 marker，可把截断后的恢复成本从「重跑脚本并写文件」降为一次 `read`；
`details.fullOutputPath` 不存在时维持原措辞（脚本内收敛/写入文件后 read）。
`details` / `structuredContent` / `isError` / `usage` 四项显式回传（只替换 content 而不回传
后三者会丢 codemode 的 `fullOutputPath` 与结构化内容）。嵌套调用结果（`parentToolCallId`
存在）**不治理**——脚本靠 `outputSchema` 拿 `structuredContent` 做判断，截断即毁数据。
机制依据与源码位置见源码顶部注释块与 `.agents/notes/20261004-ctx-orchestrate-hosted-extension.md`。
`CTX_ORCH_GOVERN=0` 可关；`CTX_ORCH_DEBUG=1` 时诊断同时打 stderr（默认只写
`~/.pi/agent/logs/ctx-orchestrate.log`）。

环境变量：`CTX_ORCH_TEXT_CAP`（默认 3000，下限 200）、`CTX_ORCH_GOVERN`（默认开）、
`CTX_ORCH_DEBUG`、`CTX_ORCH_DIAG_DIR`。

## 权威源 / 目标 / 来源
- 权威源（本仓，唯一）：`extensions/ctx-orchestrate/src/ctx-orchestrate.ts`
  （绝对路径 `/root/workspace/pi-extensions/extensions/ctx-orchestrate/src/ctx-orchestrate.ts`）
- 目标安装路径：`/root/.pi/agent/extensions/ctx-orchestrate.ts`
- 来源：**自研新写**（2026-10-04，w14 codemode 灰度终态设施任务）。非从部署副本纳管——
  本仓是其唯一权威源，此前不存在任何副本。

## 纳管时的一致性记录

> **纳管时历史快照（非当前内容）**：下面三项 md5 / 字节数 / 行数与「`--audit` 报 `MATCH`」
> 只描述该记录书写时点的内容（已核实等于本仓基线提交里那份 25394 字节 / 613 行的文件，
> 亦即改前备份 `/root/.pi/agent/extensions/ctx-orchestrate.ts.bak-20261025-digest-hint`）。
> 其后源码只改过**提示文字**：2026-10-25 把 `ctx_search_digest` 的调用说明收口为「只在
> codemode 脚本内以 `tools.ctx_search_digest({...})` 调用、不裸顶层调用、不输出裸
> `<tool_call>` 文本」，本 README 的调用示例也改为可直接复制的具体对象；当前内容以文件
> 本身为准。本轮按任务禁用 `install.sh` 与 `--audit`，未跑巡检，故**不得**据本节声称
> 当前仍是 `MATCH`。

- md5：`0e33386a7178868cbefe31969f08720b`（纳管时历史快照）
- 字节数：`25394`（613 行，纳管时历史快照）
- 部署副本由 `bash install.sh ctx-orchestrate` 依 md5 幂等同步；`--audit` 报 `MATCH`（同为该时点的记录）。
- 历史沿革：纳管初值为 `bc486f54…`/42681 字节，vitest 计数补丁后为
  `e9127d93…`/44253 字节，oracle 应修项 2（marker 携带 `details.fullOutputPath`，双审
  应修收口）后为 `280f7767…`/45076 字节；各版均依 `install.sh` 同步且 `cmp` 逐字节一致。
- **2026-10-24 摘除 `ctx_symbol_read` / `ctx_test_digest`**：两者连同实现函数、schema、
  解析器（`parsePathLines` / `parseCalleeNames` / `parseTaskOutput`）、常量与 prompt 片段
  一并删除，当前仅保留 `ctx_search_digest`；改前部署件备份为
  `/root/.pi/agent/extensions/ctx-orchestrate.ts.bak-20261024-pre-drop2`
  （md5 `280f7767637f0e06cfb6db4c58ec8420`）。后续 Go 版移植亦**不带**这两个工具。

## 出处与许可
- **作者声明为自研，未经第三方独立核实。** 本仓不做独立溯源，也不代其声明任何许可证
  归属。

## 类型检查（已知限制，如实披露）
本扩展运行时 `import { Type } from "typebox"`（与 `ctxmode.ts` / `codegraph-go.ts` 同：
由 pi 宿主环境提供）。本仓 `node_modules/typebox` 已存在，且 `tsconfig.json` 的 `include`
已列 `extensions/ctx-orchestrate/src/**/*.ts`，故它**已在**本仓 `tsc --noEmit` 图内
（`tsc --noEmit --listFiles` 可见该文件），`pnpm check` 实测 EXIT=0。
补充校验：node 直加载冒烟（`/tmp/ctx-orch-smoke.mjs`，临时文件不入库），断言改后的注册面
（唯一 `ctx_search_digest` + 一个 `tool_result` 钩子）、治理钩子 3000 字硬顶的三项行为
（顶层截断 / 嵌套不治理 / 非 codemode 不动）与 `ctx_search_digest` 端到端
（rg 输出解析 + 跨模式去重 + 按文件聚合），25 项断言全部通过。

## 改动流程（铁律）
1. 只在本仓 `extensions/ctx-orchestrate/src/ctx-orchestrate.ts` 改；**严禁**直接编辑
   `/root/.pi/agent/extensions/ctx-orchestrate.ts`。
2. `bash install.sh ctx-orchestrate` 同步后 `/reload` 或新会话生效。
