# pi-extensions/go

pi 全面 Go 化迁移工程（eqi12 开发机环境）。

- 仓库位置：`/root/workspace/pi-extensions/go/`
- 根 Module：`github.com/dorokuma/pi-extensions/go`（组件为独立嵌套 Module）
- 详细规划与批次推进表请参见 [go/PLAN.md](PLAN.md)。

## 仓库与目录用途

本项目位于 `pi-extensions` 仓库下的 `go/` 子目录中，旨在将 eqi12 上的 coding agent 整体技术栈全面迁移至 Go 二进制体系，逐步替换原版 TS/Node 实现（包括 pi 主程序、自研扩展、npm 依赖包、context-pruning、herdsman 扩展及守护进程等），直至彻底卸载 Node 运行时。

批次推进概要（详见 [go/PLAN.md](PLAN.md)）：
- 第 0 批：并行环境搭建（PiG 部署、隔离配置目录、仓库骨架）
- 第 1 批：小件扩展移植（herdr-agent-state、exclude-tools-guard、no-tables、pi-env、pi-tinyfish、pi-context-pruning；gemini-safety 已弃用并删除）
- 第 2 批：中件扩展移植（moshi-hooks、ctxmode、jev-gate、ctx-orchestrate、auto-continue、codegraph-go）
- 第 3 批：界面类扩展移植（cache-stats、pi-my-compact-ui）
- 第 4 批：大件扩展移植（pi-vcc、rpiv-ask-user-question、herdsman 扩展侧）
- 第 5 批：herdsman 守护进程与 CLI Go 化（位于 herdsman 仓库 `go-port` 分支 `go/`）
- 第 6 批：切换与卸载 Node（需人工确认）

## Module 架构与组件规范

本项目采用**根 Module + 组件嵌套 Module**的复合架构：

1. **架构构成**：
   - **根 Module**（`go.mod`：`github.com/dorokuma/pi-extensions/go`）：负责管理工程根脚手架、Makefile 仓级门禁、构建验证工具及公共库（如 `internal/version`）。
   - **组件嵌套 Module**（`go/<name>/go.mod`）：每个移植的扩展组件均拥有独立的根级子目录与独立的 `go.mod`（例如 `go/no-tables/go.mod` 声明 `module github.com/dorokuma/pi-extensions/go/no-tables`），每个组件子目录自身就是一个独立的 PiG 扩展。

2. **为什么组件必须自带 `go.mod`**：
   - 依据 PiG 扩展加载规范（只读参考仓 `/root/workspace/ref/PiG` 中的 `docs/extension-authoring.md:346`，PiG 源码检出，不在本仓内），PiG 在扫描扩展目录时，依据目录中是否存在 `go.mod`（或 `Cargo.toml`、Python factory 等）判定其为一个独立的语言扩展单元，且仅加载扩展根目录自身。若没有独立的 `go.mod`，PiG 将无法将其作为 Go 扩展正确识别与加载。

3. **SDK 解析机制与禁止 `replace` 指令**：
   - **本地环境与静态分析（`go.work` + `go/pigsdk`）**：在本地开发、代码静态分析（`go vet`）、单元测试（`go test`）及 IDE 补全时，依赖根目录的 `go/go.work` 将 `github.com/MichaelKinsy/PiG/extensions/sdk` 统一解析至本地镜像 `go/pigsdk`。
   - **PiG 运行时构建注入（`GOWORK=off` + staged SDK）**：PiG 在构建与加载扩展时，会显式传入环境变量 `GOWORK=off`（依据只读参考仓 `/root/workspace/ref/PiG` 中的 `coding/extension/host/subprocess/builder.go:853`，PiG 源码检出，不在本仓内），并通过临时 modfile 将依赖重定向至当前运行环境的 staged SDK 目录（依据只读参考仓 `/root/workspace/ref/PiG` 中的 `docs/extension-authoring.md:352`）。
   - **禁止 `replace` 约束**：组件的 `go.mod` **绝不得书写 `replace` 指向本地 SDK**。因为 PiG 构建器遵循「显式 replacement 优先」规则，组件若带有本地路径的 `replace`，将劫持 PiG 运行时的 staged SDK 注入通道，破坏跨环境部署与便携性。

