// ctx-orchestrate.ts — codemode 灰度「终态设施」
//
// 落点：把 3 个高频多步流程固化为组合工具（exposure:"codemode"），并给 codemode 的
// text() 回传加一道物理硬顶。与 ctxmode.ts / codegraph-go.ts 等既有扩展的关系：
//   - 不 spawn 任何进程、不注册 command、不动 settings.json；只在本文件里
//     registerTool ×3 + pi.on("tool_result") ×1。
//   - 对被组合的 codegraph / ctx_run / ctx_fs 一律用 ctx.executeTool() 嵌套调用，
//     结果不单独进 transcript（记 nestedCalls），组合工具自己负责摘要。
//
// 设计依据（已核对）：
//   - registerTool 字段名与 dist/core/extensions/types.d.ts 完全一致：
//     name/label/description/promptSnippet/promptGuidelines/parameters/outputSchema/
//     exposure/namespace/annotations/execute。
//   - exposure:"codemode" = 注册即可达、codemode 脚本内 tools.* 与 searchTools() 可见，
//     不主动声明给模型；namespace{name,description,instructions} 参与 codemode 分组，
//     instructions 由 describeNamespace() 读取。
//   - outputSchema + structuredContent：脚本内拿到结构化对象；text content 仍是模型面。
//     失败但带数据时用 isError:true（不 throw），脚本拿得到 structuredContent。
//   - ctx.executeTool(name,args,{signal})：永不因工具失败而 reject，失败以
//     isError:true 的 AgentToolCallOutcome 回来；本文件仍再包一层 try/catch 兜底。
//   - tool_result 钩子可替换 content/details/structuredContent/isError/usage；
//     只替换 content 而不回传 structuredContent 会丢结构化内容，故治理钩子显式回传。
//
// 环境变量：
//   CTX_ORCH_TEXT_CAP    codemode 回传字符硬顶（默认 3000）
//   CTX_ORCH_GOVERN=0    关闭治理钩子（默认开）
//   CTX_ORCH_DEBUG=1     诊断写 stderr（默认只写日志文件，避免污染 TUI 输入行）

import type {
  AgentToolResult,
  ExtensionAPI,
  ExtensionToolContext,
  ToolResultEventResult,
} from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"

// ---------------------------------------------------------------------------
// 常量与环境
// ---------------------------------------------------------------------------

const NAMESPACE_NAME = "ctxmode"

/** codemode text() 回传硬顶默认值（字符，UTF-16 code unit）。 */
const DEFAULT_TEXT_CAP = 3000

/** 治理钩子触发前的总字符阈值（低于硬顶不动手，避免无谓改写）。 */
const GOVERN_MIN_TOTAL = 200

/** 单条 excerpt 的字符上限（摘要必须短，完整内容由模型自己按需再取）。 */
const EXCERPT_CAP = 200

/** codegraph node 一次返回的行数上限（与 Go 端硬顶 2000 之间留余量）。 */
const CG_NODE_LIMIT = 60

/** depth>=2 时最多展开多少个被调用符号的定义（防止扇出爆炸）。 */
const DEPTH2_MAX_SYMBOLS = 6

/** 单行原始输出进摘要的最大行数。 */
const TAIL_DEFAULT = 40
const TAIL_HARD = 400

/** rg 命中默认可_limit 上限与硬顶。 */
const HITS_DEFAULT = 50
const HITS_HARD = 500

const CONCURRENCY = 4

function envInt(name: string, fallback: number): number {
  const raw = process.env[name]
  if (raw === undefined || raw === "") return fallback
  const n = Number(raw)
  return Number.isFinite(n) ? Math.floor(n) : fallback
}

function envFlag(name: string, fallback: boolean): boolean {
  const raw = (process.env[name] || "").trim().toLowerCase()
  if (!raw) return fallback
  return raw === "1" || raw === "true" || raw === "yes" || raw === "on"
}

/** UTF-16 安全夹取：不把代理对切成两半。 */
function clampUtf16Index(s: string, i: number, towardStart: boolean): number {
  if (i <= 0) return 0
  if (i >= s.length) return s.length
  if ((s.charCodeAt(i) & 0xfc00) === 0xdc00) return towardStart ? i - 1 : i + 1
  return i
}

/** 夹住整数到 [1, hard]，非数字/越界回退默认。 */
function clampInt(value: unknown, def: number, hard: number): number {
  let n = typeof value === "number" && Number.isFinite(value) ? Math.floor(value) : def
  if (n < 1) n = def
  if (n > hard) n = hard
  return n
}

function errMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}

function optStr(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() !== "" ? value.trim() : undefined
}

function oneLine(text: string, cap = EXCERPT_CAP): string {
  const flat = text.replace(/\s+/g, " ").trim()
  if (flat.length <= cap) return flat
  return flat.slice(0, clampUtf16Index(flat, cap - 1, true)) + "…"
}

// ---- 诊断（TUI-safe：默认只写文件，禁 console.*） ---------------------------

let diagPath: string | null = null
let diagDegraded = false

function diagLog(msg: string): void {
  const line = `[ctx-orchestrate] ${msg}`
  if (envFlag("CTX_ORCH_DEBUG", false)) console.error(line)
  try {
    if (!diagPath) {
      const dir = process.env.CTX_ORCH_DIAG_DIR || path.join(os.homedir() || "/tmp", ".pi", "agent", "logs")
      fs.mkdirSync(dir, { recursive: true })
      diagPath = path.join(dir, "ctx-orchestrate.log")
    }
    fs.appendFileSync(diagPath, `${new Date().toISOString()} ${line}\n`)
  } catch (err) {
    diagPath = null
    if (!diagDegraded) {
      diagDegraded = true
      diagLog(`diagnostics degraded: ${errMessage(err)}`)
    }
  }
}

// ---------------------------------------------------------------------------
// 嵌套调用：对 ctx.executeTool 的失败防御
// ---------------------------------------------------------------------------

interface NestedOutcome {
  ok: boolean
  /** 文本内容（多个 text block 以 \n 连接）。 */
  text: string
  isError: boolean
  error?: string
}

