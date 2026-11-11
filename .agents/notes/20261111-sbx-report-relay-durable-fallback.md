---
date: 2026-11-11
module: go/sandbox
status: active
---

# sbx 回报中继限流归错桶：一次实测投递失败暴露的失败分类缺陷（2026-11-11）

[pattern:report-relay-failure-classification]（计数 1/2，首次记录）

> **本笔记是登记而非修复。** 本轮为 A 档纯文档任务，零代码改动、零测试改动；
> 是否立项修复由用户另行决定。下文「修复方向」仅为候选方案，未评估、未实施。
>
> 标签改名注记：首版使用 `[pattern:report-relay-no-durable-fallback]`，其指向的
> 「中继每次尝试落盘兜底」方案已被否决（理由见「修复方向」前的撤销注记），
> 保留旧标签会把后续归并引到已否决方向，故改现名。

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
- 客户端退出码映射（`report_client_linux.go:86-95`，文件头协议注释 `:24-25`）：
  回包 `0` → rc=0、`1` → stderr `hc: report rejected: …` + rc=1、
  其余（含 `2` 投递失败）→ stderr `hc: report delivery failed: …` + rc=75。
  即**协议拒绝（rc=1）与投递失败/超时（rc=75）本来就是两个不同退出码**；
  此外 dial/写帧/空回包等本地失败一律 rc=75（`:56`、`:62`、`:79`、`:84`），
  非 `send` 形态退 126（`:47`）。
- 关键量级对照：同机 `hc get` / `hc ls` 实测各约 2ms、对非法地址 `hc send`
  rc=3 约 2ms（主代理实测，本会话 hc 通道被角色守卫拦截未能复测）。5s 预算
  对正常路径非常宽裕，本次超时属**投递时机拥塞**（父终端繁忙时的串行投递 +
  herdr-core 当日曾重启一次），不是 `hc` 常态慢。
- 已配平项（记录为设计正确、不建议改动）：`reportDrain = 6s`（`:52`）大于
  `reportHcTimeout`，注释明确「保证在途投递可收尾」；读写 deadline 各 5s
  （`:53-54`）；帧上限 64KiB（`:45`）。

## 缺陷清单（观察项，均未修）

1. **限流被归错桶**：限流（`reportBurstTokens = 3` / `reportRefillEvery = 30s` /
   `reportSessionMax = 20`，`:48-50`）是**可重试**条件，却在
   `process()`（`:362-364`）里与 `non-parent address`（`:355-356`）、
   `empty text`（`:358-360`）这类**永久**拒绝同归 `reportStatusRejected`，
   客户端因此拿到 rc=1——在既有「rejected = 协议拒绝」语义下看起来像
   「别重试」，语义被误标。令牌桶撞限与会话上限（`takeToken()` `:373`，
   `reportSessionMax` 判定 `:394`）共用同一条 rejected 路径，属同一缺陷。
2. **「只发一次」纪律 × 无客户端重试**：派发契约要求子代理**只发一次、
   勿重试**（hc send 回报是强制收口义务），而客户端（`reportSend`，
   `report_client_linux.go:53`）对任何失败——包括投递失败 rc=75 这类明确
   可重试的形态——均无重试，二者叠加把整条回报通道变成单点。

## 撤销注记（本版已删除的方案）

- 首版「无持久兜底：中继每次尝试落盘 attempts/*.md 并回带路径」方案已撤销。
  事实依据：报告本身已有持久副本——即子代理会话本身，既定恢复路径是父侧
  一次 `hc read <子终端> --lines N` 取回原文（本轮正是这样恢复的）；新增落盘
  只是把同一内容存两处、多一个磁盘写失败面。且只读角色（reviewer/scout）
  跑在禁写沙盒里，为兜一个投递抖动给只读角色开可写路径，是**扩大信任边界
  换可靠性，代价大于收益**。
- 首版以「53×41 小屏恰好未滚出属运气而非设计保证」论证无兜底，降级为事实
  描述：`hc read` 可取回的范围取决于 scrollback 大小与会话存活；正文更长
  或会话已退出时该路径可取内容受限，不构成落盘理由。

## 修复方向（候选方案登记：未评估、未实施）

- a) 中继侧把 `rate limited` 从 `rejected` 挪到可重试状态（或新增一个状态
  字节），使客户端能区分「别重试」与「可重试」。需同步：帧协议注释
  （`report_relay.go:37` 与客户端 `report_client_linux.go:24-25`）、
  客户端 `switch` 映射（`:87-95`）、`main_test.go` 现有断言——
  已有多处对状态字节的断言：`rate_limit_burst_and_session_cap`
  （`:686-716`，断言撞限回 `reportStatusRejected` + reason 含 "rate"）、
  `session_cap_via_injectable_clock`（`:719-743`）、
  `non_parent_address_rejected`（`:678-683`）、
  `delivery_failure_maps_status2`（`:880-886`，断言失败回
  `reportStatusFailed`）、`frame_size_boundary_64kib`（`:787-840`）、
  `active_conn_cap_prevents_dos`（`:989-1054`）。客户端侧退出码映射
  （rc=0/1/75/126）无任何测试断言。
- b) 客户端对可重试失败（投递失败 rc=75，及 a 落地后的限流状态）做
  **有界退避重试，至多一次**（约 100/400ms 量级），不新增任何落盘。
- `reportHcTimeout` **保持 5s 不变**：同机 `hc get`/`hc ls` 实测约 2ms，
  超时源于投递时机拥塞（父终端忙 + 当日 core 重启），不是预算不足；
  且 `reportDrain=6s > reportHcTimeout=5s` 是刻意配平，放大超时须同步动
  drain 并拖慢 relay 关闭与沙箱退出，掩盖 core 侧真实故障。

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
