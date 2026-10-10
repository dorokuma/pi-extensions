#!/usr/bin/env node
/**
 * jev-gate 校准跑批（阶段 A：构造式校准）
 *
 * 读同一份问句表 src/questions.json（线上扩展用的就是它），对 calibrate/fixtures.json
 * 里的每个样本发一次 System One 请求，然后算：
 *   - bool 问句：各阈值下的准确率/精确率/召回率，以及「建议阈值」（取满足
 *     P(真值=true | p ≥ t) ≥ --target 的最小 t，且被预测为正的样本数 ≥ --min）
 *   - 可靠性曲线：把概率分桶，比较「平均预测概率」与「经验命中率」——用于判断
 *     概率能否当概率用（校准良好则两列接近；系统性偏高即过度自信）
 *   - choice 问句：argmax 准确率，以及要求 top 概率 ≥ 0.80 / 0.95 时的准确率与覆盖率
 *
 * 用法：
 *   node run.mjs [--fixtures <path>] [--out <path.md>] [--target 0.95] [--min 3] [--rpm 15]
 * 凭据：WINDHUB_API_KEY（进程环境）→ 回落 $PI_CODING_AGENT_DIR/settings.json 的 env 块。
 * 退出码：缺凭据 / 请求失败 / 样本缺失 → 非零（fail-fast，不静默）。
 */
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { loadTables } from "./tables.mjs"

const HERE = path.dirname(fileURLToPath(import.meta.url))
const SRC = path.join(HERE, "..", "src")
// —— 问句表：从扩展源码抽取（见 tables.mjs；键集合强校验，失败即 exit 3）——
let QUESTIONS
try {
  QUESTIONS = loadTables()
} catch (err) {
  console.error(`FATAL: ${err.message}`)
  process.exit(3)
}

const arg = (name, dflt) => {
  const i = process.argv.indexOf(`--${name}`)
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : dflt
}
const FIXTURES = JSON.parse(fs.readFileSync(arg("fixtures", path.join(HERE, "fixtures.json")), "utf8"))
const BASE_URL = (process.env.WINDHUB_BASE_URL || "https://windhub.cc/v1").replace(/\/+$/u, "")
const MODEL_ID = process.env.JEV_MODEL_ID || "jev-latest"
const TARGET = Number(arg("target", "0.95"))
const MINPOS = Number(arg("min", "3"))
const CONC = Number(arg("concurrency", "1"))
const OUT = arg("out", "")

function resolveKey() {
  if (process.env.WINDHUB_API_KEY?.trim()) return process.env.WINDHUB_API_KEY.trim()
  const dir = process.env.PI_CODING_AGENT_DIR || path.join(os.homedir(), ".pi", "agent")
  const f = path.join(dir, "settings.json")
  try {
    const v = JSON.parse(fs.readFileSync(f, "utf8"))?.env?.WINDHUB_API_KEY
    if (typeof v === "string" && v.trim()) return v.trim()
  } catch { /* fall through */ }
  return null
}
const KEY = resolveKey()
if (!KEY) {
  console.error(`FATAL: WINDHUB_API_KEY not found (env nor ${os.homedir()}/.pi/agent/settings.json)`)
  process.exit(2)
}

// —— 与扩展同构的问句构造（唯一差别：这里只读，不做阈值判定）——
const noul = (instructions) => ({ type: "noul", instructions })
function questionsFor(task, role) {
  if (task === "contract_lint") {
    const fields = QUESTIONS.contractFields[role]
    if (!fields) throw new Error(`contract_lint needs role in ${Object.keys(QUESTIONS.contractFields)}`)
    return Object.fromEntries(Object.entries(fields).map(([id, a]) => [id, noul(a)]))
  }
  if (task === "gate_scan") {
    return Object.fromEntries(Object.entries(QUESTIONS.gateQuestions).map(([id, a]) => [id, noul(a)]))
  }
  if (task === "return_triage") {
    return {
      kind: { type: "choice", instructions: "Classify this subagent return. Choose the single best label.", criteria: QUESTIONS.triageCriteria },
      states_conclusion: noul("Does the text state an explicit completion conclusion and contain the required outputs of its role?"),
    }
  }
  if (task === "disagreement") {
    const q = Object.fromEntries(Object.entries(QUESTIONS.disagreementQuestions).map(([id, a]) => [id, noul(a)]))
    q.relation = { type: "choice", instructions: "Compared with report A, what does report B do about the findings? Answer from the text only.", criteria: QUESTIONS.disagreementRelation }
    return q
  }
  if (task === "source_audit") {
    return Object.fromEntries(Object.entries(QUESTIONS.sourceAuditQuestions).map(([id, a]) => [id, noul(a)]))
  }
  throw new Error(`unknown task ${task}`)
}

