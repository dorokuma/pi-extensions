package pivcc

import "strings"

// Ports src/extract/commits.ts: git commits from `bash` tool calls, paired with
// the hash the following tool result printed.

// commitMsgRe is commits.ts COMMIT_MSG_RE. `[A-Za-z]*m` before the `-` is
// what makes a combined flag such as -am or -qam match.
var commitMsgRe = mustCompile(`git` + jsSpace + `+commit[^\n]*?` + jsSpace + `-[A-Za-z]*m` + jsSpace +
	`+(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'|\$?'((?:[^'\\]|\\.)*)')`)

// stdinHeredocCommitRe is commits.ts STDIN_HEREDOC_COMMIT_RE: a commit that
// reads its message from stdin (`-F -` / `--file=-`) with the heredoc feeding
// it on the same command. TS's `-` before the `(?=\s)` lookahead stays a literal,
// and the lookahead is spelled out as the one whitespace character it requires,
// which the rest of the pattern can safely skip over.
var stdinHeredocCommitRe = mustCompile(`git` + jsSpace + `+commit[^\n]*?(?:` + jsSpace + `+-[A-Za-z]*F|` +
	jsSpace + `+--file)(?:` + jsSpace + `+|=)-` + jsSpace +
	`[^\n]*?<<-?` + jsSpace + `*(?:'([^']+)'|"([^"]+)"|([A-Za-z_]\w*))[^\n]*(?:\r?\n|\\n)`)

// hashRe is commits.ts HASH_RE.
var hashRe = mustCompile(`\b([0-9a-f]{7,12})\b`)

