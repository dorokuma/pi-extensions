# magpie-image

## 概况
把本机 **magpie 网关**注册成 pi 的**出图（image）provider**，让 codemode 的
`models.generateImages()` 可用，且**不需要任何 OpenRouter 凭据**。

| 项 | 值 |
|---|---|
| provider 名（id） | `magpie-image` |
| 模型 id | `workbuddy-ai/gpt-image-2.5-sunburst` |
| 端点 | `http://127.0.0.1:3425/v1/images/generations`（OpenAI 兼容） |
| api 判别键 | `magpie-images`（`images` 实现表的 key） |
| 鉴权 | `Authorization: Bearer magpie`（magpie 本地网关接受任意 bearer，此值非密钥） |
| cost | 全 0（本地网关，不自计费） |

**为什么用独立 provider 名 `magpie-image` 而不直接占用 `magpie`**：pi 的 provider
注册契约是「提供 `models` 列表即**整体替换**该 provider id 下的全部 chat/image/classifier
模型」。`magpie` 这个 id 已经被 `models.json` 里的 chat 模型占用（`group/glm-5.3`、
`group/kimi-k3`、`group/deepseek-flash` 等）。若注册时复用 `magpie` 作 id，会把这些 chat
模型一次性抹掉；用独立 id 注册则 `magpie` 原封不动。

**为什么必须是扩展而不能写进 `models.json`**：pi 的 `models.json` 无法声明 image 模型，
image 模型连同它的 `generateImages` 实现只能由 provider 注册提供。

行为要点：
- `generateImages` **永不抛异常**：失败以 `stopReason: "error"`（或 signal 已中止时的
  `"aborted"`）的 `AssistantImages` 返回，`errorMessage` 带原因（含 HTTP 状态与前 300 字响应体）。
- prompt 取 context 里全部 text block 拼接；参考图不经此端点发送。
- 环境变量覆盖：`MAGPIE_IMAGE_MODEL`（默认 `workbuddy-ai/gpt-image-2.5-sunburst`）、
  `MAGPIE_IMAGE_BASE_URL`（默认 `http://127.0.0.1:3425/v1`，末尾斜杠归一）。
- 默认值取自 magpie 自身 `settings.json` 的 `imageGen` 与其本地端点
  （本机实测 `/root/.config/magpie/settings.json` 的 `imageGen` 即上表模型 id）。
- 网关请求/响应形态（源码头注已核）：请求 `{model, prompt}`；
  响应 `{created, data:[{b64_json, ...}], model, usage}`，本扩展取 `data[0].b64_json`
  并标注 `mimeType: "image/png"`。

## 权威源 / 目标 / 来源
- 权威源（本仓，唯一）：`extensions/magpie-image/src/magpie-image.ts`
  （绝对路径 `/root/workspace/pi-extensions/extensions/magpie-image/src/magpie-image.ts`）
- 目标安装路径：`/root/.pi/agent/extensions/magpie-image.ts`
- 来源：自本机部署副本逐字纳管（`/root/.pi/agent/extensions/magpie-image.ts`），内容一字未改。

## 纳管时的一致性记录
- md5：`7c76dc2112e63c9da430e85e85fbebac`
- 字节数：`6520`（191 行）
- 与部署副本 `cmp` 逐字节一致（2026-10-09 纳管时校验）；
  `bash install.sh --audit` 该项 `MATCH — unchanged`。

## 出处与许可
- **作者声明为自研，未经第三方独立核实。** 本仓不做独立溯源，也不代其声明任何许可证
  归属。
- 与 `/root/.config/magpie/`（网关配置）的关系：本扩展只**读**其端点与 `imageGen` 默认值
  的既有口径，不写、不改 magpie 的任何配置。

## 改动流程（铁律）
1. 只在本仓 `extensions/magpie-image/src/magpie-image.ts` 改；**严禁**直接编辑部署副本。
2. `pnpm check` 通过后 `bash install.sh magpie-image` 同步，`/reload` 或新会话生效。
