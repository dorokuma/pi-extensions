package pivcc

import (
	"regexp"
	"strings"
)

// Ports src/extract/tracked-commands.ts **with the pi-vcc-0.9.0-ctx-run patch
// applied**: shell-ish command texts are taken from every tool_call block (not
// only from `bash`), covering args.command, args.argv and args.commands.

// MaxEntryChars is tracked-commands.ts MAX_ENTRY_CHARS: per-entry display cap.
const maxEntryChars = 80

// unquotedSeparators is tracked-commands.ts unquotedSeparators(): the indexes of
// every unquoted `;`, `&`, `|` or newline.
//
// Indices are byte offsets here rather than UTF-16 code units; every character
// it inspects is ASCII, so the two agree and slicing on the boundaries it
// returns is safe in UTF-8.
func unquotedSeparators(command string) []int {
	seps := make([]int, 0, 4)
	quote := byte(0)
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote != 0 {
			// Backslash escapes inside "..." only; inside '...' it is literal.
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '\\':
			i++ // `it\'s` is not an opening quote
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || isCommentBoundary(command[i-1])):
			// A comment runs to the newline; its apostrophes are not quotes.
			for i+1 < len(command) && command[i+1] != '\n' {
				i++
			}
		case c == '&' && (byteAt(command, i-1) == '>' || byteAt(command, i-1) == '<' || byteAt(command, i+1) == '>'):
			// `2>&1`, `<&3`, `&>file` are redirections, not separators. byteAt
			// guards the left neighbour so a leading `&` reads as 0 (what TS's
			// `cmd[i - 1]` yields as `undefined`) instead of panicking on -1.
			continue
		case c == ';' || c == '&' || c == '|' || c == '\n':
			seps = append(seps, i)
		}
	}
	return seps
}

