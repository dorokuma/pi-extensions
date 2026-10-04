/**
 * auto-continue — after a transient provider/relay/network failure settles,
 * send a user "continue" so the model picks the task back up.
 *
 * Pi already retries 429 / 5xx / timeouts internally (`settings.retry`).
 * This extension covers the leftovers that still land as a failed turn,
 * especially 中转站 405, Cloudflare 52x, empty HTML bodies, and exhausted
 * built-in retries. It does not retry auth (401/403), quota, overflow,
 * truncation (`stopReason: length`), or user abort.
 *
 * 早衰 stop（v3，静默停止根因修复 A）：
 *   上游中转通道会在任务未完成时提前返回 finish_reason=stop（已观测 12/12 样本），
 *   形态两种：① 只有 thinking、没有任何 text 块；② thinking + 明显中途断句的 text
 *   （以 ：/，/、/； 结尾、未闭合 ``` 代码块、或 markdown 标题/列表项刚起头就没
 *   下文）。`isContinueWorthyError` 只认 `stopReason === "error"`，这种 stop 会被
 *   当成「正常完成」直接放行，任务被静默丢掉。
 *
 *   `isPrematureStop()` 与它并列：`stopReason === "stop"`、本条 assistant 消息无
 *   toolCall、且结构上没把话说完 → 命中后**复用同一条续跑链路**
 *   （agent_settled → delayMs → sendUserMessage("继续")）与 streak/maxAttempts
 *   上限，不另起炉灶。为防误伤正常完成，另加两道闸（见 TOOL_USE_WINDOW_MS /
 *   USER_PROGRESS_STALL_MS）：本 turn 此前 5 分钟内有过 toolUse 活动（任务确实
 *   在进行中），且距上一条真人 user 消息已超过 90 秒（用户等久了还没看到收尾）。
 *   命中/未命中的分支码（`stop:premature-thinking-only` /
 *   `stop:premature-truncated-text` / `premature:*`）随既有 notify 一起打出来。
 *
 *   与 esc 锁的边界：疑似早衰的 stop **不算**「上一轮正常结束」，所以不会按 R4
 *   自动解除 `userCancelled`——esc 之后照样要用户手发消息（或 `/auto-continue on`）
 *   才恢复。压缩 busy（R3）、输入框草稿、非 idle / 有排队消息这几道既有拦截对所有
 *   续跑分支一视同仁，早衰 stop 不绕过任何一个。
 *
 * esc 取消语义（v2，R1–R4 补修）：
 *   用户在流式期间按 esc 中止 assistant turn，本轮自动继续会被**永久**取消。
 *   中止一旦被观察到（assistant 消息 `stopReason === "aborted"`，或裸 ESC 落在
 *   agent run 进行中），立即 cancelPending() 清掉 pending timer、streak 归零、
 *   `lastMessages` 清空，并置上 `userCancelled`。`userCancelled` 置位期间
 *   `agent_settled` 一律不再调度「继续」——包括被中止 turn 一条新 assistant 消息
 *   都没产出、`lastMessages` 里仍留着上一轮旧错误的残留场景。
 *
 *   锁的收敛范围（R4）：从 esc 起，到「下一次正常结束」或「人工输入」为止。
 *   上一轮 `stopReason === "stop"`（正常完成）时 agent_settled 自动解除
 *   `userCancelled`，否则 steer()/followUp()/sendCustomMessage({triggerTurn:true})
 *   这类不经过 input/before_agent_start 的 turn 会把锁永久粘住，RPC 客户端 abort
 *   之后 auto-continue 就永久静默了。
 *
 *   pi 的事件模型限制（务必了解，否则会重复踩坑）：
 *   - pi **没有** interrupt/abort 扩展事件。用户中止只能靠 assistant 消息的
 *     `stopReason === "aborted"` 观察；而「用户 esc 中止」与「程序化 abort」
 *     （`ctx.abort()` / rpc abort）在当前事件模型下**不可区分**——两者都只体现为
 *     同一个 aborted stopReason，所以锁对这两种来源一视同仁。
 *   - 内置 retry（`settings.retry`，默认 maxRetries=3 / baseDelayMs=2000，见
 *     dist/core/settings-manager.js `getRetrySettings`）的 `auto_retry_start` /
 *     `auto_retry_end` 走 `AgentSession._emit()`（UI 事件总线），**不经过**
 *     `_extensionRunner`，扩展收不到。退避窗口里按 esc 只会走
 *     `session.abortRetry()`（只 abort 睡眠，`agent.abort()` 是 no-op），扩展侧同样
 *     收不到任何 aborted 信号。因此 R1 用 `ctx.ui.onTerminalInput` 抓裸 ESC：
 *     `TUI.handleInput()` 先把原始数据喂给 inputListeners，之后才做按键分发。
 *   - 计时中（pending timer 已挂起、pi 处于 idle）时，pi 的 editor.onEscape 对
 *     `isStreaming === false` 没有任何 abort 语义，扩展收不到信号，2500ms 后「继续」
 *     照发。所以 R2 同样靠裸 ESC：仅 cancelPending() 取消本轮，**不**置
 *     `userCancelled`（用户还想让后续错误继续续上）。`onTerminalInput` 在 RPC 模式
 *     是 no-op、老版本可能没有，那种环境下的兜底只有「计时中敲任意字符」——输入框一有
 *     草稿，timer 到期时 `editorHasDraft` 会挡住这次「继续」。
 *   - `isIdle()` 只看 `_isAgentRunActive`，完全不反映 auto-compaction；压缩期
 *     误发「继续」只能靠扩展事件 session_before_compact / session_compact 维护本地
 *     busy 标志来挡（R3）。
 *   - tree 导航（navigateTree）不重置锁；new / fork / switch / reload 都会先派发
 *     `session_shutdown`（见 AgentSessionRuntime.teardownCurrent / reload），由它统一重置。
 *
 *   解除条件有三个：用户真正**手动**发一条消息、执行 `/auto-continue on`、或上一轮
 *   正常结束（`stopReason === "stop"` 且**不是**疑似早衰——见顶部 v3 节）。扩展自己
 *   用 `pi.sendUserMessage` 发的「继续」不算手动消息，不会解除。
 *   「扩展自发」与「用户手发」则靠 `InputEvent.source`（`sendUserMessage` 固定为
 *   "extension"，交互式提交是 "interactive"）加 `BeforeAgentStartEvent.prompt` 文本
 *   共同区分。
 *
 * Install: ~/.pi/agent/extensions/auto-continue.ts  (auto-discovered)
 *
 * Default: ON for every new conversation. Preference is stored in
 * ~/.pi/agent/auto-continue.json so you do not need /auto-continue on.
 *
 * Commands: /auto-continue [on|off|status]
 * Env:
 *   PI_AUTO_CONTINUE=0          force disable this process
 *   PI_AUTO_CONTINUE=1          force enable this process
 *   PI_AUTO_CONTINUE_MAX=20     consecutive continues per error streak
 *   PI_AUTO_CONTINUE_DELAY_MS=2500
 */

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

