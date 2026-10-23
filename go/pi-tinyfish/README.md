# pi-tinyfish (Go Port)

pi-tinyfish 的 Go 语言原生移植版本。基于 PiG Go SDK 实现，彻底移除原版 Node `@tiny-fish/sdk` 依赖，直接通过 Go 标准库 `net/http` 实现与 TinyFish 搜索和网页抓取 API 的无缝通信。

**模块信息**
- Module: `github.com/dorokuma/pi-extensions/go/pi-tinyfish`
- 架构位置: `/root/workspace/pi-extensions/go/pi-tinyfish`
- 导出工厂: `func Extension() *sdk.Extension`
- 工具名称: `tinyfish_search`、`tinyfish_fetch`

**架构决策与去除 SDK 背景**

原版 TypeScript 插件封装了 `@tiny-fish/sdk`（版本 0.0.8），其本质是一个轻量级的 HTTP 包装层，提供了基类客户端、Zod 参数校验、`p-retry` 指数退避和错误类映射。由于 eqi12 的全面 Go 化目标是彻底剔除 Node.js 运行时与所有 npm 包依赖，直接使用 Go 官方标准库 `net/http` 移植客户端是自然且高效的途径。

决策优势：
1. 零第三方依赖：仅依赖 Go 1.26 标准库（`net/http`、`context`、`encoding/json`）与 PiG Go SDK。
2. 零 Node.js 运行时：摆脱 nvm/Node.js/npm 包生命周期，二进制随 PiG 原生分发。
3. 内存与启动开销极低：无需启动 V8 引擎或加载庞大的 `node_modules`。

潜在风险与应对：
- 上游 API 协议变更风险：若 TinyFish 服务端更新请求体字段或鉴权机制，需在 `api.go` 维护。我们在实现中将所有 API 端点、Header 键、默认超时和重试参数提取为清晰的常量与数据结构，后续升级仅需改动对应字段。

**TS 与 Go 逐项对照表**

原版行为基准位于 `/root/.pi/agent/npm/node_modules/pi-tinyfish/index.ts`（621 行）与 SDK 源码。

| 特性 / 机制 | 原版 TypeScript 实现 (行号 / SDK 源码) | Go 原生实现 (`pi-tinyfish`) | 对齐说明 |
|---|---|---|---|
| 基础 Endpoint | `https://agent.tinyfish.ai` (`client.js:7`) | `DefaultBaseURL = "https://agent.tinyfish.ai"` | 已对齐 |
| Search 端点 | `GET /v1/search` (`search/index.js:30`) | `GET /v1/search` (`api.go:Search`) | URL query 编码对齐 |
| Fetch 端点 | `POST /v1/fetch` (`fetch/index.js:31`) | `POST /v1/fetch` (`api.go:Fetch`) | JSON 请求体对齐 |
| API Key 鉴权 | `X-API-Key: process.env["TINYFISH_API_KEY"]` (`client.js:43`) | `req.Header.Set("X-API-Key", c.apiKey)` | 环境变量 `TINYFISH_API_KEY`，缺 key 时拒调并报错 |
| Content-Type | `application/json` (`client.js:44`) | `req.Header.Set("Content-Type", "application/json")` | 已对齐 |
| Accept | `application/json` (`client.js:45`) | `req.Header.Set("Accept", "application/json")` | 已对齐 |
| User-Agent | `tinyfish-typescript/0.0.8` (`client.js:46`) | `DefaultUserAgent = "tinyfish-typescript/0.0.8"` | 已对齐 |
| 请求 Origin | `X-TF-Request-Origin: tinyfish-js` (`client.js:47`) | `req.Header.Set("X-TF-Request-Origin", "tinyfish-js")` | 已对齐 |
| 渠道追踪 | `TF_API_INTEGRATION` 注入请求体 (`client.js:51-60`) | 读取 `TF_API_INTEGRATION` 并注入 POST JSON | 环境变量存在且非空时注入 |
| Search 超时 | 10,000ms (`index.ts:18`)，支持参数 override (1000..120000) | `DefaultSearchTimeout = 10 * time.Second` | 对齐插件覆盖值，非 SDK 默认值 |
| Fetch 超时 | 150,000ms (`index.ts:19`)，支持参数 override (1000..180000) | `DefaultFetchTimeout = 150 * time.Second` | 对齐插件覆盖值，非 SDK 默认值 |
| 超时跨度范围 | 贯穿整个重试序列而非单次请求 (`client.js:107`) | `context.WithTimeout(ctx, c.timeout)` 包裹全重试循环 | 行为一致（已知差异见下表） |
| 重试次数与状态 | 最多 2 次重试，仅 408、429、5xx 重试 (`client.js:124`) | 最多 2 次重试（共 3 次尝试），仅 408、429、>=500 重试 | 400/401/403/404/422 立即失败 |
| 指数退避策略 | 500ms 起始，factor 2.0，最大 8000ms (`client.js:150-155`) | 500ms -> 1000ms，最大 8000ms，factor 2.0 | 退避间隔与算法已对齐 |
| 结果截断通知 | `truncateHead` + 50KB 默认 + notice 文本 (`index.ts:181-202`) | `sdk.TruncateHead` + notice 格式化输出 | 格式与文案已对齐 |
| 搜索响应排版 | Markdown 格式化 (`index.ts:204-232`) | `FormatSearchResponse` | 字段、编号、缩进已对齐 |
| 抓取响应排版 | Markdown/HTML/JSON 格式化 (`index.ts:255-299`) | `FormatFetchResponse` | 语法高亮块与错误列表已对齐 |
| 链接预览限制 | 超过 50 个截断并显示 `... N more` (`index.ts:22`) | `LinkPreviewLimit = 50` 截断与计数提示 | 已对齐 |

