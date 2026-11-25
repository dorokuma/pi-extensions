import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import fs from "node:fs";
import net from "node:net";

/**
 * exclude-tools-guard.ts
 *
 * 全局工具排除守卫：
 * 保证在所有会话（交互式 TUI、--print、-p、--mode json、RPC、恢复旧会话等）中，
 * 彻底排除 grep, find, ls, bash 等工具，强制走 CodeGraph、ctx_fs、ctx_run。
 *
 * 子代理会话（C1）额外强制剔除主代理专用工具 jev_check 与 ask_user_question，
 * 并清空主代理专用 APPEND_SYSTEM.md（addendum 段）：
 * 派发层已用 --exclude-tools 与 --append-system-prompt '' 传参，但 `herdr agent start --kind pi`
 * 与直接跑 bin/pi 的旁路不带这些旗标，故在此按会话身份运行期兜底。
 */

// ===== SUBAGENT-IDENTITY-BEGIN =====
// 子代理身份判据（fail-closed）。本块在 jev-gate.ts 与 exclude-tools-guard.ts 中逐字节一致，
// 改动时必须同步两份（含风格，勿按宿主文件风格重排）；一致性校验：
//   diff <(sed -n '/SUBAGENT-IDENTITY-BEGIN/,/SUBAGENT-IDENTITY-END/p' jev-gate.ts) \
//        <(sed -n '/SUBAGENT-IDENTITY-BEGIN/,/SUBAGENT-IDENTITY-END/p' exclude-tools-guard.ts)
//
// 2026-10-08 加固背景（第一方在真实 CLI 上实测复现）：旧判据为「HERDR_PANE_ID 存在则只看它是否
// 以 :p1 结尾；否则看 argv 特征；都不满足即主代理」。只要把 HERDR_PANE_ID unset、置空或改写成
// 以 :p1 结尾（例如 herdr:tab7:p1），子代理进程内就回到主代理能力：classifier 表 24 条、
// jev_check 出现在工具表、windhub/jev-latest 可解析。根因是「判主代理」只依赖单个可被环境赋值
// 伪造的变量。
//
// 2026-11-25 hc-only 迁移：经典 herdr（herdr.sock / HERDR_PANE_ID / pane.process_info）分支随
// herdr 退役整体移除，herdr-core（hc）成为唯一的 S1/S2 正证明路径，与
// go/exclude-tools-guard v0.2.0 及 go/jev-gate v0.4.0 的共享身份核心对齐。
//
// 新判据：主代理需要下列正向证据全部成立；任一条缺位、无法确证、查询失败或超时，一律判子代理。
//   S1 HC_TERMINAL_ID 非空且匹配 ^t[0-9a-z]+$（hc 终端地址形状，如 "t12t"）。来源＝环境变量，
//      因此只作必要条件，单独伪造它无法通过 S2/S3。
//   S2 向运行中的 herdr-core server 发 JSON-RPC 2.0 terminal.get 查询该终端，回执必须 id 回环、
//      result 为对象、address 回环、state 存在时必须为 "live"、parent 键必须存在且为 JSON 字面
//      null（顶层主终端；`hc run --bg` 的子代理终端必带父地址——parent 缺失/""/非字符串、
//      非 "live" 的 state 一律 fail-closed）、pid 为正整数。来源＝hc server 的运行态；
//      socket 路径写死常量而不读 HC_TERMINAL_SOCKET，否则又引入一个可伪造的环境变量。
//   S3 本进程的 /proc 祖先链必须包含 S2 回执的 pid（作为 shell_pid）。来源＝内核进程表：祖先关系
//      由内核维护，环境赋值伪造不了；setsid/daemonize 只会让链条断掉（→ 子代理），不会凭空接到
//      别的终端的 shell 上。
//   S4 从本进程到 shell_pid 之间的祖先里不得出现其他 agent 运行时（pi / ctxmode）。来源＝内核进程表
//      ＋ /proc/<pid>/cmdline：主代理的 pi 由终端 shell 直接拉起，任何「pi 里再起 pi」都必然留下
//      agent 祖先，环境变量无法隐藏它。
//   残余面（已知、本次不覆盖）：能改终端内容物的旁路仍可绕——例如把另一个 pi 塞进顶层终端的 shell 里
//   执行，进程树看起来就是 shell 直接拉起。彻底封死需要 herdr-core 在终端启动时注入不可继承的凭据。
//
// 负向标记（命中即子代理；这些信号只能把结论推向子代理，不存在「伪造放行」的方向）：
//   PI_SUBAGENT=1 / PI_ROLE 非空 / argv 含 "--name role-"、"/tmp/herdr-role-sessions"、"/tmp/dispatch-"。
//
// 诊断：PI_IDENTITY_DEBUG=1 只控制是否把判定理由打到 stderr，不参与判定。

