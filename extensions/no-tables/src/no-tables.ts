// No-Tables Extension for Pi
// 1. Automatically converts any markdown table in assistant output to bullet lists.
// 2. Converts markdown formats unsupported by pi terminal (h3-h6 headings, images, footnotes).
// 3. Converts markdown links to plain text and bare URLs.
// Silent, zero-config, no user interaction needed.

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"

export default function (pi: ExtensionAPI) {
  pi.on("message_end", async (event) => {
    if (event.message.role !== "assistant") return

    const parts = event.message.content
    if (!parts) return

    let changed = false
    const newParts = parts.map((part: any) => {
      if (part.type !== "text") return part
      const converted = convertUnsupportedMarkdown(convertTables(part.text))
      if (converted !== part.text) {
        changed = true
        return { ...part, text: converted }
      }
      return part
    })

    if (!changed) return
    return { message: { ...event.message, content: newParts } }
  })
}

export function convertUnsupportedMarkdown(md: string): string {
  // Rule F runs BEFORE the split below.
  //
  // The split keys purely on where fence runs sit in the string, and the
  // even/odd alternation it produces ("even = prose, odd = code") is fixed at
  // that moment; rules A-E then rewrite only the prose half. F's whole job is to
  // normalize those fence positions first, so the text the split hands to A-E is
  // already the final one. Run it afterwards instead and the heading case breaks:
  // rule A rewrites "### T" to "**T**" and its trailing \s*$ swallows the newline
  // F inserted, so the fence lands glued to the bold text again ("**T**```") --
  // i.e. it re-creates the very defect F exists to remove.
  const normalized = normalizeMisplacedFences(md)

  // Split by fenced code blocks (``` or ~~~) so rules A-D only apply outside code blocks
  const segments = normalized.split(/(```[\s\S]*?```|~~~[\s\S]*?~~~)/g)
  for (let i = 0; i < segments.length; i += 2) {
    segments[i] = applyMarkdownRules(segments[i])
  }

  // Second F pass over the joined result. It exists for exactly one reason:
  // rule A's "**$2**" replacement can consume the newline that the first pass
  // inserted (a heading that is the last line of its prose segment ends up as
  // "**T**" immediately followed by the fence). Re-running F restores the
  // separation, and F is idempotent on already-clean text, so this is a no-op
  // for every input that does not hit that interaction.
  return normalizeMisplacedFences(segments.join(""))
}

