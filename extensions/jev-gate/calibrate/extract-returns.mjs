#!/usr/bin/env node
/**
 * calibrate/extract-returns.mjs — 阶段 B 语料抽取：从真实会话里取子代理回传原文。
 *
 * 来源：`~/.pi/agent/sessions/**\/*.jsonl` 里的 `herdsman.agent_event` 记录
 *   - 回传原文：`data.compactHistory.lastAssistantMessage.text`
 *   - 产生角色：`data.compactHistory.historyRef.path` 里的 `role-<name>-<hash>`
 *   - 任务上下文：`data.compactHistory.lastUserMessage.text`（给标注者看"当时要求了什么"）
 *
 * 输出（默认落 `../../.agents/notes/jev-gate-phase-b/`，该目录已被 gitignore）：
 *   candidates.json — 抽样后的候选回传（含 ask 预览，供人工标注）
 * 注意：回传含真实工程内容，**不得入库**；脚本默认只写上述 gitignore 目录。
 *
 * 用法：
 *   node extract-returns.mjs [--per-role 6] [--min-chars 400] [--out <dir>]
 *                            [--sessions <dir>] [--include-prefix <session-substring>]
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
const PER_ROLE = Number(arg("per-role", "6"))
const MIN_CHARS = Number(arg("min-chars", "400"))
// 上限与扩展一致：超过 JEV_MAX_STATE_CHARS 的样本在生产里本来就会被截断，
// 拿它测 kind/契约会测出「工具截断」而不是模型能力，故直接排除。
const MAX_CHARS = Number(arg("max-chars", process.env.JEV_MAX_STATE_CHARS || "24000"))
const SESSIONS = arg("sessions", process.env.PI_SESSIONS_DIR || path.join(os.homedir(), ".pi", "agent", "sessions"))
const OUTDIR = arg("out", path.join(REPO, ".agents", "notes", "jev-gate-phase-b"))
const INCLUDE = arg("include-prefix", "")

function* walk(dir) {
  let ents = []
  try { ents = fs.readdirSync(dir, { withFileTypes: true }) } catch { return }
  for (const e of ents) {
    const p = path.join(dir, e.name)
    if (e.isDirectory()) yield* walk(p)
    else if (e.isFile() && e.name.endsWith(".jsonl")) yield p
  }
}

const seen = new Set()
const byRole = new Map()
let scannedFiles = 0, scannedLines = 0, events = 0, kept = 0, tooLong = 0

for (const file of walk(SESSIONS)) {
  if (INCLUDE && !file.includes(INCLUDE)) continue
  scannedFiles++
  let fh
  try { fh = fs.readFileSync(file, "utf8") } catch { continue }
  for (const line of fh.split("\n")) {
    if (!line.includes('"herdsman.agent_event"')) continue
    scannedLines++
    let rec
    try { rec = JSON.parse(line) } catch { continue }
    const d = rec?.data
    const lam = d?.compactHistory?.lastAssistantMessage
    const text = typeof lam?.text === "string" ? lam.text : ""
    if (!text || text.trim().length < MIN_CHARS) continue
    if (text.length > MAX_CHARS) { tooLong++; continue }
    const ref = d?.compactHistory?.historyRef?.path || lam?.ref || ""
    const ask = String(d?.compactHistory?.lastUserMessage?.text ?? "")
    // 角色判定优先用任务原文的「角色：xxx」字段（与派发协议同源），
    // 其次用 herdr role 会话路径 role-<name>-<hash>；两者都拿不到才记 unknown。
    const ROLES = ["scout", "planner", "researcher", "worker", "verifier", "reviewer", "oracle"]
    const mAsk = /角色[：:]\s*([A-Za-z]+)/.exec(ask)
    const mRef = /role-([a-z]+)-[0-9a-f]{4,}/.exec(ref)
    const role = (mAsk && ROLES.includes(mAsk[1].toLowerCase())) ? mAsk[1].toLowerCase()
      : (mRef && ROLES.includes(mRef[1])) ? mRef[1] : "unknown"
    const key = `${role}|${d?.agentId ?? "?"}|${lam?.timestamp ?? text.length}`
    if (seen.has(key)) continue
    seen.add(key)
    events++
    const item = {
      id: `${role}-${(d?.agentId ?? "x").slice(-6)}-${String(lam?.timestamp ?? "").slice(11, 19).replace(/:/g, "")}`,
      role,
      len: text.length,
      ts: lam?.timestamp ?? null,
      status: d?.status ?? null,
      agentId: d?.agentId ?? null,
      sourceFile: path.relative(SESSIONS, file),
      askPreview: ask.replace(/\s+/gu, " ").slice(0, 400),
      text,
    }
    if (!byRole.has(role)) byRole.set(role, [])
    byRole.get(role).push(item)
    kept++
  }
}

// 抽样：每角色取最近的 PER_ROLE 条，并保证长度分散（按长度分位取，避免全是大报告）
const picked = []
const perRoleInfo = {}
for (const [role, items] of [...byRole.entries()].sort()) {
  items.sort((a, b) => String(b.ts).localeCompare(String(a.ts)))
  const recent = items.slice(0, Math.max(PER_ROLE * 4, PER_ROLE))
  recent.sort((a, b) => a.len - b.len)
  const out = []
  if (recent.length <= PER_ROLE) out.push(...recent)
  else for (let i = 0; i < PER_ROLE; i++) out.push(recent[Math.floor((i * (recent.length - 1)) / (PER_ROLE - 1))])
  perRoleInfo[role] = { available: items.length, picked: out.length,
    lenMin: out.length ? Math.min(...out.map((o) => o.len)) : 0,
    lenMax: out.length ? Math.max(...out.map((o) => o.len)) : 0 }
  picked.push(...out)
}
picked.sort((a, b) => (a.role < b.role ? -1 : a.role > b.role ? 1 : b.len - a.len))

fs.mkdirSync(OUTDIR, { recursive: true })
const outFile = path.join(OUTDIR, "candidates.json")
fs.writeFileSync(outFile, JSON.stringify({
  _doc: "阶段 B 语料（真实回传）。含真实工程内容，gitignore 目录，禁止入库。",
  extractedAt: new Date().toISOString(),
  sessionsRoot: SESSIONS,
  stats: { scannedFiles, events, kept, picked: picked.length, perRoleInfo },
  items: picked,
}, null, 2) + "\n", "utf8")

console.log(`sessions scanned: ${scannedFiles}`)
console.log(`agent_event with usable return text: ${events} (kept ${kept}; excluded >${MAX_CHARS} chars: ${tooLong})`)
for (const [r, i] of Object.entries(perRoleInfo)) {
  console.log(`  ${r.padEnd(11)} available=${String(i.available).padStart(4)}  picked=${i.picked}  len ${i.lenMin}-${i.lenMax}`)
}
console.log(`\nwrote ${outFile} (${picked.length} items)`)
