package pivcc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for local patch #1 (pi-vcc-0.9.0-ctx-run). Every case the patch's
// verify-eqi12.mjs asserts is reproduced here, plus the patch's own boundary
// scenarios (the three argument shapes, non-shell tools, argv filtering,
// command-name matching, dedup/reorder and the ssh remote scan).

// toolCall builds a tool_call block with the given name and args.
func toolCall(name string, args map[string]any) NormalizedBlock {
	return NormalizedBlock{Kind: KindToolCall, Name: name, Args: args}
}

// activityLines renders an activity as the [Tracked Commands] body lines joined
// with "\n", which is what verify-eqi12.mjs checks against.
func activityLines(activity TrackedCommandActivity) string {
	return strings.Join(FormatTrackedCommands(activity), "\n")
}

// entriesFor returns one command's entries in use order.
func entriesFor(activity TrackedCommandActivity, name string) []string {
	return activity.ByCommand[name]
}

// ── verify-eqi12.mjs section C: the synthetic mixed tool calls ──────────────

// TestVerifyEqi12_SyntheticMixed reproduces the verify script's section C: a
// mix of bash, ctx_run (argv/commands/command shapes) and non-shell tools.
func TestVerifyEqi12_SyntheticMixed(t *testing.T) {
	trackCommands := []string{"git", "herdr", "gh", "journalctl", "docker"}
	blocks := []NormalizedBlock{
		toolCall("bash", map[string]any{"command": "journalctl -u docker -n 5; git diff --stat"}),
		toolCall("ctx_run", map[string]any{"action": "execute", "argv": []any{"herdr", "pane", "read", "wA:p6V", "--lines", "120"}}),
		toolCall("ctx_run", map[string]any{"action": "batch", "commands": []any{map[string]any{"label": "a", "command": "gh run list --limit 3"}}}),
		toolCall("ctx_run", map[string]any{"action": "execute", "command": "cd /root/workspace/prism && git status --porcelain | head -5"}),
		toolCall("ctx_run", map[string]any{"action": "execute", "argv": []any{"git", "log", "--oneline", "-3"}}),
		toolCall("read", map[string]any{"path": "/etc/passwd"}),
		toolCall("ask_user_question", map[string]any{"questions": []any{map[string]any{"question": "git status ok?"}}}),
	}

	out := activityLines(ExtractTrackedCommands(blocks, trackCommands))

	// The five assertions verify-eqi12.mjs makes on section C.
	for _, want := range []string{
		"journalctl -u docker -n 5",          // bash path still works
		"herdr pane read wA:p6V --lines 120", // argv captured
		"gh run list --limit 3",              // batch commands captured
		"git status --porcelain",             // ctx_run.command scanned
		"git log --oneline -3",               // ctx_run.argv joined
	} {
		if !strings.Contains(out, want) {
			t.Errorf("section C output is missing %q\noutput:\n%s", want, out)
		}
	}
	if strings.Contains(out, "git status ok") {
		t.Errorf("a non-shell tool leaked into the tracked commands\noutput:\n%s", out)
	}
}

// TestVerifyEqi12_SectionBNeedAssertions reproduces the `need(...)` assertions
// the script applies to the replayed session output: those only fire when the
// newest real session carries such calls, so the port checks the equivalent
// guarantee — a replayed ctx_run batch whose command is a tracked name is
// captured, and `formatTrackedCommands` prefixes each line with the name.
func TestVerifyEqi12_TrackedLinePrefix(t *testing.T) {
	activity := ExtractTrackedCommands([]NormalizedBlock{
		toolCall("ctx_run", map[string]any{"commands": []any{"git status", "git log --oneline -3"}}),
	}, []string{"git"})
	lines := FormatTrackedCommands(activity)
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want one git line", lines)
	}
	if !strings.HasPrefix(lines[0], "git:") {
		t.Fatalf("line %q does not start with \"git:\"", lines[0])
	}
}

// ── patch boundary scenarios ────────────────────────────────────────────────