/** herdr-core server 的 unix socket（常量写死：HC_TERMINAL_SOCKET 可被环境赋值伪造，故不采用）。 */
const HC_SOCKET_FILE = "/run/user/0/herdr-core/default.sock"
/** 单次 socket 查询超时（毫秒）；超时按子代理处置。 */
const IDENTITY_QUERY_TIMEOUT_MS = 1500
/** /proc 祖先链回溯上限（防御异常/环状 stat）。 */
const IDENTITY_ANCESTOR_MAX_HOPS = 32
/** hc 终端地址形状（如 "t12t"）；socket 参数绝不由它派生。 */
const HC_TERMINAL_ID_PATTERN = /^t[0-9a-z]+$/u

interface HcTerminalInfo {
  pid: number
  /** null = 顶层终端（parent 为 JSON null）；非空字符串 = `hc run --bg` 子代理终端的父地址。 */
  parent: string | null
}

interface AncestorProcess {
  pid: number
  ppid: number
  cmdline: string
}

/**
 * 经 herdr-core server socket 查询终端记录（JSON-RPC 2.0 terminal.get）。
 * 任何失败（连接/超时/非 JSON/回执 id 不符/result 非对象/address 不符/state 存在但非 "live"/
 * parent 键缺失或为空串或类型不符/pid 非正整数）返回 null —— 调用方按子代理处置。
 */
function queryHcTerminalInfo(termId: string, timeoutMs: number = IDENTITY_QUERY_TIMEOUT_MS): Promise<HcTerminalInfo | null> {
  return new Promise((resolve) => {
    let settled = false
    let socket: ReturnType<typeof net.connect> | undefined
    let timer: ReturnType<typeof setTimeout> | undefined
    const finish = (value: HcTerminalInfo | null): void => {
      if (settled) return
      settled = true
      if (timer !== undefined) clearTimeout(timer)
      if (socket) {
        try {
          socket.destroy()
        } catch {
          // 连接已关闭
        }
      }
      resolve(value)
    }
    const handleReply = (text: string): void => {
      let reply: { id?: unknown; result?: unknown }
      try {
        reply = JSON.parse(text) as typeof reply
      } catch {
        finish(null)
        return
      }
      if (reply?.id !== "pi-identity" || typeof reply?.result !== "object" || reply.result === null || Array.isArray(reply.result)) {
        finish(null)
        return
      }
      const record = reply.result as Record<string, unknown>
      if (record.address !== termId) {
        finish(null)
        return
      }
      // state 在线上可选；存在时必须为 "live"，否则不能证明一个存活终端。
      if ("state" in record && record.state !== "live") {
        finish(null)
        return
      }
      // parent 键必须存在：JSON null = 顶层终端；非空字符串 = 父地址（子代理终端）；
      // 缺失、空串或其它类型都无法证明顶层形态，整条记录 fail-closed。
      if (!("parent" in record)) {
        finish(null)
        return
      }
      const rawParent = record.parent
      let parent: string | null
      if (rawParent === null) {
        parent = null
      } else if (typeof rawParent === "string" && rawParent !== "") {
        parent = rawParent
      } else {
        finish(null)
        return
      }
      const pid = Number(record.pid)
      if (!Number.isInteger(pid) || pid <= 0) {
        finish(null)
        return
      }
      finish({ pid, parent })
    }
    socket = net.connect(HC_SOCKET_FILE)
    timer = setTimeout(() => finish(null), timeoutMs)
    let buffer = ""
    socket.on("connect", () => {
      try {
        socket?.write(`${JSON.stringify({ id: "pi-identity", jsonrpc: "2.0", method: "terminal.get", params: { caller: termId, terminal: termId } })}\n`)
      } catch {
        finish(null)
      }
    })
    socket.on("data", (chunk: Buffer) => {
      buffer += chunk.toString("utf8")
      const newline = buffer.indexOf("\n")
      if (newline < 0) {
        if (buffer.length > 1048576) finish(null)
        return
      }
      handleReply(buffer.slice(0, newline))
    })
    socket.on("error", () => finish(null))
    socket.on("close", () => {
      if (settled) return
      handleReply(buffer)
    })
  })
}

