# prism

## 概况
为 Pi 提供两个 slash 命令，结果进会话滚动区、**不送模型**：
- `/quota` → 上游套餐用量（`prism quota`，可 `--provider` / `--json`）。
- `/usage` → 本地 token 账本（`prism usage`，`models|keys|accounts|providers|days|hours|errors` 预设）。
通过 `node:child_process` 的 `spawn` 调起 `prism` 二进制，用 pi-tui 的
`truncateToWidth` / 自定义 entry renderer 渲染静态报表。依赖 `/root/workspace/prism`
的 Go 二进制在 PATH 中。

## 权威源 / 目标 / 来源
- 权威源（本仓，唯一）：`extensions/prism/src/prism.ts`
  （绝对路径 `/root/workspace/pi-extensions/extensions/prism/src/prism.ts`）
- 目标安装路径：`/root/.pi/agent/extensions/prism.ts`
- 来源：自本机部署副本逐字纳管（`/root/.pi/agent/extensions/prism.ts`），内容一字未改。

## 纳管时的一致性记录
- md5：`e1c6c8debe8cc8a77425ca69f345d35d`
- 字节数：`7725`
- 与部署副本 `cmp` 逐字节一致（2026-10-04 收纳时校验）。

## 出处与许可
- **已核实为自研**（2026-10-04 修复轮回填）：另一路源码/工件核查判定为自研；作者亦声明
  `auto-continue` / `no-tables` 为自研。此前的「出处仍待核」占位**已作废**。本仓仍不代其
  声明任何许可证归属（如作者另行声明许可，以作者声明为准）。
- 与仓 `/root/workspace/prism` 的关系：该仓保持**纯 Go 后端**职责；本扩展（TS 侧）已
  收归本仓，改动只在本仓 `extensions/prism/src/` 进行，不碰 `/root/workspace/prism`。

## 改动流程（铁律）
1. 只在本仓 `extensions/prism/src/prism.ts` 改；**严禁**直接编辑部署副本。
2. `bash install.sh prism` 同步后 `/reload` 或新会话生效。