// TestCommandTextsOf_ThreeShapes pins the patch's commandTextsOf() contract
// exactly: command, argv (strings only, joined by spaces), and commands
// (strings or objects with a string command).
func TestCommandTextsOf_ThreeShapes(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want []string
	}{
		{
			name: "command only",
			args: map[string]any{"command": "git status"},
			want: []string{"git status"},
		},
		{
			name: "argv only, non-strings dropped",
			args: map[string]any{"argv": []any{"git", 7, "log", nil, "--oneline"}},
			want: []string{"git log --oneline"},
		},
		{
			name: "argv with no string elements contributes nothing",
			args: map[string]any{"argv": []any{1, 2, 3}},
			want: []string{},
		},
		{
			name: "commands with strings and objects",
			args: map[string]any{"commands": []any{"git status", map[string]any{"label": "x", "command": "gh pr list"}, 42, map[string]any{"label": "no-command"}}},
			want: []string{"git status", "gh pr list"},
		},
		{
			name: "all three shapes are collected in order",
			args: map[string]any{
				"command":  "git diff",
				"argv":     []any{"git", "log"},
				"commands": []any{"git branch"},
			},
			want: []string{"git diff", "git log", "git branch"},
		},
		{
			name: "a non-shell tool contributes nothing",
			args: map[string]any{"questions": []any{map[string]any{"question": "git status ok?"}}},
			want: []string{},
		},
		{
			name: "no arguments at all",
			args: nil,
			want: []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := commandTextsOf(test.args)
			if len(got) != len(test.want) {
				t.Fatalf("commandTextsOf = %#v, want %#v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("commandTextsOf[%d] = %q, want %q (all: %#v)", i, got[i], test.want[i], got)
				}
			}
		})
	}
}

// TestExtractTrackedCommands_NonBashToolIsScanned is the patch's core behaviour
// change: a tool that is not `bash` still contributes its command.
func TestExtractTrackedCommands_NonBashToolIsScanned(t *testing.T) {
	activity := ExtractTrackedCommands([]NormalizedBlock{
		toolCall("ctx_run", map[string]any{"command": "kubectl get pods -n prod"}),
	}, []string{"kubectl"})
	entries := entriesFor(activity, "kubectl")
	if len(entries) != 1 || entries[0] != "kubectl get pods -n prod" {
		t.Fatalf("kubectl entries = %#v, want the whole command", entries)
	}
}

// TestExtractTrackedCommands_UpstreamBashOnlyWouldMissThis is the regression pin
// for the patch: the same block under a bash-only scan (what upstream does)
// yields nothing. It documents why the patch exists.
func TestExtractTrackedCommands_UpstreamBashOnlyWouldMissThis(t *testing.T) {
	block := toolCall("ctx_run", map[string]any{"command": "git status"})
	// The upstream rule: `b.name !== "bash"` skips the block.
	if block.Name == "bash" {
		t.Fatal("test setup: the block must not be a bash call")
	}
	activity := ExtractTrackedCommands([]NormalizedBlock{block}, []string{"git"})
	if len(entriesFor(activity, "git")) != 1 {
		t.Fatal("the patched scan must see the non-bash command")
	}
}

