// jev-gate.ts — Jev（TypeSafe System One 分类器）门禁与巡检工具
//
// 落点：把 5 类「答案空间可提前写死、高频、判断不能拖到几秒」的判定固化为一个直连工具
// `jev_check`；并把 windhub 侧的 Jev 注册成 pi 的分类器 provider，使 codemode 的
// `models.classify()` 也能直接调用（既有体系文档所述的正规通道）。
//
// 与既有扩展的关系：
//   - 不 spawn 任何进程、不注册 command、不动 settings.json；只做 registerProvider ×1
//     + registerTool ×1，与 ctx-orchestrate / ctxmode / codegraph-go 零耦合。
//   - 不嵌套调用任何 pi 工具（不用 ctx.executeTool），因此不产生 nestedCalls。
//
// 设计依据（已核对，2026-10-06）：
//   - ProviderConfig.classifiers?: Partial<Record<ClassifierApi, ProviderClassifier>>；
//     KnownClassifierApi = "typesafe-system-one" | "cloudflare-workers-ai-system-one" | "llama-cpp-classify"，
//     且 ClassifierApi = KnownClassifierApi | (string & {})。内置 typesafe transport 为
//     `url = new URL("systemone", `${model.baseUrl.replace(/\/+$/u,"")}/`)`、
//     `payload = (model, request) => ({ model: model.id, ...request })`，
//     故把 model 级 baseUrl 指向 windhub 即可复用内置实现，无需自写 classify。
//   - ProviderClassifierModelConfig 必填：type:"classifier" + id/name/input/cost/contextWindow。
//   - registerTool 字段名与本仓 ctx-orchestrate 一致；execute(_id, params, signal, _onUpdate, ctx)。
//   - 线上实测（windhub，2026-10-06）：type:"noul" → 200；"choice" → 200
//     （{choice,confidence,probabilities}）；"score" → 200（{score,confidence,legend,probabilities}）；
//     **type:"bool" → 400 upstream_error**（下游不接受该字面量）。本文件因此只发 noul/choice/score，
//     绝不发 bool；pi 内置 transport 对 bool 问题会自行改写为 noul，两条路径互不冲突。
//
// 判定纪律（写死在本文件内，不交给调用方）：
//   - 契约 lint：字段「存在」的阈值 0.80，低于阈值即列为缺失（不确定 → 视为缺失）。
//   - 门禁扫描：缺陷「存在」的阈值 0.50，高于阈值即判定命中（不确定 → 视为命中并阻断）。
//   - 回传分类：仅当「正常交付」概率 ≥ 0.95 才给出正常；异常标签需概率 ≥ 0.80；否则一律
//     返回「不确定 → 按异常处理」。宁留不误关。
//   - 结论一律标注为「辅助信号」：不得作为放行依据，也不构成关闭 pane 的依据。
//
// Jev 的能力边界（官方文档，勿越界使用）：英文为训练主语言（中文可用但需自评）；
// 不擅长计数、数学、精确数值与日期比较；状态里无关信息过多时准确率下降；
// 多层间接推理、双重否定、提示注入下更不稳定。故本工具只做「结构/分类」判定。
//
// 数据外发提示：本工具把 payload 文本发送至 windhub.cc。payload 只应是回传正文或报告本身，
// 不得包含凭据、密钥或未脱敏的环境信息。
//
// 环境变量：
//   WINDHUB_API_KEY       必填。凭据解析顺序：进程环境 → settings.json 的 env 块（见 resolveApiKey）。
//   WINDHUB_BASE_URL      默认 https://windhub.cc/v1
//   JEV_MODEL_ID          默认 jev-latest
//   JEV_MAX_STATE_CHARS   payload 字符上限（默认 24000；超出显式截断并在结果内标注）
//   JEV_TIMEOUT_MS        单次调用超时毫秒（默认 60000）

