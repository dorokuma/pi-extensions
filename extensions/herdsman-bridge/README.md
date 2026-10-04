# herdsman-bridge

## 概况
一个**极薄的转发壳**：把 Herdsman 的 pi 扩展从本机工作区包再导出，使每个 pi 会话都加载
Herdsman 的后台通道扩展。实现体不复制到本仓，而是直接再导出宿主仓源码的默认导出——
由 pi 的 jiti 扩展加载器当 TypeScript 直接加载。

```ts
export { default } from "/root/workspace/herdsman/packages/herdsman-pi/src/index.ts";
```

`herdr-agent-state.ts`（另一个扩展文件）由 **herdr 二进制托管**，本仓与本扩展都按约定
在其旁挂自定义钩子，**绝不写它**（见 AGENTS.md 铁律⑤）。

## 权威源 / 目标 / 来源
- 权威源（本仓，唯一）：`extensions/herdsman-bridge/src/herdsman-bridge.ts`
  （绝对路径 `/root/workspace/pi-extensions/extensions/herdsman-bridge/src/herdsman-bridge.ts`）
- 运行时再导出目标（宿主仓，禁改）：`/root/workspace/herdsman/packages/herdsman-pi/src/index.ts`
- 目标安装路径：`/root/.pi/agent/extensions/herdsman-pi.ts`
  （注意：仓内文件名是 `herdsman-bridge.ts`，安装后的部署文件名是 `herdsman-pi.ts`，内容逐字一致）
- 来源：自本机部署副本逐字纳管（`/root/.pi/agent/extensions/herdsman-pi.ts`），内容一字未改。

## 纳管时的一致性记录
- md5：`3d909e1a87dd28a8d21e7e9b00668bad`
- 字节数：`890`
- 与部署副本 `cmp` 逐字节一致（2026-10-04 收纳时校验）。

## 出处与许可
- 本仓只承载这个**转发壳**；其再导出的实现体、许可证与作者归属均在宿主仓
  `/root/workspace/herdsman`（包 `@dorokuma/herdsman-pi`）。据宿主仓 `packages/herdsman-pi/package.json`
  观察：`license: MIT`、`author: dorokuma`。本仓不对该实现体独立主张任何许可，真正的权威以
  宿主仓为准。改实现请回宿主仓走既有流程（铁律③）；本仓只在此维护转发壳。

## 双加载防护（install.sh 强制执行）
再导出会与「settings.json `packages` 里装的同名 npm 包」造成**同扩展双实例**（双 daemon
client / 重复注册）。因此 `install.sh` 在写本扩展前必读
`/root/.pi/agent/settings.json` 的 `packages`，若已含 `@dorokuma/herdsman-pi` 相关 npm 包则
**显式报错退出**（详见 `federated/registry.json` 的 `doubleLoadGuard`）。

## 改动流程（铁律）
1. 只在本仓 `extensions/herdsman-bridge/src/herdsman-bridge.ts` 改壳；实现体回 `/root/workspace/herdsman`。
2. `bash install.sh herdsman-bridge` 同步后 `/reload` 或新会话生效。
