# hello 扩展示例

本目录为由 `pig extension init` 生成的最小工厂扩展示例（已在第 0 批并行环境中验证通过）。

## 说明

- **SDK 依赖机制**：本扩展的 `go.mod` 中声明了 `require github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0`。在实际运行时，PiG 会在构建期自动将其替换/解析为版本匹配的 staged SDK（位于 `$PIG_HOME/state/pigsdk/go/`）。
- **嵌套 Module 隔离**：本目录是一个独立的嵌套 Go Module（`module example.com/hello`），不会参与外部 `go/` 主 module 的 `go build ./...`、`go vet ./...` 或 `go test ./...`，保证主仓库构建的干净与独立。
- **运行方式**：
  在隔离测试环境中，通过指定 `-e` 参数直接加载本目录（不得裸调 pig，理由见 `go/reports/batch-0.md` 第 10 节）：
  ```bash
  # 在 go/ 目录下执行：
  ./bin/pig-test -e ./examples/hello -p "调用 hello_ping 工具并返回其结果"
  # 或使用 Makefile 目标：
  make examples-run
  ```
  预期将触发 `hello_ping` 工具并返回 `pong from hello`。
