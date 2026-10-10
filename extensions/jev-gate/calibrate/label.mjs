#!/usr/bin/env node
/**
 * calibrate/label.mjs — 阶段 B 人工标注 + 生成校准样本。
 *
 * 工作流：
 *   node label.mjs list                    # 待标注清单（id / 角色 / 长度 / 预览）
 *   node label.mjs show <id> [--chars N]   # 看某条原文（默认 4000 字，--full 看全文）
 *   node label.mjs set <id> kind=<标签> [字段=present|absent ...]
 *   node label.mjs status                  # 进度
 *   node label.mjs merge                   # labels.json → fixtures-b.json（给 run.mjs 用）
 *
 * 标注口径：
 *   kind    —— 该回传属于哪一类（8 选 1，见 tables.mjs 的 TRIAGE_LABELS）
 *   契约字段 —— 只对该回传的**产生角色**所对应的字段标注，present/absent 二选一；
 *               拿不准就留空（留空不算错，只是不参与该字段的统计）。
 *   中文口径从严：只标你亲眼在原文里看到的。
 *
 * 文件（默认都在 ../../.agents/notes/jev-gate-phase-b/，gitignore，禁止入库）：
 *   candidates.json 由 extract-returns.mjs 产出
 *   labels.json     本脚本读写
 */
import fs from "node:fs"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { loadTables, TRIAGE_LABELS } from "./tables.mjs"

const HERE = path.dirname(fileURLToPath(import.meta.url))
const REPO = path.resolve(HERE, "..", "..", "..")
const arg = (n, d) => {
  const i = process.argv.indexOf(`--${n}`)
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : d
}
const OUTDIR = arg("out", path.join(REPO, ".agents", "notes", "jev-gate-phase-b"))
const CAND = path.join(OUTDIR, "candidates.json")
const LABELS = path.join(OUTDIR, "labels.json")

let T
try { T = loadTables() } catch (err) {
  console.error(`FATAL: ${err.message}`)
  process.exit(3)
}

const load = (p, dflt) => (fs.existsSync(p) ? JSON.parse(fs.readFileSync(p, "utf8")) : dflt)
const save = (p, obj) => fs.writeFileSync(p, JSON.stringify(obj, null, 2) + "\n", "utf8")

const cand = load(CAND, null)
if (!cand) {
  console.error(`FATAL: ${CAND} 不存在，先跑 node extract-returns.mjs`)
  process.exit(2)
}
let doc = load(LABELS, null)
if (!doc) {
  doc = {
    _doc: "阶段 B 人工标注。kind 为 8 选 1；契约字段 present/absent；拿不准留 null（不参与统计）。",
    createdAt: new Date().toISOString(),
    items: cand.items.map((it) => ({
      id: it.id, role: it.role, len: it.len, ts: it.ts, status: it.status,
      askPreview: it.askPreview, sourceFile: it.sourceFile,
      kind: null,
      contract: Object.fromEntries(Object.keys(T.contractFields[it.role] ?? {}).map((f) => [f, null])),
    })),
  }
  save(LABELS, doc)
  console.log(`initialized ${LABELS} with ${doc.items.length} items`)
}

// 位置参数解析：跳过取值型 flag 及其值（否则 `--out X status` 会把 cmd 读成 --out）
const VALUE_FLAGS = new Set(["out", "chars", "fixtures", "target", "min", "rpm", "per-role", "min-chars", "sessions", "include-prefix", "concurrency", "retry429"])
const positional = []
for (let i = 2; i < process.argv.length; i++) {
  const a = process.argv[i]
  if (a.startsWith("--")) {
    if (!VALUE_FLAGS.has(a.slice(2))) continue
    i++ // 跳过其值
    continue
  }
  positional.push(a)
}
const cmd = positional[0] ?? "status"
const preview = (s, n = 160) => String(s ?? "").replace(/\s+/gu, " ").slice(0, n)

