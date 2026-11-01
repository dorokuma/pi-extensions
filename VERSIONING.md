# 版本管理规范

本文规定 `pi-extensions` monorepo 的版本号、标签、变更日志与发布流程。所有 Go 模块与可安装命令行工具在正式发布时遵循本规范。

## 语义化版本（SemVer）

版本号格式为 `MAJOR.MINOR.PATCH`（可选后缀如 `-rc.1` 仅用于预发布，正式发布不含后缀）。

| 升位 | 何时升 | 示例 |
|------|--------|------|
| **MAJOR** | 不兼容的 API / CLI / 策略行为变更（破坏性变更） | `0.9.0` → `1.0.0`，或 `1.2.3` → `2.0.0` |
| **MINOR** | 向后兼容的功能新增（新预设、新 flag、新导出符号） | `0.1.0` → `0.2.0` |
| **PATCH** | 向后兼容的缺陷修复、文档/测试补强、安全加固且不改对外契约 | `0.1.0` → `0.1.1` |

规则补充：

1. **首个公开版本从 `v0.x` 起**：`0.y.z` 表示 API 尚未冻结；`0.y` 内仍可能出现破坏性变更，但必须在 CHANGELOG 中用 `### Breaking Changes` 标明，并按需升 MINOR（而非只升 PATCH）。
2. 进入 `v1.0.0` 后，破坏性变更必须升 MAJOR，并在 CHANGELOG 与发布说明中显式列出迁移步骤。
3. 预发布标签（如 `v1.2.0-rc.1`）不得覆盖已推送的正式标签；正式发布去掉预发布后缀。

## Monorepo 下的 Go 模块标签

本仓库是多模块 monorepo。Go 模块不在仓库根目录时，**git tag 必须带模块路径前缀**，以满足 Go 模块解析约定：

```text
<module-subdir>/v<MAJOR>.<MINOR>.<PATCH>
```

示例：

| 模块（go.mod） | 首个正式 tag |
|----------------|--------------|
| `github.com/dorokuma/pi-extensions/go/sandbox` | `go/sandbox/v0.1.0` |
| 其他 `go/<name>` 模块 | `go/<name>/vX.Y.Z` |

约定：

1. 每个独立 Go 模块维护自己的版本号与 `CHANGELOG.md`（放在模块目录下）。
2. 一次发布只打**该模块**对应的一个 annotated tag；不要为无关模块附带 tag。
3. 仓库级文档（如本 `VERSIONING.md`）的变更可随任一模块发布一并提交，不单独占用模块版本号。
4. 历史 `archive/*` 标签仅用于归档快照，**不是** SemVer 发布标签，不得用于 `go get`。

消费者引用示例：

```bash
go get github.com/dorokuma/pi-extensions/go/sandbox@v0.1.0
# 对应 git tag: go/sandbox/v0.1.0
```

## CHANGELOG 格式

每个已发布模块在其目录下维护 `CHANGELOG.md`，采用 [Keep a Changelog](https://keepachangelog.com/) 风格：

- 顶部为 `## [Unreleased]`，开发中变更先记在这里。
- 发布时将 Unreleased 条目移入 `## [X.Y.Z] - YYYY-MM-DD`。
- 常用小节：`### Added` / `### Changed` / `### Fixed` / `### Security` / `### Breaking Changes` / `### Known Risks`。
- 破坏性变更必须单独放在 `### Breaking Changes`，并写明旧行为、新行为与迁移方式。

## 分支规则

1. **`main` 只收经过审查与测试的合并**（或快进合并的已审已测分支）。禁止直接在 `main` 上做未审实验。
2. 功能开发走 `feat/<topic>`，缺陷修复走 `fix/<topic>`；其他类型可用 `docs/`、`chore/`、`build/` 等前缀。
3. **禁止对 `main` 做 force push**（`git push --force` / `--force-with-lease` 均禁止）。改写历史只允许在尚未推送或明确个人用途的特性分支上。
4. 发布提交应落在 `main` 上；发布 tag 必须指向 `main` 上的 commit。

## 发布流程清单

对某一 Go 模块（以 `go/sandbox` 为例）执行正式发布时，按序完成：

1. **测试全绿**：在模块目录执行 `gofmt -l`、`go vet ./...`、`go test -race ./...`（每步独立重定向到日志并检查 `rc=0`；测试输出不得经管道截断）。
2. **更新 CHANGELOG**：将 `[Unreleased]` 整理为 `[X.Y.Z] - 日期`，写清 Added/Fixed/Breaking/Known Risks。
3. **注入版本并提交**：命令行工具用 `-ldflags "-X main.version=X.Y.Z"` 注入；源码中的默认值可为 `dev`。将 VERSIONING/CHANGELOG/代码变更提交到 `main`（一个逻辑清晰的 commit）。
4. **打 annotated tag**：`git tag -a go/sandbox/vX.Y.Z -m "..."`。
5. **推送**：先 `git fetch`；确认 `origin/main` 是本地 `main` 的祖先后，普通推送 `main` 与该 tag。**绝不 `--force`**；不推送无关分支或 `archive/*` tag。
6. **安装**：将二进制安装到约定路径（默认 `/root/.local/bin/<cmd>`）；若已有现役文件，先备份为 `.<cmd>.bak-YYYYMMDD-<version>`。
7. **冒烟**：至少执行 `--version` 与一条代表性格命令，确认退出码与关键输出。
8. **清理**：删除本次 `/tmp` 测试产物。

若任一步失败：停下并报告，不扩大范围，不强制重试危险操作。

## 破坏性变更如何标注

1. CHANGELOG 使用 `### Breaking Changes` 小节，逐条说明。
2. 提交信息使用 `feat!:` / `fix!:` 或正文 `BREAKING CHANGE:` 页脚（Conventional Commits）。
3. `v0.x`：允许在 MINOR 升位中引入破坏性变更，但仍须按上两条披露。
4. `v1.0.0+`：破坏性变更必须升 MAJOR；CLI 删除 flag 或改变默认安全语义时，优先先提供弃用期（警告 + 旧别名），再在下一 MAJOR 移除。
