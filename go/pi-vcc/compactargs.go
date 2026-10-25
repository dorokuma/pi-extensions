package pivcc

import (
	"strconv"
	"strings"
)

// Ports src/core/compact-args.ts: parse `/pi-vcc keep:N prompt` arguments.

// PiVccCompactInstruction is compact-args.ts PI_VCC_COMPACT_INSTRUCTION, the
// customInstructions marker the /pi-vcc command hands to ctx.compact().
const PiVccCompactInstruction = "__pi_vcc__"

// keepTokenRe is compact-args.ts KEEP_TOKEN_RE.
var keepTokenRe = mustCompile(`^keep:(\d+)$`)

// keepStartRe is compact-args.ts's `/^keep:(\d+)(?:\s+|$)([\s\S]*)$/`.
var keepStartRe = mustCompile(`^keep:(\d+)(?:` + jsSpace + `+|$)([\s\S]*)$`)

// ParsedCompactionArgs is compact-args.ts ParsedCompactionArgs.
type ParsedCompactionArgs struct {
	FollowUpPrompt        string
	KeepUserTurns         int
	HasKeepUserTurns      bool
	KeepUserTurnsExplicit bool
}

// parseKeepUserTurns is compact-args.ts parseKeepUserTurns(): a non-safe-integer
// value becomes MAX_SAFE_INTEGER, which normalizeKeepUserTurns then treats as
// "keep everything short of all turns".
func parseKeepUserTurns(raw string) (int, bool) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value > 9007199254740991 {
		// Number.MAX_SAFE_INTEGER: still a finite, positive keep count.
		return 9007199254740991, false
	}
	return int(value), true
}

// ParseKeepAndPrompt is compact-args.ts parseKeepAndPrompt().
func ParseKeepAndPrompt(args string) ParsedCompactionArgs {
	trimmed := jsTrimSpace(args)
	if trimmed == "" {
		return ParsedCompactionArgs{}
	}

	if match := keepStartRe.FindStringSubmatch(trimmed); match != nil {
		keep, _ := parseKeepUserTurns(match[1])
		return ParsedCompactionArgs{
			FollowUpPrompt:        jsTrimSpace(match[2]),
			KeepUserTurns:         keep,
			HasKeepUserTurns:      true,
			KeepUserTurnsExplicit: true,
		}
	}

	parts := jsSplitWhitespace(trimmed)
	last := ""
	if len(parts) > 0 {
		last = parts[len(parts)-1]
	}
	if match := keepTokenRe.FindStringSubmatch(last); match != nil {
		keep, _ := parseKeepUserTurns(match[1])
		return ParsedCompactionArgs{
			FollowUpPrompt:        jsTrimSpace(jsSlice(trimmed, 0, jsLen(trimmed)-jsLen(last))),
			KeepUserTurns:         keep,
			HasKeepUserTurns:      true,
			KeepUserTurnsExplicit: true,
		}
	}

	return ParsedCompactionArgs{FollowUpPrompt: trimmed}
}

// BuildPiVccCustomInstructions is compact-args.ts buildPiVccCustomInstructions().
func BuildPiVccCustomInstructions(keepUserTurns int, hasKeepUserTurns bool) string {
	if !hasKeepUserTurns {
		return PiVccCompactInstruction
	}
	return PiVccCompactInstruction + " keep:" + strconv.Itoa(keepUserTurns)
}

var _ = strings.Join