// TestExtractTrackedCommands_CommandNameMatching pins the top-level invocation
// rules: a name must occupy a command position and be followed by whitespace.
func TestExtractTrackedCommands_CommandNameMatching(t *testing.T) {
	tests := []struct {
		name    string
		command string
		tracked string
		want    []string
	}{
		{"plain invocation", "git status", "git", []string{"git status"}},
		{"prefixed binary does not match", "ssh-keygen -t rsa", "ssh", nil},
		{"hyphenated prefix does not match", "docker-compose up", "docker", nil},
		{"quoted separator is not a boundary", `echo "a; docker restart"`, "docker", nil},
		{"unquoted separator is a boundary", "echo hi; docker restart api", "docker", []string{"docker restart api"}},
		{"pipe is a boundary", "ps aux | grep git", "git", nil},
		{"pipe boundary keeps the segment", "cat log | docker logs -f api", "docker", []string{"docker logs -f api"}},
		{"newline is a boundary", "cd /tmp\ngit diff", "git", []string{"git diff"}},
		{"VAR= prefix is skipped but kept", "FOO=1 git status", "git", []string{"FOO=1 git status"}},
		{"sudo is skipped but kept", "sudo -u root kubectl get pods", "kubectl", []string{"sudo -u root kubectl get pods"}},
		{"env with a value flag is skipped", "env -u HOME git status", "git", []string{"env -u HOME git status"}},
		{"prefix form matches a longer command", "gh pr merge 12", "gh pr", []string{"gh pr merge 12"}},
		{"prefix form does not match a sibling", "gh run list", "gh pr", nil},
		{"redirection is not a separator", "make 2>&1 | tee log", "make", []string{"make 2>&1"}},
		// A leading `&` has no left neighbour: it is a plain separator, not the
		// `2>&1` / `<&3` redirection shape (which is why the scan must not index
		// command[-1]).
		{"leading ampersand is a separator", "& git status", "git", []string{"git status"}},
		// `&>file` is a redirection even at index 0, so the `&` is not a
		// separator; the segment then starts at `&>out`, which is not a tracked
		// name, exactly as TS's `cmd[i + 1] === ">"` check reads it.
		{"leading ampersand redirection is not a separator", "&>out git status", "git", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			activity := ExtractTrackedCommands([]NormalizedBlock{
				toolCall("ctx_run", map[string]any{"command": test.command}),
			}, []string{test.tracked})
			got := entriesFor(activity, test.tracked)
			if len(got) != len(test.want) {
				t.Fatalf("entries = %#v, want %#v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("entries[%d] = %q, want %q", i, got[i], test.want[i])
				}
			}
		})
	}
}

// TestExtractTrackedCommands_ReRunMovesToTail pins the dedup-and-reorder rule:
// re-running an entry refreshes it to the end, so the cap keeps the
// most-recently-USED entries.
func TestExtractTrackedCommands_ReRunMovesToTail(t *testing.T) {
	activity := ExtractTrackedCommands([]NormalizedBlock{
		toolCall("ctx_run", map[string]any{"command": "git status"}),
		toolCall("ctx_run", map[string]any{"command": "git diff"}),
		toolCall("ctx_run", map[string]any{"command": "git status"}),
	}, []string{"git"})
	entries := entriesFor(activity, "git")
	want := []string{"git diff", "git status"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %#v, want %#v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("entries = %#v, want %#v", entries, want)
		}
	}
}

// TestExtractTrackedCommands_CapKeepsNewest pins the tail cap through
// formatTrackedCommands: eleven distinct runs keep the newest ten.
func TestExtractTrackedCommands_CapKeepsNewest(t *testing.T) {
	blocks := make([]NormalizedBlock, 0, 11)
	for i := 1; i <= 11; i++ {
		blocks = append(blocks, toolCall("ctx_run", map[string]any{"command": "docker restart api" + string(rune('a'+i-1))}))
	}
	lines := FormatTrackedCommands(ExtractTrackedCommands(blocks, []string{"docker"}))
	if len(lines) != 1 {
		t.Fatalf("lines = %#v, want one docker line", lines)
	}
	if !strings.HasPrefix(lines[0], "docker: (+1 earlier) ") {
		t.Fatalf("line %q does not carry the earlier marker right after the name", lines[0])
	}
	// The oldest entry is `docker restart apia`; the tail cap must have
	// dropped it, so its absence is what pins "keeps the NEWEST".
	if strings.Contains(lines[0], "docker restart apia") {
		t.Fatalf("the cap kept the oldest entry: %q", lines[0])
	}
	if !strings.Contains(lines[0], "docker restart apik") {
		t.Fatalf("the cap dropped the newest entry: %q", lines[0])
	}
}

