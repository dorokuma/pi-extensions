package pivcc

import "strings"

// Ports src/core/format-recall.ts: path shortening and the recall output shapes.

// ShortPath is format-recall.ts shortPath(): paths inside the working directory
// become "./relative/path", deeper ones show their last three components.
//
// The TS original captures `process.cwd()` at module load; this port takes the
// working directory as an argument so a caller (and a test) controls it.
func ShortPath(fullPath, cwd string) string {
	normalized := strings.ReplaceAll(fullPath, "\\", "/")
	cwdNormalized := strings.ReplaceAll(cwd, "\\", "/")
	if strings.HasPrefix(normalized, cwdNormalized+"/") {
		return "." + normalized[len(cwdNormalized):]
	}
	parts := strings.Split(normalized, "/")
	if len(parts) > 3 {
		return ".../" + strings.Join(parts[len(parts)-3:], "/")
	}
	return normalized
}

// TouchedPageSize is format-recall.ts TOUCHED_PAGE_SIZE.
const TouchedPageSize = 5

// FormatTouchedOutput is format-recall.ts formatTouchedOutput().
func FormatTouchedOutput(touched []TouchedFile, page int, pageSize int, cwd string) string {
	if len(touched) == 0 {
		return "No file operations found in session history."
	}
	ps := pageSize
	if ps == 0 {
		ps = TouchedPageSize
	}
	totalPages := ceilDiv(len(touched), ps)
	currentPage := page
	if currentPage < 1 {
		currentPage = 1
	}
	start := (currentPage - 1) * ps
	if start > len(touched) {
		start = len(touched)
	}
	end := start + ps
	if end > len(touched) {
		end = len(touched)
	}
	pageFiles := touched[start:end]

	header := jsNumberString(float64(len(touched))) + " files touched"
	if totalPages > 1 {
		header = "Page " + jsNumberString(float64(currentPage)) + "/" + jsNumberString(float64(totalPages)) +
			" (" + jsNumberString(float64(len(touched))) + " total files)"
	}

	lines := make([]string, 0, len(pageFiles))
	for _, file := range pageFiles {
		displayPath := ShortPath(file.Path, cwd)
		indices := make([]string, 0, len(file.Entries))
		for _, entry := range file.Entries {
			indices = append(indices, "#"+jsNumberString(float64(entry.Index))+" ("+entry.ToolName+")")
		}
		lines = append(lines, "  "+displayPath+"    "+strings.Join(indices, ", "))
	}

	result := header + ":\n\n" + strings.Join(lines, "\n")
	if currentPage < totalPages {
		result += "\n\n--- Use page:" + jsNumberString(float64(currentPage+1)) + " for more results ---"
	}
	return result
}

// FormatRecallOutput is format-recall.ts formatRecallOutput().
func FormatRecallOutput(entries []SearchHit, query string, headerOverride string, hasQuery bool, hasHeader bool) string {
	if len(entries) == 0 {
		if hasQuery {
			return "No matches for \"" + query + "\" in session history."
		}
		return "No entries in session history."
	}

	var header string
	switch {
	case hasHeader:
		header = headerOverride + ":"
		if hasQuery {
			header = headerOverride + " for \"" + query + "\":"
		}
	case hasQuery:
		header = "Found " + jsNumberString(float64(len(entries))) + " matches for \"" + query + "\":"
	default:
		header = "Session history (" + jsNumberString(float64(len(entries))) + " entries):"
	}

	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		fileSuffix := ""
		if len(entry.Files) > 0 {
			fileSuffix = " files:[" + strings.Join(entry.Files, ", ") + "]"
		}
		body := entry.Summary
		if hasQuery && entry.HasSnippet {
			body = entry.Snippet
		}
		lines = append(lines, "#"+jsNumberString(float64(entry.Index))+" ["+entry.Role+"]"+fileSuffix+" "+body)
	}
	return header + "\n\n" + strings.Join(lines, "\n\n")
}

// ceilDiv is Math.ceil(a / b) for non-negative values.
func ceilDiv(a, b int) int {
	if b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}
