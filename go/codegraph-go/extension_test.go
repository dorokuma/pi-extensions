package codegraphgo

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func notifyRecorder() (func(string, string), *[]string) {
	out := []string{}
	return func(m, k string) { out = append(out, k+":"+m) }, &out
}
func TestRuntimeLifecycleAndCommands(t *testing.T) {
	h := isolated(t)
	r := newRuntime()
	registered, spawns := 0, 0
	r.register = func() { registered++ }
	r.startAt = func(w, n string) (*CodeGraphClient, error) {
		spawns++
		c := newCodeGraphClient(w, n)
		c.tools = []MCPTool{{Name: "codegraph"}}
		c.serverInstructions = "instructions"
		return c, nil
	}
	notify, messages := notifyRecorder()
	if r.beforeAgentStart("base") != nil {
		t.Fatal("nil decision")
	}
	if c, msg := r.getClient(); c != nil || !strings.Contains(msg, "尚无会话决策") {
		t.Fatal(msg)
	}
	r.commandInfo(h, notify)
	if !strings.Contains((*messages)[0], "运行中: 否") || !strings.Contains((*messages)[0], "搜索默认/上限: 70/120") {
		t.Fatal(*messages)
	}
	r.commandStop(notify)
	if (*messages)[1] != "info:codegraph-go 未运行" {
		t.Fatal(*messages)
	}
	r.sessionStart(h, notify)
	if spawns != 0 || registered != 1 || !strings.HasPrefix((*messages)[2], "info:codegraph-go 可用（按需）：") {
		t.Fatal(*messages)
	}
	prompt := r.beforeAgentStart("base").(map[string]any)["systemPrompt"].(string)
	if !strings.Contains(prompt, "家目录模式已开") || strings.Contains(prompt, "server instructions") {
		t.Fatal(prompt)
	}
	c, msg := r.getClient()
	if c == nil || msg != "" || spawns != 1 {
		t.Fatal(msg)
	}
	if got := r.beforeAgentStart("base").(map[string]any)["systemPrompt"].(string); !strings.Contains(got, "## CodeGraph server instructions\n\ninstructions") {
		t.Fatal(got)
	}
	r.commandStart(h, notify)
	if !strings.Contains((*messages)[3], "已在运行:") || spawns != 1 {
		t.Fatal(*messages)
	}
	r.commandInfo(h, notify)
	if !strings.Contains((*messages)[4], "运行中: "+h) || !strings.Contains((*messages)[4], "WORKDIR 环境变量: （未设）") {
		t.Fatal(*messages)
	}
	r.commandStop(notify)
	if r.client != nil || r.decision == nil || (*messages)[5] != "info:已停止" {
		t.Fatal("stop")
	}
	r.commandStart(h, notify)
	if spawns != 2 || !strings.Contains((*messages)[6], "，1 个工具") {
		t.Fatal(*messages)
	}
	p := filepath.Join(h, "p")
	t.Setenv("CODEGRAPH_GO_WORKDIR", p)
	r.sessionStart(h, notify)
	if r.client != nil || registered != 1 || r.decision.Workdir != p {
		t.Fatal("new workdir")
	}
	prompt = r.beforeAgentStart("base").(map[string]any)["systemPrompt"].(string)
	if !strings.Contains(prompt, "当前索引根就是单个项目目录") {
		t.Fatal(prompt)
	}
	r.commandStart(h, notify)
	if r.client == nil {
		t.Fatal("manual start")
	}
	r.shutdown()
	if r.client != nil {
		t.Fatal("shutdown")
	}
	t.Setenv("CODEGRAPH_GO_WORKDIR", "/opt/outside")
	r.sessionStart(h, notify)
	if r.decision.OK || !strings.HasPrefix((*messages)[len(*messages)-1], "warning:codegraph-go 不可用:") {
		t.Fatal(*messages)
	}
	prompt = r.beforeAgentStart("base").(map[string]any)["systemPrompt"].(string)
	if !strings.Contains(prompt, "本会话 CodeGraph 未启用:") {
		t.Fatal(prompt)
	}
	if c, msg = r.getClient(); c != nil || !strings.Contains(msg, "授权根") {
		t.Fatal(msg)
	}
	r.commandStart(h, notify)
	if !strings.HasPrefix((*messages)[len(*messages)-1], "error:启动失败:") {
		t.Fatal(*messages)
	}
	r.commandInfo(h, notify)
	if !strings.Contains((*messages)[len(*messages)-1], "解析: 拒绝 —") {
		t.Fatal(*messages)
	}
	t.Setenv("CODEGRAPH_GO_WORKDIR", h)
	r.startAt = func(string, string) (*CodeGraphClient, error) { return nil, errors.New("fixture fail") }
	r.sessionStart(h, notify)
	if c, msg = r.getClient(); c != nil || msg != "codegraph-go 启动失败: fixture fail" {
		t.Fatal(msg)
	}
	r.commandStart(h, notify)
	if (*messages)[len(*messages)-1] != "error:启动失败: fixture fail" {
		t.Fatal(*messages)
	}
	result, err := r.execute(map[string]any{"action": "search"})
	if err != nil || !strings.Contains(result.(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "启动失败") {
		t.Fatal(result, err)
	}
}
func TestRuntimeLazyConcurrentAndDecisionFrozen(t *testing.T) {
	c, count := helperClient(t)
	r := newRuntime()
	r.register = func() {}
	r.decision = &WorkdirDecision{OK: true, Workdir: "/decided-root", Note: "fixture note"}
	var acquisitions atomic.Int32
	r.startAt = func(w, n string) (*CodeGraphClient, error) {
		acquisitions.Add(1)
		if w != "/decided-root" || n != "fixture note" {
			return nil, errors.New("M4 root changed")
		}
		if err := c.start(); err != nil {
			return nil, err
		}
		return c, nil
	}
	t.Setenv("CODEGRAPH_GO_WORKDIR", "/must-not-reconsult")
	var wg sync.WaitGroup
	errs := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			got, msg := r.getClient()
			if got != c || msg != "" {
				errs <- msg
			}
		})
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
	if acquisitions.Load() != 1 || count.Load() != 1 {
		t.Fatal("duplicate lazy spawn")
	}
	result, err := r.execute(map[string]any{"action": "search"})
	if err != nil || !strings.Contains(result.(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "max=70") {
		t.Fatal(result, err)
	}
	r.shutdown()
}
func TestRuntimeToolError(t *testing.T) {
	c, _ := helperClient(t, "error")
	r := newRuntime()
	r.client = c
	result, err := r.execute(map[string]any{"action": "search"})
	text := result.(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if err != nil || text != "codegraph error: invalid argument" {
		t.Fatal(result, err)
	}
}
func TestProjectHints(t *testing.T) {
	h := isolated(t)
	for i := 0; i < 12; i++ {
		writeFixture(t, filepath.Join(h, "project"+strconv.Itoa(i), "go.mod"), "module fixture")
	}
	r := newRuntime()
	r.register = func() {}
	notify, msg := notifyRecorder()
	r.sessionStart(h, notify)
	if !strings.Contains((*msg)[0], " | 项目: ") || !strings.HasSuffix((*msg)[0], "…") {
		t.Fatal(*msg)
	}
	r.commandInfo(h, notify)
	if !strings.Contains((*msg)[1], "家目录项目: ") {
		t.Fatal(*msg)
	}
}
func TestToolSchema(t *testing.T) {
	def := toolDefinition()
	if def.Name != "codegraph" || def.Label != "CodeGraph" || def.PromptSnippet != "Code search & call-graph analysis via unified action router" || len(def.PromptGuidelines) != 10 {
		t.Fatal(def)
	}
	props := def.Parameters["properties"].(map[string]any)
	actions := props["action"].(map[string]any)["enum"].([]string)
	if !reflect.DeepEqual(actions, []string{"search", "files", "explore", "callees", "callers", "impact", "node", "status", "affected", "communities", "store_fact", "search_facts"}) || len(props) != 29 || !reflect.DeepEqual(def.Parameters["required"], []string{"action"}) {
		t.Fatal(props)
	}
	for n, p := range props {
		if p.(map[string]any)["description"] == "" {
			t.Fatal(n)
		}
	}
}

// sdkFrames implements the SDK's uint32 big-endian length-prefixed JSON wire,
// distinct from the MCP server's newline-delimited JSON transport.
type sdkFrames struct{ conn net.Conn }

func (f sdkFrames) Encode(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	frame := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	_, err = f.conn.Write(append(frame, body...))
	return err
}
func (f sdkFrames) Decode(value any) error {
	var header [4]byte
	if _, err := io.ReadFull(f.conn, header[:]); err != nil {
		return err
	}
	body := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(f.conn, body); err != nil {
		return err
	}
	return json.Unmarshal(body, value)
}

// The SDK IPC harness invokes the real registered callbacks and services late
// registerTool/ui.notify calls. No reflection or direct callback substitution.
func TestSDKLifecycleRegistration(t *testing.T) {
	h := isolated(t)
	host, child := net.Pipe()
	defer host.Close()
	e := Extension()
	done := make(chan error, 1)
	go func() { done <- e.RunWithConn(child) }()
	host.SetDeadline(time.Now().Add(5 * time.Second))
	enc, dec := sdkFrames{host}, sdkFrames{host}
	var frame map[string]any
	if err := dec.Decode(&frame); err != nil {
		t.Fatal(err)
	}
	reg := frame["register"].(map[string]any)
	if reg["name"] != "codegraph-go" || reg["tools"] != nil || len(reg["commands"].([]any)) != 3 || len(reg["handlers"].([]any)) != 3 {
		t.Fatal(reg)
	}
	if err := enc.Encode(map[string]any{"type": "ready", "ready": map[string]any{"cwd": h, "mode": "rpc"}}); err != nil {
		t.Fatal(err)
	}
	request := func(id string, req map[string]any) map[string]any {
		t.Helper()
		if err := enc.Encode(map[string]any{"type": "request", "id": id, "request": req}); err != nil {
			t.Fatal(err)
		}
		for {
			var f map[string]any
			if err := dec.Decode(&f); err != nil {
				t.Fatal(err)
			}
			switch f["type"] {
			case "call":
				call := f["call"].(map[string]any)
				if call["method"] == "registerTool" {
					args := call["args"].(map[string]any)
					if args["name"] != "codegraph" || len(args["prompt_guidelines"].([]any)) != 10 {
						t.Fatal(args)
					}
				}
				if err := enc.Encode(map[string]any{"type": "call_result", "id": f["id"], "call_result": map[string]any{"result": nil}}); err != nil {
					t.Fatal(err)
				}
			case "response":
				if f["id"] == id {
					resp := f["response"].(map[string]any)
					if resp["error"] != nil {
						t.Fatal(resp)
					}
					return resp
				}
			}
		}
	}
	request("1", map[string]any{"method": "event", "event": "session_start", "handler_id": 1, "args": map[string]any{}})
	resp := request("2", map[string]any{"method": "event", "event": "before_agent_start", "handler_id": 2, "args": map[string]any{"systemPrompt": "base", "systemPromptOptions": map[string]any{"sections": map[string]any{}}}})
	if prompt := resp["result"].(map[string]any)["_pigPromptResult"].(map[string]any)["systemPrompt"].(string); !strings.HasPrefix(prompt, "base\n\n") || !strings.Contains(prompt, "索引根:") {
		t.Fatal(resp)
	}
	request("3", map[string]any{"method": "command", "tool": "codegraph-info", "args": ""})
	request("4", map[string]any{"method": "command", "tool": "codegraph-stop", "args": ""})
	request("5", map[string]any{"method": "event", "event": "session_shutdown", "handler_id": 3, "args": map[string]any{}})
	if err := enc.Encode(map[string]any{"type": "shutdown"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
