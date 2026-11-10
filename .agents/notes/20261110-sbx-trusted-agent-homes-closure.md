---
date: 2026-11-10
module: go/sandbox
status: active
---

# fix/trusted-agent-homes-sandbox-preset 合并收口记录（2026-11-10）

[pattern:closure-record]
[pattern:env-home-trust-boundary]

## 分支与合并信息

- 工作分支：`fix/trusted-agent-homes-sandbox-preset`（已合并、已删除）
- 分支 HEAD：`bd8d32a`（docs(sandbox): 修正 trustedLookupHomes 超集注释——收紧结论仅限两面，三面同步撤销 homeDir() 枚举（纯注释修正，不改行为））
- 含 feature commit 四个（新→旧）：
  - `bd8d32a` docs(sandbox): 修正 trustedLookupHomes 超集注释
  - `d2ced59` fix(sandbox): 二进制收录面改由 trustedLookupHomes 枚举，撤出调用方可控 $HOME
  - `e975fc5` fix(sandbox): isolate agyStateBin enum test fixtures, assert host homes
  - `c9f81bd` fix(sandbox): 隔离 HOME 下按 trustedHomeDir 枚举宿主受信 agent 安装/查找根
- 合并 commit（--no-ff）：`31ede76`
- 门禁档位：C 档（verifier 三轮证据、reviewer 第三轮 confirmed-closed、oracle 方向按用户批准实施、gate_scan 无命中）
- worktree `/root/workspace/.wt/pi-extensions/fix-trusted-agent-homes-sandbox-preset` 已
  `worktree remove`；登记 JSON
  `.wt/pi-extensions/fix-trusted-agent-homes-sandbox-preset.json` `status=merged`
- 远端：`origin https://github.com/dorokuma/pi-extensions.git`，`git push origin main`
  `fc0875c..31ede76` 成功

## 变更概述

- `go/sandbox/preset.go`（+144/-27 行差）：env-home 信任边界收紧——二进制收录面不再
  信任调用方可控的 `$HOME`，改由 `trustedLookupHomes`/`trustedHomeDir` 枚举宿主受信
  agent 安装/查找根。
- `go/sandbox/sandbox_test.go`：新增 277 行测试，agyStateBin 枚举测试夹具隔离，
  断言宿主 home 受信根。

## 遗留项

- 无未闭环遗留项。
- 观察项（不阻断）：新增测试在宿主无 claude 版本化安装/无 agy 安装时对应子断言会
  SKIP，CI 若无该布局则场景覆盖静默为零。详见同日期
  `20261110-sbx-test-coverage-silent-skip.md`（[pattern:test-coverage-silent-skip]）。
