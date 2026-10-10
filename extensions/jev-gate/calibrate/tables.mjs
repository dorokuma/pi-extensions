/**
 * calibrate/tables.mjs — 从扩展源码抽取问句表（唯一来源），并做键集合强校验。
 *
 * 为什么抽取而不是共享 JSON 文件：部署物是**单文件拷贝**（install.sh 只复制 <name>.ts），
 * 扩展不得依赖运行时附带文件。因此表 inline 在 src/jev-gate.ts 里，校准侧按标记抽取同一批表；
 * 抽取失败或键集合不符 → 抛错退出，绝不用「错问句」做校准。
 */
import fs from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"

const HERE = path.dirname(fileURLToPath(import.meta.url))
export const SRC_PATH = path.join(HERE, "..", "src", "jev-gate.ts")
const SRC_TS = fs.readFileSync(SRC_PATH, "utf8")

function tableFromTs(marker, label) {
  const i = SRC_TS.indexOf(marker)
  if (i < 0) throw new Error(`marker not found for ${label}`)
  const j = SRC_TS.indexOf("\n}\n", i)
  if (j < 0) throw new Error(`block end not found for ${label}`)
  const block = SRC_TS.slice(i, j + 2)
  let body = block.slice(block.indexOf("{") + 1, block.lastIndexOf("}"))
  body = body.replace(/(\n\s*)([A-Za-z_][A-Za-z0-9_]*):\s/g, '$1"$2": ')
  body = body.replace(/,(\s*[}\]]|\s*$)/g, "$1")
  try {
    return JSON.parse("{" + body + "}")
  } catch (err) {
    throw new Error(`cannot parse ${label}: ${err.message}`)
  }
}

export const EXPECTED = {
  roles: ["scout", "planner", "researcher", "worker", "verifier", "reviewer", "oracle"],
  gateQuestions: ["g1_evidence_missing_fields", "g1_criterion_not_executed", "g2_unresolved_must_fix",
    "g3_unresolved_fatal_doubt", "g4_leftovers_unrecorded"],
  triageCriteria: ["normal_delivery", "provider_error", "empty_return", "no_artifact_idle",
    "truncated", "exec_timeout", "refusal", "evidence_incomplete"],
  sourceAuditQuestions: ["all_claims_sourced", "has_verifiable_urls", "marks_unverified"],
  disagreementQuestions: ["has_material_disagreement", "same_finding_set"],
  disagreementRelation: ["agrees", "b_raises_new", "b_refutes_a", "both_raise_different", "b_defers"],
}
export const TRIAGE_LABELS = EXPECTED.triageCriteria

export function loadTables() {
  const T = {
    contractFields: tableFromTs("/** 各角色的输出契约硬字段", "contractFields"),
    gateQuestions: tableFromTs("/** 门禁缺陷扫描", "gateQuestions"),
    triageCriteria: tableFromTs("/** 回传分类标签", "triageCriteria"),
    sourceAuditQuestions: tableFromTs("/** 来源核验问句", "sourceAuditQuestions"),
    disagreementQuestions: tableFromTs("/** 分歧提取：bool 问句", "disagreementQuestions"),
    disagreementRelation: tableFromTs("/** 分歧提取：关系标签", "disagreementRelation"),
  }
  const diffs = []
  const cmp = (label, got, exp) => {
    const g = [...got].sort().join(","), w = [...exp].sort().join(",")
    if (g !== w) diffs.push(`${label}: got [${g}] want [${w}]`)
  }
  cmp("roles", Object.keys(T.contractFields), EXPECTED.roles)
  cmp("gateQuestions", Object.keys(T.gateQuestions), EXPECTED.gateQuestions)
  cmp("triageCriteria", Object.keys(T.triageCriteria), EXPECTED.triageCriteria)
  cmp("sourceAuditQuestions", Object.keys(T.sourceAuditQuestions), EXPECTED.sourceAuditQuestions)
  cmp("disagreementQuestions", Object.keys(T.disagreementQuestions), EXPECTED.disagreementQuestions)
  cmp("disagreementRelation", Object.keys(T.disagreementRelation), EXPECTED.disagreementRelation)
  for (const [r, f] of Object.entries(T.contractFields)) {
    if (!f || typeof f !== "object" || !Object.keys(f).length) diffs.push(`contractFields.${r}: empty`)
  }
  if (diffs.length) throw new Error("问句表校验失败，拒绝用错问句：\n  " + diffs.join("\n  "))
  return T
}
