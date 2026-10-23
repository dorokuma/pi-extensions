package herdragentstate

import (
	"log"
	"os"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Config represents herdr environment configuration.
type Config struct {
	Env        string
	SocketPath string
	PaneID     string
}

// ConfigFromEnv reads configuration from process environment variables.
func ConfigFromEnv() Config {
	return Config{
		Env:        os.Getenv("HERDR_ENV"),
		SocketPath: os.Getenv("HERDR_SOCKET_PATH"),
		PaneID:     os.Getenv("HERDR_PANE_ID"),
	}
}

// Enabled returns true if all required HERDR environment variables are present and valid.
// Matches TS lines 18-20: HERDR_ENV === "1" && !!socketPath && !!paneId
func Enabled(cfg Config) bool {
	return cfg.Env == "1" && cfg.SocketPath != "" && cfg.PaneID != ""
}

// Extension constructs and registers the herdr-agent-state extension for PiG.
// If HERDR environment variables are not set or incomplete, the extension remains completely no-op.
func Extension() *sdk.Extension {
	e := sdk.New("herdr-agent-state")

	cfg := ConfigFromEnv()
	if !Enabled(cfg) {
		return e
	}

	endpoint := ResolveSocketEndpoint(cfg.SocketPath)
	client := NewClient(endpoint, cfg.PaneID, nil, nil)
	sm := NewStateManager(client)

	// Event bus: herdr:blocked
	if _, err := e.Events().On("herdr:blocked", sm.HandleHerdrBlocked); err != nil {
		log.Printf("[herdr-agent-state] failed to subscribe to herdr:blocked: %v", err)
	}

	// Pi/PiG lifecycle events
	e.OnEvent(sdk.EventSessionStart, sm.HandleSessionStart)
	e.OnEvent(sdk.EventAgentStart, sm.HandleAgentStart)
	e.OnEvent(sdk.EventAgentSettled, sm.HandleAgentSettled)

	return e
}