**与 TypeScript 的差异与 SDK 缺口说明**

1. 进度通知（`onUpdate`）
   - TS 原版：`onUpdate?.({ content: [{ type: "text", text: ... }], details: { ... } })`。
   - Go 原版：PiG SDK `sdk.Context` 提供了 `ctx.OnUpdate(partial any) error` 方法，本组件完整调用了该接口，在请求发出前发送包含 phase 与 query/urlCount 的进度通知。

2. 终端渲染器（`renderCall` 与 `renderResult`）
   - TS 原版：依赖 `@earendil-works/pi-tui` 的 `Text` 对象和动态 `theme` 参数（如 `theme.fg("toolTitle", ...)`、`theme.bold(...)`）。
   - PiG Go SDK：通过 `ToolRenderCallFunc` 和 `ToolRenderResultFunc` 返回终端行切片 `([]string, error)`。
   - SDK 缺口：PiG Go SDK 的渲染回调参数签名中未暴露动态 theme 主题着色对象，因此 Go 组件在折叠状态（显示摘要计数与截断信息）、展开状态（显示完整多行文本）、出错状态（提取首行报错）以及 partial/phase 状态下返回清晰干净的标准终端文本行，避免硬编码 ANSI 导致不同终端或主题下的色彩冲突。渲染状态逻辑与折叠展开口径与 TS 原版行为一致（已知差异见下表）。

3. 参数钳制与非法格式回退
   - TS 原版：`index.ts:312-313` 在 `searchRequest` 中对 `page` 执行 `normalizeNumber(..., 0, 0, 10)` 钳制至 [0, 10] 范围；`index.ts:327-328` 在 `fetchRequest` 中对非法 `format` 返回 `undefined`，静默回退服务端默认 markdown 格式。
   - Go 原版：`executeSearch` 与 `executeFetch` 采用相同语义，分别对 `page` 做 clamp 钳制、对非法 `format` 静默回退 markdown，与 TS 行为保持一致。

4. 网络错误文案与响应体读取重试
   - TS 原版：SDK 中 `_request` 捕获底层网络异常并抛出 `new APIConnectionError(...)`（保留 `error.message`）；响应体读取（`response.json()`）在 `_request` 外部执行，读取失败不参与重试。
   - Go 原版：Go 在重试耗尽后直接返回底层网络错误 `err`；在读取响应体 `io.ReadAll` 失败时（如连接在传输中途断开）将其纳入指数退避重试，提升流中断时的韧性。

5. 文本长度指标计算口径（`textLength`）
   - TS 原版：`page.text` 统计使用 JS `string.length`（UTF-16 code units 数量）。
   - Go 原版：使用 `len(StringifyFetchText(page.Text))`（UTF-8 字节数）。包含非 ASCII 字符（如中文）时，Go 统计的数值会大于 TS 原版。

6. 请求取消传播（正向差异）
   - TS 原版：插件未将 `_signal` 参数传递给 SDK 客户端。
   - Go 原版：通过 `toContext(ctx)` 监听 `sdk.Context.Done()`，在任务被取消时能够即时取消 HTTP 请求或终止退避重试，具备更优的资源释放与超时控制能力。

**安全铁律与 API Key 保护**

- 本组件严格遵循项目安全约束：绝不在任何日志、代码、文档、错误消息或结构体打印中输出 API Key 明文。
- API Key 仅在运行时通过环境变量 `TINYFISH_API_KEY` 注入（或通过测试构造函数隐式传递假 key）。
- 当未配置 API Key 时，组件直接在参数校验阶段返回安全错误提示，绝不发出任何外部网络请求。

**测试执行与验证**

本项目测试全部基于 `net/http/httptest` 假服务器，绝不向 TinyFish 发出真实外部请求，全程零网络、零真 Key：

```sh
cd /root/workspace/pi-extensions/go/pi-tinyfish
go test -count=1 -v ./...
```

测试覆盖范围：
- 搜索成功响应映射（query、location、language、page、results、total_results）。
- 网页抓取成功响应映射（markdown、html、json 格式、links、image_links、latency_ms、errors 列表）。
- 非重试状态码（400, 401, 403, 404, 422）立即终止并返回解析后的错误信息。
- 可重试状态码（408, 429, 500, 502, 503）连续重试至上限（共 3 次请求）后终止。
- 退避机制校验（注入 sleeper 函数，验证 500ms、1000ms 指数退避，测试全程不产生真实 sleep 阻塞）。
- 整体超时校验（设置微超时，验证总超时拦截生效且不多发请求）。
- 参数边界校验（空 query、urls 为空、urls 超出 10 个限制、page 越界钳制、非法 format 回退 markdown）。
- 结果截断与 notice 文本格式化（验证未截断 vs 截断，验证 50KB 阈值与 notice 格式）。
- `TF_API_INTEGRATION` 环境变量注入验证（存在时注入 JSON，不存在时忽略）。
- 渲染器功能验证（折叠态、展开态、局部更新态、报错态、多字节 UTF-8 字符安全截断）。

**隔离目录加载验证**

根据铁律与 PLAN.md，真实调用 TinyFish API 需要有效的 `TINYFISH_API_KEY` 环境变量。在自动化测试或冒烟验证期间，PiG 运行时隔离加载验证仅测试工具注册发现与基础心跳：

```sh
cd /root/workspace/pi-extensions/go
./bin/pig-test -e ./pi-tinyfish -p "回复 OK"
```

通过 `--mode json` 模式可检查 PiG 的 `toolsAdded` 列表中是否包含 `tinyfish_search` 和 `tinyfish_fetch`，无需发起真实网络调用。
