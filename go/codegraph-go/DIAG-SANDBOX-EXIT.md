# 阶段1诊断修正记录 — codegraph-go 子进程退出诊断（MARK-CG-CLIENT-R2-W67-20261105-01）

模式标签：`fix/cg-ext-sandbox-diag`（阶段1：透传有界/脱敏退出诊断 + 错误收尾，非功能模式）。
本轮为第二轮（最终）集中返修：独立 reviewer 的 7 must + 1 should 逐项处置。

## 范围与基线

- 用户授权：真实 PiG 扩展在在线 sbx reviewer-agent 下运行时 codegraph-go
  子进程被拒/退出只报裸 `disconnected`；阶段1只要求错误可诊断、有界、
  脱敏且子进程不泄漏，不改功能权限/索引路由。
- 基线：`20f064847cb39081f493184b113186e75ce743dc`（main）。
- 工作树：`/root/workspace/.wt/pi-extensions/fix-cg-ext-sandbox-diag`，
  分支 `fix/cg-ext-sandbox-diag`。未 commit/push/merge/部署。

## 数据流模型（修后）

```
child ──stdout──> readResponses ──> pending[id]        ─┐ EOF/error → retireOrphaned
child ──stderr──> readStderr → consumeStderr(raw carry)─┤ (200ms nudge → TERM/KILL)
sendRequest ──> writeCh(64,deadline) ──> requestWriter ──┐ SetWriteDeadline
monitor: Wait()→drain 300ms→build diag→cleanup→pipeTeardown→close(p.done) ┘
diagLog ──> diagQueue(256,drop-oldest) ──> diagSink(单 goroutine,file/DEBUG)
terminateProc: 关 stdin+quit(不拿 writeMu)→SIGTERM 1s→SIGKILL→reap≤2s
```

进程身份：`p.exitErr` 始终存该进程自己的 Wait/写错误诊断；`c.lastExitErr`
兜底最近一次。monitor 是唯一 `Wait()` 调用者，`p.done` 只由它关闭。

## R1–R8 逐项处置

- **R1 跨分块脱敏安全**（client.go consumeStderr）：重写为有界行切割状态机。
  carry 存**原始未脱敏**片段（仅控制符规范化），sanitize 只在「完整记录」提交
  点执行——不再发生「半脱敏片段+原始续片」重组泄密；控制符/零宽/双向符在
  **匹配之前**剔除，`TO<KEN` 无法重组为未脱敏键；单引号未闭合与双引号同口径
  遮蔽行尾。超 4KiB 的行：规范化→脱敏→截断**头部**提交一次并丢弃本行其余部分
  （不保留 reassembly 窗口、不假装还原）；DEBUG 镜像与环缓冲共享同一条已截断
  记录。仅覆盖已知凭据形状，不宣称任意密文可识别。
- **R2 诊断出口统一口径**：daemon RPC error 在 `readResponses` 投递时经
  `sanitizeDiagText`（与 stderr 同口径，仍保留叶子 error 字符串匹配面）；
  spawn 错误文本含 `workdir=`/`bin=` **结构化字段原值**（脱敏绝不改真实路径，
  `TestClientMissingBinaryDiag` 断言真实路径可定位）；`exitError` 的
  workdir/phase/exit 字段原值，仅 daemon 自由文本（stderr tail、RPC message）
  脱敏。生产调用点全核：`start/doStart/sendRequest/sendNotification/callTool/
  stop/abortStart/writeFailed/retireOrphaned/monitor` 无旁路。
- **R3 请求 deadline 覆盖排队+实际写**：stdin 写入改单 writer goroutine 消费
  有界 `writeCh`（64），`writeReq` 携带请求级绝对 deadline；`enqueueWrite`
  非阻塞入队，慢路 select 同时等队列位/quit/自身 deadline；`writeNow` 在
  writeMu 内 `SetWriteDeadline`（`cmd.StdinPipe()` 实测返回 `*os.File`，
  支持 deadline）。满管道/超时 ⇒ `writeFailed` 退役所属 p（TERM→KILL→reap），
  不误杀继任/共享 daemon；`callTool` 一次重试规则不变。
- **R4 有界进程退役**：`terminateProc` 先 `pipeTeardown`（stdin 先关，不拿
  writeMu，被卡写立即释放；quit 通知 writer 退出）→SIGTERM 1s→SIGKILL→reap≤2s。
  monitor 是唯一 Wait 责任人，`p.done` 单点关闭，清引用前不丢 reap；
  新增 `retireOrphaned`：stdout/stderr 结束而子进程仍活时 200ms nudge 宽限后
  同样收尾，旧 monitor 因 identity guard 不清新实例。spawn 失败路径同档。
- **R5 诊断 sink 反压**：`diagLog` 改有界 `diagQueue`（256）+ 单 sink
  goroutine，写满丢最旧并计数报告；**无 diagMu、无不归本 client 的输出关闭、
  无每行 goroutine/无界队列**。DEBUG 镜像与环内记录同源（已脱敏+已截断）。
  DEBUG 出口经可替换 `diagOut`（锁保护），测试用不可读管道证明
  请求/stop/reader 均不被 sink 阻塞；不承诺日志无损。
