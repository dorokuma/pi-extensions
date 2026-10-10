#!/usr/bin/env node
/**
 * calibrate/prelabel.mjs — 阶段 B 草稿标注（给人工确认打底，**不产生真值**）。
 *
 * 机制：用观察员模型（默认 magpie 网关的 `group/glm-5.3`）对该条回传给出 kind + 契约字段的**草稿**，
 * 写入 labels.json 的 `draft` 字段。真值只认人工确认：`label.mjs accept <id>` 才会把草稿
 * 提升为 kind/contract，`merge` 也只读 kind/contract。因此模型输出永远不会直接进真值集。
 *
 * 问句与线上 Jev 完全同源（从 src/jev-gate.ts 的同一批断言里取），所以草稿与 Jev 判的是同一命题。
 *
 * 用法：
 *   node prelabel.mjs [--model group/glm-5.3] [--effort low] [--out <dir>] [--max-chars 12000] [--delay-ms 1200] [--only id1,id2]
 * 上游：magpie 网关（默认 `http://127.0.0.1:3425/v1`，OpenAI 兼容）。base url 与 apiKey 优先取
 * 环境变量 MAGPIE_BASE_URL / MAGPIE_API_KEY，其次回落 ~/.pi/agent/models.json 的 providers.magpie
 * （与 pi 同源）；key 一律不硬编码。本机网关不校验 Authorization，缺 key 时只告警并匿名发送。
 * 注意：上游是推理模型，推理与答案共享 max_tokens，不限制 reasoning_effort 时推理会吃光预算、
 * 返回 content=null（实测 group/glm-5.3 默认即如此）。故默认发 reasoning_effort=low（可用
 * --effort / PRELABEL_EFFORT 覆盖；与 pi 发往本网关的字段一致）。
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
const MODEL = arg("model", process.env.PRELABEL_MODEL || "group/glm-5.3")
const EFFORT = arg("effort", process.env.PRELABEL_EFFORT || "low") // 空则不发该字段
const MAX_CHARS = Number(arg("max-chars", process.env.JEV_MAX_STATE_CHARS || "24000"))
const DELAY = Number(arg("delay-ms", "1200"))
const ONLY = arg("only", "").split(",").filter(Boolean)

const TRIAGE_GLOSS = {
  normal_delivery: "正常交付：有明确完成结论且含该角色要求的产出",
  provider_error: "上游/provider 报错",
  empty_return: "空回传或缺结论",
  no_artifact_idle: "有文字但没产出（未触及任务目标）",
  truncated: "明显截断：中途断裂、缺关键结论",
  exec_timeout: "执行超时：明确说某步跑不完",
  refusal: "拒答",
  evidence_incomplete: "证据不完整",
}

let T
try { T = loadTables() } catch (err) { console.error(`FATAL: ${err.message}`); process.exit(3) }

const AGENT_DIR = process.env.PI_CODING_AGENT_DIR || path.join(os.homedir(), ".pi", "agent")
const MODELS_JSON = path.join(AGENT_DIR, "models.json")

/**
 * magpie 网关的 base url 与 key：优先环境变量，其次回落 pi 的 models.json 的 providers.magpie
 * （与 pi 同源）。key 不硬编码；本机网关不校验 Authorization，故缺 key 时只告警并匿名发送。
 */
function magpieConfig() {
  const envBase = process.env.MAGPIE_BASE_URL?.trim()
  const envKey = process.env.MAGPIE_API_KEY?.trim()
  if (envBase && envKey) return { base: envBase.replace(/\/+$/u, ""), key: envKey }
  let prov = {}
  try {
    prov = JSON.parse(fs.readFileSync(MODELS_JSON, "utf8"))?.providers?.magpie ?? {}
  } catch (err) {
    console.error(`FATAL: cannot read ${MODELS_JSON}: ${err.message}（可设 MAGPIE_BASE_URL/MAGPIE_API_KEY 绕过）`)
    process.exit(2)
  }
  const base = (envBase || prov.baseUrl || "http://127.0.0.1:3425/v1").replace(/\/+$/u, "")
  const key = envKey || (typeof prov.apiKey === "string" ? prov.apiKey.trim() : "")
  if (!key) console.error(`warn: magpie apiKey 未找到（env MAGPIE_API_KEY 或 ${MODELS_JSON} providers.magpie.apiKey）—— 不带 Authorization 发送`)
  return { base, key }
}
const { base: BASE, key: KEY } = magpieConfig()