import type { AgentToolResult, ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"
import fs from "node:fs"
import net from "node:net"
import os from "node:os"
import path from "node:path"

// ---------------------------------------------------------------------------
// 常量与环境
// ---------------------------------------------------------------------------

const PROVIDER = "windhub"
const BASE_URL = (process.env.WINDHUB_BASE_URL || "https://windhub.cc/v1").replace(/\/+$/u, "")
const MODEL_ID = process.env.JEV_MODEL_ID || "jev-latest"
const KEY_ENV_NAME = "WINDHUB_API_KEY"

const MAX_STATE_CHARS = Number(process.env.JEV_MAX_STATE_CHARS || 24000)
const TIMEOUT_MS = Number(process.env.JEV_TIMEOUT_MS || 60000)

/**
 * 三档判定的阈值（2026-10-06 构造式校准结论）：
 * Jev 对「存在类」问句系统性欠自信——真值 true 的样本实测落在 0.26–0.53，
 * 因此单点阈值无法分开，改为只用两端、中间带一律报「不确定（需人工确认）」。
 *   字段：p < 0.35 缺失 ｜ 0.35–0.80 不确定 ｜ ≥ 0.80 存在
 *   门禁：p < 0.35 干净 ｜ 0.35–0.80 不确定 ｜ ≥ 0.80 命中
 * 缺陷方向问句实测最好（g1/g2/g3 准确率 1.00），故门禁判定阈值由 0.50 上调到 0.80，
 * 避免对干净交付误报（旧值下 g4 假阳性 0.73）。
 */
const FIELD_MISSING_MAX = 0.35
const FIELD_PRESENT_MIN = 0.8
const GATE_CLEAR_MAX = 0.35
const GATE_FLAG_MIN = 0.8
/** 回传分类：判为「正常交付」所需的最低概率（其余一律按异常处理）。 */
const TRIAGE_NORMAL_MIN = 0.95
/** 回传分类：给出具体异常标签所需的最低概率。 */
const TRIAGE_ANOMALY_MIN = 0.8

const TASKS = ["contract_lint", "gate_scan", "return_triage", "disagreement", "source_audit"] as const
type Task = (typeof TASKS)[number]

const ROLES = ["scout", "planner", "researcher", "worker", "verifier", "reviewer", "oracle"] as const
type Role = (typeof ROLES)[number]

/**
 * 解析凭据。两处来源，按序取第一个非空值：
 *   ① 进程环境 process.env.WINDHUB_API_KEY —— pi 侧 provider 密钥的正规来源
 *      （providers.md：「Set the variable before starting Pi」）；
 *   ② `${PI_CODING_AGENT_DIR:-~/.pi/agent}/settings.json` 的 `env` 块 —— pi 的 settings.env
 *      并不注入 pi 进程环境（environment-variables.md 只定义它服务于 shell 工具会话），
 *      故此处显式回落读取，使凭据仍只存于 0600 非版本控制文件里。
 * 两处都没有时返回 undefined，由调用方 fail-closed 报错，绝不静默降级。
 */
let keyLookupError: string | null = null

function resolveApiKey(): string | undefined {
  const fromEnv = process.env[KEY_ENV_NAME]
  if (fromEnv && fromEnv.trim()) return fromEnv.trim()
  const dir = process.env.PI_CODING_AGENT_DIR || path.join(os.homedir(), ".pi", "agent")
  const file = path.join(dir, "settings.json")
  try {
    const parsed = JSON.parse(fs.readFileSync(file, "utf8")) as { env?: Record<string, unknown> }
    const v = parsed?.env?.[KEY_ENV_NAME]
    if (typeof v === "string" && v.trim()) return v.trim()
    keyLookupError = `${KEY_ENV_NAME} is absent from ${file}`
    return undefined
  } catch (err) {
    keyLookupError = `cannot read ${file}: ${err instanceof Error ? err.message : String(err)}`
    return undefined
  }
}

// ---------------------------------------------------------------------------
// 问句表（instructions 用英文：Jev 以英文为主要训练语言；state 可为中文）
// ---------------------------------------------------------------------------

type Q = Record<string, Record<string, unknown>>

/** noul 问句：返回「命题为真」的概率。 */
function noul(instructions: string): Record<string, unknown> {
  return { type: "noul", instructions }
}

/** choice 问句：labels 为候选标签，criteria 说明每个标签的含义。 */
function choice(instructions: string, criteria: Record<string, string>): Record<string, unknown> {
  return { type: "choice", instructions, criteria }
}

/*
 * 问句表（唯一来源）。部署物是单文件拷贝（install.sh 只复制 <name>.ts），
 * 因此表必须 inline 在本文件里；离线校准脚本 calibrate/run.mjs 用标记从本文件抽取同一批表，
 * 并对键集合做强校验——抽取失败即报错，不会静默用错问句。
 */
/** 各角色的输出契约硬字段。键为字段名，值为英文断言。 */
const CONTRACT_FIELDS: Record<Role, Record<string, string>> = {
  scout: {
    has_locations: "Does the report give concrete file paths or symbol names WITH line numbers?",
    has_relationships: "Does it describe caller/callee or impact relationships between the pieces?",
    has_risk_clues: "Does it list observed risk clues (stated as facts, without severity claims)?",
    has_starting_point: "Does it name which file the downstream role should open first, and why?",
  },
  planner: {
    has_goal: "Does the plan state the goal in one sentence?",
    has_steps_with_targets: "Are the steps numbered and pointed at concrete files, functions or line numbers?",
    has_changed_files: "Does it list which files will be changed?",
    has_risks_and_rollback: "Does it list risk points together with a rollback strategy?",
    has_verify_commands: "Does it give the commands that will verify the change?",
  },
  researcher: {
    has_sources: "Does every conclusion carry the source URL it came from?",
    has_actionable_conclusion: "Does it give an actionable conclusion, rather than only a list of links?",
    marks_unverified: "Does it mark single-source claims as not cross-verified?",
  },
  worker: {
    has_completed_list: "Does the report list what was changed and why, item by item?",
    has_files_changed: "Does it list the paths of the files it changed?",
    has_baseline_statement: "Does it state what baseline was run before the change?",
    shows_verification_commands: "Does it show the exact build/test commands and their actual results?",
  },
  verifier: {
    has_environment: "Does the report name the isolated copy path it used and the baseline commit or hash?",
    has_commands: "Does it list the commands that were executed?",
    has_evidence_quadruple: "For each criterion, does it give the exit code, raw output, artifact hash and duration?",
    no_conclusion: "Does it avoid giving any conclusion, severity, recommendation or fix?",
  },
  reviewer: {
    has_verdict: "Does it state whether the change can be released?",
    has_findings_with_location: "Is every finding located by file and line?",
    has_severity_levels: "Are the findings classified into severity levels (fatal / should-fix / suggestion)?",
    has_concrete_advice: "Are the suggestions concrete and actionable rather than generic style remarks?",
  },
  oracle: {
    has_conclusion_first: "Does it lead with a confirm-or-refute conclusion together with its basis?",
    has_tiered_findings: "Are the findings tiered, each with file and line, a trigger condition and a counter-path?",
    has_counter_questions: "Does it leave the decision-critical questions open for the caller to answer?",
  },
}
/** 门禁缺陷扫描：问句为「缺陷存在」方向，概率越高越应阻断。 */
const GATE_QUESTIONS: Record<string, string> = {
  g1_evidence_missing_fields: "Does the verification evidence lack any of these: environment, executed commands, exit code, raw output, artifact hash, duration?",
  g1_criterion_not_executed: "Is any verification criterion classified as not-run (timeout or refusal) or evidence-incomplete, rather than actually executed and passing?",
  g2_unresolved_must_fix: "Is there any fatal or must-fix item that is NOT marked as fixed and re-verified?",
  g3_unresolved_fatal_doubt: "Is there any unresolved fatal objection or blind spot raised against this change?",
  g4_leftovers_unrecorded: "Based only on the follow-up list included in the text, is there any should-fix or observation item mentioned in the conclusions that is absent from that list and was not explicitly waived?",
}
/** 回传分类标签（与既有异常清单同构）。 */
const TRIAGE_CRITERIA: Record<string, string> = {
  normal_delivery: "Normal delivery: it states a completion conclusion and contains the required outputs.",
  provider_error: "Provider or upstream error: an error status, upstream failure or aborted run.",
  empty_return: "Empty or missing conclusion: no substantive body, or the required output is absent.",
  no_artifact_idle: "Idle with no artifact: text is present but the task goal was never reached.",
  truncated: "Visible truncation: the report breaks off and a required part is missing.",
  exec_timeout: "Execution timeout: it says a step could not run within the time limit.",
  refusal: "Refusal: the model or provider declined to perform the task.",
  evidence_incomplete: "Evidence incomplete: the verifier could not produce complete evidence.",
}

// ---------------------------------------------------------------------------
// 纯函数：判定与格式化
// ---------------------------------------------------------------------------

function oneLine(s: string, max = 300): string {
  const t = String(s ?? "").replace(/\s+/gu, " ").trim()
  return t.length > max ? `${t.slice(0, max)}…` : t
}

/** 取 answers 中某个问句的 bool 形态概率（noul → probability）。 */
function probOf(answers: Record<string, unknown>, id: string): number | null {
  const a = answers?.[id] as Record<string, unknown> | undefined
  if (!a) return null
  const p = a.probability ?? a.noul
  return typeof p === "number" && Number.isFinite(p) ? p : null
}

/** 取 choice 形态答案。 */
function choiceOf(answers: Record<string, unknown>, id: string): { label: string; p: number; probs: Record<string, number> } | null {
  const a = answers?.[id] as Record<string, unknown> | undefined
  if (!a) return null
  const label = typeof a.choice === "string" ? a.choice : null
  if (!label) return null
  const probs = (a.probabilities ?? {}) as Record<string, number>
  const p = typeof probs[label] === "number" ? probs[label] : Number(a.confidence ?? 0)
  return { label, p, probs }
}

interface Line {
  id: string
  p: number | null
}

function fmtProbs(lines: Line[]): string {
  return lines
    .map((l) => `${l.id}=${l.p === null ? "n/a" : l.p.toFixed(2)}`)
    .join(", ")
}

// ---------------------------------------------------------------------------
// HTTP：调用 System One（原生 {model,state,questions} → {answers}）
// ---------------------------------------------------------------------------

interface JevCall {
  answers: Record<string, unknown>
  usage?: { input?: number; output?: number }
  model?: string
  truncated?: { originalChars: number; usedChars: number }
}

async function callJev(payload: string, questions: Q, signal?: AbortSignal): Promise<JevCall> {
  const apiKey = resolveApiKey()
  if (!apiKey) {
    throw new Error(
      `missing ${KEY_ENV_NAME}: absent from the process environment; ${keyLookupError ?? "settings.json fallback unavailable"} (fail-closed, no fallback)`,
    )
  }

  let state = payload
  let truncated: JevCall["truncated"]
  if (state.length > MAX_STATE_CHARS) {
    truncated = { originalChars: state.length, usedChars: MAX_STATE_CHARS }
    state = `${state.slice(0, MAX_STATE_CHARS)}\n\n[TRUNCATED: payload was ${truncated.originalChars} characters, only the first ${MAX_STATE_CHARS} were sent]`
  }

  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(new Error(`timeout after ${TIMEOUT_MS}ms`)), TIMEOUT_MS)
  const onAbort = (): void => ctrl.abort(new Error("aborted by caller"))
  signal?.addEventListener("abort", onAbort, { once: true })

  try {
    const res = await fetch(`${BASE_URL}/systemone`, {
      method: "POST",
      headers: { authorization: `Bearer ${apiKey}`, "content-type": "application/json" },
      body: JSON.stringify({ model: MODEL_ID, state, questions }),
      signal: ctrl.signal,
    })
    const raw = await res.text()
    if (!res.ok) {
      throw new Error(`HTTP ${res.status} from ${BASE_URL}/systemone: ${oneLine(raw, 300)}`)
    }
    let body: Record<string, unknown>
    try {
      body = JSON.parse(raw) as Record<string, unknown>
    } catch {
      throw new Error(`non-JSON response from ${BASE_URL}/systemone: ${oneLine(raw, 200)}`)
    }
    const answers = (body.answers ?? {}) as Record<string, unknown>
    if (!answers || typeof answers !== "object" || Object.keys(answers).length === 0) {
      throw new Error(`response carried no answers: ${oneLine(raw, 200)}`)
    }
    const u = (body.usage ?? {}) as Record<string, unknown>
    return {
      answers,
      usage: { input: Number(u.input_tokens ?? 0) || 0, output: Number(u.output_tokens ?? 0) || 0 },
      model: typeof body.model === "string" ? body.model : undefined,
      truncated,
    }
  } finally {
    clearTimeout(timer)
    signal?.removeEventListener("abort", onAbort)
  }
}

