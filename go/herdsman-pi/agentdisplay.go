package herdsmanpi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// herdrAgentToken mirrors /^[a-z][a-z0-9_-]{0,31}$/.
var herdrAgentToken = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// agentDisplayNames mirrors AGENT_DISPLAY_NAMES.
var agentDisplayNames = map[string]string{
	"claude":   "Claude",
	"codex":    "Codex",
	"gemini":   "Gemini",
	"opencode": "OpenCode",
	"pi":       "Pi",
	"pig":      "pig",
}

// safeAgentToken mirrors `HERDR_AGENT_TOKEN.test(value) ? value : null`.
func safeAgentToken(value string) (string, bool) {
	if herdrAgentToken.MatchString(value) {
		return value, true
	}
	return "", false
}

// AgentDisplayName mirrors the TS `agentDisplayName`.
func AgentDisplayName(agent string) string {
	safeAgent, ok := safeAgentToken(agent)
	if !ok {
		safeAgent = "unknown"
	}
	if display, found := agentDisplayNames[safeAgent]; found {
		return display
	}
	return safeAgent
}

// AgentIdentityLabel mirrors the TS `agentIdentityLabel({ agent, name })`.
// The name is nil-able: TS tests `input.name ? safeAgentToken(...) : null`, so
// an empty string and a missing name behave the same.
func AgentIdentityLabel(agent string, name *string) string {
	kind := AgentDisplayName(agent)
	displayName := ""
	if name != nil && *name != "" {
		if safe, ok := safeAgentToken(*name); ok {
			displayName = safe
		}
	}
	if displayName != "" {
		return displayName + " · " + kind
	}
	return kind
}

// HerdsmanPiLogLevel mirrors the TS union.
type HerdsmanPiLogLevel string

const (
	LogInfo  HerdsmanPiLogLevel = "info"
	LogWarn  HerdsmanPiLogLevel = "warn"
	LogError HerdsmanPiLogLevel = "error"
)

// herdmanHome mirrors the TS `defaultHerdsmanHome` used by the logger: an
// absolute HERDSMAN_HOME wins, otherwise `~/.herdsman`.
func herdsmanHome() string {
	if configured := strings.TrimSpace(os.Getenv("HERDSMAN_HOME")); configured != "" && filepath.IsAbs(configured) {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/root"
	}
	return filepath.Join(home, ".herdsman")
}

// LogHerdsmanPi mirrors the TS `logHerdsmanPi`: append one line to
// `<herdsman home>/logs/herdsman-pi-<YYYYMMDD>.log` and never surface an error.
// Diagnostics must never write to the terminal or interrupt the extension.
func LogHerdsmanPi(level HerdsmanPiLogLevel, message string) {
	defer func() {
		// A panic from a helper must not take the extension down either; the TS
		// falls back to the same empty catch.
		_ = recover()
	}()
	home := herdsmanHome()
	now := time.Now().UTC()
	date := strings.ReplaceAll(now.Format("2006-01-02"), "-", "")
	dir := filepath.Join(home, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	file := filepath.Join(dir, "herdsman-pi-"+date+".log")
	line := now.Format("2006-01-02T15:04:05.000Z") + " [" + string(level) + "] " + message + "\n"
	fh, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = fh.WriteString(line)
	_ = fh.Close()
}

// logInfo/logWarn are the shorthands the port uses at the TS call sites.
func logInfo(message string) { LogHerdsmanPi(LogInfo, message) }
func logWarn(message string) { LogHerdsmanPi(LogWarn, message) }

// sdkLogName is the extension name handed to sdk.New.
const sdkLogName = "herdsman-pi"

// newExtension is the single sdk.New call site, so tests can assert the name.
func newExtension() *sdk.Extension { return sdk.New(sdkLogName) }
