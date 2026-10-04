# auto-continue

## 概况
在上游 provider / 中转站 / 网络出现**瞬时**失败并沉降后，自动补发一次用户 "continue"，
让模型把任务重新捡起来。Pi 自带的 `settings.retry` 只覆盖 429 / 5xx / 超时；本扩展补其
遗漏——典型如中转站 405、Cloudflare 52x、空 HTML 响应体、内置重试耗尽。它**不**重试
401/403（鉴权）、配额、上下文溢出、截断（`stopReason: length`）或用户中止。

运行期内按 `esc` 中止的语义见源码顶部 v2 注释（`userCancelled` 锁的收敛范围与 pi 事件
模型的限制）。

## 权威源 / 目标 / 来源
- 权威源（本仓，唯一）：`extensions/auto-continue/src/auto-continue.ts`
  （绝对路径 `/root/workspace/pi-extensions/extensions/auto-continue/src/auto-continue.ts`）
- 目标安装路径：`/root/.pi/agent/extensions/auto-continue.ts`
- 来源：自本机部署副本逐字纳管（`/root/.pi/agent/extensions/auto-continue.ts`），内容一字未改。

## 纳管时的一致性记录
- md5：`4341280ec027415bf7b9276d965a49e7`
- 字节数：`29714`
- 与部署副本 `cmp` 逐字节一致（2026-10-04 收纳时校验）。

## 出处与许可
- **作者声明为自研，未经第三方独立核实。** 本仓不做独立溯源，也不代其声明任何许可证
  归属；改动请回本仓 `extensions/auto-continue/src/`，类型检查通过后
  `bash install.sh auto-continue` 同步。

## 改动流程（铁律）
1. 只在本仓 `extensions/auto-continue/src/auto-continue.ts` 改；**严禁**直接编辑
   `/root/.pi/agent/extensions/auto-continue.ts`。
2. `pnpm setup && bash install.sh auto-continue` 后，`/reload` 或新会话生效。
