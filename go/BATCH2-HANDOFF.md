# 第 2 批交接文档

> **性质**：本地交接文档（随本次提交入库，不属于线上部署产物）。
> **读者**：下一位接手的主代理。
> **分支**：`feature/go-batch1`（本批工作自 `de148fc` 起算）。
> **编写时工作树**：` M go/go.work` + `?? go/ctx-orchestrate/`，以及本文件本身；除此之外无其它未提交改动。
> **口径**：本文只交接状态、决策与经验，不重复实现细节。细节以源码与各组件 `README.md`、`.agents/notes/` 为准；本文未重跑验证，凡引用以往测试结果处均标注证据来源。

## 1. 现状清单

### 1.1 已完成提交（本分支，自 de148fc 起）

- **`de148fc` refactor(ctx-orchestrate): drop ctx_symbol_read and ctx_test_digest**
  TS 版精简。Go 移植按此精简范围进行，即 Go 版**不含** `ctx_symbol_read` 与 `ctx_test_digest` 两个工具。

- **`18fb2c3` fix(auto-continue): guard timer generations and cancellation phases**
  TS auto-continue 四相位计时器取消修复，并新增 `test/auto-continue-timer-race.test.mjs`（7 用例）。
  源与部署件已一致：md5 `55775e1fda107b4da9704c007d33b7b8` / 52326 字节，`cmp` 退出 0。
  备份 `/root/.pi/agent/extensions/auto-continue.ts.bak-20261024-pre-timer-race`（旧版 md5 `8d5c33da9ffce60f400ed73f90df32e1` / 46766 字节），按 AGENTS.md 第 6 条保留，未归档、未清理。
  **未 `/reload`、未重启**：运行中的 pi 仍跑旧逻辑。

- **`7ab5bb7` fix(exclude-tools-guard): strip leaked APPEND_SYSTEM addendum in subagent sessions**
  补交 2026-10-22 的子代理 `APPEND_SYSTEM <addendum>` 剥离加固。TS 维持现状为权威源，md5 `30722dbb05328b63a2cb87729072b7a0`（12450 字节），与部署件 `cmp` 一致。Go 版已在 `9cd6108` 按此移植。

- **`b271d34` feat(go): port auto-continue extension to Go (batch 2)**
  `go/auto-continue/` 共 15 文件；`go/go.work` 仅新增 `./auto-continue` 一行。
  README 行号已对齐真实定义（如 `state.go:717` `fireContinue`、`state.go:823` `HandleCommand`）。
  真实入口续跑演练达到判据：RPC + 本地假 provider，假 provider 第二次请求的 `user_messages` 含「继续」。

### 1.2 工作树未提交草稿

- `go/ctx-orchestrate/` 共 **11 个文件**（未跟踪）：`diag.go`、`digest.go`、`digest_test.go`、`extension.go`、`extension_test.go`、`go.mod`、`govern.go`、`govern_test.go`、`README.md`、`text.go`、`text_test.go`。
- `go/go.work` 的工作树改动只剩一行新增 `./ctx-orchestrate`（`./auto-continue` 已随 `b271d34` 入库）。
- **已验证部分**（证据来自上一轮 verifier 会话原文，非本次重跑）：三轮 `go test -race` 各 36 顶层 + 21 子测试（合计 57 条）PASS、`DATA RACE` 0。worker 侧自报套件通过，并实测触发过治理截断。
 - **待收尾 4 项（已拍板、未实施）**：
  1. 数值边界：`CTX_ORCH_TEXT_CAP` 为 NaN/Inf/非正/1e19 溢出时安全回退 3000 或饱和，补单测，README 记录与 TS 的差异；**不改线上 TS**。
  2. 默认诊断路径：改为非空 `PIG_HOME` 优先 `$PIG_HOME/agent/logs`，否则 `<home>/.pi/agent/logs`，补单测与 README。
  3. marker 预算：保持与 TS 一致（cap-180 预留），只在 README 写明这是为对齐 TS 的已知偏差。
  4. 授权用本地假 provider 在 `./bin/pig-test -e ./ctx-orchestrate` 补跑 codemode 真实入口演练，输出写入 README。
