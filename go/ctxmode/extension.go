package ctxmode

import (
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

//go:embed tools.json
var toolsJSON []byte

type toolDefinition struct {
	Name             string     `json:"name"`
	Label            string     `json:"label"`
	Description      string     `json:"description"`
	PromptSnippet    string     `json:"promptSnippet"`
	PromptGuidelines []string   `json:"promptGuidelines"`
	Parameters       sdk.Schema `json:"parameters"`
}

func definitions() []toolDefinition {
	var defs []toolDefinition
	if err := json.Unmarshal(toolsJSON, &defs); err != nil {
		panic(err)
	}
	return defs
}

type extensionState struct {
	mu              sync.Mutex
	client          *CtxmodeClient
	toolsRegistered bool
}

func (s *extensionState) getClient() *CtxmodeClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}
func (s *extensionState) stop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return false
	}
	s.client.Stop()
	s.client = nil
	return true
}
func (s *extensionState) start(workdir string, onlyIfAbsent bool, register func()) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if onlyIfAbsent && s.client != nil {
		return false, nil
	}
	resolved, err := filepath.Abs(workdir)
	if err != nil {
		return false, err
	}
	s.client = NewCtxmodeClient(resolved)
	if err := s.client.Start(); err != nil {
		s.client.Stop()
		s.client = nil
		return false, err
	}
	if !s.toolsRegistered {
		register()
		s.toolsRegistered = true
	}
	return true, nil
}

type toolResult struct {
	Content []map[string]string `json:"content"`
	Details map[string]any      `json:"details"`
}

func textResult(text string) toolResult {
	return toolResult{Content: []map[string]string{{"type": "text", "text": text}}, Details: map[string]any{}}
}
func (s *extensionState) run(ctx context.Context, name string, params map[string]any) (any, error) {
	c := s.getClient()
	if c == nil {
		return textResult("ctxmode 未运行。用 /ctxmode-start 启动。"), nil
	}
	text, err := c.CallTool(ctx, name, params)
	if err != nil {
		if strings.Contains(err.Error(), "cancelled (user pressed esc)") {
			return nil, err
		}
		return textResult("ctxmode error: " + err.Error()), nil
	}
	return textResult(text), nil
}

// SDK Context has Done/Err but no Deadline; this adapter forwards its Esc signal.
type toolContext struct{ sdk.Context }

func (toolContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (toolContext) Value(any) any               { return nil }
func (s *extensionState) registerTools(e *sdk.Extension) {
	for _, def := range definitions() {
		e.RegisterTool(sdk.ToolDefinition{
			Name: def.Name, Label: def.Label, Description: def.Description, PromptSnippet: def.PromptSnippet, PromptGuidelines: def.PromptGuidelines, Parameters: def.Parameters,
			Execute: func(ctx sdk.Context, params map[string]any) (any, error) {
				return s.run(toolContext{ctx}, def.Name, params)
			},
		})
	}
}

// Extension is PiG's factory entry point, matching the existing Go ports.
// The host generates a runner for this exported factory; no init-side effects.
func Extension() *sdk.Extension {
	e := sdk.New("ctxmode")
	state := &extensionState{}
	register := func() { state.registerTools(e) }
	start := func(ctx sdk.Context, onlyIfAbsent bool, failLevel string) {
		workdir := envString("CTXMODE_WORKDIR", ctx.Cwd())
		started, err := state.start(workdir, onlyIfAbsent, register)
		if err != nil {
			ctx.Notify("ctxmode 启动失败: "+err.Error(), failLevel)
		} else if !started {
			ctx.Notify("ctxmode 已在运行", "info")
		} else {
			ctx.Notify("ctxmode 已启动 (workdir: "+workdir+")", "info")
		}
	}
	e.OnSessionStart(func(ctx sdk.Context, _ map[string]any) (any, error) { start(ctx, false, "warning"); return nil, nil })
	e.OnEvent("before_agent_start", func(_ sdk.Context, event map[string]any) (any, error) {
		if c := state.getClient(); c != nil {
			if instructions := c.GetServerInstructions(); instructions != "" {
				prompt, _ := event["systemPrompt"].(string)
				if p, ok := event["systemPrompt"].(*sdk.SystemPrompt); ok {
					var err error
					prompt, err = p.Get()
					if err != nil {
						return nil, err
					}
				}
				return map[string]any{"systemPrompt": prompt + "\n\n## Ctxmode server instructions\n\n" + instructions}, nil
			}
		}
		return nil, nil
	})
	e.OnSessionShutdown(func(_ sdk.Context, _ map[string]any) (any, error) { state.stop(); return nil, nil })
	e.RegisterCommand("ctxmode-start", sdk.CommandOptions{Description: "启动 ctxmode", Handler: func(ctx sdk.Context, _ string) error { start(ctx, true, "error"); return nil }})
	e.RegisterCommand("ctxmode-stop", sdk.CommandOptions{Description: "停止 ctxmode", Handler: func(ctx sdk.Context, _ string) error {
		if state.stop() {
			ctx.Notify("ctxmode 已停止", "info")
		} else {
			ctx.Notify("ctxmode 未运行", "info")
		}
		return nil
	}})
	return e
}