4. **新增组件的标准步骤**：
   - **建立组件子目录**：在 `go/` 下新建 `<component>/` 目录；
   - **初始化 `go.mod` 与扩展工厂**：在组件目录下创建 `go.mod`（声明对应 module 并依赖 `github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0`），并在 `extension.go` 中实现 `func Extension() *sdk.Extension` 工厂入口；
   - **纳入工作区**：在 `go/go.work` 的 `use` 列表中追加 `./<component>`，打通本地 SDK 解析；
   - **门禁自动覆盖**：根目录 Makefile 通过 `$(wildcard */go.mod)` 自动发现新组件，执行 `make build`、`make test`、`make vet` 即可自动执行该组件的构建、测试与静态分析。

## 目录约定与纪律

- 复合 Module 架构：根 Module（Go 1.26.0）管理脚手架与通用包，各组件为独立嵌套 Module。
- 组件独立目录：每个移植组件在 `go/` 根目录下拥有独立的子目录（如 `no-tables`）。
- **受 `pi-extensions` 仓库的 `AGENTS.md` 铁律约束**：
  - 提交时**只 add `go/`**，绝不夹带仓内已有未提交改动（如 `extensions/exclude-tools-guard/` 下的未提交修改）。
  - **绝不写 `herdr-agent-state.ts`**（由 herdr 二进制托管）。
- 提交纪律：每个组件单独进行一次提交，保证改动边界清晰与可回溯。
- 报告归档：每批次执行总结与验证记录存放于 `go/reports/` 目录下（如 `go/reports/batch-N.md`）。

## 构建与测试命令

本项目通过 Makefile 提供标准化构建与验证入口（在 `/root/workspace/pi-extensions/go` 目录下执行），门禁命令自动覆盖根 module 与所有嵌套组件模块：

- 编译全部包：
  ```sh
  make build
  # 遍历根 module 与所有嵌套组件模块执行 go build ./...
  ```
- 运行测试：
  ```sh
  make test
  # 遍历根 module 与所有嵌套组件模块执行 go test ./...
  ```
- 静态检查：
  ```sh
  make vet
  # 遍历根 module 与所有嵌套组件模块执行 go vet ./...
  ```
- SDK 校验：
  ```sh
  make sdk-check
  # 将本地 go/pigsdk 与当前环境 staged SDK 进行逐文件 md5sum 对齐校验
  ```
- 隔离目录验证：
  ```sh
  make install-test
  # 默认使用 PIG_ISOLATED_DIR ?= $(PIG_HOME_DIR)/agent（PIG_HOME 或 ~/.pig）
  ```
- 编译与验证 examples 扩展（通过 PiG runner 编译）：
  ```sh
  make examples-smoke
  # 等价于: ./bin/pig-test -e ./examples/hello --mode json -p "调用 hello_ping 工具并返回其结果"
  ```
- 运行 examples 扩展并调用工具：
  ```sh
  make examples-run
  # 等价于: ./bin/pig-test -e ./examples/hello -p "调用 hello_ping 工具并返回其结果"
  ```
- 查看目标帮助：
  ```sh
  make help
  ```

## pig 调用建议走 bin/pig-test

Makefile 的 `examples-*` 目标通过包装脚本 `./bin/pig-test` 调用系统 `pig`，避免环境变量把运行时重定向到生产 `/root/.pi/agent`。

