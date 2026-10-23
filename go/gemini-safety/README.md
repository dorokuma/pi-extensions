# gemini-safety (Go 移植版)

本扩展是原 TS 版 `gemini-safety.ts` 向 PiG Go 扩展体系的行为等价移植实现 [^1]。

## 背景与设计初衷

在 Google Gemini API 中：
- Google 的 OpenAI 兼容端点如果收到任何名为 `safetySettings` 的字段，会直接抛出 HTTP 400 错误。
- 而 Google 原生 `v1beta/...:streamGenerateContent` API 则在 `config.safetySettings` 下接收内容安全过滤配置。
- 原版 TS 扩展通过拦截 `before_provider_request` 钩子，仅针对 native Gemini payload（特征：`model` 包含 `gemini` 且请求体含有 `contents` 数组）将 4 项分类的 `safetySettings` 全部注入为 `BLOCK_NONE`。
- 对于非 Gemini 模型（如 Claude、GPT-4o 等）或非 native Gemini 格式（如 OpenAI-shaped completions，使用 `messages`），扩展保持严格无操作（no-op）。

## 与原 TS 版的行为对照

| 行为点 | 原 TS 版 (`gemini-safety.ts`) | Go 版 (`gemini-safety/extension.go`) |
|---|---|---|
| **扩展注册** | `export default function geminiSafety(pi: ExtensionAPI)` | `func Extension() *sdk.Extension`，通过 PiG factory 机制暴露 |
| **事件注册** | `pi.on("before_provider_request", ...)` | `e.OnEvent(sdk.EventBeforeProviderRequest, ...)` |
| **入参数据结构** | `event.payload` | `data["payload"]`（通过 PiG 子进程 wire 协议传递） |
| **对象判断** | `!payload \|\| typeof payload !== "object"` | `payload, ok := raw.(map[string]any); if !ok \|\| payload == nil` |
| **原生 contents 判定** | `!Array.isArray(payload.contents)` | `contents, ok := payload["contents"]`<br>`switch contents.(type) { case []any: ... }`<br>（L58-L67，通过 type switch 判定数组） |
| **model 判定** | `typeof payload.model !== "string" \|\| !payload.model.includes("gemini")` | `model, ok := payload["model"].(string); if !ok \|\| !strings.Contains(model, "gemini")` |
| **配置合并** | `config: { ...config, safetySettings: ... }` | 浅拷贝原有 `config` 键值并覆盖设置深拷贝的 4 条 `safetySettings` |
| **4 大安全分类** | `HARM_CATEGORY_HARASSMENT`<br>`HARM_CATEGORY_HATE_SPEECH`<br>`HARM_CATEGORY_SEXUALLY_EXPLICIT`<br>`HARM_CATEGORY_DANGEROUS_CONTENT`<br>均为 `BLOCK_NONE` | 与 TS 严格一致，4 类全部设为 `BLOCK_NONE` |
| **原始入参修改** | 保持不可变，返回新解构对象 | 保持不可变，浅拷贝构造新 payload map 返回，原始 map 完全只读 |
| **返回值与宿主采用** | 返回新 payload 对象；若不改写返回 `undefined` | 返回 `(newPayload, nil)`；若不改写返回 `(nil, nil)`。宿主（PiG）收到非 nil 返回值时自动替换当前请求 payload |

## Module 形态

- Module 路径：`github.com/dorokuma/pi-extensions/go/gemini-safety`
- Go 版本：`go 1.26`
- 依赖声明：
  ```go
  require github.com/MichaelKinsy/PiG/extensions/sdk v0.0.0
  ```
- 本地解析：通过上层 `go/go.work` 映射至本地 `go/pigsdk`；PiG 构建时则由其内部 `builder.go` 通过 `GOWORK=off` 与临时 `-modfile` 自动注入其暂存的 SDK。

## 如何运行本地测试

在 `/root/workspace/pi-extensions/go/gemini-safety` 目录下：

```bash
# 静态分析
go vet ./...

# 编译检查
go build ./...

# 运行对照单元测试
go test -v -count=1 ./...
```

测试全面覆盖：
1. `payload` 非对象、基本类型或 `nil` 时不改写；
2. 无 `contents` 或 `contents` 非数组时不改写（含 OpenAI 格式 `messages`）；
3. `model` 非字符串或不含 `gemini` 时不改写；
4. Gemini 原生 payload 成功注入 4 条 `BLOCK_NONE`，保留其它所有 `config` 键与顶层字段；
5. 原始入参 map 保持完全只读不可变；
6. 已有 `safetySettings` 时以本扩展配置覆盖（对齐 TS 语义）。

## 如何在隔离 PiG 环境中加载

使用根目录的测试启动器 `./bin/pig-test`（确保严格运行在 `/root/.pig-test` 隔离环境下）：

```bash
cd /root/workspace/pi-extensions/go
./bin/pig-test -e ./gemini-safety -p "回复 OK"
```

[^1]: 行为等价与理论差异说明：在原 TS 版中，若 `config` 传入数组，解构扩展语法 `{ ...config, safetySettings: ... }` 会将其展开为数字键对象（如 `{"0": ..., safetySettings: ...}`）；而 Go 版若遇到 `config` 非 `map[string]any` 对象，则会丢弃并重建为空 map 重新注入 `safetySettings`。但在实际协议（wire protocol）中，`config` 恒为 JSON 对象，因此传入数组的分支在实际运行时不可达（为不可达差异），两者在业务逻辑上严格行为等价。