const LOG = "auto-continue";
const STATUS_KEY = "auto-continue";

const DEFAULT_MAX = 20;
const DEFAULT_DELAY_MS = 2500;

// 只发两个汉字「继续」：不带任何说明、括号或英文。
const CONTINUE_PROMPT = "继续";

/**
 * 裸 ESC（0x1b）。方向键 / Alt+key / 功能键都是以 ESC 开头的多字节序列
 * （"\x1b[A"、"\x1b[3~"、"\x1bOP" …），只有整整一个字节的 ESC 才算
 * 「用户徒手敲了一下 esc」。
 */
const BARE_ESC = "\u001b";

/**
 * R3 看门狗：`session_compact` 只在压缩正常结束时派发；取消 / 摘要失败时 pi 只派发
 * UI 层的 `compaction_end`，扩展看不到。busy 标志卡住超过这个时间就自行解锁，
 * 免得 auto-continue 被一个永不清理的标志永久静默。
 */
const COMPACTION_BUSY_MAX_MS = 300_000;

/**
 * 早衰 stop 闸 A 窗口：本 turn 此前 5 分钟内有过 toolUse 活动，才算「任务进行中」。
 * 一次提问 + 一次回答、全程没有任何工具动作的 turn 即使 stop 得早也不算早衰——
 * 那更可能是一次简短的正常收尾。
 */
const TOOL_USE_WINDOW_MS = 300_000;

/**
 * 早衰 stop 闸 B 阈值：距上一条真人 user 消息超过 90 秒仍无进展，才算「卡死了」。
 * 90 秒内的收尾（包括以冒号 / 列表项收尾的简短回答）一律不碰。
 */
const USER_PROGRESS_STALL_MS = 90_000;

/** HTTP statuses that are usually safe to retry / continue. */
export const CONTINUE_HTTP_STATUSES = new Set([
  405, // 中转站 / nginx / WAF 乱回 Method Not Allowed
  408, // Request Timeout
  409, // 部分网关拿 conflict 表示上游抢锁/瞬时冲突
  421, // Misdirected Request
  425, // Too Early
  429, // 内置 retry 用尽后仍值得再发一轮
  499, // nginx Client Closed Request
  500,
  502,
  503,
  504,
  520, // Cloudflare: unknown
  521, // web server down
  522, // connection timed out
  523, // origin unreachable
  524, // timeout
  525, // SSL handshake
  529, // site overloaded
  530,
]);

/**
 * Errors that look transient even without a clean status code.
 * Keep this an allowlist — 401/403/quota/overflow must not match.
 */
export const CONTINUE_ERROR_PATTERN = new RegExp(
  [
    // 三位状态码只有在带 HTTP/status/code/error 前缀时才算瞬时信号。
    // 裸数字不算："token limit 500 exceeded" 这类纯文本必须落到 unmatched。
    "(?:http(?:[/\\s]?\\d(?:\\.\\d)?)?|status(?:[\\s_]*code)?|error[\\s_]*code|code|error)[\\s:=#_-]*(?:405|408|409|429|499|50[0-4]|52[0-5]|529)\\b",
    "method not allowed",
    "请求方法不允许",
    "too many requests",
    "rate.?limit",
    "overloaded",
    "resourceexhausted",
    "service.?unavailable",
    "bad gateway",
    "gateway time-?out",
    "server.?error",
    "internal.?error",
    "provider.?returned.?error",
    "upstream",
    "无可用渠道",
    "渠道繁忙",
    "上游负载",
    "负载已饱和",
    "线路繁忙",
    "分组负载",
    "try (your request )?again",
    "please retry",
    "you can retry",
    "retry delay",
    "temporarily unavailable",
    "try again later",
    "network.?error",
    "connection.?error",
    "connection.?refused",
    "connection.?lost",
    "connection reset",
    "econnreset",
    "econnrefused",
    "enotfound",
    "eai_again",
    "etimedout",
    "und_err_",
    "other side closed",
    "socket hang up",
    "socket connection was closed",
    "reset before headers",
    "fetch failed",
    "getaddrinfo",
    "timed? ?out",
    "timeout",
    "aborted due to timeout",
    "idle timeout",
    "headers timeout",
    "body timeout",
    "terminated",
    "websocket.?closed",
    "websocket.?error",
    "ended without",
    "stream ended before",
    "http2 request did not get a response",
    "unexpected token\\s*<",
    "unexpected end of json",
    "invalid json",
    "not valid json",
    "malformed json",
    "empty response",
    "no body",
    "cloudflare",
    "cf-ray",
    "just a moment",
    "error code:\\s*5\\d\\d",
  ].join("|"),
  "i",
);