- **原因（历史）**：迁移早期裸调 `pig` 曾在缺少环境变量时向宿主 `~/.pig` 写入大量文件（见 [go/reports/batch-0.md](reports/batch-0.md) 第 10 节）。试用隔离目录 `/root/.pig-test` 已废弃；当前正式 PiG home 即为 `~/.pig`。
- **包装实现**：`./bin/pig-test` 清除 `PIG_HOME`、`PIG_CODING_AGENT_DIR`、`PIG_CODING_AGENT_SESSION_DIR` 与 `PIG_USE_PI_DIRS`，再 `exec /root/.local/bin/pig "$@"`，使运行落在默认 `~/.pig`。
- **用法示例**：
  ```sh
  # 运行 examples/hello 扩展
  ./bin/pig-test -e ./examples/hello -p "调用 hello_ping 工具并返回其结果"

  # 诊断当前路径配置
  ./bin/pig-test diagnose
  # 或直接：pig diagnose
  ```

## examples 说明与限制

`examples/hello` 是用于打通与验证 PiG Go 扩展体系的最小示例，其设计与主模块存在明确边界：

1. **独立的嵌套 Module**：
   `examples/hello/go.mod` 声明自身为独立的 `module example.com/hello`。在 `go/` 根目录执行 `go list ./...` 仅会列出主 module 下的包（如 `github.com/dorokuma/pi-extensions/go/internal/version`），`examples/hello` 不在主 module 范围内，因此它不归根目录的 `go build ./...` 管。
2. **依赖 staged SDK，普通 `go build` 必然失败**：
   `examples/hello/go.mod` 声明的依赖为：
   ```go
   require github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0
   ```
   若直接在 `examples/hello` 目录下执行普通 `go build ./...` 或 `go mod download`，由于远端版本库并不存在该伪版本标签，必然报错失败：
   ```text
   $ cd examples/hello && go mod download
   go: github.com/MichaelKinsy/PiG/extensions/sdk@v0.0.0: reading github.com/MichaelKinsy/PiG/extensions/sdk/go.mod at revision extensions/sdk/v0.0.0: unknown revision extensions/sdk/v0.0.0
   ```
   该扩展**只能由 PiG 在构建期动态替换为 staged SDK 才能编译成功**（PiG 会在 runtime cell 构建期将 SDK 映射至 `$PIG_HOME/state/pigsdk/sdk-<hash>/`，该目录名随 SDK 内容哈希变化）。
3. **构建与运行标准入口**：
   必须使用 `make examples-smoke` 或 `make examples-run`（或通过 `./bin/pig-test -e ./examples/hello ...`）来进行编译与验证。

## 测试配置目录说明

- 默认 PiG home 为 `$PIG_HOME` 或 `~/.pig`（`PIG_ISOLATED_DIR` 默认 `$(PIG_HOME_DIR)/agent`）。
- Makefile 中的 `install-test` 预设了防御检查，严禁指向或覆盖原版生产目录 `/root/.pi/agent`。
- 若指定目录不存在，`make install-test` 会给出明确提示并自动建立该目录，避免误操作。

## 硬规矩摘要

1. **生产环境绝对不碰**：原版 pi 在全部 Go 版本验收前保持原样可用；严禁改动 `/root/.pi/agent/` 下任何文件，严禁修改 `~/.pi/agent/bin/pi` 启动器，严禁修改 `/root/.gemini/config/hooks.json`，严禁重启正在运行的 herdsman 守护进程。
2. **提交与代码归属**：所有新代码进入 `/root/workspace/pi-extensions/go/`，每组件一个独立嵌套子 module（含独立 `go.mod`），一个组件一次提交；提交时只 add `go/`，绝不夹带仓内已有未提交改动（如 exclude-tools-guard），绝不写 `herdr-agent-state.ts`。
3. **生产 pi 不碰**：PiG 使用 `~/.pig`；绝对不读取或修改原版 pi（`/root/.pi/agent`）的会话与设置。
4. **全绿测试准入**：每个组件提交前必须保证 `go build ./...`、`go vet ./...`、`go test ./...` 全部通过，并对照原版 TS 行为编写对等单测，并在 PiG 实际加载跑通。
5. **如实报告**：每批结束输出 `go/reports/batch-N.md`，忠实记录命令原文与行为差异，杜绝未验证推测。
6. **不改 PiG 源码**：若遇 PiG 上游缺口先在报告记录并采取规避策略，不直接修改 PiG 源码。
