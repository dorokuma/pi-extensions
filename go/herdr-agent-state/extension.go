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

	// Event bus: herdr:blocked, subscribed from OnLoaded rather than here: a
	// subscription made while the factory runs dials the host socket itself and
	// waits on a synchronous host RPC (EventBus.On -> factoryBusConn), which
	// deadlocks /reload while the host is still accepting an earlier member's
	// connection. Per the SDK's OnLoaded contract (pigsdk/host_state_subscriptions.go)
	// the handler runs once this extension is registered with the host and has
	// received its state, before the host serves any request (session_start
	// included) — a listener dispatch excepted, since a dispatch does not wait
	// behind the handler. So ctx.Events() rides the running connection, and the
	// subscription is in place before agent blocking can emit it.
	e.OnLoaded(func(ctx sdk.Context) {
		if _, err := ctx.Events().On("herdr:blocked", sm.HandleHerdrBlocked); err != nil {
			log.Printf("[herdr-agent-state] failed to subscribe to herdr:blocked: %v", err)
		}
	})

	// Pi/PiG lifecycle events
	e.OnEvent(sdk.EventSessionStart, sm.HandleSessionStart)
	e.OnEvent(sdk.EventAgentStart, sm.HandleAgentStart)
	e.OnEvent(sdk.EventAgentSettled, sm.HandleAgentSettled)
	e.OnEvent(sdk.EventSessionShutdown, sm.HandleSessionShutdown)

	return e
}