export const SKIP_ERROR_PATTERN = new RegExp(
  [
    "\\b401\\b",
    "\\b403\\b",
    "unauthorized",
    "forbidden",
    "invalid.?api.?key",
    "incorrect api key",
    "authentication",
    "permission denied",
    "insufficient_quota",
    "insufficient.?quota",
    "out of budget",
    "quota exceeded",
    "billing",
    "gousagelimiterror",
    "freeusagelimiterror",
    "monthly usage limit",
    "余额不足",
    "额度不足",
    "令牌已过期",
    "token expired",
    "context.?length",
    "context.?overflow",
    "prompt.?too.?long",
    "maximum context",
    "please reduce",
    "content.?filter",
    "content.?policy",
    "safety",
    "moderation",
    "model.?not.?found",
    "unknown model",
    "invalid.?request",
    "unprocessable",
  ].join("|"),
  "i",
);

type AssistantLike = {
  role?: string;
  stopReason?: string;
  errorMessage?: string;
};

/** assistant 消息 content 块的最小形状（text / thinking / toolCall 只取判定要用的字段）。 */
type ContentBlockLike = {
  type?: string;
  text?: string;
  thinking?: string;
};

/**
 * 早衰 stop 判定用到的最小助手消息形状。只取判定用得上的字段，其余一概不关心——
 * 冒烟测试里的假消息对象可以直接喂进来。
 */
type PrematureStopLike = {
  role?: string;
  stopReason?: string;
  content?: unknown;
  timestamp?: number;
};

/** 一条带时间戳的消息的最小形状（messages 扫描用）。 */
type MessageLike = {
  role?: string;
  content?: unknown;
  timestamp?: number;
};

/** 早衰 stop 判定所需的运行期事实。全部可注入，冒烟测试不依赖真实时钟与事件流。 */
export interface PrematureStopFacts {
  /** 本 run 的消息（agent_end.messages）：toolUse 活动 / user 消息的兜底来源。 */
  messages?: readonly unknown[];
  /** 最近一次 toolUse 活动的时间戳（扩展自己跟踪，优先于 messages 扫描）。 */
  lastToolUseTs?: number;
  /** 最近一条真人 user 消息的时间戳（扩展自己发的「继续」不算）。 */
  lastUserMessageTs?: number;
  /** 注入的当前时间；默认 Date.now()。 */
  now?: number;
}

type Verdict = { ok: true; reason: string } | { ok: false; reason: string };

/** content 归一成块数组：不是数组（旧数据 / 字符串 content）就当空。 */
function asBlocks(content: unknown): ContentBlockLike[] {
  return Array.isArray(content) ? (content as ContentBlockLike[]) : [];
}

/** content 里全部 text 块按序拼接（早衰判定只看文本尾部，不需要块边界）。 */
function blockText(content: unknown): string {
  return asBlocks(content)
    .filter((b): b is { type: "text"; text: string } => b?.type === "text" && typeof b.text === "string")
    .map((b) => b.text)
    .join("");
}

function hasThinkingBlock(content: unknown): boolean {
  return asBlocks(content).some((b) => b?.type === "thinking");
}

function hasToolCallBlock(content: unknown): boolean {
  return asBlocks(content).some((b) => b?.type === "toolCall");
}

/** user 消息 content（string 或 text 块数组）取纯文本。 */
function userMessageText(content: unknown): string {
  if (typeof content === "string") return content;
  return blockText(content);
}

/**
 * text 是否呈「中途断句」形态。只认强信号，宁可漏判也不误伤正常收尾：
 *   ① 行尾就是断句符号（全角 ：，、； 与半角 : , ;）；
 *   ② ``` 代码围栏数为奇数——代码块没关上；
 *   ③ 最后一行是无内容的 markdown 标题 / 列表项（刚起头就断了）；
 *   ④ 省略号收尾（…… 或 ...）。
 * 正常收尾（句号 / 感叹号 / 问号 / 闭括弧 / 表格末行等）一律不命中。
 */
