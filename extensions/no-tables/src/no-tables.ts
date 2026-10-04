// No-Tables Extension for Pi
// Automatically converts any markdown table in assistant output to bullet lists.
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
      const converted = convertTables(part.text)
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
