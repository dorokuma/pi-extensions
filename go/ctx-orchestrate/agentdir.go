package ctxorchestrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// resolveAgentDir resolves the agent configuration directory a Go extension
// must read or write, without ever falling back to ~/.pi.
//
// Priority:
//  1. PIG_EXT_HOST_INFO.agentDir (via sdk.AgentDir — what PiG injects into
//     every extension subprocess),
//  2. PI_CODING_AGENT_DIR,
//  3. PIG_CODING_AGENT_DIR,
//  4. $PIG_HOME/agent,
//  5. ~/.pig/agent.
//
// It returns "" only when the last step needs a home directory and none can
// be resolved. A leading "~"/"~/" in an env-supplied path expands the way a
// shell would.
//
// This body is duplicated verbatim in every component that resolves an agent
// directory: each component is its own Go module (PiG builds it with
// GOWORK=off inside that module only), so a sibling internal/ or root-module
// helper cannot be imported at runtime. Keep the copies byte-identical.
func resolveAgentDir() string {
	if dir, err := sdk.AgentDir(); err == nil {
		if dir = strings.TrimSpace(dir); dir != "" {
			return expandAgentDirHome(dir)
		}
	}
	if dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); dir != "" {
		return expandAgentDirHome(dir)
	}
	if dir := strings.TrimSpace(os.Getenv("PIG_CODING_AGENT_DIR")); dir != "" {
		return expandAgentDirHome(dir)
	}
	if home := strings.TrimSpace(os.Getenv("PIG_HOME")); home != "" {
		return filepath.Join(expandAgentDirHome(home), "agent")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".pig", "agent")
	}
	return ""
}

// expandAgentDirHome expands a leading "~" the way a shell would.
func expandAgentDirHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// resolveAgentDirWithHome is resolveAgentDir except the ~/.pig default resolves
// from the supplied home instead of os.UserHomeDir, so a caller that resolved
// its home earlier (or fakes one in a test) keeps a single source of truth. The
// env-driven steps (host info, the two agent-dir variables, PIG_HOME) are
// identical.
func resolveAgentDirWithHome(home string) string {
	if raw := strings.TrimSpace(os.Getenv("PIG_EXT_HOST_INFO")); raw != "" {
		var info struct {
			AgentDir string `json:"agentDir"`
		}
		if err := json.Unmarshal([]byte(raw), &info); err == nil && strings.TrimSpace(info.AgentDir) != "" {
			return info.AgentDir
		}
	}
	if dir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("PIG_CODING_AGENT_DIR")); dir != "" {
		return dir
	}
	if pigHome := strings.TrimSpace(os.Getenv("PIG_HOME")); pigHome != "" {
		return filepath.Join(pigHome, "agent")
	}
	if home = strings.TrimSpace(home); home != "" {
		return filepath.Join(home, ".pig", "agent")
	}
	return ""
}
