#!/usr/bin/env node
/**
 * calibrate/observe-usage.mjs — jev_check 的**实际使用观测**（只读，零埋点）。
 *
 * 目的：回答「它到底有没有被用、有没有增量、有没有被误用」——
 * 这些问题离线校准答不了，只能从真实会话里读。
 *
 * 数据源：`~/.pi/agent/sessions/**\/*.jsonl`
 *   调用：assistant 消息的 content 块 `{type:"toolCall", name, arguments, id}`
 *   结果：`{role:"toolResult", toolName, toolCallId, isError, details, content[]}`
 *
 * 指标：
 *   ① 调用量（按 task / 按 contract_lint 的 role）与错误率
 *   ② 三档分布：契约字段 present/uncertain/missing、门禁 flagged/uncertain/clear
 *   ③ 概率直方图 + 按问句的「中间带(0.35–0.80)占比」→ 直接回答"有没有增量"
 *   ④ 调用耗时（toolResult.timestamp − toolCall 所在消息的时间）
 *   ⑤ 误用启发式：调用后同会话紧接着出现的工具名（含 pane/close/commit/git 的会标出来）
 *      —— 只列证据，不下结论
 *   ⑥ 调用率（近似）：分母用主代理收到的「子代理更新」条数（herdsman-wake-context 的
 *      "- completed ..." 条目数），分子是 jev_check 调用数
 *   ⑦ 成本：usage 行里的 in/out token，按 Jev 官方 $0.042/M 输入估算
 *
 * 用法：
 *   node observe-usage.mjs [--out report.md] [--tool jev_check] [--days 30] [--census]
 */
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"

const HERE = path.dirname(fileURLToPath(import.meta.url))
const REPO = path.resolve(HERE, "..", "..", "..")
const arg = (n, d) => {
  const i = process.argv.indexOf(`--${n}`)
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : d
}
const TOOL = arg("tool", "jev_check")
const OUT = arg("out", "")
const DAYS = Number(arg("days", "0")) // 0 = 全部
const CENSUS = process.argv.includes("--census")
const SESSIONS = arg("sessions", process.env.PI_SESSIONS_DIR || path.join(os.homedir(), ".pi", "agent", "sessions"))
const MID_LO = 0.35, MID_HI = 0.8
const PRICE_IN = 0.042 // USD / M input tokens（Jev 官方）

function* walk(dir) {
  let ents = []
  try { ents = fs.readdirSync(dir, { withFileTypes: true }) } catch { return }
  for (const e of ents) {
    const p = path.join(dir, e.name)
    if (e.isDirectory()) yield* walk(p)
    else if (e.isFile() && e.name.endsWith(".jsonl")) yield p
  }
}

const cutoff = DAYS > 0 ? Date.now() - DAYS * 86400e3 : 0
const calls = []
const toolCensus = new Map()
const wakeCounts = { completed: 0, files: 0 }
let scannedFiles = 0