// —— 节流：上游限流为「每分钟最多 20 次请求（含失败尝试）」，故默认按 15 rpm 放行 ——
const RPM = Number(arg("rpm", "15"))
const MIN_INTERVAL = Math.ceil(60000 / Math.max(1, RPM))
const RETRY_429 = Number(arg("retry429", "2"))
let nextSlot = 0
async function pace() {
  const now = Date.now()
  const wait = Math.max(0, nextSlot - now)
  nextSlot = Math.max(now, nextSlot) + MIN_INTERVAL
  if (wait > 0) await new Promise((r) => setTimeout(r, wait))
}

async function callOnce(payload, questions) {
  for (let attempt = 0; ; attempt++) {
    await pace()
    const res = await fetch(`${BASE_URL}/systemone`, {
      method: "POST",
      headers: { authorization: `Bearer ${KEY}`, "content-type": "application/json" },
      body: JSON.stringify({ model: MODEL_ID, state: payload, questions }),
    })
    const raw = await res.text()
    if (res.status === 429 && attempt < RETRY_429) {
      const waitMs = 62000
      process.stderr.write(`[429] rate limited; sleeping ${waitMs / 1000}s then retry (retry ${attempt + 1}/${RETRY_429})\n`)
      await new Promise((r) => setTimeout(r, waitMs))
      continue
    }
    if (!res.ok) throw new Error(`HTTP ${res.status}: ${raw.slice(0, 200)}`)
    const body = JSON.parse(raw)
    return { answers: body.answers ?? {}, model: body.model, usage: body.usage ?? {} }
  }
}

const samples = FIXTURES.samples
const results = []
let cursor = 0
async function worker() {
  while (cursor < samples.length) {
    const s = samples[cursor++]
    const q = questionsFor(s.task, s.role)
    try {
      const out = await callOnce(s.payload, q)
      results.push({ sample: s, answers: out.answers, model: out.model, usage: out.usage, error: null })
    } catch (err) {
      results.push({ sample: s, answers: {}, model: null, usage: {}, error: String(err?.message ?? err) })
    }
  }
}
await Promise.all(Array.from({ length: Math.max(1, CONC) }, worker))
results.sort((a, b) => (a.sample.id < b.sample.id ? -1 : 1))

const failures = results.filter((r) => r.error)
const usable = results.filter((r) => !r.error)
const judged = usable.filter((r) => !r.sample.ambiguous)

// —— 指标 ——
const boolStats = new Map() // id -> [{p, truth}]
const choiceStats = new Map() // id -> [{label, p, probs, truth}]
for (const r of judged) {
  for (const [id, truth] of Object.entries(r.sample.expect)) {
    const a = r.answers[id]
    if (a && typeof a.choice === "string") {
      const probs = a.probabilities ?? {}
      if (!choiceStats.has(id)) choiceStats.set(id, [])
      choiceStats.get(id).push({ label: a.choice, p: Number(probs[a.choice] ?? a.confidence ?? 0), probs, truth })
    } else if (a) {
      const p = a.probability ?? a.noul
      if (!boolStats.has(id)) boolStats.set(id, [])
      boolStats.get(id).push({ p: Number(p), truth: truth === true })
    }
  }
}

function boolTable(rows) {
  const out = []
  for (let t = 0.3; t <= 0.951; t += 0.05) {
    const T = Number(t.toFixed(2))
    let tp = 0, fp = 0, tn = 0, fn = 0
    for (const { p, truth } of rows) {
      const pred = p >= T
      if (pred && truth) tp++
      else if (pred && !truth) fp++
      else if (!pred && !truth) tn++
      else fn++
    }
    const prec = tp + fp ? tp / (tp + fp) : NaN
    const rec = tp + fn ? tp / (tp + fn) : NaN
    out.push({ T, tp, fp, tn, fn, prec, rec, acc: (tp + tn) / rows.length })
  }
  return out
}
function suggest(rows) {
  const table = boolTable(rows)
  const ok = table.filter((r) => Number.isFinite(r.prec) && r.prec >= TARGET && r.tp + r.fp >= MINPOS)
  if (!ok.length) return null
  return ok.reduce((a, b) => (b.T < a.T ? b : a)) // 最小的满足阈
}
function buckets(rows) {
  const edges = [0, 0.2, 0.4, 0.6, 0.8, 1.01]
  const out = []
  for (let i = 0; i < edges.length - 1; i++) {
    const rs = rows.filter((r) => r.p >= edges[i] && r.p < edges[i + 1])
    if (!rs.length) { out.push(null); continue }
    out.push({
      range: `${edges[i]}-${edges[i + 1] > 1 ? 1 : edges[i + 1]}`,
      n: rs.length,
      meanP: rs.reduce((a, r) => a + r.p, 0) / rs.length,
      emp: rs.filter((r) => r.truth).length / rs.length,
    })
  }
  return out
}