// ---------------------------------------------------------------------------
// 各任务的问句构造与判定
// ---------------------------------------------------------------------------

const ADVISORY =
  "ADVISORY ONLY — this is a classifier signal, not a verdict. It never authorizes releasing a change " +
  "or closing a pane. The existing gate rules remain authoritative."

/** 来源核验问句。 */
const SOURCE_QUESTIONS: Record<string, string> = {
  all_claims_sourced: "Does every conclusion in the text carry a source of its own?",
  has_verifiable_urls: "Does the text contain at least one http:// or https:// link?",
  marks_unverified: "Does the text say that a claim came from a single source and was not cross-verified?",
}
/** 分歧提取：bool 问句。 */
const DISAGREEMENT_QUESTIONS: Record<string, string> = {
  has_material_disagreement: "Does the part after the dashes mention a finding that the part before the dashes does not mention, or contradict such a finding?",
  same_finding_set: "Do the part before the dashes and the part after the dashes discuss the same findings?",
}
/** 分歧提取：关系标签。 */
const DISAGREEMENT_RELATION: Record<string, string> = {
  agrees: "B agrees with A on the findings and their severity.",
  b_raises_new: "B raises findings or doubts that A did not mention.",
  b_refutes_a: "B refutes or contradicts a finding that A stated.",
  both_raise_different: "Both sides raise what the other did not mention.",
  b_defers: "B defers or leaves the question open instead of judging.",
}