- **提交前还缺**：完成上述 4 项后的 verifier（`gofmt`/`build`/`vet`/`test -race -v`）与 reviewer 闭环。提交时须一起带上 `go/go.work` 的 `./ctx-orchestrate` 一行，信息 `feat(go): port ctx-orchestrate extension to Go (batch 2)`。
- 明确状态：**ctx-orchestrate 尚未提交**，Go 组件全部**未部署**。

### 1.3 尚未开工的 4 个扩展（第 2 批剩余）

- **jev-gate**
  - 权威源：仓内 `extensions/jev-gate/src/jev-gate.ts`（hosted）。
  - 部署件：`/root/.pi/agent/extensions/jev-gate.ts`，1005 行。
  - 注意：非 LLM 分类器的判定阈值已校准并冻结（见 `.agents/notes/20261006-jev-gate-calibration*.md`），移植**不得改阈值语义**；分类器结论不得用于计数、数值比较或安全边界。

- **moshi-hooks**
  - 权威源：本仓**无 `src`**（非 hosted 扩展），以部署件 `/root/.pi/agent/extensions/moshi-hooks.ts`（836 行）为基线。
  - 注意：移植前先确认其依赖与宿主钩子面，再定权威源归属。

- **ctxmode**
  - 权威源：federated，宿主仓 `/root/workspace/ctxmode`。
  - 部署件：`/root/.pi/agent/extensions/ctxmode.ts`，960 行。
  - 注意：工具面大（`ctx_run`/`ctx_fs`/`ctx_git`/`ctx_kb`/`ctx_bg`/`ctx_stats`），移植需逐工具对齐；改动应回宿主仓流程。

- **codegraph-go**
  - 权威源：federated，宿主仓 `/root/workspace/codegraph-go`。
  - 部署件：`/root/.pi/agent/extensions/codegraph-go.ts`，1237 行。
  - 注意：宿主仓本身是 Go 项目，与 Go 版功能存在重叠风险，移植前先定边界。

- **第 1 批遗留**：no-tables 的 CRLF/CR 行终止符对齐列入下一批待修（见 `go/no-tables/README.md`「下一批待修：CRLF / CR 行终止符对齐」，条目 1–5、7；U+2028/2029 仅文档化，不作永久接受）。

## 2. 已拍板决策

以下均来自用户拍板。

- **auto-continue（Go）**
  - 显式四相位 `continuePhase`：`idle`/`armed`/`checking`/`committed`。
  - 旧回调先比代号，过期回调不碰本代状态。
  - ESC 在同一临界区观察并作废代号。
  - 发送前在锁内取 claim（`committed`）后才出锁调用。
  - 不改阈值/文案/streak 语义。
  - D1 偏好路径尊重 `PIG_HOME`（隔离红线）。
  - D4：print/JSON 末轮续跑不保证，不做 keepalive。
- **auto-continue（TS）**：按同一语义修复并单独提交（`18fb2c3`）；测试中的交错是台架防御性重放，**不宣称线上已发生**；已同步部署件但**未 reload/重启**。
- **exclude-tools-guard**：TS 保持 2026-10-22 版本为权威源，历史补交单独提交（`7ab5bb7`），不另做审查轮次；Go 版在 `9cd6108`，残余面（无负向标记的子代理在独立 pane 可能被判 main）记录在 Go README。
- **ctx-orchestrate**：即 1.2 的 4 条（数值边界、`PIG_HOME` 诊断路径、marker 预算对齐 TS 并文档化、授权假 provider 入口演练）。
- **通用纪律**：每个组件单独提交、同一笔必须带上对应 `go.work` 条目；不 push、不部署、不 `/reload`、不重启任何 pi；生产 `/root/.pi` 不在移植中写入。

## 3. 踩过的坑与经验

