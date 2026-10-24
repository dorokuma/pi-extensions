// CodeGraph-Go is the PiG port of the Pi stdio bridge, not the indexing server.
package codegraphgo

import (
	"fmt"
	"os"
	"strings"
	"sync"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

type runtimeState struct {
	mu              sync.Mutex
	client          *CodeGraphClient
	decision        *WorkdirDecision
	toolsRegistered bool
	startAt         func(string, string) (*CodeGraphClient, error)
	register        func()
}

func newRuntime() *runtimeState { return &runtimeState{startAt: startClientAt} }
func (r *runtimeState) registerLocked() {
	if !r.toolsRegistered {
		r.register()
		r.toolsRegistered = true
	}
}
func (r *runtimeState) getClient() (*CodeGraphClient, string) {
	// Serializing the lazy acquisition guarantees a single spawn. Once acquired,
	// tools run without this mutex and JSON-RPC multiplexes their requests.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		return r.client, ""
	}
	d := r.decision
	if d == nil {
		return nil, "codegraph-go 尚未初始化（尚无会话决策）"
	}
	if !d.OK {
		return nil, d.Reason
	}
	c, err := r.startAt(d.Workdir, d.Note)
	if err != nil {
		diagLog("on-demand start failed: " + err.Error())
		return nil, "codegraph-go 启动失败: " + err.Error()
	}
	r.client = c
	return c, ""
}
func (r *runtimeState) sessionStart(cwd string, notify func(string, string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := makeWorkdirDecision(cwd)
	r.decision = &d
	if d.OK {
		if r.client != nil && r.client.workdir != d.Workdir {
			r.client.stop()
			r.client = nil
		}
		r.registerLocked()
		projects := []string{}
		if isHomeDir(d.Workdir) {
			projects = listProjects(d.Workdir)
		}
		hint := ""
		if len(projects) > 0 {
			hint = " | 项目: " + strings.Join(projects[:min(10, len(projects))], ", ")
			if len(projects) > 10 {
				hint += "…"
			}
		}
		notify(fmt.Sprintf("codegraph-go 可用（按需）：%s（%s）%s", d.Workdir, d.Note, hint), "info")
	} else {
		if r.client != nil {
			r.client.stop()
			r.client = nil
		}
		notify("codegraph-go 不可用: "+d.Reason, "warning")
	}
}
func (r *runtimeState) beforeAgentStart(prompt string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.decision
	if d == nil {
		return nil
	}
	if !d.OK {
		return map[string]any{"systemPrompt": prompt + "\n\n## CodeGraph Tools\n\n本会话 CodeGraph 未启用: " + d.Reason + "\n请用普通文件工具，或设置 CODEGRAPH_GO_WORKDIR。"}
	}
	projectLine := "当前索引根就是单个项目目录，一般不必再加 path。"
	if isHomeDir(d.Workdir) {
		projects := listProjects(d.Workdir)
		names := ""
		if len(projects) > 0 {
			names = "（" + strings.Join(projects, ", ") + "）"
		}
		projectLine = "家目录模式已开：索引里只有像项目的一级目录" + names + "。用户点名项目时代理自动补 path=，不依赖用户传。"
	}
	if r.client != nil {
		if instructions := r.client.getServerInstructions(); instructions != "" {
			prompt += "\n\n## CodeGraph server instructions\n\n" + instructions
		}
	}
	prompt += fmt.Sprintf("\n\n## CodeGraph Tools\n\n索引根: `%s`（%s）\n%s\n单次结果预算约 %d 字 / %d 行（过大才压缩；正常体量不砍）。", d.Workdir, d.Note, projectLine, outputChars(), outputLines())
	return map[string]any{"systemPrompt": prompt}
}
func (r *runtimeState) shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		r.client.stop()
		r.client = nil
	}
}
func (r *runtimeState) commandStart(cwd string, notify func(string, string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client != nil {
		notify(fmt.Sprintf("已在运行: %s（%s）", r.client.workdir, r.client.workdirNote), "info")
		return
	}
	d := makeWorkdirDecision(cwd)
	if !d.OK {
		notify("启动失败: "+d.Reason, "error")
		return
	}
	c, err := r.startAt(d.Workdir, d.Note)
	if err != nil {
		notify("启动失败: "+err.Error(), "error")
		return
	}
	r.client = c
	r.decision = &d
	r.registerLocked()
	notify(fmt.Sprintf("已启动: %s（%s），%d 个工具", c.workdir, c.workdirNote, len(c.getTools())), "info")
}
func (r *runtimeState) commandStop(notify func(string, string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.client == nil {
		notify("codegraph-go 未运行", "info")
		return
	}
	r.client.stop()
	r.client = nil
	notify("已停止", "info")
}
func (r *runtimeState) commandInfo(cwd string, notify func(string, string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.decision
	if d == nil {
		fallback := makeWorkdirDecision(cwd)
		d = &fallback
	}
	parsed := "拒绝 — " + d.Reason
	projects := []string{}
	if d.OK {
		parsed = d.Workdir + "（" + d.Note + "）"
		if isHomeDir(d.Workdir) {
			projects = listProjects(d.Workdir)
		}
	}
	roots := strings.Join(allowedRoots(), ", ")
	if roots == "" {
		roots = "（无）"
	}
	running := "否"
	if r.client != nil {
		running = r.client.workdir + "（" + r.client.workdirNote + "）"
	}
	sd, sh := budget("SEARCH")
	fd, fh := budget("FILES")
	yd, yh := budget("SYMBOL")
	lines := []string{"cwd: " + cwd, "解析: " + parsed, "授权根: " + roots, "运行中: " + running, fmt.Sprintf("输出上限: %d 字 / %d 行（仅超限才裁）", outputChars(), outputLines()), fmt.Sprintf("搜索默认/上限: %d/%d", sd, sh), fmt.Sprintf("文件列表默认/上限: %d/%d", fd, fh), fmt.Sprintf("符号默认/上限: %d/%d", yd, yh)}
	if len(projects) > 0 {
		lines = append(lines, "家目录项目: "+strings.Join(projects, ", "))
	}
	env := os.Getenv("CODEGRAPH_GO_WORKDIR")
	if env == "" {
		env = "（未设）"
	}
	lines = append(lines, "WORKDIR 环境变量: "+env)
	notify(strings.Join(lines, "\n"), "info")
}
func (r *runtimeState) execute(params map[string]any) (any, error) {
	c, text := r.getClient()
	if c != nil {
		result, err := c.callTool("codegraph", params)
		text = result
		if err != nil {
			text = "codegraph error: " + err.Error()
		}
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "details": map[string]any{}}, nil
}
func Extension() *sdk.Extension {
	e := sdk.New("codegraph-go")
	r := newRuntime()
	r.register = func() {
		def := toolDefinition()
		def.Execute = func(_ sdk.Context, p map[string]any) (any, error) { return r.execute(p) }
		e.RegisterTool(def)
	}
	e.OnSessionStart(func(ctx sdk.Context, _ map[string]any) (any, error) {
		r.sessionStart(ctx.Cwd(), func(m, k string) { ctx.Notify(m, k) })
		return nil, nil
	})
	e.OnEvent("before_agent_start", func(_ sdk.Context, data map[string]any) (any, error) {
		p, _ := data["systemPrompt"].(string)
		if rendered, ok := data["systemPrompt"].(*sdk.SystemPrompt); ok {
			var err error
			p, err = rendered.Get()
			if err != nil {
				return nil, err
			}
		}
		return r.beforeAgentStart(p), nil
	})
	e.OnSessionShutdown(func(_ sdk.Context, _ map[string]any) (any, error) { r.shutdown(); return nil, nil })
	e.RegisterCommand("codegraph-start", sdk.CommandOptions{Description: "启动 codegraph-go", Handler: func(ctx sdk.Context, _ string) error {
		r.commandStart(ctx.Cwd(), func(m, k string) { ctx.Notify(m, k) })
		return nil
	}})
	e.RegisterCommand("codegraph-stop", sdk.CommandOptions{Description: "停止 codegraph-go", Handler: func(ctx sdk.Context, _ string) error {
		r.commandStop(func(m, k string) { ctx.Notify(m, k) })
		return nil
	}})
	e.RegisterCommand("codegraph-info", sdk.CommandOptions{Description: "查看 codegraph 工作目录与上限", Handler: func(ctx sdk.Context, _ string) error {
		r.commandInfo(ctx.Cwd(), func(m, k string) { ctx.Notify(m, k) })
		return nil
	}})
	return e
}