const f2 = (x) => (Number.isFinite(x) ? x.toFixed(2) : "—")
const L = []
const PHASE = arg("phase", path.basename(arg("fixtures", "fixtures.json")).includes("fixtures-b") ? "B" : "A")
L.push(`# jev-gate 校准报告（阶段 ${PHASE}：${PHASE === "B" ? "真实回传（人工标注）" : "构造式"}）`)
L.push(``)
L.push(`- 时间：${new Date().toISOString()}`)
L.push(`- 端点：\`${BASE_URL}/systemone\`　模型：\`${MODEL_ID}\`（返回 ${usable[0]?.model ?? "?"}）`)
L.push(`- 样本：${samples.length} 条（计入指标 ${judged.length} 条，含糊样本 ${usable.length - judged.length} 条，请求失败 ${failures.length} 条）`)
L.push(`- 建议阈值目标：精确率 ≥ ${TARGET}，且被预测为正的样本数 ≥ ${MINPOS}`)
L.push(`- 问句表来源：\`../src/jev-gate.ts\`（从与部署物同源的源码抽取，键集合已强校验）`)
L.push(``)
if (failures.length) {
  L.push(`## 请求失败`)
  for (const f of failures) L.push(`- ${f.sample.id}：${f.error}`)
  L.push(``)
}

L.push(`## bool 问句`)
L.push(``)
L.push(`| 问句 | n | 正例 | 准确率@0.5 | 建议阈值 | 该阈值精确率 | 该阈值召回 | 覆盖(正预测) |`)
L.push(`|---|---|---|---|---|---|---|---|`)
for (const [id, rows] of boolStats) {
  const b = buckets(rows)
  const s = suggest(rows)
  const at50 = boolTable(rows).find((r) => Math.abs(r.T - 0.5) < 1e-9)
  const nPos = rows.filter((r) => r.truth).length
  L.push(`| ${id} | ${rows.length} | ${nPos} | ${f2(at50?.acc)} | ${s ? s.T.toFixed(2) : "—"} | ${s ? f2(s.prec) : "—"} | ${s ? f2(s.rec) : "—"} | ${s ? s.tp + s.fp : "—"} |`)
  void b
}
L.push(``)

L.push(`## 可靠性曲线（概率能否当概率用）`)
L.push(``)
L.push(`| 问句 | 桶 | n | 平均预测概率 | 经验命中率 | 偏差 |`)
L.push(`|---|---|---|---|---|---|`)
for (const [id, rows] of boolStats) {
  for (const b of buckets(rows)) {
    if (!b) continue
    L.push(`| ${id} | ${b.range} | ${b.n} | ${b.meanP.toFixed(2)} | ${b.emp.toFixed(2)} | ${(b.meanP - b.emp >= 0 ? "+" : "")}${(b.meanP - b.emp).toFixed(2)} |`)
  }
}
L.push(``)

L.push(`## choice 问句`)
L.push(``)
L.push(`| 问句 | n | argmax 准确率 | top≥0.80 准确率(覆盖) | top≥0.95 准确率(覆盖) | 平均置信度 |`)
L.push(`|---|---|---|---|---|---|`)
for (const [id, rows] of choiceStats) {
  const acc = rows.filter((r) => r.label === r.truth).length / rows.length
  const cov = (th) => {
    const rs = rows.filter((r) => r.p >= th)
    return rs.length ? `${f2(rs.filter((r) => r.label === r.truth).length / rs.length)} (${rs.length}/${rows.length})` : "— (0)"
  }
  const mc = rows.reduce((a, r) => a + r.p, 0) / rows.length
  L.push(`| ${id} | ${rows.length} | ${f2(acc)} | ${cov(0.8)} | ${cov(0.95)} | ${mc.toFixed(2)} |`)
}
L.push(``)

L.push(`## 错判明细`)
L.push(``)
for (const r of judged) {
  for (const [id, truth] of Object.entries(r.sample.expect)) {
    const a = r.answers[id]
    if (!a) { L.push(`- ${r.sample.id}｜${id}：无答案`); continue }
    if (typeof a.choice === "string") {
      if (a.choice !== truth) L.push(`- ${r.sample.id}｜${id}：预测 ${a.choice}(p=${Number(a.probabilities?.[a.choice] ?? a.confidence ?? 0).toFixed(2)})，真值 ${truth}`)
    } else {
      const p = Number(a.probability ?? a.noul)
      const pred = p >= 0.5
      if (pred !== truth) L.push(`- ${r.sample.id}｜${id}：p=${p.toFixed(2)}（@0.5 判 ${pred}），真值 ${truth}`)
    }
  }
}
L.push(``)
const report = L.join("\n")

if (OUT) {
  fs.writeFileSync(OUT, report + "\n", "utf8")
  console.log(`report written: ${OUT}`)
} else {
  process.stdout.write(report)
}
const totalUsage = usable.reduce((a, r) => a + (r.usage?.input_tokens ?? 0), 0)
console.error(`samples=${samples.length} failed=${failures.length} input_tokens=${totalUsage}`)
if (failures.length) process.exit(1)