逐条按「现象 → 原因 → 做法」记录。

1. **假 provider 首轮 502 让续跑判据失真**
   - 现象：一次演练中两次请求都只是 fixture、没有「继续」，真实续跑被判为未达标。
   - 原因：PiG 内建 `auto_retry` 吞掉了假 provider 的首轮 502。
   - 做法：改用 200 + 中途断句正文（无 tool call、无 thinking、`durationMs`≈105s > 90s，两道闸都过）走 premature-stop，才真正触发。

2. **假 provider 真实入口演练的正确写法**
   - 现象：不按套路搭台架时，请求根本到不了扩展。
   - 原因/做法：本地回环 OpenAI 兼容 + SSE 服务；`~/.pig-test/agent/models.json` 临时加 provider（指向 127.0.0.1），演练后按原 md5 `7ef998265f4515f58cbe201c117f7b36` 还原；`./bin/pig-test` 写死 `PIG_HOME=/root/.pig-test`；RPC 模式 stdin 一行 JSON + `sleep 150`；**达标判据**是假 provider 收到第二次请求且 user 消息为「继续」。

3. **ctx-orchestrate 的 codemode 工具暴露**
   - 现象：看不到 codemode 工具，治理钩子也不触发。
   - 原因：不是 exposure bug，而是宿主按 `--tools` 白名单决定可见性；未激活时 codemode 不可见，治理钩子也就无从触发。
   - 做法：宿主 callable 判定缺口按 PLAN 只记录、不改 PiG 源码。

4. **go.work 与提交**
   - 现象：并行处理两个组件时提交互相污染。
   - 原因：index.lock 争用，且一次提交只能含本组件一行。
   - 做法：并行改两个组件必须串行提交；先把另一行从工作树临时移出 → `add` → `commit` → 立刻恢复。

5. **jev_check 429**
   - 现象：分类器调用连续 429。
   - 原因：当日免费额度耗尽。
   - 做法：按 SYSTEM.md §3 降级人工核验，不反复重试、不把分类器当门禁替代。

6. **verifier 包装器环境变量坑**
   - 现象：判据根本没启动，得到 EXIT 127。
   - 原因：把 `NODE_PATH=…` 当成可执行文件名。
   - 做法：必须以 `env NODE_PATH=… node …` 形式传。

7. **README 行号漂移**
   - 现象：约 30 处 `state.go` 行号失真（`fireContinue` 曾写 663，实际 717）。
   - 原因：改过状态机后行号整体偏移。
   - 做法：提交前用脚本按定义行重算，不手填。

8. **未跟踪目录不计入 `git diff --stat`**
   - 现象：`diff --stat` 里看不到新增测试/组件。
   - 原因：未跟踪文件不在 diff 里。
   - 做法：新增内容要看 `git status` 补证，别只看 diff。

9. **大输出被 3000 字硬顶截断**
   - 现象：查询结果被截断，结论失真。
   - 原因：codemode 输出有 3000 字上限。
   - 做法：查询收敛（只取摘要/计数/关键路径），需要全文先落文件再精确读片段。

## 4. 接手须知

- 本文写在 `go/BATCH2-HANDOFF.md`，属本地文档并随本次提交入库；细节以实现与各组件 `README.md` 为准。
- **生产部署现状**：TS auto-continue 已同步到 `/root/.pi/agent/extensions/auto-continue.ts` 但未 reload/重启；**Go 组件均未部署**，全部未在线生效。
- **下一步建议顺序**：先收尾并提交 ctx-orchestrate（1.2 的 4 项 + verifier/reviewer + 带上 `go.work` 的 `./ctx-orchestrate` 一行）；再开 jev-gate（仓内有源，先做）；moshi-hooks / ctxmode / codegraph-go 需先定权威源与边界再动手。
- 纪律提醒：不 push、不部署、不 `/reload`、不重启任何 pi；不写 `herdr-agent-state.ts`；`.bak-*` 未获书面解冻不得动。
