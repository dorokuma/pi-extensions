# ctx-orchestrate

## 概况
codemode 灰度的**终态设施**：把 3 个高频多步流程固化为组合工具，并给 codemode 的
`text()` 回传加一道物理硬顶。本扩展**不 spawn 任何进程、不注册 command、不读
settings.json**——只 `registerTool` ×3 + `pi.on("tool_result")` ×1。

三个组合工具（`exposure: "codemode"`，不主动声明给模型，仅 codemode 脚本内
`tools.*` / `searchTools()` 可达；`namespace: ctxmode`，`describeNamespace("ctxmode")`
读 instructions）：

| 工具 | 嵌套调用 | 输出（outputSchema → structuredContent） |
|---|---|---|
| `ctx_symbol_read(symbol, depth?, max_files?, path?, file?)` | `codegraph` node → 并发 callers/callees → （depth=2）再解析 ≤6 个被调用符号 | `{symbol, files:[{path,line,role}], summary, errors?}` |
| `ctx_test_digest(kind, target, tail?, cwd?, args?, timeout_ms?)` | `ctx_run` action=run_task | `{kind,target,passed,failed,failures:[{name,excerpt}],summary,exit_code?,argv?,tail?,error?}` |
| `ctx_search_digest(patterns[], path?, glob?, max_hits?, …)` | 并发多个 `ctx_fs` action=rg（有界并发 4） | `{patterns, files:[{path,hits:[{line,excerpt,patterns}]}], summary, errors?}` |

三者共用同一套纪律：`annotations.readOnlyHint=true`、`outputSchema` + `structuredContent`
与 text 双通道返回、`ctx.executeTool()` 嵌套调用的失败防御（`Promise.allSettled`
+ 部分失败降级：失败落在 `structuredContent.errors/.failures` 或 `error` 字段，
整体失败才 `isError:true`，绝不因单步失败抛异常）。

## ctx_test_digest 计数解析覆盖
pass/fail **自全文输出提取**（独立于 `tail` 窗口），优先识别各框架自带的用例汇总行：
- **jest**：`Tests:  2 failed, 140 passed, 142 total`（冒号式，旧正则）。
- **vitest**（默认 reporter，2026-10-04 增补）：`Tests  2 failed | 140 passed (142)` /
  `Tests  142 passed (142)`。用 `\bTests\b` 定位**用例**汇总行后逐段取 `<count> <status>`
  加总 passed/failed，免疫 failed/passed 先后顺序；**不**误伤 `Test Files  …`（文件数）或
  pytest 的 `test session`（词形不匹配）。
  补丁背景：vitest 无冒号且 `|` 分隔，旧 jest 正则（需冒号）与 pytest 正则（需 passed 与
  failed 同时出现）都匹配不到，故全通过时 `passed/failed=0`（e2e 现场缺口）。
- **pytest / go / tap**：沿用既有正则与逐行计数，本次改动对它们**零回归**（已差分验证
  old==new）。

已知限制（既有口径，本次未改，如实披露）：pytest 仅 "N passed" 而未见 failed 时旧逻辑不认
（→0）、pytest "N failed, M passed" 语序会被旧正则反向、tap 的 `ok` 行被双计——属
pytest/tap 自身口径，铁律要求零回归故不动；vitest 失败用例名取 `×`/`✕` 行并已剥离尾部
` <n>ms` 时长，而详细错误在 vitest 单独的 "Failed Tests" 段（通常走 stderr），受块式提取所限
excerpt 有限。

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
- md5：`280f7767637f0e06cfb6db4c58ec8420`
- 字节数：`45076`（1115 行）
- 部署副本由 `bash install.sh ctx-orchestrate` 依 md5 幂等同步；`--audit` 报 `MATCH`。
- 历史沿革：纳管初值为 `bc486f54…`/42681 字节，vitest 计数补丁后为
  `e9127d93…`/44253 字节，oracle 应修项 2（marker 携带 `details.fullOutputPath`，双审
  应修收口）后为上值；三者均依 `install.sh` 同步且 `cmp` 逐字节一致。

## 出处与许可
- **作者声明为自研，未经第三方独立核实。** 本仓不做独立溯源，也不代其声明任何许可证
  归属。

## 类型检查（已知限制，如实披露）
本扩展运行时 `import { Type } from "typebox"`（与 `ctxmode.ts` / `codegraph-go.ts` 同：
由 pi 宿主环境提供）。本仓 `node_modules` 下**没有** `typebox`（只有
`@earendil-works/*`、`@types/node`、`typescript`），故它**不纳入**本仓 `tsc --noEmit`
图（`tsconfig.json` 的 `include` 未列它）——与 `herdsman-bridge` 同一处置思路：
纳入会让 `pnpm check` 因 TS2307 报错，掩盖其余内生扩展的真实检查结论。
替代校验：jiti/node 直加载冒烟（`/tmp/ctx-orch-smoke.mjs`，含字段名与 types.d.ts
逐一比对、三个工具的真输出回放、治理钩子截断断言），实测通过。

## 改动流程（铁律）
1. 只在本仓 `extensions/ctx-orchestrate/src/ctx-orchestrate.ts` 改；**严禁**直接编辑
   `/root/.pi/agent/extensions/ctx-orchestrate.ts`。
2. `bash install.sh ctx-orchestrate` 同步后 `/reload` 或新会话生效。