/** 读 /proc/<pid>/stat 取 ppid（从最后一个 ')' 之后切分：comm 里可能含空格与括号）。 */
function readProcPpid(pid: number): number | null {
  try {
    const stat = fs.readFileSync(`/proc/${pid}/stat`, "utf8")
    const close = stat.lastIndexOf(")")
    if (close < 0) return null
    const fields = stat.slice(close + 2).trim().split(/\s+/u)
    const ppid = Number(fields[1])
    return Number.isInteger(ppid) && ppid >= 0 ? ppid : null
  } catch {
    return null
  }
}

/** 读 /proc/<pid>/cmdline（NUL 分隔），读不到返回空串。 */
function readProcCmdline(pid: number): string {
  try {
    return fs.readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0").filter((part) => part.length > 0).join(" ")
  } catch {
    return ""
  }
}

/** 祖先链（含自身，自内向外）；任一环读不到即截断。 */
function readAncestorChain(pid: number, maxHops: number): AncestorProcess[] {
  const chain: AncestorProcess[] = []
  const seen = new Set<number>()
  let current = pid
  for (let hop = 0; hop < maxHops; hop++) {
    if (!Number.isInteger(current) || current <= 0 || seen.has(current)) break
    seen.add(current)
    const ppid = readProcPpid(current)
    if (ppid === null) break
    chain.push({ pid: current, ppid, cmdline: readProcCmdline(current) })
    if (ppid === 0 || ppid === current) break
    current = ppid
  }
  return chain
}

/** 是否为 agent 运行时进程（pi / ctxmode / claude 家族本体或包装形态）。 */
function isAgentRuntimeCmdline(cmdline: string): boolean {
  if (!cmdline) return false
  const argv0 = cmdline.split(" ")[0] ?? ""
  const base = argv0.slice(argv0.lastIndexOf("/") + 1)
  if (base === "pi" || base === "ctxmode" || base === "claude" || base === "claude-code" || base === "claude-pty") return true
  return /pi-coding-agent|bundle\/cli\.js|(?:^|\/)claude\/versions\//u.test(cmdline)
}

/** 命中负向标记则返回描述，否则 null。 */
function subagentLaunchMarker(): string | null {
  if (process.env.PI_SUBAGENT === "1") return "PI_SUBAGENT=1"
  if (process.env.PI_ROLE) return "PI_ROLE is set"
  const argv = process.argv.join(" ")
  if (argv.includes("--name role-")) return "argv has --name role-"
  if (argv.includes("/tmp/herdr-role-sessions")) return "argv has /tmp/herdr-role-sessions"
  if (argv.includes("/tmp/dispatch-")) return "argv has /tmp/dispatch-"
  return null
}

/**
 * 身份判定。isSubagent=true 表示「子代理，或无法确证为主代理」。
 * 无参数：pid/env/argv 一律取自本进程，不接受调用方注入（避免新增伪造面）。
 */
