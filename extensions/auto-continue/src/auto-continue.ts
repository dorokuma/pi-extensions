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
 *   正常结束（`stopReason === "stop"`）。扩展自己用 `pi.sendUserMessage` 发的「继续」
 *   不算手动消息，不会解除。
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
      // 扩展自己发的 prompt 不算「用户手动发消息」，不清除 esc 取消标志。
      return;
    }
    promptFromExtension = false;
    selfSending = false;
    // interactive / rpc：用户真正手动发消息 → 解除 esc 取消锁定。
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
      streak = 0;
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
    if (userCancelled && assistant?.stopReason === "stop") {
      userCancelled = false;
      try {
        ctx.ui.notify(`[${LOG}] 上一轮已正常结束，自动继续已恢复`, "info");
      } catch {
        /* ignore */
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

    const verdict = isContinueWorthyError(assistant, lastHttpStatus);
    if (!verdict.ok) {
      if (assistant?.stopReason && assistant.stopReason !== "error") streak = 0;
      return;
    }

    if (streak >= maxAttempts) {
      ctx.ui.notify(
        `[${LOG}] 连续 ${streak} 次瞬时错误已达上限，停手。可 /auto-continue 看状态，或自己发「继续」。`,
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
    ctx.ui.notify(
      `[${LOG}] ${reason}，${wait}ms 后自动继续（${attempt}/${maxAttempts}）`,
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
    description: "瞬时错误后自动发「继续」。用法: /auto-continue [on|off|status]",
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
