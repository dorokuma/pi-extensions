---
date: 2026-11-10
module: go/sandbox
status: active
---

# sbx trusted-agent-homes 测试覆盖静默 SKIP 观察项（2026-11-10）

[pattern:test-coverage-silent-skip]
[pattern:env-home-trust-boundary]

## 观察

`fix/trusted-agent-homes-sandbox-preset`（merge `31ede76`）新增的
`go/sandbox/sandbox_test.go` 用例中，存在依赖宿主文件系统布局的子断言：

- claude 相关断言依赖宿主的 claude **版本化安装目录**存在；
- agy 相关断言依赖宿主存在 agy 安装。

宿主不满足对应布局时子断言走 `Skip`，测试仍为 PASS。

## 影响面

若 CI 环境无该布局（无 claude 版本化安装 / 无 agy 安装），则对应场景的实际
覆盖静默为零——CI 呈现全绿，但 env-home 信任边界枚举路径未被真实断言到。

## 处置建议（不阻断）

oracle 观察项，当前不阻断。后续可考虑：

- 在 CI 层补一条 SKIP 计数告警（测试输出统计 Skip 子断言数）；
- 或构造宿主无关的临时版本化目录夹具，使断言不依赖真实宿主布局。