async function resolveAgentIdentity(): Promise<{ isSubagent: boolean; reason: string }> {
  const marker = subagentLaunchMarker()
  if (marker) return { isSubagent: true, reason: `launch marker: ${marker}` }

  const termId = (process.env.HC_TERMINAL_ID ?? "").trim()
  if (!termId) return { isSubagent: true, reason: "S1: HC_TERMINAL_ID unset or empty" }
  if (!HC_TERMINAL_ID_PATTERN.test(termId)) {
    return { isSubagent: true, reason: `S1: HC_TERMINAL_ID=${termId} is not a terminal address (no ^t[0-9a-z]+$ match)` }
  }

  const terminal = await queryHcTerminalInfo(termId)
  if (!terminal) return { isSubagent: true, reason: `S2: hc terminal.get for ${termId} failed, timed out or mismatched` }
  if (terminal.parent !== null) {
    return { isSubagent: true, reason: `S1: HC_TERMINAL_ID=${termId} has parent=${terminal.parent} (subagent terminal)` }
  }

  const chain = readAncestorChain(process.pid, IDENTITY_ANCESTOR_MAX_HOPS)
  if (chain.length === 0) return { isSubagent: true, reason: "S3: /proc ancestry for this process is unreadable" }
  const index = chain.findIndex((entry) => entry.pid === terminal.pid)
  if (index < 0) {
    const seen = chain.map((entry) => `${entry.pid}${entry.cmdline ? `(${entry.cmdline.slice(0, 40)})` : ""}`).join(" <- ")
    return {
      isSubagent: true,
      reason: `S3: terminal ${termId} shell_pid=${terminal.pid} is not in this process ancestry [${seen}]`,
    }
  }
  for (let hop = 1; hop < index; hop++) {
    const ancestor = chain[hop]
    if (isAgentRuntimeCmdline(ancestor.cmdline)) {
      return {
        isSubagent: true,
        reason: `S4: agent runtime ancestor pid=${ancestor.pid} "${ancestor.cmdline.slice(0, 60)}" between this process and terminal shell`,
      }
    }
  }
  return { isSubagent: false, reason: `S1-S4 hold: terminal=${termId} shell_pid=${terminal.pid} hop=${index}` }
}

/** 本会话是否子代理。fail-closed：无法确证为主代理时一律 true。 */
async function isSubagentSession(): Promise<boolean> {
  const verdict = await resolveAgentIdentity()
  if (process.env.PI_IDENTITY_DEBUG === "1") {
    console.error(`[subagent-identity] ${verdict.isSubagent ? "subagent" : "main"}: ${verdict.reason}`)
  }
  return verdict.isSubagent
}
// ===== SUBAGENT-IDENTITY-END =====

export default function (pi: ExtensionAPI) {
  const EXCLUDED_TOOLS = new Set(["grep", "find", "ls", "bash"]);
  /** 子代理会话额外剔除：jev_check 是喂给主代理的门禁信号，ask_user_question 是面向用户的问询通道。 */
  const SUBAGENT_EXCLUDED_TOOLS = new Set(["jev_check", "ask_user_question"]);

  const enforceExclusions = async (): Promise<boolean> => {
    const isSubagent = await isSubagentSession();
    const excluded = isSubagent
      ? new Set([...EXCLUDED_TOOLS, ...SUBAGENT_EXCLUDED_TOOLS])
      : EXCLUDED_TOOLS;
    const currentActive = pi.getActiveTools();
    const filtered = currentActive.filter((t) => !excluded.has(t));
    if (filtered.length !== currentActive.length) {
      pi.setActiveTools(filtered);
    }
    return isSubagent;
  };

  // 会话启动时生效（身份判定 fail-closed，见上面 SUBAGENT-IDENTITY 块）
  pi.on("session_start", async () => {
    await enforceExclusions();
  });

  // 每个 turn 开始时二次兜底（防止子代理或会话恢复中途被激活，并剥离主代理专用 APPEND_SYSTEM.md）
  pi.on("before_agent_start", async (event) => {
    const isSubagent = await enforceExclusions();
    if (!isSubagent) return;
    const leakedAppend = event.systemPromptOptions.appendSystemPrompt;
    event.systemPromptOptions.appendSystemPrompt = "";
    if (
      leakedAppend &&
      event.systemPromptOptions.forceSystemPrompt !== undefined
    ) {
      const addendumBlock = `<addendum>\n${leakedAppend}\n</addendum>`;
      event.systemPromptOptions.forceSystemPrompt =
        event.systemPromptOptions.forceSystemPrompt
          .replaceAll(`\n\n${addendumBlock}`, "")
          .replaceAll(addendumBlock, "");
    }
  });
}