function questionsFor(task: Task, role: Role | undefined): Q {
  switch (task) {
    case "contract_lint": {
      const fields = CONTRACT_FIELDS[role as Role]
      if (!fields) throw new Error(`contract_lint requires role in ${ROLES.join("|")}`)
      const q: Q = {}
      for (const [id, assertion] of Object.entries(fields)) q[id] = noul(assertion)
      return q
    }
    case "gate_scan": {
      const q: Q = {}
      for (const [id, assertion] of Object.entries(GATE_QUESTIONS)) q[id] = noul(assertion)
      return q
    }
    case "return_triage": {
      return {
        kind: choice("Classify this subagent return. Choose the single best label.", TRIAGE_CRITERIA),
        states_conclusion: noul(
          "Does the text state an explicit completion conclusion and contain the required outputs of its role?",
        ),
      }
    }
    case "disagreement": {
      const q: Q = {}
      for (const [id, assertion] of Object.entries(DISAGREEMENT_QUESTIONS)) q[id] = noul(assertion)
      q.relation = choice(
        "Compared with report A, what does report B do about the findings? Answer from the text only.",
        DISAGREEMENT_RELATION,
      )
      return q
    }
    case "source_audit": {
      const q: Q = {}
      for (const [id, assertion] of Object.entries(SOURCE_QUESTIONS)) q[id] = noul(assertion)
      return q
    }
    default:
      throw new Error(`unknown task ${String(task)}`)
  }
}