// TestExtractTrackedCommands_SSHRemoteScan pins the ssh remote-command scan the
// patch keeps: commands inside an ssh invocation are tracked too.
func TestExtractTrackedCommands_SSHRemoteScan(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"quoted remote command", `ssh host "docker compose restart api"`, []string{"docker compose restart api"}},
		{"single-quoted remote command", `ssh host 'kubectl get pods'`, []string{"kubectl get pods"}},
		{"unquoted remote command", `ssh host docker ps`, []string{"docker ps"}},
		{"bool flag does not consume the host", `ssh -T host 'docker restart api'`, []string{"docker restart api"}},
		{"value flag consumes its argument", `ssh -i key-prod prod 'docker restart api'`, []string{"docker restart api"}},
		{"no remote command", `ssh host`, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			activity := ExtractTrackedCommands([]NormalizedBlock{
				toolCall("ctx_run", map[string]any{"command": test.command}),
			}, []string{"ssh", "docker", "kubectl"})
			got := entriesFor(activity, "docker")
			if test.want != nil && strings.HasPrefix(test.want[0], "docker") {
				if len(got) != len(test.want) {
					t.Fatalf("docker entries = %#v, want %#v", got, test.want)
				}
				if got[0] != test.want[0] {
					t.Fatalf("docker entries = %#v, want %#v", got, test.want)
				}
				return
			}
			kubectl := entriesFor(activity, "kubectl")
			if test.want != nil && strings.HasPrefix(test.want[0], "kubectl") {
				if len(kubectl) != len(test.want) || (len(kubectl) > 0 && kubectl[0] != test.want[0]) {
					t.Fatalf("kubectl entries = %#v, want %#v", kubectl, test.want)
				}
				return
			}
			if len(got) != 0 || len(kubectl) != 0 {
				t.Fatalf("expected no remote match, got docker=%#v kubectl=%#v", got, kubectl)
			}
		})
	}
}

// TestExtractTrackedCommands_SSHIsExcludedFromItsOwnRemoteScan pins that the
// remote scan never re-tracks "ssh" from inside its own argument: the recorded
// ssh entry is the local invocation only, and the remote command's inner
// command is tracked once.
func TestExtractTrackedCommands_SSHIsExcludedFromItsOwnRemoteScan(t *testing.T) {
	// The remote command's own `ssh other` is not a top-level segment of the
	// local command, so the scan never descends into it; the docker invocation
	// it wraps is still found because it IS a top-level segment of the remote
	// string.
	activity := ExtractTrackedCommands([]NormalizedBlock{
		toolCall("ctx_run", map[string]any{"command": `ssh host "docker ps"`}),
	}, []string{"ssh", "docker"})
	if got := entriesFor(activity, "ssh"); len(got) != 1 || got[0] != `ssh host "docker ps"` {
		t.Fatalf("ssh entries = %#v, want only the local invocation", got)
	}
	if got := entriesFor(activity, "docker"); len(got) != 1 || got[0] != "docker ps" {
		t.Fatalf("docker entries = %#v, want the remote command", got)
	}
	// An ssh name inside the remote string must not add a second ssh entry.
	nested := ExtractTrackedCommands([]NormalizedBlock{
		toolCall("ctx_run", map[string]any{"command": `ssh host "ssh other docker ps"`}),
	}, []string{"ssh", "docker"})
	if got := entriesFor(nested, "ssh"); len(got) != 1 {
		t.Fatalf("nested ssh entries = %#v, want exactly the local invocation", got)
	}
}

// TestExtractTrackedCommands_DisabledByEmptyTrackList pins that an empty
// trackCommands is a no-op.
func TestExtractTrackedCommands_DisabledByEmptyTrackList(t *testing.T) {
	activity := ExtractTrackedCommands([]NormalizedBlock{
		toolCall("ctx_run", map[string]any{"command": "git status"}),
	}, nil)
	if len(FormatTrackedCommands(activity)) != 0 {
		t.Fatalf("an empty trackCommands rendered lines: %#v", FormatTrackedCommands(activity))
	}
}