export function looksTruncatedText(text: string): boolean {
  const trimmed = text.replace(/\s+$/u, "");
  if (trimmed === "") return false;
  // ① 断句符号收尾
  if (/[：，、；:;,]$/u.test(trimmed)) return true;
  // ② 未闭合的 ``` 代码块
  if (((trimmed.match(/```/g) ?? []).length) % 2 === 1) return true;
  const lines = trimmed.split("\n");
  const lastLine = lines[lines.length - 1] ?? "";
  // ③ markdown 标题行（含只有 # 的裸标题）/ 无内容的列表项（- * + 或 1. ）
  if (/^ {0,3}#{1,6}(?:\s|$)/u.test(lastLine)) return true;
  if (/^ {0,3}(?:[-*+]|\d+[.)])\s*$/u.test(lastLine)) return true;
  // ④ 省略号收尾
  if (/(?:\.\.\.|…)$/u.test(trimmed)) return true;
  return false;
}

/** messages 里最近一次 toolUse 活动的时间戳（assistant 的 toolCall 块 / toolResult 消息）。 */
function lastToolUseTsFromMessages(messages: readonly unknown[] | undefined): number | undefined {
  if (!Array.isArray(messages)) return undefined;
  let newest: number | undefined;
  for (const raw of messages) {
    const m = raw as MessageLike | undefined;
    if (!m || typeof m.timestamp !== "number") continue;
    if (m.role === "toolResult" || hasToolCallBlock(m.content)) {
      if (newest === undefined || m.timestamp > newest) newest = m.timestamp;
    }
  }
  return newest;
}

/**
 * messages 里最近一条**真人** user 消息的时间戳。扩展自己发的「继续」也落在
 * messages 里（agent_end 的 newMessages 含本轮 prompt），但它不是新输入——跳过它，
 * 否则续跑链的第二发会因为 90s 闸不成立而断掉。
 */
function lastUserTsFromMessages(messages: readonly unknown[] | undefined): number | undefined {
  if (!Array.isArray(messages)) return undefined;
  let newest: number | undefined;
  for (const raw of messages) {
    const m = raw as MessageLike | undefined;
    if (!m || m.role !== "user" || typeof m.timestamp !== "number") continue;
    if (userMessageText(m.content).trim() === CONTINUE_PROMPT) continue;
    if (newest === undefined || m.timestamp > newest) newest = m.timestamp;
  }
  return newest;
}

/** 正数时间戳才算数；0 / undefined / NaN 一律当「没有证据」。 */
function positiveTs(ts: number | undefined): number | undefined {
  return typeof ts === "number" && Number.isFinite(ts) && ts > 0 ? ts : undefined;
}

/**
 * 早衰 stop（静默停止）判定，与 isContinueWorthyError 并列的第二条续跑入口。
 *
 * 结构特征（命中任一即怀疑早衰）：
 *   a) 无任何 text 块——thinking-only，或连 thinking 都没有的空消息；
 *   b) text 结尾呈断句（：/，/、/； 收尾、未闭合 ``` 代码块、markdown 标题/列表项
 *      无内容）。
 * 带 toolCall 的 stop 一概不碰（模型还想干活，交给下一轮即可）。
 *
 * 两道闸（防误伤正常完成，必须同时满足）：
 *   A) 该 turn 此前 5 分钟内有过 toolUse 活动（TOOL_USE_WINDOW_MS）；
 *   B) 距上一条真人 user 消息超过 90 秒仍无进展（USER_PROGRESS_STALL_MS）。
 * 时间戳优先用扩展自己跟踪的 lastToolUseTs / lastUserMessageTs（agent_settled 时
 * lastMessages 已就绪、turn_end 时还是上一轮残留，两种时点都不能只信 messages），
 * 取不到才回落到 messages 扫描；两边都没有证据时按「不续」处理（保守）。
 */
export function isPrematureStop(
  message: PrematureStopLike | undefined,
  facts: PrematureStopFacts = {},
): Verdict {
  if (!message || message.role !== "assistant") {
    return { ok: false, reason: "no-assistant" };
  }
  if (message.stopReason !== "stop") {
    // error / length / toolUse / aborted / deferred 都不归这里管：
    // error 走 isContinueWorthyError，其余本就不续。
    return { ok: false, reason: `stopReason:${message.stopReason ?? "none"}` };
  }
  if (hasToolCallBlock(message.content)) {
    return { ok: false, reason: "stop:has-tool-call" };
  }

  const text = blockText(message.content);
  const shape: Verdict =
    text.trim() === ""
      ? {
          ok: true,
          reason: hasThinkingBlock(message.content)
            ? "stop:premature-thinking-only"
            : "stop:premature-empty-content",
        }
      : looksTruncatedText(text)
        ? { ok: true, reason: "stop:premature-truncated-text" }
        : { ok: false, reason: "stop:complete-text" };
  if (!shape.ok) return shape;

  const now = positiveTs(facts.now) ?? Date.now();

  // 闸 A：本 turn 此前 5 分钟内有过 toolUse 活动。
  const toolUseTs = positiveTs(facts.lastToolUseTs) ?? lastToolUseTsFromMessages(facts.messages);
  if (toolUseTs === undefined || now - toolUseTs > TOOL_USE_WINDOW_MS) {
    return { ok: false, reason: "premature:no-recent-tool-use" };
  }

  // 闸 B：距上一条真人 user 消息 > 90 秒无进展。
  const userTs = positiveTs(facts.lastUserMessageTs) ?? lastUserTsFromMessages(facts.messages);
  if (userTs === undefined || now - userTs <= USER_PROGRESS_STALL_MS) {
    return { ok: false, reason: "premature:user-message-too-recent" };
  }

  return shape;
}

/**
 * 早衰 stop 原因码 → 人话（只用于 UI 提示，不参与判定；判定口径以 isPrematureStop
 * 返回的 reason 为准，日志/双审都认那个码）。
 */
const PREMATURE_HINTS: Record<string, string> = {
  "stop:premature-thinking-only": "疑似早衰 stop：只有 thinking 没有正文",
  "stop:premature-truncated-text": "疑似早衰 stop：正文中途断句",
  "stop:premature-empty-content": "疑似早衰 stop：正文与 thinking 都是空的",
};

/**
 * Extract an HTTP status code only when the text carries an explicit
 * HTTP / status / code / error prefix. A bare 3-digit number (token counts,
 * port numbers, byte sizes, "token limit 500 exceeded") is NOT a status code.
 */
export function extractHttpStatus(text: string): number | undefined {
  const m = text.match(
    /\b(?:http(?:[/\s]?\d(?:\.\d)?)?|status(?:[\s_]*code)?|error[\s_]*code|code|error)[\s:=#_-]*([1-5]\d\d)\b/i,
  );
  if (!m) return undefined;
  const n = Number(m[1]);
  return Number.isFinite(n) ? n : undefined;
}

export function isContinueWorthyError(
  message: AssistantLike | undefined,
  httpStatus?: number,
): { ok: true; reason: string } | { ok: false; reason: string } {
  if (!message || message.role !== "assistant") {
    return { ok: false, reason: "no-assistant" };
  }
  if (message.stopReason === "aborted") {
    return { ok: false, reason: "aborted" };
  }
  if (message.stopReason !== "error") {
    // `length` 也走这里：截断不是网络抖动，不自动续（也不发抖动 prompt）。
    return { ok: false, reason: `stop:${message.stopReason ?? "none"}` };
  }

  const err = String(message.errorMessage ?? "").trim();
  const status = httpStatus ?? extractHttpStatus(err);

  // 401/403 是认证/权限问题，续也没用，解析出状态码后立刻放弃。
  if (status === 401 || status === 403) {
    return { ok: false, reason: "non-retryable" };
  }

  // SKIP 命中即终局：不再用 CONTINUE_HTTP_STATUSES 给任何状态码开洞。
  if (err && SKIP_ERROR_PATTERN.test(err)) {
    return { ok: false, reason: "non-retryable" };
  }

  if (status !== undefined && CONTINUE_HTTP_STATUSES.has(status)) {
    return { ok: true, reason: `http-${status}` };
  }
  if (err && CONTINUE_ERROR_PATTERN.test(err)) {
    return { ok: true, reason: "pattern" };
  }
  return { ok: false, reason: "unmatched" };
}

type PersistShape = {
  enabled: boolean;
  maxAttempts: number;
  delayMs: number;
};

function settingsPath(): string {
  return join(homedir(), ".pi", "agent", "auto-continue.json");
}

function clampInt(n: number, min: number, max: number, fallback: number): number {
  if (!Number.isFinite(n)) return fallback;
  return Math.min(max, Math.max(min, Math.floor(n)));
}

function defaultPersist(): PersistShape {
  return { enabled: true, maxAttempts: DEFAULT_MAX, delayMs: DEFAULT_DELAY_MS };
}

function loadPersist(): PersistShape {
  const fallback = defaultPersist();
  try {
    const raw = readFileSync(settingsPath(), "utf8");
    const parsed = JSON.parse(raw) as Partial<PersistShape>;
    return {
      enabled: parsed.enabled !== false,
      maxAttempts: clampInt(Number(parsed.maxAttempts), 1, 20, fallback.maxAttempts),
      delayMs: clampInt(Number(parsed.delayMs), 0, 60_000, fallback.delayMs),
    };
  } catch {
    savePersist(fallback);
    return fallback;
  }
}

function savePersist(state: PersistShape): void {
  const path = settingsPath();
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, `${JSON.stringify(state, null, 2)}\n`, "utf8");
}

function envEnabledOverride(): boolean | undefined {
  const v = process.env.PI_AUTO_CONTINUE?.trim().toLowerCase();
  if (!v) return undefined;
  if (v === "0" || v === "false" || v === "off" || v === "no") return false;
  if (v === "1" || v === "true" || v === "on" || v === "yes") return true;
  return undefined;
}

function envInt(name: string, fallback: number, min: number, max: number): number {
  const raw = process.env[name];
  if (raw === undefined || raw.trim() === "") return fallback;
  const n = Number(raw);
  if (!Number.isFinite(n)) return fallback;
  return Math.min(max, Math.max(min, Math.floor(n)));
}

function lastAssistant(messages: AssistantLike[] | undefined): AssistantLike | undefined {
  if (!messages) return undefined;
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i]?.role === "assistant") return messages[i];
  }
  return undefined;
}