if (cmd === "list") {
  const pending = doc.items.filter((i) => i.kind === null)
  console.log(`pending ${pending.length} / ${doc.items.length}`)
  for (const i of pending) {
    console.log(`\n[${i.id}] role=${i.role} len=${i.len} status=${i.status ?? "?"}`)
    console.log(`  ask: ${preview(i.askPreview, 140)}`)
    console.log(`  fields: ${Object.keys(i.contract).join(", ")}`)
  }
} else if (cmd === "show") {
  const id = positional[1]
  const it = cand.items.find((x) => x.id === id)
  if (!it) { console.error(`no such id: ${id}`); process.exit(2) }
  const full = process.argv.includes("--full")
  const chars = Number(arg("chars", "4000"))
  console.log(`[${it.id}] role=${it.role} len=${it.len} ts=${it.ts} status=${it.status}`)
  console.log(`ask: ${it.askPreview}`)
  console.log("--- text ---")
  console.log(full ? it.text : it.text.slice(0, chars) + (it.text.length > chars ? `\n…[${it.text.length - chars} more chars; --chars N or --full]` : ""))
} else if (cmd === "set") {
  const id = positional[1]
  const it = doc.items.find((x) => x.id === id)
  if (!it) { console.error(`no such id: ${id}`); process.exit(2) }
  const kv = positional.slice(2)
  if (!kv.length) { console.error("nothing to set; usage: set <id> kind=<label> field=present|absent …"); process.exit(2) }
  for (const pair of kv) {
    const [k, v] = pair.split("=")
    if (k === "kind") {
      if (!TRIAGE_LABELS.includes(v)) { console.error(`invalid kind ${v}; expected: ${TRIAGE_LABELS.join("|")}`); process.exit(2) }
      it.kind = v
    } else if (k in it.contract) {
      if (!["present", "absent"].includes(v)) { console.error(`invalid value ${v} for ${k}; use present|absent`); process.exit(2) }
      it.contract[k] = v === "present"
    } else {
      console.error(`unknown field ${k}; available: ${Object.keys(it.contract).join(", ")}`)
      process.exit(2)
    }
  }
  save(LABELS, doc)
  console.log(`updated ${id}: kind=${it.kind} contract=${JSON.stringify(it.contract)}`)
} else if (cmd === "review") {
  // 草稿审阅表：人工只做「确认/纠正」，不从头读长报告。--out 可写 markdown 文件。
  const L = []
  L.push(`# 阶段 B 草稿审阅表（模型草稿，未确认为真值）`)
  L.push(``)
  L.push(`- 生成：${new Date().toISOString()}`)
  L.push(`- 条目：${doc.items.length}（草稿 ${doc.items.filter((i) => i.draft && !i.draft.error).length} 条，失败 ${doc.items.filter((i) => i.draft?.error).length} 条）`)
  L.push(`- 确认方式：\`node label.mjs accept --all\` 全量确认；或 \`accept <id>\` 逐条；改错用 \`set <id> field=present|absent\``)
  L.push(`- 真值只认 kind/contract；draft 仅作草稿，merge 不读 draft。`)
  L.push(``)
  L.push(`| id | 角色 | 长度 | 草稿 kind | 依据 | 草稿字段 |`)
  L.push(`|---|---|---|---|---|---|`)
  for (const i of doc.items) {
    const d = i.draft ?? {}
    const conf = i.kind !== null ? "确认" : "待确认"
    const fields = Object.entries(d.contract ?? {})
      .map(([f, v]) => `${f}=${v === true ? "present" : v === false ? "absent" : "?"}`).join(" ")
    L.push(`| ${i.id} | ${i.role} | ${i.len} | ${d.error ? "（失败）" : d.kind ?? "—"} [${conf}] | ${d.error ? d.error.slice(0, 40) : (d.kindReason ?? "")} | ${fields || "—"} |`)
  }
  L.push(``)
  L.push(`## 逐条上下文（前 200 字任务原文）`)
  L.push(``)
  for (const i of doc.items) {
    L.push(`- **${i.id}**（${i.role}, ${i.len} 字）：${preview(i.askPreview, 200)}`)
  }
  const text = L.join("\n")
  const out = arg("out-file", "")
  if (out) { fs.writeFileSync(out, text + "\n", "utf8"); console.log(`wrote ${out}`) } else process.stdout.write(text + "\n")
} else if (cmd === "accept") {
  const all = process.argv.includes("--all")
  const targets = all ? doc.items.filter((i) => i.draft && !i.draft.error) : doc.items.filter((i) => i.id === positional[1])
  if (!targets.length) { console.error("nothing to accept; use accept <id> or accept --all"); process.exit(2) }
  let n = 0
  for (const i of targets) {
    const d = i.draft
    if (!d || d.error) continue
    if (TRIAGE_LABELS.includes(d.kind)) i.kind = d.kind
    for (const [f, v] of Object.entries(d.contract ?? {})) if (f in i.contract && v !== null) i.contract[f] = v
    i.acceptedFrom = d.by
    i.acceptedAt = new Date().toISOString()
    n++
  }
  save(LABELS, doc)
  console.log(`accepted ${n} item(s) from drafts`)
} else if (cmd === "status") {
  const done = doc.items.filter((i) => i.kind !== null).length
  const fields = doc.items.reduce((a, i) => a + Object.values(i.contract).filter((v) => v !== null).length, 0)
  const totalFields = doc.items.reduce((a, i) => a + Object.keys(i.contract).length, 0)
  console.log(`kind 已标 ${done}/${doc.items.length}；契约字段已标 ${fields}/${totalFields}`)
  const byKind = {}
  for (const i of doc.items) if (i.kind) byKind[i.kind] = (byKind[i.kind] ?? 0) + 1
  if (Object.keys(byKind).length) console.log("kind 分布:", byKind)
} else if (cmd === "merge") {
  const samples = []
  let skipped = 0
  for (const i of doc.items) {
    const candItem = cand.items.find((x) => x.id === i.id)
    if (!candItem || !i.kind) { skipped++; continue }
    const expect = { kind: i.kind }
    for (const [f, v] of Object.entries(i.contract)) if (v !== null) expect[f] = v
    // 契约与 triage 拆成两个样本：契约样本只带该角色字段，triage 样本带 kind
    const contractFields = Object.fromEntries(Object.entries(expect).filter(([k]) => k !== "kind" && k !== "states_conclusion"))
    if (Object.keys(contractFields).length) {
      samples.push({ id: `${i.id}-contract`, task: "contract_lint", role: i.role,
        payload: candItem.text, expect: contractFields })
    }
    samples.push({ id: `${i.id}-triage`, task: "return_triage", payload: candItem.text,
      expect: { kind: i.kind } })
  }
  const out = path.join(OUTDIR, "fixtures-b.json")
  save(out, { _doc: `阶段 B 样本（真实回传，人工标注）。generated ${new Date().toISOString()}；来源 ${LABELS}`,
    samples })
  console.log(`wrote ${out}: ${samples.length} samples (skipped ${skipped} unlabeled items)`)
  console.log(`run: node run.mjs --fixtures ${out} --rpm 15`)
} else {
  console.error(`unknown command ${cmd}; use list|show|set|status|merge`)
  process.exit(2)
}