/** 三档切分：低于 lower 判「否」，高于 upper 判「是」，中间带单独列为「不确定」。 */
function triage3(lines: Line[], lower: number, upper: number) {
  return {
    yes: lines.filter((l) => l.p !== null && l.p >= upper).map((l) => l.id),
    mid: lines.filter((l) => l.p !== null && l.p >= lower && l.p < upper).map((l) => l.id),
    no: lines.filter((l) => l.p === null || l.p < lower).map((l) => l.id),
  }
}

function verdictFor(
  task: Task,
  answers: Record<string, unknown>,
): { lines: Line[]; verdict: string[]; note?: string } {
  switch (task) {
    case "contract_lint": {
      const lines: Line[] = Object.keys(answers).map((id) => ({ id, p: probOf(answers, id) }))
      const { yes, mid, no } = triage3(lines, FIELD_MISSING_MAX, FIELD_PRESENT_MIN)
      const verdict = [
        `contract: present ${yes.length}/${lines.length}${yes.length ? ` (${yes.join(", ")})` : ""}` +
          (no.length ? ` | MISSING(<${FIELD_MISSING_MAX}): ${no.join(", ")}` : "") +
          (mid.length ? ` | UNCERTAIN(${FIELD_MISSING_MAX}-${FIELD_PRESENT_MIN}, read the return yourself): ${mid.join(", ")}` : ""),
      ]
      return {
        lines,
        verdict,
        note: "三档依据 2026-10-06 构造式校准：中间带只报不确定，不当作存在。",
      }
    }
    case "gate_scan": {
      const lines: Line[] = Object.keys(GATE_QUESTIONS).map((id) => ({ id, p: probOf(answers, id) }))
      const { yes, mid } = triage3(lines, GATE_CLEAR_MAX, GATE_FLAG_MIN)
      const noAnswer = lines.filter((l) => l.p === null).map((l) => l.id)
      const verdict = [
        yes.length
          ? `gates FLAGGED(>=${GATE_FLAG_MIN}): ${yes.join(", ")}`
          : `gates: nothing above ${GATE_FLAG_MIN}`,
        ...(mid.length ? [`gates UNCERTAIN(${GATE_CLEAR_MAX}-${GATE_FLAG_MIN}, confirm manually): ${mid.join(", ")}`] : []),
        ...(noAnswer.length ? [`no answer for: ${noAnswer.join(", ")} (confirm manually)`] : []),
      ]
      return {
        lines,
        verdict,
        note: "单向信任：命中即可阻断；中间带仅提示人工确认，避免对干净交付误报。",
      }
    }
    case "return_triage": {
      const c = choiceOf(answers, "kind")
      const pConc = probOf(answers, "states_conclusion")
      const lines: Line[] = [
        { id: "states_conclusion", p: pConc },
        ...Object.entries(c?.probs ?? {}).map(([k, v]) => ({ id: `kind:${k}`, p: v })),
      ]
      let verdict: string[]
      if (!c) {
        verdict = ["triage: no answer → treat as anomaly (宁可误留)"]
      } else if (c.label === "normal_delivery") {
        verdict =
          c.p >= TRIAGE_NORMAL_MIN
            ? [`triage: normal_delivery (p=${c.p.toFixed(2)})`]
            : [`triage: UNCERTAIN (normal_delivery p=${c.p.toFixed(2)} < ${TRIAGE_NORMAL_MIN}) → treat as anomaly`]
      } else if (c.p >= TRIAGE_ANOMALY_MIN) {
        verdict = [`triage: ${c.label} (p=${c.p.toFixed(2)})`]
      } else {
        verdict = [`triage: UNCERTAIN (top=${c.label} p=${c.p.toFixed(2)} < ${TRIAGE_ANOMALY_MIN}) → treat as anomaly`]
      }
      return { lines, verdict, note: "异常方向可直接采信；「正常」方向不作为关闭 pane 的依据（判据仍是既有四要素）。" }
    }
    case "disagreement": {
      const rel = choiceOf(answers, "relation")
      const lines: Line[] = [
        { id: "has_material_disagreement", p: probOf(answers, "has_material_disagreement") },
        { id: "same_finding_set", p: probOf(answers, "same_finding_set") },
        ...Object.entries(rel?.probs ?? {}).map(([k, v]) => ({ id: `relation:${k}`, p: v })),
      ]
      const pMat = probOf(answers, "has_material_disagreement")
      const verdict = [
        rel ? `relation: ${rel.label} (p=${rel.p.toFixed(2)})` : "relation: no answer",
        pMat === null
          ? "material disagreement: no answer"
          : pMat >= GATE_FLAG_MIN
            ? `material disagreement: YES (p=${pMat.toFixed(2)})`
            : pMat >= GATE_CLEAR_MAX
              ? `material disagreement: UNCERTAIN (p=${pMat.toFixed(2)}) → read both reports`
              : `material disagreement: no (p=${pMat.toFixed(2)})`,
      ]
      return { lines, verdict }
    }
    case "source_audit": {
      const lines: Line[] = Object.keys(answers).map((id) => ({ id, p: probOf(answers, id) }))
      const { yes, mid, no } = triage3(lines, FIELD_MISSING_MAX, FIELD_PRESENT_MIN)
      const verdict = [
        `sourcing: ok ${yes.length}/${lines.length}` +
          (no.length ? ` | FLAGGED(<${FIELD_MISSING_MAX}): ${no.join(", ")}` : "") +
          (mid.length ? ` | UNCERTAIN(${FIELD_MISSING_MAX}-${FIELD_PRESENT_MIN}, verify yourself): ${mid.join(", ")}` : ""),
      ]
      return { lines, verdict }
    }
    default:
      throw new Error(`unknown task ${String(task)}`)
  }
}

