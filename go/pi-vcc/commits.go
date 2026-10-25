package pivcc

import "strings"

// Ports src/extract/commits.ts: git commits from `bash` tool calls, paired with
// the hash the following tool result printed.

// commitMsgRe is commits.ts COMMIT_MSG_RE.
var commitMsgRe = mustCompile(`git` + jsSpace + `+commit[^\n]*?-m` + jsSpace + `+(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'|\$?'((?:[^'\\]|\\.)*)')`)

// hashRe is commits.ts HASH_RE.
var hashRe = mustCompile(`\b([0-9a-f]{7,12})\b`)

// heredocCommitCmdRe is commits.ts HEREDOC_COMMIT_CMD_RE.
var heredocCommitCmdRe = mustCompile(`^git` + jsSpace + `+commit[^\n"'&;|]*?-m` + jsSpace +
	`+"\$\(cat` + jsSpace + `+<<-?` + jsSpace + `*(?:'([^']+)'|"([^"]+)"|([A-Za-z_]\w*))(?:` + jsSpace +
	`*\n|\\n)`)

// CommitInfo is commits.ts CommitInfo.
type CommitInfo struct {
	Hash    string
	HasHash bool
	Message string
}

// cleanMessage is commits.ts cleanMessage().
func cleanMessage(message string) string {
	message = strings.ReplaceAll(message, `\"`, `"`)
	message = strings.ReplaceAll(message, `\'`, `'`)
	return jsTrimSpace(message)
}

// commitLinesOf is commits.ts linesOf(): split on a literal "\n" escape or a
// real newline.
func commitLinesOf(text string) []string {
	var out []string
	for _, chunk := range strings.Split(text, `\n`) {
		out = append(out, strings.Split(chunk, "\n")...)
	}
	return out
}

// firstLineOf is commits.ts firstLineOf().
func firstLineOf(text string) string {
	lines := commitLinesOf(text)
	first := ""
	if len(lines) > 0 {
		first = lines[0]
	}
	return jsTrimSpace(first)
}

// firstHeredocBodyLine is commits.ts firstHeredocBodyLine().
func firstHeredocBodyLine(rest, delimiter string) string {
	for _, line := range commitLinesOf(rest) {
		trimmed := jsTrimSpace(stripLeadingTabs(line))
		if trimmed == delimiter {
			break
		}
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// stripLeadingTabs is the `^\t+` shift the heredoc body line takes.
func stripLeadingTabs(line string) string {
	i := 0
	for i < len(line) && line[i] == '\t' {
		i++
	}
	return line[i:]
}

// subjectFromHeredocCmd is commits.ts subjectFromHeredocCmd().
func subjectFromHeredocCmd(command string) (string, bool) {
	loc := heredocCommitCmdRe.FindStringSubmatchIndex(command)
	if loc == nil {
		return "", false
	}
	delimiter := ""
	for group := 1; group <= 3; group++ {
		if loc[2*group] >= 0 {
			delimiter = command[loc[2*group]:loc[2*group+1]]
			break
		}
	}
	return cleanMessage(firstHeredocBodyLine(command[loc[1]:], delimiter)), true
}

// heredocOpenerRe is commits.ts's `/^\$\(cat\s+<</`.
var heredocOpenerRe = mustCompile(`^\$\(cat` + jsSpace + `+<<`)

// bracketHashRe is commits.ts's `/\[\S+\s+([0-9a-f]{7,12})\]/`.
var bracketHashRe = mustCompile(`\[` + jsNotSpace + `+` + jsSpace + `+([0-9a-f]{7,12})\]`)

// rangeHashRe is commits.ts's `/\b([0-9a-f]{7,12})\.\.([0-9a-f]{7,12})\b/`.
var rangeHashRe = mustCompile(`\b([0-9a-f]{7,12})\.\.([0-9a-f]{7,12})\b`)

// gitCommitCheckRe is commits.ts's `/\bgit\s+commit\b/`.
var gitCommitCheckRe = mustCompile(`\bgit` + jsSpace + `+commit\b`)

// ExtractCommits is commits.ts extractCommits().
func ExtractCommits(blocks []NormalizedBlock) []CommitInfo {
	commits := make([]CommitInfo, 0, 4)
	for i := 0; i < len(blocks); i++ {
		block := blocks[i]
		if block.Kind != KindToolCall || block.Name != "bash" {
			continue
		}
		command, _ := block.Args["command"].(string)
		if !gitCommitCheckRe.MatchString(command) {
			continue
		}
		match := commitMsgRe.FindStringSubmatchIndex(command)
		if match == nil {
			continue
		}
		group := func(index int) (string, bool) {
			if match[2*index] < 0 {
				return "", false
			}
			return command[match[2*index]:match[2*index+1]], true
		}
		first, hasFirst := group(1)
		heredoc := ""
		hasHeredoc := false
		if hasFirst && heredocOpenerRe.MatchString(first) {
			if subject, ok := subjectFromHeredocCmd(command[match[0]:]); ok {
				heredoc = subject
				hasHeredoc = true
			}
		}
		message := heredoc
		if !hasHeredoc {
			raw := first
			if !hasFirst {
				if second, ok := group(2); ok {
					raw = second
				} else if third, ok := group(3); ok {
					raw = third
				}
			}
			message = firstLineOf(cleanMessage(raw))
		}
		if !hasHeredoc && heredocOpenerRe.MatchString(message) {
			continue
		}
		if message == "" {
			continue
		}

		hash := ""
		hasHash := false
		limit := i + 3
		if limit > len(blocks) {
			limit = len(blocks)
		}
		for j := i + 1; j < limit; j++ {
			result := blocks[j]
			if result.Kind != KindToolResult {
				continue
			}
			if m := bracketHashRe.FindStringSubmatch(result.Text); m != nil {
				hash, hasHash = m[1], true
				break
			}
			if m := rangeHashRe.FindStringSubmatch(result.Text); m != nil {
				hash, hasHash = m[2], true
				break
			}
			if m := hashRe.FindStringSubmatch(result.Text); m != nil {
				hash, hasHash = m[1], true
				break
			}
		}

		// Dedup by message+hash.
		duplicate := false
		for _, existing := range commits {
			if existing.Hash == hash && existing.Message == message {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		commits = append(commits, CommitInfo{Hash: hash, HasHash: hasHash, Message: message})
	}
	return commits
}

// FormatCommits is commits.ts formatCommits(): the most recent `limit` commits
// as "hash: subject" lines.
func FormatCommits(commits []CommitInfo, limit int) []string {
	lines := make([]string, 0, limit)
	items := commits
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	for _, commit := range items {
		prefix := ""
		if commit.HasHash && commit.Hash != "" {
			prefix = commit.Hash + ": "
		}
		lines = append(lines, prefix+commit.Message)
	}
	return lines
}
