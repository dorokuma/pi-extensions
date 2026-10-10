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
 * 子代理会话（C1）额外强制剔除主代理专用工具 jev_check 与 ask_user_question：
 * 派发层已用 --exclude-tools 传这两个名字，但 `herdr agent start --kind pi` 与直接跑
 * bin/pi 的旁路不带该旗标，故在此按会话身份运行期兜底。
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
// 新判据：主代理需要下列正向证据全部成立；任一条缺位、无法确证、查询失败或超时，一律判子代理。
//   S1 HERDR_PANE_ID 非空且以 ":p1" 结尾。来源＝环境变量，因此只作必要条件，单独伪造它无法通过
//      S2/S3（这正是旧判据的漏洞所在）。
//   S2 向运行中的 herdr server 查询该 pane，回执必须 pane_id 一致且 shell_pid > 0。来源＝herdr
//      server 的运行态；socket 路径写死常量而不读 HERDR_SOCKET_PATH，否则又引入一个可伪造的环境变量。
//   S3 本进程的 /proc 祖先链必须包含 S2 回执的 shell_pid。来源＝内核进程表：祖先关系由内核维护，
//      环境赋值伪造不了；setsid/daemonize 只会让链条断掉（→ 子代理），不会凭空接到别的 pane 的 shell 上。
//   S4 从本进程到 shell_pid 之间的祖先里不得出现其他 agent 运行时（pi / ctxmode）。来源＝内核进程表
//      ＋ /proc/<pid>/cmdline：主代理的 pi 由 pane shell 直接拉起，任何「pi 里再起 pi」都必然留下
//      agent 祖先，环境变量无法隐藏它。
//   残余面（已知、本次不覆盖）：能改 pane 内容物的旁路仍可绕——例如把另一个 pi 塞进 p1 的 shell 里
//   执行，进程树看起来就是 shell 直接拉起。彻底封死需要 herdr 在 pane 启动时注入不可继承的凭据。
//
// 负向标记（命中即子代理；这些信号只能把结论推向子代理，不存在「伪造放行」的方向）：
//   PI_SUBAGENT=1 / PI_ROLE 非空 / argv 含 "--name role-"、"/tmp/herdr-role-sessions"、"/tmp/dispatch-"。
//
// 诊断：PI_IDENTITY_DEBUG=1 只控制是否把判定理由打到 stderr，不参与判定。

/** herdr server 的 unix socket（常量写死：HERDR_SOCKET_PATH 可被环境赋值伪造，故不采用）。 */
const HERDR_SOCKET_FILE = "/root/.config/herdr/herdr.sock"
/** 单次 socket 查询超时（毫秒）；超时按子代理处置。 */
const IDENTITY_QUERY_TIMEOUT_MS = 1500
/** /proc 祖先链回溯上限（防御异常/环状 stat）。 */
const IDENTITY_ANCESTOR_MAX_HOPS = 32

interface PaneProcessInfo {
  shellPid: number
}

interface AncestorProcess {
  pid: number
  ppid: number
  cmdline: string
}

/**
 * 经 herdr server socket 查询 pane 的 shell_pid。
 * 任何失败（连接/超时/非 JSON/回执不符/pane 不存在）返回 null —— 调用方按子代理处置。
 */
function queryPaneShellPid(paneId: string, timeoutMs: number = IDENTITY_QUERY_TIMEOUT_MS): Promise<PaneProcessInfo | null> {
  return new Promise((resolve) => {
    let settled = false
    let socket: ReturnType<typeof net.connect> | undefined
    let timer: ReturnType<typeof setTimeout> | undefined
    const finish = (value: PaneProcessInfo | null): void => {
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
      let reply: { id?: unknown; result?: { process_info?: { pane_id?: unknown; shell_pid?: unknown } } }
      try {
        reply = JSON.parse(text) as typeof reply
      } catch {
        finish(null)
        return
      }
      const info = reply?.result?.process_info
      const shellPid = Number(info?.shell_pid)
      if (reply?.id === "pi-identity" && info?.pane_id === paneId && Number.isInteger(shellPid) && shellPid > 0) {
        finish({ shellPid })
      } else {
        finish(null)
      }
    }
    socket = net.connect(HERDR_SOCKET_FILE)
    timer = setTimeout(() => finish(null), timeoutMs)
    let buffer = ""
    socket.on("connect", () => {
      try {
        socket?.write(`${JSON.stringify({ id: "pi-identity", method: "pane.process_info", params: { pane_id: paneId } })}\n`)
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

/** 是否为 agent 运行时进程（pi / ctxmode 本体或包装形态）。 */
function isAgentRuntimeCmdline(cmdline: string): boolean {
  if (!cmdline) return false
  const argv0 = cmdline.split(" ")[0] ?? ""
  const base = argv0.slice(argv0.lastIndexOf("/") + 1)
  if (base === "pi" || base === "ctxmode") return true
  return /pi-coding-agent|bundle\/cli\.js/u.test(cmdline)
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

  const paneId = (process.env.HERDR_PANE_ID ?? "").trim()
  if (!paneId) return { isSubagent: true, reason: "S1: HERDR_PANE_ID unset or empty" }
  if (!paneId.endsWith(":p1")) {
    return { isSubagent: true, reason: `S1: HERDR_PANE_ID=${paneId} is not a main pane (no ":p1" suffix)` }
  }

  const pane = await queryPaneShellPid(paneId)
  if (!pane) return { isSubagent: true, reason: `S2: herdr pane query for ${paneId} failed, timed out or mismatched` }

  const chain = readAncestorChain(process.pid, IDENTITY_ANCESTOR_MAX_HOPS)
  if (chain.length === 0) return { isSubagent: true, reason: "S3: /proc ancestry for this process is unreadable" }
  const index = chain.findIndex((entry) => entry.pid === pane.shellPid)
  if (index < 0) {
    const seen = chain.map((entry) => `${entry.pid}${entry.cmdline ? `(${entry.cmdline.slice(0, 40)})` : ""}`).join(" <- ")
    return {
      isSubagent: true,
      reason: `S3: pane ${paneId} shell_pid=${pane.shellPid} is not in this process ancestry [${seen}]`,
    }
  }
  for (let hop = 1; hop < index; hop++) {
    const ancestor = chain[hop]
    if (isAgentRuntimeCmdline(ancestor.cmdline)) {
      return {
        isSubagent: true,
        reason: `S4: agent runtime ancestor pid=${ancestor.pid} "${ancestor.cmdline.slice(0, 60)}" between this process and pane shell`,
      }
    }
  }
  return { isSubagent: false, reason: `S1-S4 hold: pane=${paneId} shell_pid=${pane.shellPid} hop=${index}` }
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

  const enforceExclusions = async (): Promise<void> => {
    const excluded = (await isSubagentSession())
      ? new Set([...EXCLUDED_TOOLS, ...SUBAGENT_EXCLUDED_TOOLS])
      : EXCLUDED_TOOLS;
    const currentActive = pi.getActiveTools();
    const filtered = currentActive.filter((t) => !excluded.has(t));
    if (filtered.length !== currentActive.length) {
      pi.setActiveTools(filtered);
    }
  };

  // 会话启动时生效（身份判定 fail-closed，见上面 SUBAGENT-IDENTITY 块）
  pi.on("session_start", async () => {
    await enforceExclusions();
  });

  // 每个 turn 开始时二次兜底（防止子代理或会话恢复中途被激活）
  pi.on("before_agent_start", async () => {
    await enforceExclusions();
  });
}
