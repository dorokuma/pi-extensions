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
  此外 dial/写帧/空回包等本地失败一律 rc=75（`:56`、`:62`、`:79`、`:84`）；
  `hc --help` / `hc --version` 打印受限说明后退 0（`:38-40`），
  其余非 `send` 形态退 126（`:43-47`）。
- 关键量级对照：同机 `hc get` / `hc ls` 实测各约 2ms、对非法地址 `hc send`
  rc=3 约 2ms（主代理实测，本会话 hc 通道被角色守卫拦截未能复测）。
  **注意：`hc get`/`hc ls` 约 2ms 只说明 hc 本体轻量，不是成功 `hc send`
  路径的耗时证据**，不能据此排除 send 本身慢。5s 预算对正常路径的宽裕度
  由此降级为估计；本次 `signal: killed` 有两条可产生同一表象的路径：
  ① `reportHcTimeout` 5s 超时 kill（`report_relay.go:417-419`）；
  ② `close()` 的 drain 超时经 `r.cancel()` 取消在途 deliver
  （`report_relay.go:299-305`）。具体走哪条待核（见「修复方向」末项）。
- 已配平项（记录为设计正确、不建议改动）：`reportDrain = 6s`（`:52`）大于
  `reportHcTimeout`，注释明确「保证在途投递可收尾」；读写 deadline 各 5s
  （`:53-54`）；帧上限 64KiB（`:45`）。

## 缺陷清单（观察项，均未修）

1. **多种性质不同的拒绝被压成同一个 `rejected`/rc=1**：中继把恢复窗口
   与是否值得重试都不同的四类拒绝压进 `reportStatusRejected`，客户端因此
   无法判断该等 30 秒、该放弃、还是该立即重试。四类分别是：
   - **突发桶耗尽**：`reportBurstTokens = 3`（`report_relay.go:48`）、
     `reportRefillEvery = 30s`（`:49`）。性质是**可恢复但恢复窗口是 30 秒
     量级**，100/400ms 的有界重试不会让它恢复。
   - **会话封顶**：`reportSessionMax = 20`（`:50`），判定在 `takeToken()`
     的 `if r.sent >= reportSessionMax || r.tokens <= 0 { return false }`
     （`:394`）；`sent` 只在 `:398` 递增、**不随补令牌重置**，对单次会话
     而言是**永久**拒绝，重试无意义。
   - **中继连接超限**：`serve()` 在 `len(r.conns) >= reportMaxActiveConns`
     时直接写 `{reportStatusRejected, "relay busy"}` 拒绝帧后丢连接
     （`:250-253`），不经 `process()`；同样是 rc=1，但它是瞬时拥塞信号，
     与前三类的语义又不同。
   - **投递/本地失败回 75 的多种来源**：`process()`（`:365-366`）把所有
     `deliver()` 错误一律回 `reportStatusFailed`；客户端
     `reportSend`（`report_client_linux.go:53`）对缺
     `PI_SANDBOX_GO_EXEC_DIR`（`:53-56`）、拨号失败/中继不可用（`:58-62`）、
     写帧失败（`:79`）、空回包（`:84`）、非 0/1 状态（`:93-95`）也各自回 75。
     因此**不能用 rc=1/75 反推「永久/瞬时」**——状态字节表达的是协议分类，
     不是重试性质。
   另：`takeToken()` 判在 `process()`（`:362-363`）里与
   `non-parent address`（`:355-356`）、`empty text`（`:358-360`）这类
   永久拒绝同归 `reportStatusRejected`，是同一缺陷的一部分。
2. **「只发一次」纪律 × 无客户端重试**：派发契约要求子代理**只发一次、
   勿重试**（hc send 回报是强制收口义务），而客户端（`reportSend`，
   `report_client_linux.go:53`）对任何失败——包括投递失败 rc=75 这类明确
   可重试的形态——均无重试，二者叠加把整条回报通道变成单点。

## 撤销注记（本版已删除的方案）