for (const file of walk(SESSIONS)) {
  let text
  try { text = fs.readFileSync(file, "utf8") } catch { continue }
  if (cutoff) {
    let st
    try { st = fs.statSync(file) } catch { continue }
    if (st.mtimeMs < cutoff) continue
  }
  scannedFiles++
  const events = [] // {idx, kind, name, callId, args, text, ts, isError}
  let wakeFile = false
  for (const line of text.split("\n")) {
    if (!line) continue
    let rec
    try { rec = JSON.parse(line) } catch { continue }
    // 近似分母：主代理收到的子代理更新
    if (rec.type === "custom_message" && rec.customType === "herdsman-wake-context") {
      wakeFile = true
      const c = String(rec.content ?? "")
      wakeCounts.completed += (c.match(/- completed /g) ?? []).length
    }
    const m = rec.message
    if (!m) continue
    if (m.role === "assistant" && Array.isArray(m.content)) {
      for (const b of m.content) {
        if (b && b.type === "toolCall") {
          toolCensus.set(b.name, (toolCensus.get(b.name) ?? 0) + 1)
          const tsRaw = m.timestamp ?? rec.timestamp
          events.push({ idx: events.length, kind: "call", name: b.name, callId: b.id,
            args: b.arguments ?? {},
            ts: typeof tsRaw === "number" ? tsRaw : (Date.parse(String(tsRaw ?? "")) || null) })
        }
      }
    } else if (m.role === "toolResult") {
      toolCensus.set(m.toolName, (toolCensus.get(m.toolName) ?? 0) + 1)
      const txt = Array.isArray(m.content)
        ? m.content.filter((c) => c?.type === "text").map((c) => String(c.text ?? "")).join("\n")
        : String(m.content ?? "")
      events.push({ idx: events.length, kind: "result", name: m.toolName, callId: m.toolCallId,
        text: txt, isError: Boolean(m.isError), ts: Number(m.timestamp) || null })
    }
  }
  if (wakeFile) wakeCounts.files++

  const byId = new Map()
  for (const ev of events) if (ev.callId) byId.set(ev.callId, { ...(byId.get(ev.callId) ?? {}), ...ev })
  events.forEach((ev, i) => {
    if (ev.kind !== "call" || ev.name !== TOOL) return
    const res = byId.get(ev.callId) ?? {}
    const after = events.slice(i + 1, i + 7).filter((e) => e.kind === "call" && e.name !== TOOL).map((e) => e.name)
    calls.push({
      file: path.relative(SESSIONS, file),
      args: ev.args,
      ts: ev.ts,
      durMs: res.ts && ev.ts ? Math.max(0, res.ts - ev.ts) : null,
      isError: Boolean(res.isError),
      text: res.text ?? "",
      after,
    })
  })
}

// —— 解析结果文本 ——
const probsById = new Map()
const stat = {
  byTask: new Map(), byRole: new Map(),
  contract: { present: 0, uncertain: 0, missing: 0 },
  gate: { flagged: 0, uncertain: 0, clear: 0 },
  triage: new Map(), source: { ok: 0, uncertain: 0, flagged: 0 },
  disagreement: new Map(),
  errors: 0, truncated: [], totalIn: 0, totalOut: 0,
}
const bump = (m, k) => m.set(k, (m.get(k) ?? 0) + 1)
const listOf = (s) => (s ? s.split(",").map((x) => x.trim()).filter(Boolean) : [])

for (const c of calls) {
  const task = c.args?.task ?? "?"
  bump(stat.byTask, task)
  if (c.args?.role) bump(stat.byRole, c.args.role)
  if (c.isError) stat.errors++
  const t = c.text
  const pm = /usage: in=(\d+) out=(\d+)/.exec(t)
  if (pm) { stat.totalIn += Number(pm[1]); stat.totalOut += Number(pm[2]) }
  if (/^TRUNCATED:/m.test(t)) stat.truncated.push(c.args?.role ?? c.args?.task ?? "?")
  const probs = {}
  const pr = /probs: (.+)$/m.exec(t)
  if (pr) for (const kv of pr[1].split(",")) {
    const [k, v] = kv.split("=").map((s) => s.trim())
    if (k && v && v !== "n/a") { probs[k] = Number(v); const arr = probsById.get(k) ?? []; arr.push(Number(v)); probsById.set(k, arr) }
  }
  const verdict = (/verdict:\s*(.+)$/m.exec(t)?.[1] ?? "")
  if (task === "contract_lint") {
    const m = /present (\d+)\/(\d+)(?: \(([^)]*)\))?/.exec(verdict)
    stat.contract.present += listOf(m?.[3]).length
    stat.contract.missing += listOf(/MISSING\([^)]*\):\s*([^|]*)/.exec(verdict)?.[1]).length
    stat.contract.uncertain += listOf(/UNCERTAIN\([^)]*\)[^:]*:\s*([^|]*)/.exec(verdict)?.[1]).length
  } else if (task === "gate_scan") {
    stat.gate.flagged += listOf(/FLAGGED\([^)]*\):\s*([^|]*)/.exec(verdict)?.[1]).length
    stat.gate.uncertain += listOf(/UNCERTAIN\([^)]*\)[^:]*:\s*([^|]*)/.exec(verdict)?.[1]).length
    if (/nothing above/.test(verdict)) stat.gate.clear += 5 // 一次全清
  } else if (task === "source_audit") {
    const m = /ok (\d+)\/(\d+)/.exec(verdict)
    stat.source.ok += Number(m?.[1] ?? 0)
    stat.source.flagged += listOf(/FLAGGED\([^)]*\):\s*([^|]*)/.exec(verdict)?.[1]).length
    stat.source.uncertain += listOf(/UNCERTAIN\([^)]*\)[^:]*:\s*([^|]*)/.exec(verdict)?.[1]).length
  } else if (task === "return_triage") {
    const m = /triage:\s*([a-z_]+|UNCERTAIN)/.exec(verdict)
    bump(stat.triage, m?.[1] ?? "no_answer")
  } else if (task === "disagreement") {
    const m = /material disagreement:\s*(YES|no|UNCERTAIN|no answer)/.exec(verdict)
    bump(stat.disagreement, m?.[1] ?? "?")
  }
  Object.assign(c, { probs, task, verdict })
}