/** 并发安全地跑一批嵌套调用：单个失败/拒绝都变成 failed 条目，绝不 Promise.all 炸全局。 */
async function runNested(
  ctx: ExtensionToolContext,
  jobs: Array<{ name: string; args: Record<string, unknown> }>,
  signal?: AbortSignal,
): Promise<Array<NestedOutcome & { name: string }>> {
  const settled = await Promise.allSettled(
    jobs.map(async (job) => {
      try {
        const outcome = await ctx.executeTool(job.name, job.args, { signal })
        const content = (outcome?.result?.content ?? []) as Array<{ type?: string; text?: string }>
        const text = content
          .filter((block) => block?.type === "text")
          .map((block) => String(block.text ?? ""))
          .join("\n")
        return {
          name: job.name,
          ok: !outcome?.isError,
          isError: Boolean(outcome?.isError),
          text,
          error: outcome?.isError ? oneLine(text || "tool error", 300) : undefined,
        }
      } catch (err) {
        // executeTool 承诺「永不因工具失败而 reject」，这里兜住宿主/信号异常。
        const msg = errMessage(err)
        return { name: job.name, ok: false, isError: true, text: "", error: oneLine(msg, 300) }
      }
    }),
  )
  return settled.map((entry, i) =>
    entry.status === "fulfilled"
      ? entry.value
      : { name: jobs[i]?.name ?? "unknown", ok: false, isError: true, text: "", error: oneLine(`rejected: ${errMessage(entry.reason)}`, 300) },
  )
}

/** 有界并发：chunk + 顺序 allSettled，避免同时打爆 ctxmode/子进程。 */
async function mapLimit<T, R>(
  items: readonly T[],
  limit: number,
  fn: (item: T, index: number) => Promise<R>,
): Promise<R[]> {
  const out: R[] = []
  for (let i = 0; i < items.length; i += Math.max(1, limit)) {
    const chunk = items.slice(i, i + Math.max(1, limit))
    const done = await Promise.allSettled(chunk.map((item, j) => fn(item, i + j)))
    for (const entry of done) out.push((entry as PromiseFulfilledResult<R>).value)
  }
  return out
}

// ---------------------------------------------------------------------------
// 解析：codegraph 文本输出 → path:line
// ---------------------------------------------------------------------------

interface Located {
  path: string
  line?: number
}

/** token 像文件路径：含 "/" 或带扩展名（排除 `Calls:` 段里的裸符号名）。 */
function looksLikePath(token: string): boolean {
  if (token.includes("/")) return true
  return /\.[A-Za-z0-9]{1,8}$/.test(token)
}