- **R6 退出身份竞态**：`doStart` 的 `c.proc != p` 分支返回 `p.exitErr`→
  `lastExitErr` 顺序的真实诊断（非裸 disconnected）；monitor 在
  `proc!=p || intentional` 分支仍把真实 Wait 结果写入 `p.exitErr`
  （仅非 intentional 时同步 lastExitErr）。拿不到退出码如实
  `exit=unknown`+`wait=`，不伪造。测试用三个确定性屏障钩子
  （afterMonitorWait/orphanHook/beforeIdentityCheck）安排
  reader→monitor→doStart 交错，无 sleep。
- **R7 断言增强**：`TestConsumeStderrCrossChunkBound` 在换行/退出**之前**
  断言 raw carry 上界（`len(p.carry)≤4096` 中途即查）+ 丢弃语义
  （carryTrunc 标记、环内仅一条截断头记录）；`TestClientExitDiagOnStartup`
  经 monitor 钩子**先存进程指针**再断言 `p.done` 回收，不再取已 nil 的
  `c.proc` 跳过；`TestClientWriteDeadlineCoversBlockedPipe` 先存 `p1` 再断言
  `p1.quit`/`p1.done`。未放宽任何阈值/超时。
- **R8 测试纪律**：上轮 `diagnostic_regression_test.go` 已撤除（删除），
  全部回归合入 `client_test.go`/`extension_test.go` 并复用 `helperClient`
  （扩展为返回 spawn 计数+PID 文件）。基线注入版另存为报告代码块
  `diag_baseline_inject_test.go`（自带 `TestDiagBaseHelper` + `diagBaseClient`，
  只用基线符号，11 项断言在裸 20f0648 全部实质 FAIL、候选全 PASS，
  含 PID kill 兜底不泄漏 helper）。确因新符号（exitError/钩子/
  sanitizeDiagText）无法在旧基线编译的测试已在交付中标记「仅候选正向」。

## 门限（声明值）

| 项 | 门限 |
|---|---|
| stderr 行（脱敏后） | 200 rune |
| stderr 环 | 20 行 |
| 未换行 carry | ≤4096B 原始片段；超限行提交脱敏头部一次+丢弃行尾 |
| 错误内 stderr tail | ≤8 行且 ≤512 字节（含分隔符） |
| stdin 写队列 | 64 项，每项 deadline=请求 deadline；写本体 SetWriteDeadline |
| Wait 后 drain 宽限 | 300 ms |
| orphan 退役宽限 | 200 ms nudge → TERM 1s → KILL → reap ≤2s |
| 诊断队列 | 256 条，满丢最旧+计数；sink 单 goroutine |
| 启动/请求超时 | env `CODEGRAPH_GO_START_TIMEOUT_MS`/`...REQUEST_TIMEOUT_MS`（默认 30 s/120 s，不变） |

## 验证（候选，cwd=go/codegraph-go）

- `gofmt -l .` → 空
- `go build ./...` → EXIT 0；`go vet ./...` → EXIT 0
- `go test -count=1 -v -timeout 180s ./...` → EXIT 0，38 PASS / 0 FAIL
- `go test -count=1 -race -v -timeout 180s ./...` → EXIT 0

## 红/绿对照（worker 侧；独立 verifier 另跑）

- 红：`git archive 20f0648 go → /tmp/cg-base`，注入
  `diag_baseline_inject_test.go`；`go test -run TestDiagBase` → EXIT 1，
  **11 项实质断言 FAIL**（missing phase=initialize/exit=3、unbounded carry
  mid-flood 8192B、`TOKEN="abcd"` 原样泄密、start timeout/orphan/write-fail
  三处子进程泄漏、sendRequest 阻塞 5s 超限、identity race 裸
  disconnected），非 build/undefined/no-tests 假红。
- 绿：同一文件拷入候选 → 12/12 PASS（含 helper 自身）。
- 仅候选正向（新符号）：TestClientIdentityRaceExitDiag（确定性屏障）、
  TestClientDiagSinkBackpressureBounded（diagOut 钩子）、
  TestStderrDiagSummaryBoundedAndSanitized 增量断言（sanitizeDiagText）、
  TestConsumeStderrCrossChunkBound（carryTrunc/诊断语义）。

## 遗留/边界

- 功能模式（sandbox 下 codegraph-go 真实可用）尚未实现；daemon 侧诊断由
  另一 worker 负责。未宣称真实 PiG/sbx reviewer 入口已过。
- 脱敏只覆盖已知凭据形状；drain 窗口外 stderr 尾段可能丢；诊断队列满时
  丢最旧（计数上报），不承诺无损。
- 退役只发信号给自有 PID，不建进程组、不触碰共享 daemon。
- 历史 notes 只读未改；本文件为既有 DIAG-SANDBOX-EXIT.md 的就位更新。
