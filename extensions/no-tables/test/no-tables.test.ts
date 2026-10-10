// Tests for the no-tables extension's markdown fixups.
//
// Focus: rule F (normalizeMisplacedFences) — a fence glued to the end of a prose
// line is moved onto its own line, so the renderer keeps treating it as a code
// block instead of swallowing every following line into one.
//
// Run: node --test extensions/no-tables/test/no-tables.test.ts
//
// Repo copy of /root/.pi/agent/extensions/__tests__/no-tables.test.ts (verbatim
// except the import path below): the deployment layout is flat
// (__tests__/ -> ../no-tables.ts), while this repo keeps the source under src/.
import { test } from "node:test"
import assert from "node:assert/strict"

import { convertUnsupportedMarkdown, normalizeMisplacedFences } from "../src/no-tables.ts"

// Verbatim shape of the assistant output that triggered this bug: the opening
// fence is glued to the bold heading, so CommonMark never sees an opener there
// and the *closing* fence becomes an opener instead, swallowing the rest.
const REPORT = [
  "**三、为什么 `none` 没加成**```",
  "$ magpie model efforts workbuddy-ai/hy4-preview-f none,high",
  'magpie: workbuddy-ai/hy4-preview-f has no reasoning level "none"   (exit 1)',
  "```",
].join("\n")

test("a: a fence glued after text moves to its own line, code bytes unchanged", () => {
  const out = normalizeMisplacedFences(REPORT)
  assert.equal(out, REPORT.replace("**```", "**\n```"))
  // the code block body is byte-for-byte identical
  assert.ok(out.includes("$ magpie model efforts workbuddy-ai/hy4-preview-f none,high"))
})

test("b: a fence already on its own line is returned unchanged", () => {
  const ok = "**标题**\n```bash\necho hi\n```\n\n正常正文\n"
  assert.equal(normalizeMisplacedFences(ok), ok)
})

test("c: tilde fences glued after text are handled the same way", () => {
  const glued = "正文~~~js\nlet a = 1\n~~~\n"
  assert.equal(normalizeMisplacedFences(glued), "正文\n~~~js\nlet a = 1\n~~~\n")
})

test("d: indented fences move to column 0 without touching code lines", () => {
  assert.equal(
    normalizeMisplacedFences("前言\n  ```bash\n    echo hi\n    ```\n"),
    "前言\n```bash\n    echo hi\n```\n",
  )
})

test("e: inline single and double backticks are never touched", () => {
  for (const s of ["用 `code` 是行内代码\n", "用 ``code`` 也是行内代码\n", "文字``code``文字\n"]) {
    assert.equal(normalizeMisplacedFences(s), s)
  }
})

test("f: a fence only *mentioned* mid-sentence is not promoted to a real opener", () => {
  // Promoting these to column 0 would turn a mention into a genuine fence and
  // swallow the following text — the very bug rule F removes.
  const mentions = [
    "明白，这是通用的渲染问题——模型把 ``` 贴在文字后面，切分就乱了。\n",
    "围栏 ``` 只有独占一行才算；一直到下一个 ``` 才闭合。\n",
    "put ``` at the start of a line instead\n",
    "a``` b```\n",
  ]
  for (const s of mentions) {
    assert.equal(normalizeMisplacedFences(s), s, `must not fire on: ${JSON.stringify(s)}`)
  }
})

test("g: a fence inside a blockquote is already valid and is left alone", () => {
  const bq = "> 命令：\n> ```\n> echo hi\n> ```\n"
  assert.equal(normalizeMisplacedFences(bq), bq)
})

test("idempotent: normalizing twice equals normalizing once", () => {
  const inputs = [
    REPORT,
    "正文~~~js\nlet a = 1\n~~~\n",
    "前言\n  ```bash\n    echo hi\n    ```\n",
    "**① 主效果**```text\nbody\n```\n",
    "文字```",
    "a```\nx\n```\nb```\ny\n```\n",
  ]
  for (const s of inputs) {
    const once = normalizeMisplacedFences(s)
    assert.equal(normalizeMisplacedFences(once), once, `not idempotent: ${JSON.stringify(s)}`)
  }
})

test("edge: unclosed fence at end of input, consecutive fences, and a language tag", () => {
  assert.equal(normalizeMisplacedFences("文字```"), "文字\n```")
  assert.equal(
    normalizeMisplacedFences("a```\nx\n```\nb```\ny\n```\n"),
    "a\n```\nx\n```\nb\n```\ny\n```\n",
  )
  assert.equal(
    normalizeMisplacedFences("文字```ts\nconst a = 1\n```\n"),
    "文字\n```ts\nconst a = 1\n```\n",
  )
})

test("edge: an indented fence that is code content inside a longer fence stays intact", () => {
  const nested = "说明：\n````markdown\n    ```\n    code\n    ```\n````\n"
  assert.equal(normalizeMisplacedFences(nested), nested)
})

test("h: a heading immediately followed by a fence is not re-glued by rule A", () => {
  // Rule A rewrites "### T" to "**T**" and its trailing \s*$ eats the newline
  // that F relies on, so without the second F pass the fence ends up glued to
  // the bold text again -- the exact defect F removes. (Pre-existing: the
  // original code produced "**T**```" here.)
  for (const md of ["### 三、结论\n```\n$ cmd\n```\n\n后续正文\n", "### 三、结论\n\n```\n$ cmd\n```\n\n后续正文\n", "#### 小标题\n```bash\necho hi\n```\n尾部\n"]) {
    const out = convertUnsupportedMarkdown(md)
    const glued = /[^\n`~ \t>][ \t]*(`{3,}|~{3,})(?=[ \t]*[A-Za-z0-9_+.\-]*[ \t]*(?:\r?\n|$))/
    assert.equal(glued.test(out), false, `still glued: ${JSON.stringify(out)}`)
    assert.ok(out.includes("**"), out)
  }
})

test("rules A-E still apply to prose that a glued fence used to swallow", () => {
  const md = ["**三、为什么 `none` 没加成**```", "$ magpie ...   (exit 1)", "```", "", "### 四、结论", "", "正常正文。"].join("\n")
  const out = convertUnsupportedMarkdown(md)
  assert.ok(out.startsWith("**三、为什么 `none` 没加成**\n```\n"), out)
  assert.ok(out.includes("**四、结论**"), out)
  assert.ok(out.endsWith("正常正文。"), out)
})