/** 从 codegraph 文本输出里抽取 `path:line`。裸符号名（clampUtf16Index:125）会被过滤。 */
export function parsePathLines(text: string): Located[] {
  const out: Located[] = []
  if (!text) return out
  const re = /([^\s():,"']+):(\d+)\b/g
  let m: RegExpExecArray | null
  while ((m = re.exec(text)) !== null) {
    const token = m[1]
    if (!looksLikePath(token)) continue
    const line = Number(m[2])
    if (!Number.isFinite(line) || line <= 0) continue
    out.push({ path: token, line })
  }
  return out
}

/** 从 codegraph node 输出的 `Calls:` 段取被调用符号名（depth=2 用）。 */
export function parseCalleeNames(nodeText: string): string[] {
  const line = nodeText
    .split("\n")
    .find((l) => /^\s*Calls:\s*\S/.test(l))
  if (!line) return []
  const names: string[] = []
  const re = /([A-Za-z_$][A-Za-z0-9_$]*)\s*:\s*\d+/g
  let m: RegExpExecArray | null
  while ((m = re.exec(line)) !== null) {
    if (!names.includes(m[1])) names.push(m[1])
  }
  return names
}

// ---------------------------------------------------------------------------
// 解析：ctx_fs rg 分组输出 → per-file hits
// ---------------------------------------------------------------------------

interface RgHit {
  line: number
  excerpt: string
}

interface RgParse {
  files: Array<{ path: string; hits: RgHit[] }>
  /** 首行 `matches=N`（工具自报命中数）。 */
  reportedMatches?: number
  /** rg 自称被截断（limit 或 200KB/500 命中捕获上限）。 */
  truncated: boolean
}

/**
 * ctx_fs rg 的输出形如：
 *   engine=rg matches=7 files=2 limit=3 truncated=false git=none indexed=...
 *   → Read .pi/agent/SYSTEM.md (27KB - use offset to read relevant section)
 *   7 matches in 2 files (full set indexed, > first-screen limit 3). Retrieve details: ...
 *   Files (M=modified, A=added, ??=untracked; then by match count):
 *   .pi/agent/SYSTEM.md 4 matches (27KB - use offset to read relevant section)
 *     55: **codemode 灰度规则**
 *     ... (+2 more)
 * 同时兼容裸 `file:line:content` 形态。
 */
export function parseRgOutput(text: string, maxPerFile: number): RgParse {
  const result: RgParse = { files: [], truncated: false }
  if (!text) return result
  const index = new Map<string, { path: string; hits: RgHit[] }>()

  const head = text.split("\n")[0] ?? ""
  const mCount = head.match(/\bmatches=(\d+)/)
  if (mCount) result.reportedMatches = Number(mCount[1])
  if (/\btruncated=true\b/.test(head) || /capture truncated/i.test(text)) result.truncated = true

  let current: { path: string; hits: RgHit[] } | undefined
  for (const raw of text.split("\n")) {
    const line = raw.replace(/\s+$/, "")
    if (!line) continue
    if (/^engine=/.test(line) || /^→\s*Read\s/.test(line)) continue
    if (/^\d+ matches in \d+ files?/.test(line)) continue
    if (/^Files \(/.test(line)) continue
    if (/^\.\.\. \(/.test(line)) continue

    // 命中行：`  55: excerpt`
    const hit = line.match(/^\s+(\d+):\s?(.*)$/)
    if (hit && current) {
      if (current.hits.length < maxPerFile) {
        current.hits.push({ line: Number(hit[1]), excerpt: oneLine(hit[2]) })
      }
      continue
    }
    // 文件头：`path 4 matches ...`（行首无空白）
    if (/^\S/.test(line)) {
      const head2 = line.match(/^(\S.*?)\s+(\d+)\s+matches?\b/)
      if (head2) {
        const p = head2[1].trim()
        current = index.get(p) ?? { path: p, hits: [] }
        index.set(p, current)
        continue
      }
      // 裸 file:line:content
      const bare = line.match(/^([^\s():]+):(\d+):(.*)$/)
      if (bare) {
        const p = bare[1]
        current = index.get(p) ?? { path: p, hits: [] }
        index.set(p, current)
        if (current.hits.length < maxPerFile) {
          current.hits.push({ line: Number(bare[2]), excerpt: oneLine(bare[3]) })
        }
      }
    }
  }
  result.files = [...index.values()]
  return result
}

// ---------------------------------------------------------------------------
// 解析：ctx_run run_task 输出 → 通过/失败计数 + 失败用例
// ---------------------------------------------------------------------------

/** 失败用例：用 type alias（非 interface），使其可赋给 structuredContent 的 JsonValue。 */
export type TestFailure = {
  name: string
  excerpt: string
}

export interface TestParse {
  argv?: string
  exitCode?: number
  passed: number
  failed: number
  failures: TestFailure[]
  /** 输出尾部若干行（给 summary 兜底）。 */
  tail: string[]
}

const TEST_KINDS = [
  "go_test",
  "go_build",
  "go_vet",
  "npm_test",
  "npm_run_build",
  "cargo_test",
  "cargo_build",
  "make",
  "custom",
] as const

/**
 * run_task 输出形如：
 *   kind: go_test
 *   argv: go test ./internal/config
 *   exit_code: 0
 *
 *   --- stdout ---
 *   ok  	pkg	0.003s
 *
 *   --- stderr ---
 *   ...
 */
export function parseTaskOutput(text: string, tailLines: number): TestParse {
  const out: TestParse = { passed: 0, failed: 0, failures: [], tail: [] }
  if (!text) return out
  const argv = text.match(/^argv:\s*(.+)$/m)
  if (argv) out.argv = argv[1].trim()
  const code = text.match(/^exit_code:\s*(-?\d+)/m)
  if (code) out.exitCode = Number(code[1])

  const body = text
    .split("\n")
    .filter((l) => !/^(kind|argv|exit_code):/.test(l) && !/^--- (stdout|stderr) ---$/.test(l))
    .join("\n")
    .trim()
  const lines = body.split("\n").filter((l) => l.trim() !== "")
  out.tail = lines.slice(-tailLines)

  // ---- 汇总行优先（jest/vitest/pytest 自带计数）----
  let counted = false
  const jest = body.match(/Tests:\s+(?:(\d+) failed,\s+)?(?:(\d+) passed,\s+)?(?:(\d+) total)?/i)
  if (jest && (jest[2] || jest[1])) {
    out.passed = Number(jest[2] ?? 0)
    out.failed = Number(jest[1] ?? 0)
    counted = true
  }
  // vitest 默认 reporter 的用例汇总行——独立于 tail 窗口，直接扫全文 body 提取：
  //   全通过：Tests  142 passed (142)
  //   有失败：Tests  2 failed | 140 passed (142)
  // 缺口根因：jest 用冒号式 "Tests:  2 failed, 140 passed, 142 total"，而 vitest 无冒号
  // 且以 `|` 分隔、按 <count> <status> 逐段给出；旧的 jest 正则（需冒号）与 pytest
  // 正则（需 passed 与 failed 同时出现且对顺序敏感）都匹配不到，故全通过时 passed/failed
  // 落 0（e2e 现场）。这里按行解析：\bTests\b 只命中用例汇总行，不命中 "Test Files  …"
  // （词尾是空格而非 s），也不命中 pytest 的 "test session …"（小写、无尾部 s）——因此对
  // pytest/go/tap 输出零匹配、对它们既有解析零影响。逐段加总 passed/failed，天然免疫
  // passed/failed 的先后顺序（vitest 先 failed 后 passed）。
  if (!counted) {
    const vitestLine = body.match(/^[ \t]*Tests\b[^\n]*$/im)
    if (vitestLine) {
      let vPass = 0
      let vFail = 0
      let vSeen = false
      const vSeg = /(\d+)\s+(passed|failed)\b/gi
      let vm: RegExpExecArray | null
      while ((vm = vSeg.exec(vitestLine[0])) !== null) {
        if (vm[2].toLowerCase() === "passed") vPass += Number(vm[1])
        else vFail += Number(vm[1])
        vSeen = true
      }
      if (vSeen && (vPass > 0 || vFail > 0)) {
        out.passed = vPass
        out.failed = vFail
        counted = true
      }
    }
  }
  const py = body.match(/(\d+)\s+passed.*?(\d+)\s+failed/i) ?? body.match(/(\d+)\s+failed.*?(\d+)\s+passed/i)
  if (!counted && py) {
    const a = Number(py[1])
    const b = Number(py[2])
    const failedFirst = /failed/i.test(body.slice(body.indexOf(py[0]), body.indexOf(py[0]) + 6))
    out.passed = failedFirst ? b : a
    out.failed = failedFirst ? a : b
    counted = true
  }

  // ---- 失败用例名 ----
  const names: Array<{ name: string; block: string[] }> = []
  const pushName = (name: string, block: string[]) => {
    const clean = name.trim()
    if (!clean) return
    const existing = names.find((n) => n.name === clean)
    if (existing) {
      if (existing.block.length < 4) existing.block.push(...block.slice(0, 4 - existing.block.length))
      return
    }
    names.push({ name: clean, block: block.slice(0, 4) })
  }
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]
    let m: RegExpMatchArray | null
    if ((m = line.match(/^\s*--- FAIL:\s+(\S+)/))) pushName(m[1], lines.slice(i + 1, i + 4))
    else if ((m = line.match(/^\s*not ok\s+\d+\s*-?\s*(.*)$/)) && m[1]) pushName(m[1], lines.slice(i + 1, i + 4))
    else if ((m = line.match(/^FAILED\s+(\S+)/))) pushName(m[1], lines.slice(i + 1, i + 4))
    else if ((m = line.match(/^\s*[✕×✗]\s+(.+)$/))) pushName(m[1].replace(/\s+\d+(?:\.\d+)?ms\s*$/i, ""), lines.slice(i + 1, i + 4))
    else if ((m = line.match(/^●\s+(.+)$/))) pushName(m[1], lines.slice(i + 1, i + 4))
    else if ((m = line.match(/^\s*FAIL\s+(\S+)\s*$/)) && /\//.test(m[1])) pushName(m[1], lines.slice(i + 1, i + 4))
  }

  // ---- 计数（go 风格 / tap）----
  if (!counted) {
    const okPkgs = (body.match(/^ok\s+\S/gm) ?? []).length
    const goPass = (body.match(/^--- PASS:/gm) ?? []).length
    const tapOk = (body.match(/^ok\s+\d+/gm) ?? []).length
    out.passed = okPkgs + goPass + tapOk
    const failPkgs = (body.match(/^FAIL\s+\S+\s+\[setup failed\]/gm) ?? []).length
    out.failed = failPkgs + names.length
  } else {
    if (out.failed === 0 && names.length > 0) out.failed = names.length
    if (out.passed === 0 && out.failed === 0 && names.length === 0) out.passed = (body.match(/^--- PASS:/gm) ?? []).length
  }
  if (out.passed === 0 && out.failed === 0 && names.length === 0 && out.exitCode && out.exitCode !== 0) {
    // 非零退出但没解析出用例名：给一条合成失败，保证调用方能判定
    out.failed = 1
    names.push({ name: "<run failed>", block: out.tail.slice(0, 3) })
  }

  out.failures = names.map((n) => ({ name: n.name, excerpt: oneLine(n.block.join(" | ").slice(0, EXCERPT_CAP * 3)) }))
  return out
}

// ---------------------------------------------------------------------------
// 组合工具 1：ctx_symbol_read
// ---------------------------------------------------------------------------

const SYMBOL_ROLES = ["definition", "caller", "callee", "callee-definition", "related"] as const
type SymbolRole = (typeof SYMBOL_ROLES)[number]

const symbolReadSchema = Type.Object({
  symbol: Type.String({ description: "Symbol name to locate and expand, e.g. a function or type." }),
  depth: Type.Optional(
    Type.Number({
      description:
        "Graph hops: 1 = definition + direct callers/callees (default). 2 = also resolve the definitions of up to 6 callees. Hard cap 2.",
    }),
  ),
  max_files: Type.Optional(Type.Number({ description: "Max files returned (default 25, hard cap 200)." })),
  path: Type.Optional(Type.String({ description: "codegraph query scope; in home mode pass the project name." })),
  file: Type.Optional(Type.String({ description: "Known definition file or basename, to pin codegraph node lookup." })),
})

const symbolReadOutput = Type.Object({
  symbol: Type.String(),
  files: Type.Array(
    Type.Object({
      path: Type.String(),
      line: Type.Optional(Type.Number()),
      role: Type.String({ enum: [...SYMBOL_ROLES] }),
    }),
  ),
  summary: Type.String(),
  errors: Type.Optional(Type.Array(Type.Object({ step: Type.String(), error: Type.String() }))),
})

async function executeSymbolRead(
  params: Record<string, unknown>,
  ctx: ExtensionToolContext,
  signal?: AbortSignal,
): Promise<AgentToolResult<unknown>> {
  const symbol = String(params.symbol ?? "").trim()
  const depth = clampInt(params.depth, 1, 2)
  const maxFiles = clampInt(params.max_files, 25, 200)
  const scope = optStr(params.path)
  const pinFile = optStr(params.file)

  const scopeArgs = (): Record<string, unknown> => ({
    ...(scope ? { path: scope } : {}),
    ...(pinFile ? { file: pinFile } : {}),
  })

  const errors: Array<{ step: string; error: string }> = []
  const located: Array<Located & { role: SymbolRole }> = []

  // 第一跳：定位 + 直接调用方/被调用方，并发
  const first = await runNested(
    ctx,
    [
      { name: "codegraph", args: { action: "node", name: symbol, limit: CG_NODE_LIMIT, ...scopeArgs() } },
      { name: "codegraph", args: { action: "callers", name: symbol, max: maxFiles, ...scopeArgs() } },
      { name: "codegraph", args: { action: "callees", name: symbol, max: maxFiles, ...scopeArgs() } },
    ],
    signal,
  )
  const [nodeRes, callersRes, calleesRes] = first
  for (const [step, res] of [
    ["node", nodeRes],
    ["callers", callersRes],
    ["callees", calleesRes],
  ] as const) {
    if (res && !res.ok) errors.push({ step, error: res.error ?? "unknown error" })
  }

  const nodeText = nodeRes?.text ?? ""
  for (const hit of parsePathLines(nodeText)) located.push({ ...hit, role: "definition" })
  for (const hit of parsePathLines(callersRes?.text ?? "")) located.push({ ...hit, role: "caller" })
  for (const hit of parsePathLines(calleesRes?.text ?? "")) located.push({ ...hit, role: "callee" })

  // 第二跳（depth=2）：展开被调用符号的定义
  if (depth >= 2) {
    const names = parseCalleeNames(nodeText).slice(0, DEPTH2_MAX_SYMBOLS)
    if (names.length > 0) {
      const second = await runNested(
        ctx,
        names.map((name) => ({
          name: "codegraph",
          args: { action: "node", name, limit: 10, ...scopeArgs() },
        })),
        signal,
      )
      second.forEach((res, i) => {
        if (!res.ok) {
          errors.push({ step: `node:${names[i]}`, error: res.error ?? "unknown error" })
          return
        }
        for (const hit of parsePathLines(res.text)) located.push({ ...hit, role: "callee-definition" })
      })
    }
  }

  // 聚合去重：path+line 相同只留一条，角色按 definition > caller > callee > callee-definition
  const priority: Record<SymbolRole, number> = {
    definition: 0,
    caller: 1,
    callee: 2,
    "callee-definition": 3,
    related: 4,
  }
  const byKey = new Map<string, Located & { role: SymbolRole }>()
  for (const entry of located) {
    if (!entry.path) continue
    const key = `${entry.path}:${entry.line ?? 0}`
    const prev = byKey.get(key)
    if (!prev || priority[entry.role] < priority[prev.role]) byKey.set(key, entry)
  }
  const sorted = [...byKey.values()].sort(
    (a, b) => priority[a.role] - priority[b.role] || a.path.localeCompare(b.path) || (a.line ?? 0) - (b.line ?? 0),
  )
  const files = sorted.slice(0, maxFiles).map((e) => ({
    path: e.path,
    ...(e.line ? { line: e.line } : {}),
    role: e.role,
  }))
  const truncated = sorted.length > files.length

  const counts = files.reduce<Record<string, number>>((acc, f) => {
    acc[f.role] = (acc[f.role] ?? 0) + 1
    return acc
  }, {})
  const parts = Object.entries(counts).map(([role, n]) => `${role}=${n}`)
  let summary = `symbol=${symbol} depth=${depth} files=${files.length}/${sorted.length}${parts.length ? ` (${parts.join(", ")})` : ""}`
  if (errors.length > 0) summary += `; ${errors.length} nested call(s) degraded: ${errors.map((e) => e.step).join(", ")}`
  if (truncated) summary += `; output capped at max_files=${maxFiles}`
  if (files.length === 0) summary += "; no location found (symbol may be unindexed; try codegraph action=status)"

  const text = [
    summary,
    ...files.slice(0, 40).map((f) => `${f.role}\t${f.path}${f.line ? `:${f.line}` : ""}`),
    files.length > 40 ? `... (+${files.length - 40} more files)` : "",
  ]
    .filter(Boolean)
    .join("\n")

  return {
    content: [{ type: "text", text }],
    details: { summary },
    structuredContent: { symbol, files, summary, ...(errors.length ? { errors } : {}) },
  }
}

// ---------------------------------------------------------------------------
// 组合工具 2：ctx_test_digest
// ---------------------------------------------------------------------------

const testDigestSchema = Type.Object({
  kind: Type.String({
    description: "Task kind understood by ctx_run run_task.",
    enum: [...TEST_KINDS],
  }),
  target: Type.String({ description: "Test/build target, e.g. ./internal/config, test, or a make target." }),
  tail: Type.Optional(Type.Number({ description: `Trailing raw lines kept for the summary (default ${TAIL_DEFAULT}, hard cap ${TAIL_HARD}).` })),
  cwd: Type.Optional(Type.String({ description: "Working directory for the task." })),
  args: Type.Optional(Type.Array(Type.String(), { description: "Extra argv appended to the task runner." })),
  timeout_ms: Type.Optional(Type.Number({ description: "ms (default 300000, max 3600000)." })),
})

const testDigestOutput = Type.Object({
  kind: Type.String(),
  target: Type.String(),
  passed: Type.Number(),
  failed: Type.Number(),
  failures: Type.Array(Type.Object({ name: Type.String(), excerpt: Type.String() })),
  summary: Type.String(),
  exit_code: Type.Optional(Type.Number()),
  argv: Type.Optional(Type.String()),
  tail: Type.Optional(Type.Array(Type.String())),
  error: Type.Optional(Type.String()),
})

async function executeTestDigest(
  params: Record<string, unknown>,
  ctx: ExtensionToolContext,
  signal?: AbortSignal,
): Promise<AgentToolResult<unknown>> {
  const kind = String(params.kind ?? "").trim()
  const target = String(params.target ?? "").trim()
  const tailLines = clampInt(params.tail, TAIL_DEFAULT, TAIL_HARD)
  const cwd = optStr(params.cwd)
  const args = Array.isArray(params.args) ? params.args.map((a) => String(a)) : undefined
  const timeoutMs =
    typeof params.timeout_ms === "number" && params.timeout_ms > 0 ? Math.floor(params.timeout_ms) : undefined

  const base = (): Record<string, unknown> => ({
    ...(cwd ? { cwd } : {}),
    ...(args && args.length ? { args } : {}),
    ...(timeoutMs ? { timeout_ms: Math.min(timeoutMs, 3600000) } : {}),
  })

  // 只跑一个任务：runNested 保持与其他工具一致的失败防御形状
  const [res] = await runNested(
    ctx,
    [{ name: "ctx_run", args: { action: "run_task", kind, target, ...base() } }],
    signal,
  )

  if (!res || !res.ok) {
    const error = res?.error ?? "ctx_run run_task unavailable"
    const summary = `${kind} ${target}: nested ctx_run failed — ${error}`
    return {
      content: [{ type: "text", text: summary }],
      details: { summary },
      isError: true,
      structuredContent: { kind, target, passed: 0, failed: 0, failures: [], summary, error },
    }
  }

  const parsed = parseTaskOutput(res.text, tailLines)
  const failedRuns = parsed.exitCode !== undefined && parsed.exitCode !== 0
  let summary =
    `${kind} ${target}: passed=${parsed.passed} failed=${parsed.failed}` +
    `${parsed.exitCode !== undefined ? ` exit=${parsed.exitCode}` : ""}` +
    `${parsed.argv ? ` argv="${parsed.argv}"` : ""}`
  if (parsed.failures.length > 0) {
    summary += `; failing: ${parsed.failures.map((f) => f.name).join(", ")}`
  }
  if (failedRuns && parsed.failures.length === 0) summary += "; run exited non-zero without a parsed case name"

  const text = [
    summary,
    ...parsed.failures.slice(0, 20).map((f) => `FAIL ${f.name}\n     ${f.excerpt}`),
    parsed.tail.length ? `--- tail (${parsed.tail.length} lines) ---\n${parsed.tail.join("\n")}` : "",
  ]
    .filter(Boolean)
    .join("\n")

  return {
    content: [{ type: "text", text }],
    details: { summary },
    ...(failedRuns ? { isError: true } : {}),
    structuredContent: {
      kind,
      target,
      passed: parsed.passed,
      failed: parsed.failed,
      failures: parsed.failures,
      summary,
      ...(parsed.exitCode !== undefined ? { exit_code: parsed.exitCode } : {}),
      ...(parsed.argv ? { argv: parsed.argv } : {}),
      tail: parsed.tail,
    },
  }
}

// ---------------------------------------------------------------------------
// 组合工具 3：ctx_search_digest
// ---------------------------------------------------------------------------

const searchDigestSchema = Type.Object({
  patterns: Type.Array(Type.String(), {
    description: "One or more rg patterns (literal or regex) searched concurrently.",
  }),
  path: Type.Optional(Type.String({ description: "Search root under the workspace." })),
  glob: Type.Optional(Type.String({ description: "File glob filter, e.g. *.ts." })),
  max_hits: Type.Optional(Type.Number({ description: `Hit budget per pattern (default ${HITS_DEFAULT}, hard cap ${HITS_HARD}).` })),
  include_hidden: Type.Optional(Type.Boolean({ description: "Also search hidden directories and files." })),
  ignore_case: Type.Optional(Type.Boolean()),
  context: Type.Optional(Type.Number({ description: "Context lines around each hit." })),
  literal: Type.Optional(Type.Boolean()),
})

const searchDigestOutput = Type.Object({
  patterns: Type.Array(Type.String()),
  files: Type.Array(
    Type.Object({
      path: Type.String(),
      hits: Type.Array(Type.Object({ line: Type.Number(), excerpt: Type.String(), patterns: Type.Array(Type.String()) })),
    }),
  ),
  summary: Type.String(),
  errors: Type.Optional(Type.Array(Type.Object({ pattern: Type.String(), error: Type.String() }))),
})

async function executeSearchDigest(
  params: Record<string, unknown>,
  ctx: ExtensionToolContext,
  signal?: AbortSignal,
): Promise<AgentToolResult<unknown>> {
  const patterns = Array.isArray(params.patterns)
    ? params.patterns.map((p) => String(p)).filter((p) => p.trim() !== "")
    : []
  if (patterns.length === 0) {
    const summary = "ctx_search_digest: no patterns given"
    return {
      content: [{ type: "text", text: summary }],
      details: { summary },
      isError: true,
      structuredContent: { patterns: [], files: [], summary, errors: [{ pattern: "", error: "patterns is empty" }] },
    }
  }
  const maxHits = clampInt(params.max_hits, HITS_DEFAULT, HITS_HARD)
  const root = optStr(params.path)
  const glob = optStr(params.glob)
  const includeHidden = params.include_hidden === true ? true : undefined
  const ignoreCase = params.ignore_case === true ? true : undefined
  const literal = params.literal === true ? true : undefined
  const context = typeof params.context === "number" && params.context > 0 ? Math.min(Math.floor(params.context), 5) : undefined

  const callArgs = (pattern: string, retry: boolean): Record<string, unknown> => ({
    action: "rg",
    pattern,
    ...(root ? { path: root } : {}),
    ...(glob ? { glob } : {}),
    limit: maxHits,
    // 隐藏目录需要 glob 才走得进（ctx_fs rg 的已知行为）：仅在重试时补 **。
    ...(retry ? { include_hidden: true, glob: "**" } : {}),
    ...(!retry && includeHidden ? { include_hidden: true } : {}),
    ...(ignoreCase ? { ignore_case: true } : {}),
    ...(literal ? { literal: true } : {}),
    ...(context ? { context } : {}),
  })

  const perPattern = await mapLimit(patterns, CONCURRENCY, async (pattern) => {
    const [res] = await runNested(ctx, [{ name: "ctx_fs", args: callArgs(pattern, false) }], signal)
    const first = res ?? { name: "ctx_fs", ok: false, isError: true, text: "", error: "no result" }
    let parse = parseRgOutput(first.text, maxHits)
    // 部分失败降级 1：给出 path 却零命中且未显式 glob 时，按隐藏目录重试一次
    if (root && parse.files.length === 0 && first.ok) {
      const [retry] = await runNested(ctx, [{ name: "ctx_fs", args: callArgs(pattern, true) }], signal)
      if (retry && retry.ok) {
        const second = parseRgOutput(retry.text, maxHits)
        if (second.files.length > 0) {
          parse = second
          parse.truncated = parse.truncated || second.truncated
        }
      }
    }
    return { pattern, outcome: first, parse }
  })

  const errors: Array<{ pattern: string; error: string }> = []
  for (const item of perPattern) {
    if (!item.outcome.ok) errors.push({ pattern: item.pattern, error: item.outcome.error ?? "rg failed" })
  }

  // 命中去重 + 按文件聚合：同一 file:line 被多个模式命中合并为一条
  interface AggHit {
    line: number
    excerpt: string
    patterns: string[]
  }
  const byPath = new Map<string, Map<string, AggHit>>()
  let totalHits = 0
  let reported = 0
  let anyTruncated = false
  for (const { pattern, parse } of perPattern) {
    reported += parse.reportedMatches ?? 0
    anyTruncated = anyTruncated || parse.truncated
    for (const file of parse.files) {
      let hits = byPath.get(file.path)
      if (!hits) {
        hits = new Map<string, AggHit>()
        byPath.set(file.path, hits)
      }
      for (const hit of file.hits) {
        const key = String(hit.line)
        const prev = hits.get(key)
        if (prev) {
          if (!prev.patterns.includes(pattern)) prev.patterns.push(pattern)
          if (prev.excerpt.length < hit.excerpt.length) prev.excerpt = hit.excerpt
          continue
        }
        hits.set(key, { line: hit.line, excerpt: hit.excerpt, patterns: [pattern] })
        totalHits++
      }
    }
  }

  const files = [...byPath.entries()]
    .map(([p, hits]) => ({
      path: p,
      hits: [...hits.values()]
        .sort((a, b) => a.line - b.line)
        .map((h) => ({ line: h.line, excerpt: h.excerpt, patterns: h.patterns })),
    }))
    .sort((a, b) => b.hits.length - a.hits.length || a.path.localeCompare(b.path))

  let summary =
    `patterns=${patterns.length} files=${files.length} hits=${totalHits}` +
    (reported ? ` (rg reported ${reported})` : "")
  if (errors.length > 0) summary += `; ${errors.length} pattern(s) degraded: ${errors.map((e) => e.pattern).join(", ")}`
  if (anyTruncated) summary += "; rg output truncated — narrow path/glob or page with offset"

  const text = [
    summary,
    ...files.slice(0, 40).flatMap((f) => [
      `${f.path} (${f.hits.length})`,
      ...f.hits.slice(0, 5).map((h) => `  ${h.line}: ${h.excerpt}  [${h.patterns.join(", ")}]`),
      f.hits.length > 5 ? `  ... (+${f.hits.length - 5} more hits)` : "",
    ]),
    files.length > 40 ? `... (+${files.length - 40} more files)` : "",
  ]
    .filter((l) => l !== "")
    .join("\n")

  return {
    content: [{ type: "text", text }],
    details: { summary },
    ...(errors.length === patterns.length ? { isError: true } : {}),
    structuredContent: {
      patterns,
      files,
      summary,
      ...(errors.length ? { errors } : {}),
    },
  }
}

// ---------------------------------------------------------------------------
// 治理钩子：codemode text() 回传硬顶
// ---------------------------------------------------------------------------

/**
 * 机制结论（已核对源码，不是推测）：
 *   - codemode 是内置扩展用 createCodemodeToolDefinition() 注册的独立工具：
 *     name="codemode"、exposure="model-only"、defaultActive=false，execute 由
 *     QuickJS 执行器承担。它**不**经过 registerTool 的二次包装口子（扩展无法包裹
 *     别人的工具定义；pi.registerToolRenderer 只影响渲染，不动发给模型的内容）。
 *   - 唯一能改别的工具回传内容的口子是 pi.on("tool_result")：见
 *     dist/core/agent-session.js `_afterToolCall` —— tool_result 处理器可返回
 *     {content, details, structuredContent, isError, usage}，多个处理器串行叠加，
 *     后者看到前者的改动。模型发出的 codemode 调用（无 parentToolCallId）同样过这条路径。
 *   - 执行器自身只有 `max_output_tokens`（默认 10000 tokens ≈ 40000 字符）的软截断，
 *     且可被脚本首行 // @options 调高，没有更低的地板。所以要压到 ~3000 字符只能靠钩子。
 *   - 关键坑：只替换 content 而不回传 structuredContent 会丢结构化内容；不回传
 *     details 会把 codemode 的 fullOutputPath / calls 记录抹掉。故这里四项全量回传。
 *     但被截断的模型看不到 details，故 details.fullOutputPath 存在时，警告 marker
 *     附上该路径，把截断后的恢复路径从「重跑脚本书写」降为一次「read」。
 *   - 嵌套调用（parentToolCallId 存在）的结果只喂给脚本：脚本靠 outputSchema 拿
 *     structuredContent 做判断，物理截断会直接毁数据。因此只治理顶层 codemode 回传。
 */

/** 治理阈值（字符）。 */
function textCap(): number {
  return Math.max(200, envInt("CTX_ORCH_TEXT_CAP", DEFAULT_TEXT_CAP))
}

function governEnabled(): boolean {
  return envFlag("CTX_ORCH_GOVERN", true)
}

interface AnyBlock {
  type?: string
  text?: string
  [key: string]: unknown
}

/** 给 codemode 回传加物理硬顶 + 警告。返回 undefined 表示「不改写」。 */
export function capCodemodeResult(event: {
  toolName: string
  parentToolCallId?: string
  content?: unknown
  details?: unknown
  structuredContent?: unknown
  isError?: boolean
  usage?: unknown
}): ToolResultEventResult | undefined {
  if (!governEnabled()) return undefined
  if (event.toolName !== "codemode") return undefined
  // 嵌套结果只给脚本用，不治理（见上方机制结论）。
  if (event.parentToolCallId) return undefined
  const blocks = Array.isArray(event.content) ? (event.content as AnyBlock[]) : []
  if (blocks.length === 0) return undefined

  let total = 0
  for (const block of blocks) if (block?.type === "text") total += String(block.text ?? "").length
  if (total <= GOVERN_MIN_TOTAL) return undefined

  const cap = textCap()
  if (total <= cap) return undefined

  // 预留警告位，保证截断后总量仍 ≤ cap。
  const budget = Math.max(0, cap - 180)
  const kept: AnyBlock[] = []
  let remaining = budget
  let dropped = 0
  for (const block of blocks) {
    if (block?.type !== "text") {
      kept.push(block)
      continue
    }
    const text = String(block.text ?? "")
    if (text.length <= remaining) {
      kept.push(block)
      remaining -= text.length
      continue
    }
    if (remaining > 0) {
      const cut = clampUtf16Index(text, remaining, true)
      kept.push({ ...block, type: "text", text: text.slice(0, cut) })
    }
    dropped += text.length - Math.max(0, remaining)
    remaining = 0
  }

  // details.fullOutputPath：codemode 执行器把完整输出落盘后的路径（存在时）。
  // 被截断的模型看不到 details，故把路径带进 marker：恢复路径从「重跑脚本书写」降为「read」。
  const fullOutputPath =
    event.details && typeof event.details === "object"
      ? (event.details as { fullOutputPath?: unknown }).fullOutputPath
      : undefined
  const hasFullOutputPath = typeof fullOutputPath === "string" && fullOutputPath.length > 0
  const marker =
    `[…ctx-orchestrate 治理] codemode 回传 ${total} 字符超过硬顶 ${cap}，已物理截断（丢弃约 ${dropped} 字符）。` +
    (hasFullOutputPath
      ? `完整输出已存至 ${fullOutputPath}，用 read 精确取片段（无需重跑）。`
      : `请在脚本内自己收敛：只回摘要/计数/关键路径，需要全文就写入文件后用 read 精确取片段。`)
  // 新建块而不是就地改：event.content 里的对象可能被宿主另作他用，不原地改他人数据。
  let lastTextIdx = -1
  for (let i = 0; i < kept.length; i++) if (kept[i]?.type === "text") lastTextIdx = i
  if (lastTextIdx >= 0) {
    const block = kept[lastTextIdx]
    kept[lastTextIdx] = { ...block, type: "text", text: `${String(block.text ?? "")}\n${marker}` }
  } else {
    kept.push({ type: "text", text: marker })
  }

  diagLog(`codemode result capped: total=${total} cap=${cap} dropped=${dropped} blocks=${kept.length}`)
  return {
    content: kept as unknown as ToolResultEventResult["content"],
    // 四项必须显式回传：丢 details 会没有 fullOutputPath；
    // 丢 structuredContent 会在「替换 content」语义下被判定为不再匹配而清空。
    details: event.details,
    structuredContent: event.structuredContent as ToolResultEventResult["structuredContent"],
    isError: event.isError,
    usage: event.usage as ToolResultEventResult["usage"],
  }
}

function installGovernanceHook(pi: ExtensionAPI): void {
  pi.on("tool_result", async (event) => {
    try {
      return capCodemodeResult(event as unknown as Parameters<typeof capCodemodeResult>[0])
    } catch (err) {
      // 钩子永不抛：抛错会被宿主记为处理器失败。
      diagLog(`tool_result hook failed (ignored): ${errMessage(err)}`)
      return undefined
    }
  })
}

// ---------------------------------------------------------------------------
// 扩展入口
// ---------------------------------------------------------------------------

export default function (pi: ExtensionAPI) {
  const namespace = {
    name: NAMESPACE_NAME,
    description:
      "ctxmode 组合工具：把 codegraph/ctx_run/ctx_fs 的多步高频流程压成一次调用，自带去重、聚合与失败降级。",
    instructions: [
      "这三个工具是组合工具：内部并发 codegraph/ctx_run/ctx_fs 并自行聚合，调用方只拿摘要与结构化结果（outputSchema → structuredContent）。",
      "中间过程克制：不要在脚本里 text()/console.log 大段原始输出；用返回值做判断即可。",
      "最终报告完整：结束脚本前给出结构化结论（计数 + 关键路径/用例名 + 一句下一步）。",
      "失败容忍：部分失败不进异常，而是落在 structuredContent.errors / .failures / isError；先看这些字段再决定重试或降级。",
      "codemode 回传有 ~3000 字符硬顶（治理钩子），超了会被物理截断并附警告。",
    ].join("\n"),
  }

  pi.registerTool({
    name: "ctx_symbol_read",
    label: "CTX Symbol Read",
    description:
      "Locate a symbol and expand its call graph in one call: codegraph node → concurrent callers/callees → dedupe → " +
      "{symbol, files:[{path,line,role}], summary}. depth=2 additionally resolves up to 6 callee definitions.",
    promptSnippet: "Symbol location + caller/callee aggregation in one call",
    promptGuidelines: [
      "Prefer ctx_symbol_read over separate codegraph node/callers/callees calls.",
      "It is read-only: pass symbol and let it expand; pass file/path only to pin an ambiguous symbol.",
    ],
    parameters: symbolReadSchema,
    outputSchema: symbolReadOutput,
    exposure: "codemode",
    namespace,
    annotations: { readOnlyHint: true },
    async execute(_id, params, signal, _onUpdate, ctx) {
      return executeSymbolRead(params as Record<string, unknown>, ctx, signal)
    },
  })

  pi.registerTool({
    name: "ctx_test_digest",
    label: "CTX Test Digest",
    description:
      "Run a test/build task through ctx_run run_task, then extract failing case names, error excerpts and pass/fail counts → " +
      "{kind,target,passed,failed,failures:[{name,excerpt}],summary}. Read-only about the workspace.",
    promptSnippet: "Test/build run condensed to pass-fail counts and failing cases",
    promptGuidelines: [
      "Use ctx_test_digest for go/npm/cargo/make test and build instead of reading raw run_task output.",
      "It mutates nothing: it only runs the task and summarizes stdout/stderr.",
    ],
    parameters: testDigestSchema,
    outputSchema: testDigestOutput,
    exposure: "codemode",
    namespace,
    annotations: { readOnlyHint: true },
    async execute(_id, params, signal, _onUpdate, ctx) {
      return executeTestDigest(params as Record<string, unknown>, ctx, signal)
    },
  })

  pi.registerTool({
    name: "ctx_search_digest",
    label: "CTX Search Digest",
    description:
      "Run several ctx_fs rg searches concurrently, dedupe hits and aggregate them per file → " +
      "{patterns, files:[{path,hits:[{line,excerpt,patterns}]}], summary}. Read-only.",
    promptSnippet: "Multi-pattern search aggregated per file",
    promptGuidelines: [
      "Use ctx_search_digest for several patterns at once instead of one rg call per pattern.",
      "Keep patterns concrete (identifiers, not '.*'); it returns excerpts, not file reads.",
    ],
    parameters: searchDigestSchema,
    outputSchema: searchDigestOutput,
    exposure: "codemode",
    namespace,
    annotations: { readOnlyHint: true },
    async execute(_id, params, signal, _onUpdate, ctx) {
      return executeSearchDigest(params as Record<string, unknown>, ctx, signal)
    },
  })

  installGovernanceHook(pi)

  diagLog(
    `registered ctx_symbol_read / ctx_test_digest / ctx_search_digest (exposure=codemode, namespace=${NAMESPACE_NAME})` +
      ` and codemode text cap governance (cap=${textCap()}, enabled=${governEnabled()})`,
  )
}