function prompt(role, fields, text) {
  const fieldLines = Object.entries(fields).map(([id, a]) => `- ${id}: ${a}`).join("\n")
  const kindLines = TRIAGE_LABELS.map((k) => `- ${k} = ${TRIAGE_GLOSS[k] ?? ""}`).join("\n")
  return `你是严格的标注员。下面是一条子代理回传的原文，产生角色是 "${role}"。
只依据原文里**实际出现的内容**判断，不要按"应该会写"来推断；没有明确出现的一律判 absent。
若正文前出现【标注工具提示】，那说明**工具**裁掉了尾部，不得据此判 truncated；
只有回传自己写了未完成、中断、无法继续，才算 truncated。

【一】回传分类 kind（8 选 1）：
${kindLines}

【二】契约字段（只判断这些，逐项 present 或 absent）：
${fieldLines || "（本角色无契约字段，fields 输出空对象）"}

只输出严格 JSON，不要代码块、不要多余文字：
{"kind":"<8选1>","kindReason":"≤30字","fields":{"<字段id>":"present|absent"},"fieldReasons":{"<字段id>":"≤15字"}}

原文如下：
---
${text}
---`
}

function firstJson(s) {
  const i = s.indexOf("{")
  const j = s.lastIndexOf("}")
  if (i < 0 || j <= i) throw new Error("no JSON object in response")
  return JSON.parse(s.slice(i, j + 1))
}

const cand = JSON.parse(fs.readFileSync(CAND, "utf8"))
let doc
try { doc = JSON.parse(fs.readFileSync(LABELS, "utf8")) } catch {
  console.error(`FATAL: ${LABELS} 不存在，先跑 node label.mjs status 初始化`); process.exit(2)
}

let done = 0, failed = 0
for (const item of doc.items) {
  if (ONLY.length && !ONLY.includes(item.id)) continue
  const src = cand.items.find((x) => x.id === item.id)
  if (!src) { console.error(`skip ${item.id}: not in candidates`); continue }
  const fields = T.contractFields[item.role] ?? {}
  const over = src.text.length > MAX_CHARS
  const text = over
    ? `【标注工具提示】回传原文共 ${src.text.length} 字，下面只展示前 ${MAX_CHARS} 字；` +
      `**剩余部分是被标注工具裁掉的，不代表回传本身被截断**。只有原文自身写明"未完成/中断/无法继续"时，才判 truncated。\n` +
      src.text.slice(0, MAX_CHARS)
    : src.text
  try {
    const res = await fetch(`${BASE}/chat/completions`, {
      method: "POST",
      headers: { ...(KEY ? { authorization: `Bearer ${KEY}` } : {}), "content-type": "application/json" },
      body: JSON.stringify({
        model: MODEL,
        messages: [{ role: "user", content: prompt(item.role, fields, text) }],
        temperature: 0, max_tokens: 3000, stream: false,
        ...(EFFORT ? { reasoning_effort: EFFORT } : {}),
      }),
    })
    const raw = await res.text()
    if (!res.ok) throw new Error(`HTTP ${res.status}: ${raw.slice(0, 160)}`)
    const body = JSON.parse(raw)
    const choice = body?.choices?.[0]
    const content = choice?.message?.content ?? ""
    if (!content) throw new Error(`empty content (finish_reason=${choice?.finish_reason ?? "?"}, reasoning_effort=${EFFORT || "unset"})`)
    const j = firstJson(content)
    const contract = {}
    for (const f of Object.keys(fields)) {
      const v = j.fields?.[f]
      contract[f] = v === "present" ? true : v === "absent" ? false : null
    }
    item.draft = {
      by: MODEL,
      at: new Date().toISOString(),
      kind: TRIAGE_LABELS.includes(j.kind) ? j.kind : null,
      kindReason: String(j.kindReason ?? "").slice(0, 200),
      contract,
      fieldReasons: j.fieldReasons ?? {},
    }
    done++
    console.log(`[ok] ${item.id} (${item.role}) kind=${item.draft.kind} fields=${Object.values(contract).filter((v) => v === true).length}present/${Object.values(contract).filter((v) => v === false).length}absent`)
  } catch (err) {
    failed++
    item.draft = { by: MODEL, at: new Date().toISOString(), error: String(err.message).slice(0, 300) }
    console.log(`[fail] ${item.id}: ${String(err.message).slice(0, 120)}`)
  }
  fs.writeFileSync(LABELS, JSON.stringify(doc, null, 2) + "\n", "utf8")
  if (DELAY) await new Promise((r) => setTimeout(r, DELAY))
}
console.log(`\ndrafts written: ${done} ok, ${failed} failed → ${LABELS}`)
console.log(`下一步：node label.mjs list 看草稿，node label.mjs accept <id>（或 accept --all）确认`)
