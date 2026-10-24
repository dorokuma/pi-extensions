package autocontinue

// Constants mirrored from the TypeScript extension (auto-continue.ts:98-143).
const (
	// LogName is TS LOG (auto-continue.ts:98): the notify/status tag. It is also
	// the extension's registered name.
	LogName = "auto-continue"
	// StatusKey is TS STATUS_KEY (auto-continue.ts:99).
	StatusKey = "auto-continue"

	// DefaultMax is TS DEFAULT_MAX (auto-continue.ts:101).
	DefaultMax = 20
	// DefaultDelayMs is TS DEFAULT_DELAY_MS (auto-continue.ts:102).
	DefaultDelayMs = 2500

	// ContinuePrompt is TS CONTINUE_PROMPT (auto-continue.ts:105): exactly two
	// Chinese characters, no explanation, parentheses or English.
	ContinuePrompt = "继续"

	// BareEsc is TS BARE_ESC (auto-continue.ts:112). Arrow keys, Alt+key and
	// function keys all start with ESC ("\x1b[A", "\x1b[3~", "\x1bOP" …), so
	// only a lone ESC byte counts as "the user pressed esc".
	BareEsc = "\x1b"

	// CompactionBusyMaxMs is TS COMPACTION_BUSY_MAX_MS
	// (auto-continue.ts:119): the compaction busy flag unlocks itself after
	// this long, so a flag nothing ever clears cannot silence auto-continue
	// forever.
	CompactionBusyMaxMs = 300000

	// ToolUseWindowMs is TS TOOL_USE_WINDOW_MS (auto-continue.ts:127): gate A's
	// "任务进行中" toolUse window.
	ToolUseWindowMs = 300000

	// UserProgressStallMs is TS USER_PROGRESS_STALL_MS (auto-continue.ts:133):
	// gate B's "no progress since the last human user message" threshold.
	UserProgressStallMs = 90000

	// RunActiveMinMs is TS RUN_ACTIVE_MIN_MS (auto-continue.ts:143): gate A's
	// parallel run-duration evidence. Same initial value as gate B, but an
	// independent notion ("how long has this run lasted"), so it stays a
	// separate constant. The boundary is strict: only more than 90s counts.
	RunActiveMinMs = 90000

	// MinAttempts and MaxAttemptsLimit bound /auto-continue max and
	// PI_AUTO_CONTINUE_MAX, as TS's `1..20` ranges do.
	MinAttempts      = 1
	MaxAttemptsLimit = 20
	// MaxDelayMs bounds PI_AUTO_CONTINUE_DELAY_MS and the persisted delayMs
	// (TS's `0..60_000`).
	MaxDelayMs = 60000
)
