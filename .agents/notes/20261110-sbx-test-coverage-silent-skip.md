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

## 收口记录（fix/sbx-addr-and-go-hygiene，worker 本地验证通过待 C 档门禁）

- `preset.go`：抽 `dedupeTrustedHomes` + `trustedAgentHomesFor`/`trustedLookupHomesFor`/`agentLookupDirsForHomes` 显式 homes 版本，`ResolveEntryAgentPaths` 薄封 `resolveEntryAgentPaths(trustedHomeDir())`，生产行为逐字节等价。
- `preset_test.go` 尾部新增 export_test 段（仅测试二进制）：`ResolveEntryAgentPathsForTest`。
- `sandbox_test.go` 两处宿主依赖断言改为 t.TempDir() 夹具（versioned claude 安装根 + agyStateBin 三成员），全量 `go test -v` SKIP 计数=0。

## [pattern:verifier-transient-fail] 结案（2026-11-10，fix/sbx-addr-and-go-hygiene）

2026-11-08 悬案（verifier ROW48 一次未定位的 `cmd/sbx` 瞬时 FAIL）已定位
并根治。

- **根因**：`appendAgyStateReadWrite`（`go/sandbox/preset.go`）在父进程
  `os.ReadDir(agyStateDir)` 快照枚举 antigravity-cli 状态根的根级非目录
  文件（`settings.json`/`history.jsonl`/`*.db` 及 SQLite 瞬态伴侣
  `*.db-shm`/`*.db-wal`/`*.db-journal`、临时原子写文件）并追加进
  `Policy.ReadWrite`；经 `childExecSpec` 传给受限子进程后，`sandbox.Apply`
  以 `unix.Open(path, O_PATH|O_CLOEXEC)` 打开。宿主 agy 在枚举与 Apply
  的毫秒级窗口内经 WAL checkpoint 删除 `.db-shm` 时即 `ENOENT`，整个
  `sbx` 报 `apply sandbox policy failed: sandbox: open "...db-shm": no
  such file or directory` 崩溃退出——枚举→打开 TOCTOU 竞态。
- **修复**：`Policy` 新增导出字段 `OptionalReadWrite`（`Validate` 同经
  `NormalizePaths` 规范化校验；仅标记「缺失可容忍」，授权仍完全由
  `ReadWrite` 决定）；`appendAgyStateReadWrite` 枚举的根级文件同步登记
  ——显式 `--rw`、`<workdir>/reports`、私有 `TMPDIR`、agyStateDataSubdirs
  数据子目录绝不登记，保持 fail-closed `ENOENT` 报错不变。字段随
  `childExecSpec.orw`（`PI_SANDBOX_INTERNAL_CHILD_SPEC`）与
  `helperConfig.orw`（`PI_SANDBOX_BROKER_CFG`，helper/broker/
  pi-test-runner 重建 Policy 全点位）100% 透传；`WithBestEffort` 值拷贝
  自动保留。`buildPathRuleEntries` 将 `OptionalReadWrite` 归一化为集合
  打标 `pathRuleEntry.transient`，`addPathRule` 仅对 `transient` 条目
  容忍 `ENOENT`（含悬空软链）安全跳过，不放宽既有 `optional` 软路径与
  `BestEffort` 语义。
- **结案证据（先红后绿）**：修复前等价形态实证——显式 `--rw <missing>`
  严格模式仍报 `sandbox: open "/tmp/sbx-transient-missing.db-shm": no
  such file or directory` 退 1（与悬案崩溃字面量同构）；修复后
  `TestApplyOptionalReadWriteTransientMissing`（缺失 .db-shm 同时在
  ReadWrite+OptionalReadWrite 的 Apply 子进程退 0，仅 ReadWrite 的对照
  子进程 ENOENT 失败）、`TestChildSpecOptionalReadWriteEndToEnd`
  （spec JSON roundtrip + 受限子进程端到端退 0 输出 transient_ok /
  strict ENOENT / 显式 `--rw` 缺失非零）、
  `TestAppendAgyStateReadWriteMarksRootFilesOptional`（枚举文件双登记、
  数据子目录与 bin 不入 ORW）、`TestAddPathRuleTransientENOENT` 与
  `TestBuildPathRuleEntriesMarksOptionalReadWriteTransient` 全绿；
  `go/sandbox` `go test -race -count=3 ./...` 三轮零失败。
- **同步索引**：`.agents/notes/20261108-sbx-mount-seal-run-view.md` 的
  `[pattern:verifier-transient-fail]` 条目已标记结案（worktree 副本；
  主检出原始登记副本为只读未动，本 worktree 副本为权威结案版本）。

## [pattern:cross-repo-regex-deploy-order] 观察项（oracle O1）

`dispatch-agent` 仓库 `3356d0c` 已先行合入并部署为
`0.7.1+3356d0c04020`，其收紧后的父终端地址校验正则在 sbx 侧
`reportAddrRe` 收紧（`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`）落地前就已
在生产侧生效——两个仓库对同一地址形态的校验正则存在跨版本偏斜风险
（sbx 放宽而 dispatch-agent 已收紧时，sbx 接受的地址会被下游拒绝），
但该偏斜窗口因 dispatch-agent 先行部署已在上线前消弭，本分支合入后
两侧口径即对齐。教训：跨仓同一校验口径变更须核对两侧部署顺序，先发
的仓库决定偏斜窗口方向。
