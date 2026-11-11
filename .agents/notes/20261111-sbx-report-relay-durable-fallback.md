---
date: 2026-11-11
module: go/sandbox
status: active
---

# sbx 回报中继无持久兜底：一次实测投递失败的缺陷登记（2026-11-11）

[pattern:report-relay-no-durable-fallback]（计数 1/2，首次记录）

> **本笔记是登记而非修复。** 本轮为 A 档纯文档任务，零代码改动、零测试改动；
> 是否立项修复由用户另行决定。下文「修复方向」仅为候选方案，未评估、未实施。

## 现场（本次实测）

一次 claude 执行体的只读角色派发（`sbx --preset reviewer-agent --report-to t5`）中，
子代理按派发纪律做**唯一一次** `hc send` 回报，返回退出码 75、原因
`report delivery failed … signal: killed`，报告未能经中继送达；主代理改用一次
`hc read <子终端> --lines 120` 才取回完整原文。

- 子代理自报正文约 **3474 UTF-8 字节**，低于中继上限
  `reportMaxTextBytes = 8000`（`report_relay.go:46`），**不是超限截断**。
- 中继投递实现：`deliver()`（`report_relay.go:407`）以
  `exec.CommandContext(ctx, r.hcPath, "send", r.parent, text)`（`:419`）投递；
  超时 `reportHcTimeout = 5 * time.Second`（`:51`），ctx 挂在 relay 根 ctx
  （`:413-417`）；超时即 kill 子进程，`process()`（`:354`）在 `:366` 回
  `reportStatusFailed` + `delivery failed: <err>`。
- 关键量级对照：同机 `hc get` / `hc ls` 实测各约 2ms、对非法地址 `hc send`
  rc=3 约 2ms（主代理实测，本会话 hc 通道被角色守卫拦截未能复测）。5s 预算
  对正常路径非常宽裕，本次超时属**投递时机拥塞**（父终端繁忙时的串行投递 +
  herdr-core 当日曾重启一次），不是 `hc` 常态慢。
- 已配平项（记录为设计正确、不建议改动）：`reportDrain = 6s`（`:52`）大于
  `reportHcTimeout`，注释明确「保证在途投递可收尾」；读写 deadline 各 5s
  （`:53-54`）；帧上限 64KiB（`:45`）。

## 缺陷清单（观察项，均未修）

1. **瞬时失败与永久失败不分类**：超时 / `signal: killed`（可重试）与
   `non-parent address` / `empty text` / `rate limited` / `bad frame`
   （永久拒绝）都收敛为客户端侧同一个非零退出码，被委托方无法判断是否值得
   重试。（`process()` 的 rejected/failed 两类状态在客户端统一映射为
   rc=75，见 `:354-368`。）
2. **无持久兜底**：中继不做「每次尝试（含失败）落一份本地文件并回带路径」，
   报告的唯一副本就是那一次投递；失败后依赖父代理用 `hc read` 捞
   scrollback。本次子终端为 53×41 小屏，正文 3474 字节恰好未滚出——属运气
   而非设计保证；正文更长或会话已退出时，交付物会永久丢失且无法复核验收。
3. **限流也落到不可重试路径**：`reportBurstTokens = 3` /
   `reportRefillEvery = 30s`（`:48-49`）撞上限时同样只回 `rejected`
   （`rate limited`，`:363-364`），与协议错误共用一种结果。
4. **任务纪律与实现强度不匹配**：派发契约要求「只发一次、勿重试」，而中继
   无任何兜底，二者叠加把整条回报通道变成单点。

## 修复方向（候选方案登记：未评估、未实施）

- 优先补第 2 条：中继每次尝试落盘（例如 `<report-dir>/attempts/<n>.md`，
  含失败项），并在拒绝/失败原因里回带该路径，使原文永远可取；
- 其次拆错误类（retryable vs permanent），由**客户端**做有界退避重试
  （约 100/400ms 两次），避免中继侧自动重投造成重复报告；
- 不建议单纯放大 `reportHcTimeout`：会拖慢 relay 关闭与沙箱退出
  （`reportDrain` 需同步放大），且掩盖 core 侧真实故障。

## 协调事实（主代理实测 + 本轮复核）

- 本仓存在**在制品分支** `fix/sbx-addr-and-go-hygiene`（worktree
  `/root/workspace/.wt/pi-extensions/fix-sbx-addr-and-go-hygiene`，
  本轮复核其 `git status --porcelain` 为 18 个文件未提交），它**恰好改了同一个文件**
  `go/sandbox/cmd/sbx/report_relay.go`：把 `reportAddrRe` 从
  `^[A-Za-z0-9._:/-]{1,64}$` 改为 `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`
  （拒绝首字符 `-`、去掉 `:` 与 `/`，防地址形态注入；该分支内位于 `:89`）。
  **本笔记登记的后续修复若立项，必须与该分支协调**，不得假定主干行号不变。
- 本笔记引用的行号以 main `9f7f53d`（本 worktree 基线）实际读取为准；
  与任务书预估行号有一处偏差：`reportMaxTextBytes` 在 `:46`（非 `:45`，
  `:45` 是 `reportMaxFrame`）；`process()` 定义在 `:354`、失败分支在 `:366`；
  `deliver()` 在 `:407-422`（非 `:410-426`）。
- 行为观察来自运行现场（`/root/.local/bin/sbx`，`sbx --version` 输出
  `0.4.0+9f7f53dcded2`，与 main `9f7f53d` 一致；二进制 mtime 2026-11-11
  10:22，但该构建来源 commit 未以独立证据链确证）；代码定位以 main
  `9f7f53d` 为准。不声称运行现场与代码定位已完全对齐。