// ---------------------------------------------------------------------------
// 工具执行
// ---------------------------------------------------------------------------

async function executeCheck(
  params: Record<string, unknown>,
  signal?: AbortSignal,
): Promise<AgentToolResult> {
  const task = params.task as Task
  const role = params.role as Role | undefined
  const payload = typeof params.payload === "string" ? params.payload : ""

  if (!TASKS.includes(task)) {
    const text = `jev_check: invalid task ${String(task)}; expected one of ${TASKS.join("|")}`
    return { content: [{ type: "text", text }], details: { summary: text }, isError: true, structuredContent: { error: text } }
  }
  if (!payload.trim()) {
    const text = "jev_check: payload is empty; nothing to judge"
    return { content: [{ type: "text", text }], details: { summary: text }, isError: true, structuredContent: { error: text } }
  }
  if (task === "contract_lint" && (!role || !ROLES.includes(role))) {
    const text = `jev_check: contract_lint requires role in ${ROLES.join("|")}`
    return { content: [{ type: "text", text }], details: { summary: text }, isError: true, structuredContent: { error: text } }
  }

  try {
    const questions = questionsFor(task, role)
    const call = await callJev(payload, questions, signal)
    const { lines, verdict, note } = verdictFor(task, call.answers)

    const head = [
      `task=${task}${role ? ` role=${role}` : ""} model=${call.model ?? MODEL_ID}`,
      `verdict: ${verdict.join(" | ")}`,
      ...(note ? [`note: ${note}`] : []),
      ...(call.truncated ? [`TRUNCATED: sent ${call.truncated.usedChars}/${call.truncated.originalChars} chars`] : []),
      `probs: ${fmtProbs(lines)}`,
      `usage: in=${call.usage?.input ?? 0} out=${call.usage?.output ?? 0}`,
      ADVISORY,
    ]
    const text = head.join("\n")

    return {
      content: [{ type: "text", text }],
      details: { summary: verdict[0] ?? task },
      structuredContent: {
        task,
        ...(role ? { role } : {}),
        model: call.model ?? MODEL_ID,
        verdict,
        probabilities: Object.fromEntries(lines.map((l) => [l.id, l.p])),
        ...(call.truncated ? { truncated: call.truncated } : {}),
        usage: call.usage ?? null,
        advisory: true,
      },
    }
  } catch (err) {
    const msg = oneLine(err instanceof Error ? err.message : String(err), 400)
    const text = `jev_check failed: ${msg}`
    return {
      content: [{ type: "text", text }],
      details: { summary: text },
      isError: true,
      structuredContent: { task, error: msg, advisory: true },
    }
  }
}