- 首版「无持久兜底：中继每次尝试落盘 attempts/*.md 并回带路径」方案已撤销。
  **首版该项理由表述有误（中继运行在沙盒外 sbx 外层父进程，`report_relay.go:24-27`
  注释与 `main.go:430-435` 明确父进程本身保持在 Landlock 规则集之外，父侧写盘
  不必然等于子侧获得写权限），「给只读角色开可写路径、扩大信任边界」的论证
  站不住，已删除；结论未变。** 撤销依据改为三条事实：① 报告本身已有持久
  副本——即子代理会话本身，既定恢复路径是父侧一次 `hc read <子终端>
  --lines N` 取回原文（本轮正是这样恢复的）；② 落盘是把同一内容存两处，
  新增一个磁盘写失败面；③ 兜一次投递抖动的收益不抵其成本。
- 首版以「53×41 小屏恰好未滚出属运气而非设计保证」论证无兜底，降级为事实
  描述：`hc read` 可取回的范围取决于 scrollback 大小与会话存活；正文更长
  或会话已退出时该路径可取内容受限，不构成落盘理由。

## 修复方向（候选方案登记：未评估、未实施）

- a) 中继侧把四类拒绝拆开、给出可判别的状态/原因：把突发桶耗尽
  （30 秒级恢复窗口）与会话封顶（永久）区分开——**会话封顶不得迁成
  可重试**——`relay busy` 路径给出独立可识别原因。需同步：帧协议注释
  （`report_relay.go:37` 与客户端 `report_client_linux.go:24-25`）、
  客户端 `switch` 映射（`:87-95`）、`main_test.go` 现有断言——
  已有多处对状态字节的断言：`rate_limit_burst_and_session_cap`
  （`:686-716`，断言撞限回 `reportStatusRejected` + reason 含 "rate"）、
  `session_cap_via_injectable_clock`（`:719-743`）、
  `non_parent_address_rejected`（`:678-683`）、
  `delivery_failure_maps_status2`（`:880-886`，断言失败回
  `reportStatusFailed`）、`frame_size_boundary_64kib`（`:787-840`）、
  `active_conn_cap_prevents_dos`（`:989-1054`）。客户端侧退出码映射
  （rc=0/1/75/126）**无直接单元测试**；集成测试
  `TestSbxReportRelayIntegration`（`main_test.go:1237`，非 root 在
  `:1237-1238` skip，本轮未实跑）已断言 `send_hi_rc=0` /
  `hc_run_rc=126` / `send_other_rc=1`（`:1436-1438`），其发送端
  （`:1344-1348`）真实运行沙盒内受限 `hc` 客户端（argv[0] 分发入口
  `landlock_linux.go:182-185`，hc 软链来自 `preset.go:1098`）。
  **未找到 rc=75（投递失败/中继不可用）路径的断言，也没有重试行为
  断言——75 这条最关键的失败路径无人守，是真正的缺口。**
- b) 客户端对可重试失败（投递失败 rc=75，及 a 落地后的突发耗尽/relay
  busy 类瞬时状态）做**有界退避重试，至多一次**（约 100/400ms 量级），
  不新增任何落盘。
- `reportHcTimeout` **保持 5s 不变**（决策不变）：`reportDrain=6s >
  reportHcTimeout=5s`（`:51-52`）是刻意配平，放大超时须同步动 drain
  并拖慢 relay 关闭与沙箱退出。本次 `signal: killed` 的根因**降级为
  待核假设**（原稿定为「父终端繁忙 + core 重启拥塞」，证据不足）：
  ① 除 5s 超时外，`close()` drain 路径（`:299-305`）也会经根 ctx 取消
  在途 `deliver`，可产生同样的 kill；② `hc get`/`hc ls` 约 2ms 不构成
  成功 send 路径的耗时证据。确诊需三项证据：该次 send 实际耗时、当时
  relay 根 ctx / 活动连接状态、herdr-core 重启时间线与本次 dispatch 的
  先后关系。

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