// heredocCommitCmdRe is commits.ts HEREDOC_COMMIT_CMD_RE.
var heredocCommitCmdRe = mustCompile(`^git` + jsSpace + `+commit[^\n"'&;|]*?` + jsSpace + `-[A-Za-z]*m` + jsSpace +
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

// subjectFromStdinHeredoc is commits.ts subjectFromStdinHeredoc(): the first
// heredoc body line of a `git commit -F -` command.
func subjectFromStdinHeredoc(command string) (string, bool) {
	loc := stdinHeredocCommitRe.FindStringSubmatchIndex(command)
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

// commitStartRe is commits.ts COMMIT_START_RE: every `git commit` occurrence.
var commitStartRe = mustCompile(`\bgit` + jsSpace + `+commit\b`)

// commandBoundaryBeforeRe is the `(?:^|[;&|(\n])\s*$` test a match must pass to
// start a new commit segment: only a command boundary counts, so prose inside a
// message that says "git commit" does not split it.
var commandBoundaryBeforeRe = mustCompile(`(?:^|[;&|(\n])` + jsSpace + `*$`)

// commitSegments is commits.ts commitSegments(): one slice per `git commit`
// command in cmd.
func commitSegments(command string) []string {
	starts := make([]int, 0, 2)
	for _, loc := range commitStartRe.FindAllStringIndex(command, -1) {
		if len(starts) == 0 || commandBoundaryBeforeRe.MatchString(command[:loc[0]]) {
			starts = append(starts, loc[0])
		}
	}
	if len(starts) == 0 {
		return nil
	}
	segments := make([]string, 0, len(starts))
	for i, at := range starts {
		end := len(command)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		segments = append(segments, command[at:end])
	}
	return segments
}

// commitGroup reads one capture group of a commitMsgRe match: ok is false when
// the group did not participate (TS's `undefined`, which `??` skips over).
func commitGroup(segment string, match []int, index int) (string, bool) {
	if match[2*index] < 0 {
		return "", false
	}
	return segment[match[2*index]:match[2*index+1]], true
}

// subjectOf is commits.ts subjectOf(): the subject of the one `git commit` in
// seg, or "" when it cannot be read.
func subjectOf(seg string) string {
	match := commitMsgRe.FindStringSubmatchIndex(seg)
	if match == nil {
		// TS: `subjectFromStdinHeredoc(seg) ?? ""`.
		if subject, ok := subjectFromStdinHeredoc(seg); ok {
			return subject
		}
		return ""
	}
	first, hasFirst := commitGroup(seg, match, 1)
	heredoc := ""
	hasHeredoc := false
	if hasFirst && heredocOpenerRe.MatchString(first) {
		if subject, ok := subjectFromHeredocCmd(seg[match[0]:]); ok {
			heredoc = subject
			hasHeredoc = true
		}
	}
	message := heredoc
	if !hasHeredoc {
		raw := first
		if !hasFirst {
			if second, ok := commitGroup(seg, match, 2); ok {
				raw = second
			} else if third, ok := commitGroup(seg, match, 3); ok {
				raw = third
			}
		}
		message = firstLineOf(cleanMessage(raw))
	}
	// TS: `heredoc === null && /^\$\(cat\s+<</.test(message) ? "" : message`.
	if !hasHeredoc && heredocOpenerRe.MatchString(message) {
		return ""
	}
	return message
}

// hashFromResults is commits.ts hashFromResults(): the hash of a lone commit,
// the first hash-looking text of the next tool results winning.
func hashFromResults(results []string) (string, bool) {
	for _, text := range results {
		if m := bracketHashRe.FindStringSubmatch(text); m != nil {
			return m[1], true
		}
		if m := rangeHashRe.FindStringSubmatch(text); m != nil {
			return m[2], true
		}
		if m := hashRe.FindStringSubmatch(text); m != nil {
			return m[1], true
		}
	}
	return "", false
}

// hashSubjectLineRe is commits.ts HASH_SUBJECT_LINE_RE: `[branch <hash>]
// <subject>` (git commit) or `<hash> [(refs)] <subject>` (git log --oneline).
// The JS pattern carries no /m flag; it is applied per line by hashForSubject.
var hashSubjectLineRe = mustCompile(`(?:\[` + jsNotSpace + `+` + jsSpace + `+([0-9a-f]{7,12})\]|^` + jsSpace +
	`*([0-9a-f]{7,12})(?:` + jsSpace + `+\([^)]*\))?)` + jsSpace + `+(` + jsDot + `*)`)

// hashForSubject is commits.ts hashForSubject(): the hash of one of several
// commits in a command — only an output line naming that commit's subject
// counts.
func hashForSubject(results []string, message string) (string, bool) {
	for _, text := range results {
		for _, line := range jsSplitLines(text) {
			m := hashSubjectLineRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if jsTrimSpace(m[3]) != message {
				continue
			}
			if m[1] != "" {
				return m[1], true
			}
			if m[2] != "" {
				return m[2], true
			}
		}
	}
	return "", false
}

// commitHasHash reports whether a commit carries a hash (TS's truthy `c.hash`).
func commitHasHash(commit CommitInfo) bool {
	return commit.HasHash && commit.Hash != ""
}

// ExtractCommits is commits.ts extractCommits(): every `git commit` command of a
// bash tool call, paired with the hash the following tool results printed.
func ExtractCommits(blocks []NormalizedBlock) []CommitInfo {
	commits := make([]CommitInfo, 0, 4)
	for i := 0; i < len(blocks); i++ {
		block := blocks[i]
		if block.Kind != KindToolCall || block.Name != "bash" {
			continue
		}
		command, _ := block.Args["command"].(string)
		segments := commitSegments(command)
		if len(segments) == 0 {
			continue
		}
		results := make([]string, 0, 2)
		for j := i + 1; j < len(blocks) && j < i+3; j++ {
			if blocks[j].Kind == KindToolResult {
				results = append(results, blocks[j].Text)
			}
		}
		for _, seg := range segments {
			message := subjectOf(seg)
			if message == "" {
				continue
			}
			// With several commits one output cannot be split reliably, so a
			// hash is taken only when its line names this subject.
			hash := ""
			hasHash := false
			if len(segments) == 1 {
				hash, hasHash = hashFromResults(results)
			} else {
				hash, hasHash = hashForSubject(results, message)
			}
			// One entry per commit: the same message seen hashless (`-q` prints
			// no hash) and hashed is one commit, keep the hashed one.
			duplicate := -1
			for k, existing := range commits {
				if existing.Message != message {
					continue
				}
				if !commitHasHash(existing) || !hasHash || existing.Hash == hash {
					duplicate = k
					break
				}
			}
			if duplicate < 0 {
				commits = append(commits, CommitInfo{Hash: hash, HasHash: hasHash, Message: message})
				continue
			}
			if hasHash && hash != "" && !commitHasHash(commits[duplicate]) {
				commits[duplicate].Hash = hash
				commits[duplicate].HasHash = true
			}
		}
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