// TestExtractTrackedCommands_OrderFollowsConfig pins that rendering order is the
// configured order, not a map iteration order.
func TestExtractTrackedCommands_OrderFollowsConfig(t *testing.T) {
	activity := ExtractTrackedCommands([]NormalizedBlock{
		toolCall("ctx_run", map[string]any{"command": "docker ps"}),
		toolCall("ctx_run", map[string]any{"command": "git status"}),
	}, []string{"git", "docker"})
	lines := FormatTrackedCommands(activity)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "git:") || !strings.HasPrefix(lines[1], "docker:") {
		t.Fatalf("lines = %#v, want git then docker", lines)
	}
}

// TestExtractTrackedCommands_CommentQuoteIsNotAQuote pins the tokenizer's
// comment rule: an apostrophe inside a comment does not open a quote.
func TestExtractTrackedCommands_CommentQuoteIsNotAQuote(t *testing.T) {
	activity := ExtractTrackedCommands([]NormalizedBlock{
		toolCall("ctx_run", map[string]any{"command": "# it's fine\ngit status"}),
	}, []string{"git"})
	if got := entriesFor(activity, "git"); len(got) != 1 || got[0] != "git status" {
		t.Fatalf("entries = %#v, want the git command after the comment line", got)
	}
}

// TestExtractTrackedCommands_SessionFileReplay is the closest Go analogue of the
// verify script's section B: replay every toolCall of a session JSONL file and
// check the tracked commands come out.
func TestExtractTrackedCommands_SessionFileReplay(t *testing.T) {
	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.jsonl")
	lines := []string{
		`{"type":"message","id":"e1","message":{"role":"assistant","content":[{"type":"toolCall","name":"ctx_run","arguments":{"action":"execute","argv":["git","log","--oneline","-3"]}}]}}`,
		`{"type":"message","id":"e2","message":{"role":"assistant","content":[{"type":"toolCall","name":"ctx_run","arguments":{"action":"execute","command":"cd /root/x && git status --porcelain | head -5"}}]}}`,
		`{"type":"message","id":"e3","message":{"role":"assistant","content":[{"type":"toolCall","name":"ctx_run","arguments":{"action":"batch","commands":[{"label":"a","command":"gh run list --limit 3"}]}}]}}`,
		`{"type":"message","id":"e4","message":{"role":"assistant","content":[{"type":"toolCall","name":"read","arguments":{"path":"/etc/passwd"}}]}}`,
	}
	if err := os.WriteFile(sessionFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	blocks := replayToolCalls(t, sessionFile)
	if len(blocks) != 4 {
		t.Fatalf("replayed %d tool calls, want 4", len(blocks))
	}
	out := activityLines(ExtractTrackedCommands(blocks, []string{"git", "gh", "herdr"}))
	for _, want := range []string{"git log --oneline -3", "git status --porcelain", "gh run list --limit 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("replay output is missing %q\noutput:\n%s", want, out)
		}
	}
}

// replayToolCalls is verify-eqi12.mjs's `walk()`, run over the session file: it
// collects every toolCall block in file order.
func replayToolCalls(t *testing.T, sessionFile string) []NormalizedBlock {
	t.Helper()
	var blocks []NormalizedBlock
	_, err := ForEachJSONLLine(sessionFile, func(line []byte) {
		if !strings.Contains(string(line), `"toolCall"`) {
			return
		}
		entry, ok := decodeJSONObject(line)
		if !ok {
			return
		}
		message, _ := entry["message"].(map[string]any)
		if message == nil {
			return
		}
		for _, part := range toolCallParts(message["content"]) {
			name, _ := part["name"].(string)
			args, _ := part["arguments"].(map[string]any)
			blocks = append(blocks, NormalizedBlock{Kind: KindToolCall, Name: name, Args: args})
		}
	})
	if err != nil {
		t.Fatalf("replay session file: %v", err)
	}
	return blocks
}