// ---------------------------------------------------------------------------
// 注册
// ---------------------------------------------------------------------------

/**
 * pi-ai 内置的分类器 provider（`providers/data/typesafe.json`：classifier jev-latest，
 * api typesafe-system-one）。它不在 models.json 里，扩展侧只能靠「覆盖注册」清空。
 */
const BUILTIN_CLASSIFIER_PROVIDER = "typesafe"

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

export default async function (pi: ExtensionAPI): Promise<void> {
  // 子代理会话不注册任何 Jev 能力（B1）：
  //   ① 不注册 windhub provider —— models.getModelsOfType("classifier") 里不再有 windhub/jev-latest，
  //      models.classify({provider:"windhub",id:"jev-latest"},…) 因 getModelOfType 返回 undefined
  //      而抛 Unknown classifier model；
  //   ② 不注册 jev_check 工具 —— 子代理不能给自己的回传背书（门禁信号只喂给主代理）；
  //   ③ 抹掉内置 typesafe 分类模型：ProviderConfig 契约「提供 models 即整体替换该 provider 的全部
  //      模型」，故 registerProvider(provider, { models: [] }) 即清空（实测 classifier 表 23 → 22，
  //      getModelOfType("classifier","typesafe","jev-latest") 由命中变为 undefined）。
  // 主代理会话（pane 已按 S1–S4 确证）走下面的原路径，行为逐字不变。
  if (await isSubagentSession()) {
    pi.registerProvider(BUILTIN_CLASSIFIER_PROVIDER, { models: [] })
    return
  }

  // 走同一解析函数，避免「只查进程 env」造成的误报（settings.json 里的凭据同样有效）。
  if (!resolveApiKey()) {
    // 注册照常进行（模型仍应可见），凭据缺失由工具在调用时 fail-closed 报错。
    console.error(
      `[jev-gate] ${KEY_ENV_NAME} could not be resolved (${keyLookupError ?? "reason unknown"})` +
        ` — jev_check will fail until it is configured`,
    )
  }

  pi.registerProvider(PROVIDER, {
    name: "WindHub",
    baseUrl: BASE_URL,
    apiKey: resolveApiKey() ?? `$${KEY_ENV_NAME}`,
    models: [
      {
        type: "classifier",
        id: MODEL_ID,
        name: "Jev (WindHub)",
        api: "typesafe-system-one",
        baseUrl: BASE_URL,
        input: ["text"],
        cost: { input: 0.042, output: 0, cacheRead: 0, cacheWrite: 0 },
        contextWindow: 64000,
      },
    ],
  })

  pi.registerTool({
    name: "jev_check",
    label: "JEV Check",
    description:
      "Jev（System One 分类器，非 LLM）做的 5 类结构化判定：契约 lint / 门禁扫描 / 回传分类 / 分歧提取 / 出处核验。" +
      " 一次调用并行回答固定问句，返回概率与写死阈值算出的结论。只做结构与分类判定，" +
      "不做计数、数值或日期比较，也不产出解释性文字。",
    promptSnippet: "Structured classifier checks (contract / gates / triage / disagreement / sourcing)",
    promptGuidelines: [
      "Use jev_check task=contract_lint role=<role> on a subagent return to detect missing required fields before acting on it.",
      "Use jev_check task=gate_scan before releasing a change: it flags G1 evidence gaps, G2 unresolved must-fix, G3 fatal doubts, G4 leftovers missing from the follow-up list. The payload MUST include the current follow-up/leftover list, otherwise G4 cannot be judged.",
      "Use jev_check task=return_triage to classify a return. Its output is advisory: the anomaly direction may be trusted, but a 'normal' label never authorizes closing a pane.",
      "Use jev_check task=disagreement with payload = report A, a line of three dashes, then report B, when reviewer and oracle differ.",
      "Use jev_check task=source_audit on researcher output to flag unsourced claims.",
      "Never use jev_check for counting, arithmetic, dates, exact comparisons, or security boundaries; never send credentials or unsanitized environment data as payload.",
    ],
    parameters: Type.Object({
      task: Type.String({
        description: TASKS.join("|"),
        enum: [...TASKS],
      }),
      payload: Type.String({
        description: "要判定的文本（回传正文/报告本体）。会外发到 windhub.cc；不得含凭据或未脱敏环境信息。",
      }),
      role: Type.Optional(
        Type.String({
          description: `task=contract_lint 时必填：${ROLES.join("|")}`,
          enum: [...ROLES],
        }),
      ),
    }),
    annotations: { readOnlyHint: true },
    async execute(_id, params, signal) {
      return executeCheck(params as Record<string, unknown>, signal)
    },
  })
}