func byteAt(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

// isCommentBoundary is the `[\s;&|(]` test on the character before `#`.
func isCommentBoundary(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\v', '\f', '\r', ';', '&', '|', '(':
		return true
	}
	return false
}

// captureEntry is tracked-commands.ts captureEntry(): cut at the next unquoted
// separator and trim. No truncation here — dedup must see the full string.
func captureEntry(text string) string {
	seps := unquotedSeparators(text)
	cut := text
	if len(seps) > 0 {
		cut = text[:seps[0]]
	}
	return jsTrimSpace(cut)
}

// wrappers is tracked-commands.ts WRAPPERS: prefixes that hide the real command
// name. They are skipped only to find the name; the entry keeps them.
var wrappers = map[string]bool{
	"sudo": true, "env": true, "nohup": true, "time": true, "command": true,
}

// wrapperValueFlagRe is tracked-commands.ts WRAPPER_VALUE_FLAGS: the flags a
// wrapper consumes a value for.
var wrapperValueFlagRe = map[string]*regexp.Regexp{
	"sudo": mustCompile(`^-[ugpCDRT]$`),
	"env":  mustCompile(`^-[uS]$`),
}

// tokenAt is tracked-commands.ts tokenAt(): the next space/tab-delimited token
// (which may run past a newline, as `\S+` does) and the index where it starts.
func tokenAt(command string, pos int) (string, int) {
	for pos < len(command) && (command[pos] == ' ' || command[pos] == '\t') {
		pos++
	}
	rest := command[pos:]
	end := strings.IndexFunc(rest, jsIsSpaceRune)
	if end < 0 {
		return rest, pos
	}
	return rest[:end], pos
}

// varAssignmentRe is tracked-commands.ts's `/^[A-Za-z_]\w*=/`.
var varAssignmentRe = mustCompile(`^[A-Za-z_]\w*=`)

// skipToCommand is tracked-commands.ts skipToCommand(): skip VAR=val prefixes
// and wrapper invocations, returning where the real command begins.
func skipToCommand(command string, pos int) int {
	for {
		tok, at := tokenAt(command, pos)
		if tok == "" {
			return at
		}
		if varAssignmentRe.MatchString(tok) {
			pos = at + len(tok)
			continue
		}
		if !wrappers[tok] {
			return at
		}
		pos = at + len(tok)
		for {
			flag, flagAt := tokenAt(command, pos)
			if !strings.HasPrefix(flag, "-") {
				break
			}
			pos = flagAt + len(flag)
			if re, ok := wrapperValueFlagRe[tok]; ok && re.MatchString(flag) {
				value, valueAt := tokenAt(command, pos)
				pos = valueAt + len(value)
			}
		}
	}
}

// findTopLevelInvocations is tracked-commands.ts findTopLevelInvocations():
// segment starts at which `name` is the command. It returns the segment start
// (before any VAR=/wrapper prefixes), not the name position.
func findTopLevelInvocations(command, name string) []int {
	bounds := make([]int, 0, 8)
	bounds = append(bounds, 0)
	for _, sep := range unquotedSeparators(command) {
		bounds = append(bounds, sep+1)
	}
	starts := make([]int, 0, 2)
	for _, bound := range bounds {
		if bound > len(command) {
			continue
		}
		pos := skipToCommand(command, bound)
		if !strings.HasPrefix(command[pos:], name) {
			continue
		}
		after := pos + len(name)
		if after >= len(command) {
			starts = append(starts, bound)
			continue
		}
		r, _ := firstRuneAt(command, after)
		if jsIsSpaceRune(r) {
			starts = append(starts, bound)
		}
	}
	return starts
}

// sshBoolFlagRe is tracked-commands.ts SSH_BOOL_FLAG.
var sshBoolFlagRe = mustCompile(`^-[46ACfGgKkMNTtVvXxYynaq]+$`)

// sshTokenRe is the `\S+` scan sshRemoteCommand uses.
var sshTokenRe = mustCompile(jsNotSpace + `+`)

// sshRemoteCommand is tracked-commands.ts sshRemoteCommand(): locate the ssh
// target by token position and return everything after it.
func sshRemoteCommand(afterSsh string) (string, bool) {
	tokens := sshTokenRe.FindAllStringIndex(afterSsh, -1)
	i := 0
	for i < len(tokens) && strings.HasPrefix(afterSsh[tokens[i][0]:tokens[i][1]], "-") {
		if sshBoolFlagRe.MatchString(afterSsh[tokens[i][0]:tokens[i][1]]) {
			i++
		} else {
			i += 2
		}
	}
	if i >= len(tokens) {
		return "", false
	}
	rest := jsTrimSpace(afterSsh[tokens[i][1]:])
	if rest == "" {
		return "", false
	}
	return rest, true
}

// TrackedCommandActivity is tracked-commands.ts TrackedCommandActivity:
// command name -> entries seen, in use order (oldest first).
type TrackedCommandActivity struct {
	// ByCommand preserves the trackCommands configuration order so rendering
	// is deterministic; each entry list is ordered oldest-to-newest with a
	// re-used entry moved to the tail.
	ByCommand map[string][]string
	// Order is the configured command-name order.
	Order []string
}

// commandTextsOf is the pi-vcc-0.9.0-ctx-run patch's commandTextsOf(): the
// shell-ish command texts a tool call executed.
//
//   - args.command: the bash-style shell string;
//   - args.argv: a direct-exec array, joined with spaces after dropping
//     non-string elements (an empty result contributes nothing);
//   - args.commands: batch items, each a string or an object with a string
//     `command` field.
//
// Calls without these fields contribute nothing, so the patch is a no-op for
// non-shell tools.
func commandTextsOf(args map[string]any) []string {
	out := make([]string, 0, 3)
	if command, ok := args["command"].(string); ok {
		out = append(out, command)
	}
	if argv, ok := args["argv"].([]any); ok {
		tokens := make([]string, 0, len(argv))
		for _, item := range argv {
			if token, isString := item.(string); isString {
				tokens = append(tokens, token)
			}
		}
		if len(tokens) > 0 {
			out = append(out, strings.Join(tokens, " "))
		}
	}
	if commands, ok := args["commands"].([]any); ok {
		for _, item := range commands {
			switch item := item.(type) {
			case string:
				out = append(out, item)
			case map[string]any:
				if command, isString := item["command"].(string); isString {
					out = append(out, command)
				}
			}
		}
	}
	return out
}

// touchEntry refreshes an entry to the tail of its list: re-running a command
// makes it the most recently used, not the first seen.
func touchEntry(entries []string, item string) []string {
	for i, existing := range entries {
		if existing == item {
			entries = append(entries[:i:i], entries[i+1:]...)
			break
		}
	}
	return append(entries, item)
}

// ExtractTrackedCommands is tracked-commands.ts extractTrackedCommands() with
// the ctx-run patch: every tool_call block's command texts are scanned for
// top-level invocations of each tracked name, and — when "ssh" is tracked —
// inside ssh's remote command as well.
func ExtractTrackedCommands(blocks []NormalizedBlock, trackCommands []string) TrackedCommandActivity {
	activity := TrackedCommandActivity{ByCommand: make(map[string][]string, len(trackCommands))}
	if len(trackCommands) == 0 {
		return activity
	}
	for _, name := range trackCommands {
		activity.ByCommand[name] = []string{}
		activity.Order = append(activity.Order, name)
	}

	trackSSH := false
	for _, name := range trackCommands {
		if name == "ssh" {
			trackSSH = true
		}
	}

	for _, block := range blocks {
		if block.Kind != KindToolCall {
			continue
		}
		for _, command := range commandTextsOf(block.Args) {
			if command == "" {
				continue
			}
			for _, name := range trackCommands {
				for _, startAt := range findTopLevelInvocations(command, name) {
					item := captureEntry(jsTrimSpace(command[startAt:]))
					if item == "" {
						continue
					}
					activity.ByCommand[name] = touchEntry(activity.ByCommand[name], item)
				}
			}
			if !trackSSH {
				continue
			}
			for _, bound := range findTopLevelInvocations(command, "ssh") {
				seg := command[bound:]
				remote, ok := sshRemoteCommand(seg[skipToCommand(seg, 0)+3:])
				if !ok {
					continue
				}
				// The remote string's quotes were the LOCAL shell's syntax;
				// strip them all so a stray quote cannot swallow the rest.
				unquoted := strings.NewReplacer("'", "", "\"", "").Replace(remote)
				for _, name := range trackCommands {
					if name == "ssh" {
						continue
					}
					for _, startAt := range findTopLevelInvocations(unquoted, name) {
						item := captureEntry(jsTrimSpace(unquoted[startAt:]))
						if item == "" {
							continue
						}
						activity.ByCommand[name] = touchEntry(activity.ByCommand[name], item)
					}
				}
			}
		}
	}
	return activity
}

// CommandSeparator is tracked-commands.ts COMMAND_SEPARATOR: the line format's
// item separator, shared with the merge in summarize.go. " | " rather than ","
// since an entry can legitimately contain a comma.
const CommandSeparator = " | "

// CommandsPerName is tracked-commands.ts COMMANDS_PER_NAME.
const CommandsPerName = 10

// renderEntry is tracked-commands.ts renderEntry(): one line, an inner
// separator escaped as " \| " so the merge keeps the entry whole, then capped.
func renderEntry(entry string) string {
	line := strings.ReplaceAll(multiSpaceRe.ReplaceAllString(entry, " "), CommandSeparator, " \\| ")
	if jsLen(line) > maxEntryChars {
		return jsSlice(line, 0, maxEntryChars) + "\u2026"
	}
	return line
}

// FormatTrackedCommands is tracked-commands.ts formatTrackedCommands(): one
// "#name: entry | entry" body line per tracked name with matches, keeping the
// newest entries.
func FormatTrackedCommands(activity TrackedCommandActivity) []string {
	lines := make([]string, 0, len(activity.Order))
	for _, name := range activity.Order {
		entries := activity.ByCommand[name]
		if len(entries) == 0 {
			continue
		}
		rendered := make([]string, 0, len(entries))
		for _, entry := range entries {
			rendered = append(rendered, renderEntry(entry))
		}
		lines = append(lines, name+": "+CapItems(rendered, CommandsPerName, CommandSeparator))
	}
	return lines
}
