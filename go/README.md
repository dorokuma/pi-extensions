# pi-extensions/go

pi 全面 Go 化迁移工程（eqi12 开发机环境）。

- 仓库位置：`/root/workspace/pi-extensions/go/`
- Module：`github.com/dorokuma/pi-extensions/go`
- 详细规划与批次推进表请参见 [go/PLAN.md](PLAN.md)。

## 仓库与目录用途

本项目位于 `pi-extensions` 仓库下的 `go/` 子目录中，旨在将 eqi12 上的 coding agent 整体技术栈全面迁移至 Go 二进制体系，逐步替换原版 TS/Node 实现（包括 pi 主程序、自研扩展、npm 依赖包、context-pruning、herdsman 扩展及守护进程等），直至彻底卸载 Node 运行时。

批次推进概要（详见 [go/PLAN.md](PLAN.md)）：
- 第 0 批：并行环境搭建（PiG 部署、隔离配置目录、仓库骨架）
- 第 1 批：小件扩展移植（gemini-safety、herdr-agent-state、exclude-tools-guard、no-tables、pi-env、pi-tinyfish、pi-context-pruning）
- 第 2 批：中件扩展移植（moshi-hooks、ctxmode、jev-gate、ctx-orchestrate、auto-continue、codegraph-go）
- 第 3 批：界面类扩展移植（cache-stats、pi-my-compact-ui）
- 第 4 批：大件扩展移植（pi-vcc、rpiv-ask-user-question、herdsman-pi 扩展侧）
- 第 5 批：herdsman 守护进程与 CLI Go 化（位于 herdsman 仓库 `go-port` 分支 `go/`）
- 第 6 批：切换与卸载 Node（需人工确认）

## 目录约定与纪律

- 单一 Go Module（`module github.com/dorokuma/pi-extensions/go`，Go 1.26.0）。
- 组件独立目录：每个移植组件在 `go/` 根目录下拥有独立的子目录（如 `internal/<component>` 或 `<component>`）。
- **受 `pi-extensions` 仓库的 `AGENTS.md` 铁律约束**：
  - 提交时**只 add `go/`**，绝不夹带仓内已有未提交改动（如 `extensions/exclude-tools-guard/` 下的未提交修改）。
  - **绝不写 `herdr-agent-state.ts`**（由 herdr 二进制托管）。
- 提交纪律：每个组件单独进行一次提交，保证改动边界清晰与可回溯。
- 报告归档：每批次执行总结与验证记录存放于 `go/reports/` 目录下（如 `go/reports/batch-N.md`）。

## 构建与测试命令

本项目通过 Makefile 提供标准化构建与验证入口（在 `/root/workspace/pi-extensions/go` 目录下执行）：

- 编译全部包：
  ```sh
  make build
  # 等价于: go build ./...
  ```
- 运行测试：
  ```sh
  make test
  # 等价于: go test ./...
  ```
- 静态检查：
  ```sh
  make vet
  # 等价于: go vet ./...
  ```
- 隔离目录验证：
  ```sh
  make install-test
  # 默认使用 PIG_ISOLATED_DIR ?= /root/.pig-test/agent
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

## pig 调用必须走 bin/pig-test

迁移工程期间，**严禁裸调系统中的 `pig`**，所有调用必须通过包装脚本 `./bin/pig-test` 或 Makefile 执行。

- **原因**：裸调 `pig` 会在缺少环境变量时触发默认回退机制，向宿主环境 `~/.pig` 写入文件（包括自动展开文档 bundle、生成锁文件、缓存及 agent 配置等；已实测证实 392 条目、10,455,075 字节污染，并在 [go/reports/batch-0.md](reports/batch-0.md) 第 10 节留存完整证据与清理记录）。
- **隔离实现**：`./bin/pig-test` 在头部清除 `PIG_CODING_AGENT_DIR`、`PIG_CODING_AGENT_SESSION_DIR` 与 `PIG_USE_PI_DIRS` 环境变量（防止绕过 `PIG_HOME` 或指向生产 `/root/.pi/agent`），并写死 `export PIG_HOME=/root/.pig-test`，再 `exec /root/.local/bin/pig "$@"`，确保一切运行时状态与产物严格收敛在 `/root/.pig-test` 隔离根目录下。
- **用法示例**：
  ```sh
  # 运行 examples/hello 扩展
  ./bin/pig-test -e ./examples/hello -p "调用 hello_ping 工具并返回其结果"

  # 诊断当前隔离路径配置
  ./bin/pig-test diagnose
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
   该扩展**只能由 PiG 在构建期动态替换为 staged SDK 才能编译成功**（PiG 会在 runtime cell 构建期将 SDK 映射至 `$PIG_HOME/state/pigsdk/go/`）。
3. **构建与运行标准入口**：
   必须使用 `make examples-smoke` 或 `make examples-run`（或通过 `./bin/pig-test -e ./examples/hello ...`）来进行编译与验证。

## 隔离测试配置目录说明

迁移期间必须确保新旧环境隔离运行：
- PiG 仅在隔离配置目录（默认 `/root/.pig-test/agent`）下启动与验证。
- Makefile 中的 `install-test` 预设了防御检查，严禁指向或覆盖原版生产目录 `/root/.pi/agent`。
- 若指定的隔离目录不存在，`make install-test` 会给出明确提示并自动建立该目录，避免误操作。

## 硬规矩摘要

1. **生产环境绝对不碰**：原版 pi 在全部 Go 版本验收前保持原样可用；严禁改动 `/root/.pi/agent/` 下任何文件，严禁修改 `~/.pi/agent/bin/pi` 启动器，严禁修改 `/root/.gemini/config/hooks.json`，严禁重启正在运行的 herdsman 守护进程。
2. **提交与代码归属**：所有新代码进入 `/root/workspace/pi-extensions/go/`，每组件一个子目录，一个组件一次提交；提交时只 add `go/`，绝不夹带仓内已有未提交改动（如 exclude-tools-guard），绝不写 `herdr-agent-state.ts`。
3. **隔离运行**：PiG 仅在 `/root/.pig-test/agent` 下运行，绝对不读取或修改原版 pi 的会话与设置。
4. **全绿测试准入**：每个组件提交前必须保证 `go build ./...`、`go vet ./...`、`go test ./...` 全部通过，并对照原版 TS 行为编写对等单测，并在 PiG 实际加载跑通。
5. **如实报告**：每批结束输出 `go/reports/batch-N.md`，忠实记录命令原文与行为差异，杜绝未验证推测。
6. **不改 PiG 源码**：若遇 PiG 上游缺口先在报告记录并采取规避策略，不直接修改 PiG 源码。