// —— 报告 ——
const L = []
const pct = (a, b) => (b ? `${((a / b) * 100).toFixed(0)}%` : "—")
L.push(`# jev_check 实际使用观测`)
L.push(``)
L.push(`- 生成：${new Date().toISOString()}　范围：${DAYS > 0 ? `近 ${DAYS} 天` : "全部"}　会话文件：${scannedFiles}`)
L.push(`- 工具：\`${TOOL}\`　调用次数：**${calls.length}**　执行错误：${stat.errors}`)
L.push(``)
if (!calls.length) {
  L.push(`> 还没有调用记录。注意：jev_check 是**主代理专用**（派发会话已排除），`)
  L.push(`> 所以记录只应出现在主代理会话里；先正常干活若干次再看。`)
} else {
  L.push(`## ① 调用量与分布`)
  L.push(``)
  L.push(`- 按 task：${[...stat.byTask].map(([k, v]) => `${k}=${v}`).join("　")}`)
  if (stat.byRole.size) L.push(`- contract_lint 按角色：${[...stat.byRole].map(([k, v]) => `${k}=${v}`).join("　")}`)
  const durs = calls.map((c) => c.durMs).filter((d) => d !== null).sort((a, b) => a - b)
  if (durs.length) {
    L.push(`- 调用→结果间隔中位：${(durs[Math.floor(durs.length / 2)] / 1000).toFixed(1)}s（n=${durs.length}）`)
    L.push(`  ⚠ 这不是纯工具耗时：该间隔从 assistant 消息时间戳算到 toolResult，中间含本回合剩余生成时间。`)
    L.push(`  实测直连 windhub 单次调用 ≈ **1.6s**（2026-10-06，3 次取中位：1.58s；样本 1.28–1.73s）。`)
  }
  L.push(`- 成本：输入 ${stat.totalIn} tok / 输出 ${stat.totalOut} tok ≈ **$${((stat.totalIn / 1e6) * PRICE_IN).toFixed(4)}**`)
  if (stat.truncated.length) L.push(`- ⚠ payload 被扩展截断：${stat.truncated.length} 次`)
  L.push(``)

  L.push(`## ② 三档分布（是否有增量）`)
  L.push(``)
  const cs = stat.contract, g = stat.gate, so = stat.source
  const cTot = cs.present + cs.uncertain + cs.missing
  if (cTot) L.push(`- 契约字段：present ${cs.present} (${pct(cs.present, cTot)})｜**uncertain ${cs.uncertain} (${pct(cs.uncertain, cTot)})**｜missing ${cs.missing} (${pct(cs.missing, cTot)})`)
  const gTot = g.flagged + g.uncertain + g.clear
  if (gTot) L.push(`- 门禁：flagged ${g.flagged}｜**uncertain ${g.uncertain}**｜clear ${g.clear}`)
  if (so.ok + so.uncertain + so.flagged) L.push(`- 出处核验：ok ${so.ok}｜uncertain ${so.uncertain}｜flagged ${so.flagged}`)
  if (stat.triage.size) L.push(`- 回传分类：${[...stat.triage].map(([k, v]) => `${k}=${v}`).join("　")}`)
  if (stat.disagreement.size) L.push(`- 分歧：${[...stat.disagreement].map(([k, v]) => `${k}=${v}`).join("　")}`)
  L.push(``)
  L.push(`> 判据（此前定下的验收线）：被标记（uncertain + 命中异常）占比 ≥ 20% 才算有增量；`)
  L.push(`> 若 uncertain 占比过高（比如 > 60%），说明它在你的回传上给不出判断，应收窄用途或调阈值。`)
  L.push(``)

  L.push(`## ③ 按问句：中间带占比（问句是否有区分度）`)
  L.push(``)
  L.push(`| 问句 | n | 均值 | 中间带(${MID_LO}–${MID_HI})占比 | 低(<${MID_LO}) | 高(≥${MID_HI}) |`)
  L.push(`|---|---|---|---|---|---|`)
  for (const [id, arr] of [...probsById].sort((a, b) => b[1].length - a[1].length)) {
    const n = arr.length
    const mid = arr.filter((v) => v >= MID_LO && v < MID_HI).length
    const lo = arr.filter((v) => v < MID_LO).length
    const hi = arr.filter((v) => v >= MID_HI).length
    L.push(`| ${id} | ${n} | ${(arr.reduce((a, b) => a + b, 0) / n).toFixed(2)} | ${pct(mid, n)} | ${pct(lo, n)} | ${pct(hi, n)} |`)
  }
  L.push(``)

  L.push(`## ④ 误用检查（启发式，只列证据不下结论）`)
  L.push(``)
  const risky = calls.filter((c) => c.after.some((n) => /pane|close|commit|git/i.test(n)))
  L.push(`- 调用后 6 条消息内出现 pane/close/commit/git 类工具的调用：**${risky.length}** 次`)
  for (const c of risky.slice(0, 10)) {
    L.push(`  - ${new Date(c.ts ?? 0).toISOString()}｜${c.task}${c.args.role ? `/${c.args.role}` : ""}｜后续工具：${c.after.join(", ")}`)
  }
  if (!risky.length) L.push(`  （无 —— 这是期望结果；规则要求「正常」标签不得作为放行或关闭 pane 的依据）`)
  L.push(``)

  L.push(`## ⑤ 调用率（近似）`)
  L.push(``)
  const denom = wakeCounts.completed
  L.push(`- 主代理收到的子代理更新条目：${denom}（来自 ${wakeCounts.files} 个会话文件的 herdsman-wake-context）`)
  L.push(`- jev_check 调用：${calls.length}　→ 近似调用率 **${pct(calls.length, denom)}**`)
  L.push(`- 说明：分母是"收到的更新条数"而非"独立任务数"，一次任务可能有多条更新；因此这是**上限近似**，`)
  L.push(`  真实调用率不低于此值。目标线：≥ 50%。`)
  L.push(``)

  L.push(`## ⑥ 明细（最近 10 次）`)
  L.push(``)
  for (const c of calls.slice(-10)) {
    L.push(`- ${c.ts ? new Date(c.ts).toISOString() : "?"}｜task=${c.task}${c.args.role ? ` role=${c.args.role}` : ""}｜payload ${String(c.args.payload ?? "").length} 字｜${c.isError ? "**错误**" : "ok"}｜${c.durMs !== null ? (c.durMs / 1000).toFixed(1) + "s" : "?"}｜${(c.verdict ?? "").slice(0, 120)}`)
  }
  L.push(``)
}

if (CENSUS) {
  L.push(`## 附：会话里出现过的工具名普查（验证解析器）`)
  L.push(``)
  for (const [n, c] of [...toolCensus].sort((a, b) => b[1] - a[1]).slice(0, 25)) L.push(`- ${n}: ${c}`)
  L.push(``)
}

const report = L.join("\n")
if (OUT) { fs.writeFileSync(OUT, report, "utf8"); console.log(`wrote ${OUT}`) }
else process.stdout.write(report)
console.error(`calls=${calls.length} errors=${stat.errors} files=${scannedFiles}`)