// Rule F: repair fence runs (3+ backticks or 3+ tildes) that CommonMark does not
// recognize as fences because of where they sit.
//
// Models regularly glue the opening fence to the end of a prose line:
//
//   **三、为什么 `none` 没加成**```
//   $ magpie ...
//   ```
//
// CommonMark only opens a code block when the fence run stands on its own line,
// so here the glued run is not an opener and the *closing* fence becomes one
// instead, swallowing every following line into a single code block. Breaking
// the line in front of the fence restores the intended semantics without
// editing one byte of the code block's content.
//
// The pass walks the lines with fence state, so a fence-looking line that is
// really code content -- an indented ``` inside a ````-fenced block, or a glued
// fence quoted inside a code block -- is never touched.
export function normalizeMisplacedFences(md: string): string {
  const out: string[] = []
  let openFence: string | null = null

  for (const line of md.split("\n")) {
    if (openFence !== null) {
      // Inside a block: only a genuine closing fence may be dedented; everything
      // else is code content and is preserved byte-for-byte.
      const dedented = dedentFenceLine(line)
      const close = /^( {0,3})(`{3,}|~{3,})[ \t]*$/.exec(dedented)
      if (close && close[2][0] === openFence[0] && close[2].length >= openFence.length) {
        openFence = null
        out.push(dedented)
      } else {
        out.push(line)
      }
      continue
    }

    // F1: a fence run glued to the end of a prose line is moved onto its own
    // line. Two conditions keep this from firing on prose that merely *mentions*
    // a fence ("模型把 ``` 贴在文字后面。"):
    //   - the lookahead requires the run to be the last thing on the line apart
    //     from an optional language tag ("```bash"), and
    //   - the guard skips a run that has another fence run earlier on the same
    //     line, so only the first run of a line can be promoted.
    // Without both, promoting a mid-sentence mention to column 0 would turn that
    // mention into a real opener and create the very bug this rule removes.
    // A preceding ">" is excluded because a fence inside a blockquote is already
    // valid where it stands, and runs of 3+ are required so inline `code` and
    // ``code`` are never touched.
    let fenceLine = line
    const glued = /([^\n`~ \t>])[ \t]*(`{3,}|~{3,})(?=[ \t]*[A-Za-z0-9_+.\-]*[ \t]*(?:\r?\n|$))/.exec(line)
    if (glued && !/`{3,}|~{3,}/.test(line.slice(0, glued.index + glued[1].length))) {
      out.push(line.slice(0, glued.index + glued[1].length))
      fenceLine = line.slice(glued.index + glued[0].length - glued[2].length).replace(/^[ \t]+/, "")
    } else {
      // F2: an indented fence run is moved to column 0. CommonMark accepts at
      // most 3 spaces of indent (more is not a fence at all), while the split
      // above ignores indentation entirely, so normalizing to column 0 is what
      // makes the two agree.
      fenceLine = dedentFenceLine(line)
    }
    out.push(fenceLine)

    // Track whether this line opened a block, using the same 3+ run shape.
    const open = /^( {0,3})(`{3,}|~{3,})(.*)$/.exec(fenceLine)
    if (open && !(open[2][0] === "`" && open[3].includes("`"))) {
      openFence = open[2]
    }
  }

  return out.join("\n")
}

function dedentFenceLine(line: string): string {
  return /^[ \t]+(?=`{3,}|~{3,})/.test(line) ? line.replace(/^[ \t]+/, "") : line
}

function applyMarkdownRules(text: string): string {
  // Rule A: h3-h6 headers to bold line
  text = text.replace(/^(#{3,6})\s+(.+?)\s*#*\s*$/gm, "**$2**")

  // Rule D: Footnote definition lines
  text = text.replace(/^\[\^([^\]]+)\]:\s*(.+)$/gm, "**[$1]** $2")

  // Rule C: Footnote references (not at definition line start)
  text = text.replace(/\[\^([^\]]+)\]/g, "[$1]")

  // Rule B: Images to links (empty alt -> [image](url))
  text = text.replace(/!+\[(.*?)\]\((.*?)\)/g, (_match, alt: string, url: string) => {
    const label = alt.trim() ? alt : "image"
    return `[${label}](${url})`
  })

  // Rule E: Links to text + url
  text = text.replace(/\[(.*?)\]\((.*?)\)/g, (_match, label: string, url: string) => {
    const trimmedLabel = label.trim()
    const trimmedUrl = url.trim()
    if (trimmedLabel === trimmedUrl) {
      return trimmedUrl
    }
    if (!trimmedLabel) {
      return trimmedUrl
    }
    return `${label} ${url}`
  })

  return text
}

function convertTables(md: string): string {
  // Match markdown tables: header row + separator + data rows
  const tableRe = /^(\|.+\|)\n(\|[-| :]+\|)\n((?:\|.+\|\n?)+)/gm

  return md.replace(tableRe, (_match, headerLine, _sep, bodyBlock: string) => {
    const headers = splitCells(headerLine)
    const rows = bodyBlock.trim().split("\n").map(splitCells)

    const bullets: string[] = []
    for (const row of rows) {
      // Use first column as label, rest as details
      const label = row[0]?.trim() || ""
      const details: string[] = []
      for (let i = 1; i < headers.length && i < row.length; i++) {
        const h = headers[i]?.trim()
        const v = row[i]?.trim()
        if (v) details.push(h ? `${h}: ${v}` : v)
      }
      if (details.length > 0) {
        bullets.push(`- **${label}** — ${details.join(", ")}`)
      } else if (label) {
        bullets.push(`- ${label}`)
      }
    }
    return bullets.join("\n") + "\n"
  })
}

function splitCells(line: string): string[] {
  return line
    .replace(/^\|/, "")
    .replace(/\|$/, "")
    .split("|")
}