function editorHasDraft(ctx: ExtensionContext): boolean {
  try {
    const text = ctx.ui.getEditorText?.();
    return typeof text === "string" && text.trim().length > 0;
  } catch {
    return false;
  }
}

export default function (pi: ExtensionAPI) {
  const persisted = loadPersist();
  const envOverride = envEnabledOverride();
  let enabled = envOverride ?? persisted.enabled;
  let maxAttempts = envInt("PI_AUTO_CONTINUE_MAX", persisted.maxAttempts, 1, 20);
  let delayMs = envInt("PI_AUTO_CONTINUE_DELAY_MS", persisted.delayMs, 0, 60_000);

  const persistNow = () => {
    savePersist({ enabled, maxAttempts, delayMs });
  };

  let streak = 0;
  let lastHttpStatus: number | undefined;
  let lastMessages: AssistantLike[] = [];
  let timer: ReturnType<typeof setTimeout> | undefined;
  let generation = 0;
  /**
   * R3：压缩期标志。`ctx.isIdle()` 只看 `_isAgentRunActive`，压缩中被判成 idle，
   * 所以只能用扩展事件 session_before_compact / session_compact 自己维护。
   */
  let compacting = false;
  let compactWatchdog: ReturnType<typeof setTimeout> | undefined;
  /** onTerminalInput 的退订函数（每次 session_start 重新注册）。 */
  let offTerminalInput: (() => void) | undefined;
  /** 供裸 ESC 回调使用的 ctx（session_start 时刷新）。 */
  let terminalCtx: ExtensionContext | undefined;
  /**
   * 用户按 esc 中止后置位。置位期间本轮自动继续被永久取消，直到用户真正手动
   * 发一条消息（或 `/auto-continue on`）才解除。扩展自己发的「继续」不清除它。
   */
  let userCancelled = false;
  /**
   * 早衰 stop 闸 A 的活动信号：最近一次 toolUse 时间（tool_execution_start / _end）。
   * **不**随 agent_start 归零——续跑链里下一次「继续」自己的 run 开头没有任何工具
   * 动作，归零会让闸 A 永久不成立；5 分钟窗口本身就把有效期限死了。
   */
  let lastToolUseTs = 0;
  /**
   * 早衰 stop 闸 B：最近一条**真人** user 消息时间。扩展自发的「继续」不算
   * （source === "extension"），否则续跑链的第二发永远过不了 90s 闸。
   */
  let lastHumanUserTs = 0;
  /**
   * 下一次 `before_agent_start` / `agent_start` 对应的是本扩展自己
   * `sendUserMessage(CONTINUE_PROMPT)` 发出的那次「继续」。发送前置位，在
   * `before_agent_start`（或 `agent_start`）里按该标志区分自发性 turn 与用户手发 turn。
   */
  let selfSending = false;
  /** 最近一次 `input` 事件来自扩展自身（`sendUserMessage` 走 source: "extension"）。 */
  let promptFromExtension = false;

  const cancelPending = () => {
    generation += 1;
    if (timer) {
      clearTimeout(timer);
      timer = undefined;
    }
  };

  const clearStatus = (ctx?: ExtensionContext) => {
    try {
      ctx?.ui.setStatus(STATUS_KEY, undefined);
    } catch {
      /* ignore */
    }
  };

  /**
   * 早衰 stop 判定（扩展内统一入口）：结构特征 + 两道闸。
   *
   * `messages` 只在 `lastMessages` 已就绪的时点（agent_settled）传入；turn_end 触发时
   * `lastMessages` 还是上一轮的残留（agent_end 尚未派发），那种时点只信扩展自己跟踪的
   * `lastToolUseTs` / `lastHumanUserTs`，不传 messages。
   */
  const judgePrematureStop = (message: AssistantLike | undefined, messages?: readonly AssistantLike[]) =>
    isPrematureStop(message, {
      messages,
      lastToolUseTs,
      lastUserMessageTs: lastHumanUserTs,
    });

  /**
   * 用户中止（esc）被观察到：cancelPending() 清掉 pending timer、streak 归零、
   * 置 userCancelled 标志。同时丢掉 lastMessages —— 被中止的 turn 可能一条新
   * assistant 消息都没产出，留着上一轮的旧错误会让后续 agent_settled 把它再判成
   * continue-worthy，从而在 delayMs 后又发一次「继续」。
   * 观察来源有两个：assistant 消息 `stopReason === "aborted"`，以及裸 ESC 落在
   * agent run 进行中（内置 retry 退避窗口那种收不到 aborted 消息的情况）。
   */
  const markUserAbort = (ctx?: ExtensionContext) => {
    cancelPending();
    streak = 0;
    lastHttpStatus = undefined;
    lastMessages = [];
    const first = !userCancelled;
    userCancelled = true;
    clearStatus(ctx);
    if (!first) return;
    try {
      ctx?.ui.notify(
        `[${LOG}] 已按 esc 中止：本轮不再自动继续，手动发消息或下一轮正常结束后恢复`,
        "info",
      );
    } catch {
      /* ignore */
    }
  };

  // ===========================================================================
  // R3：压缩期 busy 标志
  // ===========================================================================

  const clearCompactWatchdog = () => {
    if (compactWatchdog) {
      clearTimeout(compactWatchdog);
      compactWatchdog = undefined;
    }
  };

  /**
   * 进入压缩期：置 busy、清掉 pending timer、挂看门狗。
   * 压缩期间 `ctx.isIdle()` 仍返回 true（只看 `_isAgentRunActive`），
   * 所以只能靠这个本地标志挡住误发的「继续」。
   */
  const markCompactionStart = (ctx?: ExtensionContext) => {
    if (compacting) return;
    compacting = true;
    cancelPending();
    clearCompactWatchdog();
    compactWatchdog = setTimeout(() => {
      compactWatchdog = undefined;
      if (!compacting) return;
      compacting = false;
      try {
        ctx?.ui.notify(
          `[${LOG}] 压缩 busy 标志超过 ${COMPACTION_BUSY_MAX_MS}ms 未清理，已自行解锁`,
          "warning",
        );
      } catch {
        /* ignore */
      }
    }, COMPACTION_BUSY_MAX_MS);
  };

  /** 压缩结束（成功 / 取消 / 失败都算结束）。 */
  const markCompactionEnd = () => {
    compacting = false;
    clearCompactWatchdog();
  };

  // ===========================================================================
  // R1 + R2：TUI 原始输入里的裸 ESC
  // ===========================================================================

  /**
   * 抓到裸 ESC 时的处理。**不 consume**：pi 自己的 ESC 语义（abort retry /
   * abort compaction / abort run）必须照常生效，我们只清理自己的 pending 状态。
   *
   * - R2：本轮已有 pending timer（pi 处于 idle、editor.onEscape 无 abort 语义）
   *   → 只 cancelPending() 取消本轮，**不**置 userCancelled。
   * - R1：agent run 还在进行中（含内置 retry 退避窗口、工具执行）→ 用户在退避
   *   窗口按的 esc 走 `session.abortRetry()`，`agent.abort()` 是 no-op，扩展收不到
   *   任何 aborted 信号，只能在这里补上 markUserAbort。
   * - 其余（纯 idle、无计时）：不动作。那时的 ESC 可能只是清输入框 / 关补全。
   */
  const handleTerminalInput = (
    data: string,
  ): { consume?: boolean; data?: string } | undefined => {
    // 纵深防御：万一某些模式塞进来的不是字符串
    if (typeof data !== "string" || data !== BARE_ESC) return undefined;
    const ctx = terminalCtx;
    if (!ctx) return;

    if (timer) {
      cancelPending();
      clearStatus(ctx);
      try {
        ctx.ui.notify(`[${LOG}] 计时中按了 esc：已取消本轮自动继续`, "info");
      } catch {
        /* ignore */
      }
      return undefined;
    }

    let idle = true;
    try {
      idle = ctx.isIdle();
    } catch {
      idle = true;
    }
    if (idle) return undefined;
    // run 进行中（流式 / retry 退避 / 工具执行）按 esc = 用户要停
    markUserAbort(ctx);
    return undefined;
  };

  /**
   * 注册裸 ESC 监听。`ExtensionUIContext.onTerminalInput` 只有 TUI 模式有实现
   * （RPC 模式返回 no-op 退订函数），非 TUI 或旧版本安静降级。
   * session 替换（new / fork / switch / reload）会 resetExtensionUI 清掉监听，
   * 所以每次 session_start 都重新注册一次。
   */
  const registerTerminalInput = (ctx: ExtensionContext) => {
    terminalCtx = ctx;
    if (offTerminalInput) {
      try {
        offTerminalInput();
      } catch {
        /* ignore */
      }
      offTerminalInput = undefined;
    }
    try {
      const off = ctx.ui.onTerminalInput?.(handleTerminalInput);
      if (typeof off === "function") offTerminalInput = off;
    } catch {
      /* ignore：没有原始输入能力时不影响主流程 */
    }
  };

  const unregisterTerminalInput = () => {
    terminalCtx = undefined;
    if (offTerminalInput) {
      try {
        offTerminalInput();
      } catch {
        /* ignore */
      }
      offTerminalInput = undefined;
    }
  };

  pi.on("session_start", (_event, ctx) => {
    cancelPending();
    streak = 0;
    lastHttpStatus = undefined;
    lastMessages = [];
    userCancelled = false;
    selfSending = false;
    promptFromExtension = false;
    // 早衰 stop 两道闸的时间戳是「本会话」口径，换会话（new / fork / switch /
    // reload）一律清零，不把上一个会话的工具活动/用户输入带到下一个会话。
    lastToolUseTs = 0;
    lastHumanUserTs = 0;
    markCompactionEnd();
    registerTerminalInput(ctx);
    clearStatus(ctx);
  });

  pi.on("session_shutdown", (_event, ctx) => {
    cancelPending();
    userCancelled = false;
    selfSending = false;
    promptFromExtension = false;
    markCompactionEnd();
    unregisterTerminalInput();
    clearStatus(ctx);
  });

  // R3：压缩期不调度。session_compact 只在正常结束时派发，取消/失败由 agent_start
  // 与看门狗兜底解锁。
  pi.on("session_before_compact", (_event, ctx) => {
    markCompactionStart(ctx);
  });

  pi.on("session_compact", () => {
    markCompactionEnd();
  });

  pi.on("agent_start", () => {
    cancelPending();
    lastHttpStatus = undefined;
    // 只看「本次 run」产出的 assistant 消息。被 esc 中止的 run 可能一条新
    // assistant 消息都没产出，不清的话 lastMessages 会一直留着上一轮那条旧错误，
    // 下一次 agent_settled 会把它再判成 continue-worthy 又发一次「继续」。
    lastMessages = [];
    // 新的 agent run 起来了，压缩期必然已经结束（兜底解锁）。
    markCompactionEnd();
    // selfSending 只消费一次；before_agent_start 已消费时这里自然为 false。
    selfSending = false;
  });

  pi.on("after_provider_response", (event) => {
    if (typeof event.status === "number" && event.status > 0) {
      lastHttpStatus = event.status;
    }
  });

  // 早衰 stop 闸 A 的活动信号：工具一开跑/一结束就记时间。codemode 嵌套调用也会走
  // 这两个事件，同样算活动。不清于 agent_start（见 lastToolUseTs 声明处）。
  pi.on("tool_execution_start", () => {
    lastToolUseTs = Date.now();
  });

  pi.on("tool_execution_end", () => {
    lastToolUseTs = Date.now();
  });

  pi.on("agent_end", (event) => {
    lastMessages = Array.isArray(event.messages) ? event.messages : [];
  });

  /**
   * 区分「扩展自己 sendUserMessage 发的『继续』」与「用户手动发的消息」。
   * `InputEvent.source` 由 AgentSession.prompt() 决定：交互式/rpc 提交走默认的
   * "interactive"/"rpc"，而 `pi.sendUserMessage()` 内部固定 `source: "extension"`
   * （见 dist/core/agent-session.js sendUserMessage → prompt({source:"extension"})）。
   */
  pi.on("input", (event) => {
    if (event.source === "extension") {
      promptFromExtension = true;
      if (event.text.trim() === CONTINUE_PROMPT) selfSending = true;
      // 扩展自己发的 prompt 不算「用户手动发消息」，不清除 esc 取消标志，也不刷新
      // lastHumanUserTs——否则续跑链的第二发会被 90s 闸判成「用户刚发过消息」。
      return;
    }
    promptFromExtension = false;
    selfSending = false;
    // interactive / rpc：用户真正手动发消息 → 解除 esc 取消锁定，并作为闸 B 的
    // 「上一条真人 user 消息」起点。
    lastHumanUserTs = Date.now();
    userCancelled = false;
  });

  /**
   * `before_agent_start` 带原始 prompt 文本，配合 selfSending 再兜一层底：
   * 本扩展自发「继续」时不解除 userCancelled，用户手发消息时解除。
   */
  pi.on("before_agent_start", (event) => {
    const selfContinue =
      selfSending || promptFromExtension || event.prompt?.trim() === CONTINUE_PROMPT;
    selfSending = false;
    if (selfContinue) return;
    userCancelled = false;
  });

  pi.on("turn_end", (event, ctx) => {
    const msg = event.message as AssistantLike | undefined;
    if (msg?.role !== "assistant" || !msg.stopReason) return;
    if (msg.stopReason === "aborted") {
      // 用户按 esc 中止了这一轮。pi 没有专用 interrupt 事件，stopReason
      // 是唯一能观察到用户中止的信号（agent-core handleRunFailure / provider
      // stream result 都会把 aborted 写进 stopReason）。
      markUserAbort(ctx);
      return;
    }
    if (msg.stopReason !== "error") {
      // 疑似早衰的 stop 先不归零：这一轮会被 agent_settled 接进续跑链，
      // streak 要跨轮累加，否则 maxAttempts 上限永远不生效（每轮 turn_end
      // 都把它清掉，第二发又从 1/20 开始）。turn_end 时 lastMessages 还是
      // 上一轮残留，所以这里只信扩展自己跟踪的两个时间戳，不传 messages。
      if (!judgePrematureStop(msg).ok) streak = 0;
    }
  });

  pi.on("agent_settled", async (_event, ctx) => {
    if (!enabled) return;

    // R3：压缩期（isIdle() 看不出来）不调度，timer 到期回调同样要挡。
    if (compacting) {
      clearStatus(ctx);
      return;
    }

    const assistant = lastAssistant(lastMessages);

    // R4：上一轮正常结束（stopReason === "stop"）→ 自动解除 esc 锁。把锁收敛成
    // 「从 esc 到下一次正常结束或人工输入」：steer()/followUp()/sendCustomMessage
    // ({triggerTurn:true}) 这类 turn 不经过 input/before_agent_start，不解锁的话
    // rpc abort 之后 auto-continue 会永久静默。
    //
    // v3：疑似早衰的 stop 不算「正常结束」——中转通道提前吐 finish_reason=stop 时
    // 任务并没完成，若在这里解锁，esc 之后早衰分支又会把「继续」发出去，与 esc
    // 取消语义冲突。所以闸门只对「非早衰的 stop」打开。
    if (userCancelled && assistant?.stopReason === "stop") {
      if (!judgePrematureStop(assistant, lastMessages).ok) {
        userCancelled = false;
        try {
          ctx.ui.notify(`[${LOG}] 上一轮已正常结束，自动继续已恢复`, "info");
        } catch {
          /* ignore */
        }
      }
    }

    // esc 已把本轮自动继续锁掉：不管 lastMessages 里残留的是不是旧错误，都不再
    // 调度，直到用户手动发消息、/auto-continue on、或上一轮正常结束。
    if (userCancelled) {
      clearStatus(ctx);
      return;
    }

    if (assistant?.stopReason === "aborted") {
      // 本轮 turn 被用户中止：补上永久取消标记，避免 2500ms 后旧错误又触发一轮。
      markUserAbort(ctx);
      return;
    }

    let verdict = isContinueWorthyError(assistant, lastHttpStatus);
    if (!verdict.ok) {
      // 瞬时故障没命中时，再看是不是「早衰 stop」：中转通道在任务未完成时提前返回
      // finish_reason=stop，结构上没说完 + 两道闸都过，才复用下面这条续跑链路
      // （含 streak/maxAttempts 上限、idle/草稿/压缩/esc 锁检查）。
      const premature = judgePrematureStop(assistant, lastMessages);
      if (premature.ok) verdict = premature;
    }
    if (!verdict.ok) {
      if (assistant?.stopReason && assistant.stopReason !== "error") streak = 0;
      return;
    }

    if (streak >= maxAttempts) {
      ctx.ui.notify(
        `[${LOG}] 连续 ${streak} 次自动继续已达上限（瞬时错误 / 早衰 stop 都计数），停手。可 /auto-continue 看状态，或自己发「继续」。`,
        "warning",
      );
      clearStatus(ctx);
      return;
    }

    if (!ctx.isIdle() || ctx.hasPendingMessages()) return;
    if (editorHasDraft(ctx)) {
      ctx.ui.notify(`[${LOG}] 检测到输入框有草稿，不自动继续`, "info");
      return;
    }

    const token = ++generation;
    const attempt = streak + 1;
    const wait = delayMs;
    const reason = verdict.reason;

    try {
      ctx.ui.setStatus(STATUS_KEY, `${LOG} ${attempt}/${maxAttempts}`);
    } catch {
      /* ignore */
    }
    // 早衰 stop 的原因码必须留在日志里（双审/排错都认这个码），人话只作补充。
    const hint = PREMATURE_HINTS[reason];
    ctx.ui.notify(
      `[${LOG}] ${reason}${hint ? `（${hint}）` : ""}，${wait}ms 后自动继续（${attempt}/${maxAttempts}）`,
      "info",
    );

    timer = setTimeout(() => {
      timer = undefined;
      if (token !== generation) return;
      if (!enabled) return;
      // 与 agent_settled 同一套判定：锁未解 / 压缩中 / 非 idle / 有排队消息 /
      // 输入框有草稿，任何一种都不发。
      if (userCancelled || compacting) {
        clearStatus(ctx);
        return;
      }
      if (!ctx.isIdle() || ctx.hasPendingMessages() || editorHasDraft(ctx)) {
        clearStatus(ctx);
        return;
      }

      streak = attempt;
      // 标记「下一次 agent_start / before_agent_start 是本扩展自己发的继续」，
      // 免得它把 userCancelled 当成用户手动发消息给解除掉。
      selfSending = true;
      // ExtensionAPI.sendUserMessage 声明为返回 void（SendUserMessageHandler 同），
      // 实际实现是 fire-and-forget：内部 `void session.prompt().catch(→ runner.emitError)`，
      // 失败只会以扩展错误事件的形式冒出来，同步 try/catch 是死代码，这里直接调用。
      pi.sendUserMessage(CONTINUE_PROMPT);
      // 注意：这里不能立刻把 selfSending 置回 false —— input / before_agent_start
      // 事件在 sendUserMessage 返回之后的微任务里才派发，标志要留到那里被消费。
      clearStatus(ctx);
    }, wait);
  });

  pi.registerCommand("auto-continue", {
    description: "瞬时错误 / 早衰 stop 后自动发「继续」。用法: /auto-continue [on|off|status]",
    handler: async (args, ctx) => {
      const cmd = args.trim().toLowerCase();
      if (cmd === "off" || cmd === "disable" || cmd === "0") {
        enabled = false;
        cancelPending();
        clearStatus(ctx);
        persistNow();
        ctx.ui.notify(`[${LOG}] 已关闭（下次新对话也会保持关闭）`, "info");
        return;
      }
      if (cmd === "on" || cmd === "enable" || cmd === "1") {
        enabled = true;
        streak = 0;
        // /auto-continue on 是手动解除 esc 取消锁定的入口。
        userCancelled = false;
        selfSending = false;
        persistNow();
        ctx.ui.notify(`[${LOG}] 已开启，最多连续 ${maxAttempts} 次（新对话默认开）`, "info");
        return;
      }
      if (cmd.startsWith("max ")) {
        const n = Number(cmd.slice(4).trim());
        if (!Number.isFinite(n) || n < 1 || n > 20) {
          ctx.ui.notify(`[${LOG}] max 需要 1–20`, "warning");
          return;
        }
        maxAttempts = Math.floor(n);
        persistNow();
        ctx.ui.notify(`[${LOG}] max=${maxAttempts}`, "info");
        return;
      }
      ctx.ui.notify(
        `[${LOG}] ${enabled ? "on" : "off"}  streak=${streak}/${maxAttempts}  delay=${delayMs}ms` +
          `${compacting ? "  compacting（压缩中，不调度）" : ""}` +
          `${userCancelled ? "  esc-cancelled（手动发消息 / 正常结束 / on 后恢复）" : ""}`,
        "info",
      );
    },
  });
}
