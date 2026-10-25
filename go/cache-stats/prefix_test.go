package cachestats

import (
	"math"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

// Ports test/prefix-diagnostics.mjs and the prefix-r9/r11/r12/r14 suites. The
// TS suites drive the extension through mock ctx objects; the Go suite drives
// observePrefixDiagnostics with explicit scope identities and reads the same
// command output the TS suites assert on.

// scope builds a complete comparison identity.
func scope(session, provider, api, id, baseURL string) scopeIdentity {
	return scopeIdentity{
		provider: strPtr(provider),
		api:      strPtr(api),
		id:       strPtr(id),
		baseURL:  strPtr(baseURL),
		session:  strPtr(session),
	}
}

// defaultScope is the mockContext's identity (session "test-session", provider
// "audit", api "openai-completions", id "offline", baseUrl example.invalid).
func defaultScope() scopeIdentity {
	return scope("test-session", "audit", "openai-completions", "offline", "https://example.invalid/v1")
}

// toolOf builds an OpenAI-style function tool.
func toolOf(name, description string) map[string]any {
	return map[string]any{
		"type":     "function",
		"function": map[string]any{"name": name, "description": description, "parameters": map[string]any{"type": "object"}},
	}
}

// chatPayload builds an openai-chat payload with an optional system message.
func chatPayload(system any, tools []any, user string) map[string]any {
	messages := []any{}
	if system != nil {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	messages = append(messages, map[string]any{"role": "user", "content": user})
	payload := map[string]any{"model": "offline", "messages": messages}
	if tools != nil {
		payload["tools"] = tools
	}
	return payload
}

// responsesPayload builds an openai-responses payload.
func responsesPayload(instructions any, tools []any) map[string]any {
	input := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}}
	payload := map[string]any{"model": "offline", "input": input, "store": false, "stream": true}
	if instructions != nil {
		payload["instructions"] = instructions
	}
	if tools != nil {
		payload["tools"] = tools
	}
	return payload
}

// anthropicPayload builds an Anthropic-style payload.
func anthropicPayload(system any, tools []any) map[string]any {
	payload := map[string]any{"model": "offline", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if system != nil {
		payload["system"] = system
	}
	if tools != nil {
		payload["tools"] = tools
	}
	return payload
}

// observe runs one read-only observation under the given scope.
func observe(state *instanceState, payload any, identity scopeIdentity) {
	observePrefixDiagnostics(state, payload, identity)
}

func TestPrefixBaselineStableChangedPerShape(t *testing.T) {
	chat := func(system string) map[string]any {
		return chatPayload(system, []any{toolOf("read", "d")}, "user")
	}
	responses := func(instructions string) map[string]any {
		return responsesPayload(instructions, []any{toolOf("read", "d")})
	}
	anthropic := func(system string) map[string]any {
		return anthropicPayload(system, []any{map[string]any{"name": "read", "description": "d"}})
	}
	cases := []struct {
		name    string
		shape   prefixShape
		payload func(string) map[string]any
	}{
		{"openai-chat", shapeOpenAIChat, chat},
		{"openai-responses", shapeOpenAIResponses, responses},
		{"anthropic", shapeAnthropic, anthropic},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			state := createInstanceState(true)
			observe(state, testCase.payload("V1"), defaultScope())
			requireContains(t, joinedPrefix(state), "Status: baseline")
			requireContains(t, joinedPrefix(state), "Shape: "+string(testCase.shape))
			observe(state, testCase.payload("V1"), defaultScope())
			requireContains(t, joinedPrefix(state), "Status: stable")
			observe(state, testCase.payload("V2"), defaultScope())
			requireContains(t, joinedPrefix(state), "Status: changed")
		})
	}
}

func TestPrefixShapeFromStructureNotVendor(t *testing.T) {
	// The model api says anthropic, the payload is chat: the shape follows the
	// payload structure.
	state := createInstanceState(true)
	identity := scope("s", "anthropic", "anthropic-messages", "offline", "https://example.invalid")
	observe(state, chatPayload("S", []any{toolOf("read", "d")}, "u"), identity)
	text := joinedPrefix(state)
	requireContains(t, text, "Shape: openai-chat")
	requireContains(t, text, "system: present")
	requireContains(t, text, "tools: present count=1")
}

func TestPrefixSectionFormattingAndToolCount(t *testing.T) {
	state := createInstanceState(true)
	observe(state, chatPayload("SYS_V1", []any{toolOf("read", "d"), toolOf("bash", "d")}, "u"), defaultScope())
	text := joinedPrefix(state)
	requireContains(t, text, "Status: baseline")
	requireContains(t, text, "Shape: openai-chat")
	requireContains(t, text, "system: present chars=6")
	requireContains(t, text, "tools: present count=2")
	requireNotContains(t, text, "saved tokens")
	requireNotContains(t, text, "aggregate hit")

	stats := joinedStats(state, nil)
	requireContains(t, stats, "Prefix: baseline shape=openai-chat")
	requireContains(t, stats, "State: enabled")
}

func TestPrefixUnknownAndMissingShapes(t *testing.T) {
	// A payload with no recognizable fields is unknown.
	state := createInstanceState(true)
	observe(state, map[string]any{"foo": float64(1), "nested": map[string]any{"a": true}}, defaultScope())
	text := joinedPrefix(state)
	requireContains(t, text, "Status: unknown")
	requireContains(t, text, "Shape: unknown")

	// Chat messages without a system message: missing sections, not stable.
	state2 := createInstanceState(true)
	observe(state2, map[string]any{"model": "offline", "messages": []any{map[string]any{"role": "user", "content": "u"}}}, defaultScope())
	text2 := joinedPrefix(state2)
	requireContains(t, text2, "Status: unknown")
	requireContains(t, text2, "system: missing")
	requireContains(t, text2, "tools: missing")
	requireNotContains(t, text2, "Status: stable")
	observe(state2, map[string]any{"model": "offline", "messages": []any{map[string]any{"role": "user", "content": "other"}}}, defaultScope())
	text2b := joinedPrefix(state2)
	requireContains(t, text2b, "Status: unknown")
	requireNotContains(t, text2b, "Status: stable")
}

func TestPrefixEmptySectionsAreComparable(t *testing.T) {
	state := createInstanceState(true)
	payload := map[string]any{"model": "offline", "messages": []any{map[string]any{"role": "system", "content": ""}, map[string]any{"role": "user", "content": "u"}}, "tools": []any{}}
	observe(state, payload, defaultScope())
	text := joinedPrefix(state)
	requireContains(t, text, "system: empty")
	requireContains(t, text, "tools: empty")
	requireContains(t, text, "Status: baseline")
	observe(state, chatPayload("", []any{}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: stable")
}

func TestPrefixOversizeSkipsThenRebaselines(t *testing.T) {
	state := createInstanceState(true)
	ok := strings.Repeat("S", 100000)
	huge := strings.Repeat("H", 100001)
	observe(state, chatPayload(ok, []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: baseline")
	observe(state, chatPayload(ok, []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: stable")
	observe(state, chatPayload(huge, []any{toolOf("read", "d")}, "u"), defaultScope())
	skipped := joinedPrefix(state)
	requireContains(t, skipped, "Status: skipped")
	requireNotContains(t, skipped, "Status: stable")
	observe(state, chatPayload(ok, []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: baseline")
}

func TestPrefixScopeIsolation(t *testing.T) {
	payload := chatPayload("SAME", []any{toolOf("read", "d")}, "u")

	// A distinct session is a distinct scope: both start at baseline.
	state := createInstanceState(true)
	observe(state, payload, scope("sess-A", "audit", "openai-completions", "offline", "https://example.invalid/v1"))
	observe(state, payload, scope("sess-B", "audit", "openai-completions", "offline", "https://example.invalid/v1"))
	requireContains(t, joinedPrefix(state), "Status: baseline")

	// Model id isolation.
	state = createInstanceState(true)
	observe(state, payload, scope("s", "audit", "openai-completions", "m1", "https://example.invalid/v1"))
	observe(state, payload, scope("s", "audit", "openai-completions", "m2", "https://example.invalid/v1"))
	requireContains(t, joinedPrefix(state), "Status: baseline")

	// Endpoint isolation: same host, different path.
	state = createInstanceState(true)
	observe(state, payload, scope("s", "audit", "openai-completions", "offline", "https://api.example.invalid/api/paas/v4"))
	observe(state, payload, scope("s", "audit", "openai-completions", "offline", "https://api.example.invalid/api/coding/paas/v4"))
	requireContains(t, joinedPrefix(state), "Status: baseline")

	// Query routing isolation.
	state = createInstanceState(true)
	observe(state, payload, scope("s", "audit", "openai-completions", "offline", "https://api.example.invalid/v1?route=alpha"))
	observe(state, payload, scope("s", "audit", "openai-completions", "offline", "https://api.example.invalid/v1?route=beta"))
	requireContains(t, joinedPrefix(state), "Status: baseline")

	// Port and protocol isolation.
	state = createInstanceState(true)
	observe(state, payload, scope("s", "audit", "openai-completions", "offline", "https://api.example.invalid/v1"))
	observe(state, payload, scope("s", "audit", "openai-completions", "offline", "https://api.example.invalid:8443/v1"))
	requireContains(t, joinedPrefix(state), "Status: baseline")
	observe(state, payload, scope("s", "audit", "openai-completions", "offline", "http://api.example.invalid/v1"))
	requireContains(t, joinedPrefix(state), "Status: baseline")

	// The host and URL never reach the output.
	shown := joinedPrefix(state)
	requireNotContains(t, shown, "example.invalid")
	requireNotContains(t, shown, "8443")
}

func TestPrefixScopeMissingFieldsSkip(t *testing.T) {
	cases := []struct {
		name     string
		identity scopeIdentity
	}{
		{"missing provider", scopeIdentity{api: strPtr("a"), id: strPtr("i"), baseURL: strPtr("b"), session: strPtr("s")}},
		{"empty provider", scopeIdentity{provider: strPtr(""), api: strPtr("a"), id: strPtr("i"), baseURL: strPtr("b"), session: strPtr("s")}},
		{"missing api", scopeIdentity{provider: strPtr("p"), id: strPtr("i"), baseURL: strPtr("b"), session: strPtr("s")}},
		{"missing id", scopeIdentity{provider: strPtr("p"), api: strPtr("a"), baseURL: strPtr("b"), session: strPtr("s")}},
		{"missing baseUrl", scopeIdentity{provider: strPtr("p"), api: strPtr("a"), id: strPtr("i"), session: strPtr("s")}},
		{"empty baseUrl", scopeIdentity{provider: strPtr("p"), api: strPtr("a"), id: strPtr("i"), baseURL: strPtr(""), session: strPtr("s")}},
		{"missing session", scopeIdentity{provider: strPtr("p"), api: strPtr("a"), id: strPtr("i"), baseURL: strPtr("b")}},
		{"overlong session", scopeIdentity{provider: strPtr("p"), api: strPtr("a"), id: strPtr("i"), baseURL: strPtr("b"), session: strPtr(strings.Repeat("S", 16385))}},
		{"overlong baseUrl", scopeIdentity{provider: strPtr("p"), api: strPtr("a"), id: strPtr("i"), baseURL: strPtr("https://ok.invalid/" + strings.Repeat("U", 20000)), session: strPtr("s")}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			state := createInstanceState(true)
			observe(state, chatPayload("SYS_A", []any{toolOf("read", "d")}, "u"), testCase.identity)
			requireContains(t, joinedPrefix(state), "Status: skipped")
			// Recovery: a complete scope is a fresh baseline.
			observe(state, chatPayload("SYS_A", []any{toolOf("read", "d")}, "u"), defaultScope())
			requireContains(t, joinedPrefix(state), "Status: baseline")
		})
	}
}

func TestPrefixMixedExclusiveFieldsAreUnknown(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"instructions + messages", map[string]any{
			"model": "offline", "instructions": "INSTR_KEEP",
			"messages": []any{map[string]any{"role": "system", "content": "S1"}}, "tools": []any{toolOf("read", "d")},
		}},
		{"input array + messages", map[string]any{
			"model":    "offline",
			"input":    []any{map[string]any{"role": "system", "content": "A"}},
			"messages": []any{map[string]any{"role": "system", "content": "A-msg"}},
		}},
		{"input string + messages", map[string]any{
			"model": "offline", "input": "user text",
			"messages": []any{map[string]any{"role": "system", "content": "A"}},
		}},
		{"input null + messages", map[string]any{
			"model": "offline", "input": nil,
			"messages": []any{map[string]any{"role": "system", "content": "A"}},
		}},
		{"input undefined own + messages", map[string]any{
			"model": "offline", "input": nil,
			"messages": []any{map[string]any{"role": "system", "content": "A"}},
		}},
		{"system array + instructions", map[string]any{
			"model": "offline", "instructions": "X",
			"system": []any{map[string]any{"type": "text", "text": "S"}},
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			state := createInstanceState(true)
			observe(state, testCase.payload, defaultScope())
			text := joinedPrefix(state)
			requireContains(t, text, "Status: unknown")
			requireContains(t, text, "Shape: unknown")
			requireNotContains(t, text, "Status: baseline")
		})
	}
	// An unknown mix then a valid chat payload is a baseline, not shape-changed.
	state := createInstanceState(true)
	observe(state, map[string]any{"model": "offline", "instructions": "I",
		"messages": []any{map[string]any{"role": "system", "content": "S1"}}}, defaultScope())
	requireContains(t, joinedPrefix(state), "Status: unknown")
	observe(state, chatPayload("S1", []any{toolOf("read", "d")}, "u"), defaultScope())
	next := joinedPrefix(state)
	requireContains(t, next, "Status: baseline")
	requireNotContains(t, next, "Status: changed")
}

func TestPrefixCrossShapeChange(t *testing.T) {
	state := createInstanceState(true)
	chat := func() map[string]any {
		return map[string]any{"model": "offline", "messages": []any{map[string]any{"role": "system", "content": "A"}}, "tools": []any{}}
	}
	responses := func() map[string]any {
		return map[string]any{"model": "offline", "input": []any{map[string]any{"role": "system", "content": "A"}}, "tools": []any{}}
	}
	observe(state, chat(), defaultScope())
	requireContains(t, joinedPrefix(state), "Shape: openai-chat")
	observe(state, chat(), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: stable")
	observe(state, responses(), defaultScope())
	text := joinedPrefix(state)
	requireContains(t, text, "Status: changed")
	requireContains(t, text, "Shape: openai-responses")
	observe(state, responses(), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: stable")
	observe(state, chat(), defaultScope())
	text = joinedPrefix(state)
	requireContains(t, text, "Status: changed")
	requireContains(t, text, "Shape: openai-chat")
}

func TestPrefixRoleBagStructureSensitivity(t *testing.T) {
	twoSystem := map[string]any{"model": "offline", "tools": []any{toolOf("read", "d")},
		"messages": []any{
			map[string]any{"role": "system", "content": "A"},
			map[string]any{"role": "system", "content": "B"},
			map[string]any{"role": "user", "content": "u"},
		}}
	joined := map[string]any{"model": "offline", "tools": []any{toolOf("read", "d")},
		"messages": []any{
			map[string]any{"role": "system", "content": "A\nB"},
			map[string]any{"role": "user", "content": "u"},
		}}
	state := createInstanceState(true)
	observe(state, twoSystem, defaultScope())
	requireContains(t, joinedPrefix(state), "Status: baseline")
	observe(state, map[string]any{"model": "offline", "tools": []any{toolOf("read", "d")},
		"messages": []any{
			map[string]any{"role": "system", "content": "A"},
			map[string]any{"role": "system", "content": "B"},
			map[string]any{"role": "user", "content": "u"},
		}}, defaultScope())
	requireContains(t, joinedPrefix(state), "Status: stable")
	observe(state, joined, defaultScope())
	requireContains(t, joinedPrefix(state), "Status: changed")

	// Relative position swap of system and user.
	ordered := map[string]any{"model": "offline", "tools": []any{toolOf("read", "d")},
		"messages": []any{map[string]any{"role": "system", "content": "SYS"}, map[string]any{"role": "user", "content": "u"}}}
	swapped := map[string]any{"model": "offline", "tools": []any{toolOf("read", "d")},
		"messages": []any{map[string]any{"role": "user", "content": "u"}, map[string]any{"role": "system", "content": "SYS"}}}
	state = createInstanceState(true)
	observe(state, ordered, defaultScope())
	requireContains(t, joinedPrefix(state), "Status: baseline")
	observe(state, swapped, defaultScope())
	requireContains(t, joinedPrefix(state), "Status: changed")

	// system/developer relative order.
	state = createInstanceState(true)
	observe(state, map[string]any{"model": "offline", "tools": []any{toolOf("read", "d")},
		"messages": []any{map[string]any{"role": "system", "content": "S"}, map[string]any{"role": "developer", "content": "D"}}}, defaultScope())
	observe(state, map[string]any{"model": "offline", "tools": []any{toolOf("read", "d")},
		"messages": []any{map[string]any{"role": "developer", "content": "D"}, map[string]any{"role": "system", "content": "S"}}}, defaultScope())
	requireContains(t, joinedPrefix(state), "Status: changed")

	// String content vs a same-text block array.
	state = createInstanceState(true)
	observe(state, chatPayload("HELLO", []any{toolOf("read", "d")}, "u"), defaultScope())
	observe(state, chatPayload([]any{map[string]any{"type": "text", "text": "HELLO"}}, []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: changed")

	// Block split / merge.
	state = createInstanceState(true)
	observe(state, chatPayload([]any{map[string]any{"type": "text", "text": "AB"}}, []any{toolOf("read", "d")}, "u"), defaultScope())
	observe(state, chatPayload([]any{map[string]any{"type": "text", "text": "A"}, map[string]any{"type": "text", "text": "B"}}, []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: changed")

	// Block type boundary.
	state = createInstanceState(true)
	observe(state, chatPayload([]any{map[string]any{"type": "text", "text": "Z"}}, []any{toolOf("read", "d")}, "u"), defaultScope())
	observe(state, chatPayload([]any{map[string]any{"type": "input_text", "text": "Z"}}, []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: changed")

	// Empty string vs empty text block.
	state = createInstanceState(true)
	observe(state, chatPayload("", []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "system: empty")
	observe(state, chatPayload([]any{map[string]any{"type": "text", "text": ""}}, []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: changed")
}

func TestPrefixTailHistoryAndUserBodyStayStable(t *testing.T) {
	state := createInstanceState(true)
	base := map[string]any{"model": "offline", "tools": []any{toolOf("read", "d")},
		"messages": []any{map[string]any{"role": "system", "content": "SYS"}, map[string]any{"role": "user", "content": "u1"}}}
	tailed := map[string]any{"model": "offline", "tools": []any{toolOf("read", "d")},
		"messages": []any{
			map[string]any{"role": "system", "content": "SYS"},
			map[string]any{"role": "user", "content": "u1"},
			map[string]any{"role": "assistant", "content": "a1"},
			map[string]any{"role": "user", "content": "u2"},
		}}
	observe(state, base, defaultScope())
	requireContains(t, joinedPrefix(state), "Status: baseline")
	observe(state, tailed, defaultScope())
	requireContains(t, joinedPrefix(state), "Status: stable")

	// A non-target body change stays stable.
	observe(state, chatPayload("SYS", []any{toolOf("read", "d")}, "one"), defaultScope())
	observe(state, chatPayload("SYS", []any{toolOf("read", "d")}, "two"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: stable")
}

func TestPrefixToolChangesAreVisible(t *testing.T) {
	state := createInstanceState(true)
	observe(state, chatPayload("S", []any{toolOf("read", "d1")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: baseline")
	observe(state, chatPayload("S", []any{toolOf("read", "d2")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: changed")

	// Tool array order is visible.
	state = createInstanceState(true)
	toolA := toolOf("read", "a")
	toolB := toolOf("bash", "b")
	observe(state, chatPayload("S", []any{toolA, toolB}, "u"), defaultScope())
	observe(state, chatPayload("S", []any{toolA, toolB}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: stable")
	observe(state, chatPayload("S", []any{toolB, toolA}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: changed")
}

func TestPrefixExtrasRecognition(t *testing.T) {
	unique := strings.Join([]string{
		"HEAD",
		"<project_instructions>",
		"PROJ_ONLY",
		"</project_instructions>",
		"<available_skills>",
		"  <skill><name>a</name><description>d</description><location>/p/SKILL.md</location></skill>",
		"</available_skills>",
		"TAIL",
	}, "\n")
	state := createInstanceState(true)
	observe(state, chatPayload(unique, []any{toolOf("read", "d")}, "u"), defaultScope())
	text := joinedPrefix(state)
	requireContains(t, text, "Status: baseline")
	requireContains(t, text, "project_rules: present chars=")
	requireContains(t, text, "skills_index: present chars=")
	requireNotContains(t, text, "PROJ_ONLY")
	requireNotContains(t, text, "SKILL.md")
	observe(state, chatPayload(unique, []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: stable")

	// Recognized with attributes, content never shown.
	state = createInstanceState(true)
	observe(state, chatPayload(`<project_instructions extra="x">ATTR_BODY</project_instructions>`, []any{toolOf("read", "d")}, "u"), defaultScope())
	text = joinedPrefix(state)
	requireContains(t, text, "project_rules: present chars=")
	requireNotContains(t, text, "ATTR_BODY")

	// No tags: unidentified, the core still diagnoses.
	state = createInstanceState(true)
	observe(state, chatPayload("no tags here", []any{toolOf("read", "d")}, "u"), defaultScope())
	text = joinedPrefix(state)
	requireContains(t, text, "project_rules: unidentified")
	requireContains(t, text, "skills_index: unidentified")
	requireContains(t, text, "Status: baseline")
}

func TestPrefixExtrasUnidentifiedForms(t *testing.T) {
	cases := map[string]string{
		"dotted project tag":      "<project_instructions.fake>RULES</project_instructions>",
		"colon project tag":       "<project_instructions:ns>RULES</project_instructions>",
		"non-ascii project tag":   "<project_instructions中>RULES</project_instructions>",
		"plus project tag":        "<project_instructions+x>RULES</project_instructions>",
		"dotted skills tag":       "<available_skills.fake>SKILLS</available_skills>",
		"approximate tag":         "<project_instruction>not the tag</project_instruction>",
		"missing gt":              "<project_instructions" + "\nmissing-gt and padding " + strings.Repeat("x", 50),
		"nested opens":            "<project_instructions>outer <project_instructions>inner</project_instructions>",
		"multiple complete":       "<project_instructions>one</project_instructions>\n<project_instructions>two</project_instructions>",
		"leading isolated close":  "</project_instructions><project_instructions>\nX\n</project_instructions>",
		"trailing isolated close": "<project_instructions>\nX\n</project_instructions></project_instructions>",
		"unclosed open run":       strings.Repeat(" <project_instructions>", 40) + " no close here",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			state := createInstanceState(true)
			observe(state, chatPayload(body, []any{toolOf("read", "d")}, "u"), defaultScope())
			text := joinedPrefix(state)
			requireContains(t, text, "project_rules: unidentified")
			requireContains(t, text, "Status: baseline")
			requireContains(t, text, "system: present")
		})
	}
	// A valid block followed by an unclosed tail is unidentified too.
	state := createInstanceState(true)
	body := "<project_instructions>\nX\n</project_instructions>\n" + strings.Repeat("<project_instructions>", 12) + " dangling"
	observe(state, chatPayload(body, []any{toolOf("read", "d")}, "u"), defaultScope())
	text := joinedPrefix(state)
	requireContains(t, text, "project_rules: unidentified")
	requireNotContains(t, text, "Status: skipped")
	requireContains(t, text, "system: present")
}

func TestPrefixBudgetGuards(t *testing.T) {
	schemaTool := func(properties map[string]any) map[string]any {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read", "description": "d",
				"parameters": map[string]any{"type": "object", "properties": properties},
			},
		}
	}
	// A single overlong schema field name skips and stays skipped on repeat.
	longKey := strings.Repeat("K", 100001)
	state := createInstanceState(true)
	payload := chatPayload("SYS_OK", []any{schemaTool(map[string]any{longKey: map[string]any{"type": "string"}})}, "u")
	observe(state, payload, defaultScope())
	requireContains(t, joinedPrefix(state), "Status: skipped")
	observe(state, payload, defaultScope())
	text := joinedPrefix(state)
	requireContains(t, text, "Status: skipped")
	requireNotContains(t, text, "Status: stable")

	// Cumulative field names over the total char budget skip.
	state = createInstanceState(true)
	properties := map[string]any{}
	for i := 0; i < 3; i++ {
		properties["F"+string(rune('0'+i))+"_"+strings.Repeat("X", 90000)] = map[string]any{"type": "string"}
	}
	observe(state, chatPayload("SYS_OK", []any{schemaTool(properties)}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: skipped")

	// Depth over the cap skips.
	state = createInstanceState(true)
	var nested any = map[string]any{"leaf": "x"}
	for i := 0; i < 12; i++ {
		nested = map[string]any{"wrap": nested}
	}
	observe(state, chatPayload("SYS_OK", []any{schemaTool(map[string]any{"deep": nested})}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: skipped")

	// A wide array over the node budget skips.
	state = createInstanceState(true)
	wide := []any{}
	for i := 0; i < 2100; i++ {
		wide = append(wide, map[string]any{"k": float64(i)})
	}
	observe(state, chatPayload("SYS_OK", []any{schemaTool(map[string]any{"items": map[string]any{"type": "array", "enum": wide}})}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: skipped")
}

func TestPrefixWalkerGuards(t *testing.T) {
	// A cyclic value is unsafe, which surfaces as skipped.
	cyclic := map[string]any{"name": "loop"}
	cyclic["self"] = cyclic
	requireOptionalString(t, "cyclic", hashValue([16]byte{}, cyclic, &prefixBudget{}, map[uintptr]bool{}), nil)

	// A non-finite number is unsafe as well.
	requireOptionalString(t, "nan", hashValue([16]byte{}, []any{mathNaN()}, &prefixBudget{}, map[uintptr]bool{}), nil)

	// The walker is order-sensitive: two ordered objects with the same content
	// in a different key order fingerprint differently.
	salt := [16]byte{1, 2, 3}
	first := sdk.NewOrderedObject("a", "1", "b", "2")
	second := sdk.NewOrderedObject("b", "2", "a", "1")
	fpFirst := hashValue(salt, []any{first}, &prefixBudget{}, map[uintptr]bool{})
	fpSecond := hashValue(salt, []any{second}, &prefixBudget{}, map[uintptr]bool{})
	if fpFirst == nil || fpSecond == nil {
		t.Fatal("ordered objects must hash")
	}
	if *fpFirst == *fpSecond {
		t.Fatalf("key order must change the fingerprint: %s == %s", *fpFirst, *fpSecond)
	}
}

func mathNaN() float64 {
	var zero float64
	return zero / zero
}

func TestPrefixFingerprintLengthBoundaries(t *testing.T) {
	for _, n := range []int{65535, 65536, 65537, 100000} {
		system := strings.Repeat("S", n)
		changed := strings.Repeat("S", n-1) + "Z"
		state := createInstanceState(true)
		observe(state, chatPayload(system, []any{toolOf("read", "d")}, "u"), defaultScope())
		text := joinedPrefix(state)
		requireContains(t, text, "Status: baseline")
		requireContains(t, text, "system: present chars="+itoa(n))
		requireNotContains(t, text, "Status: skipped")
		observe(state, chatPayload(system, []any{toolOf("read", "d")}, "u"), defaultScope())
		requireContains(t, joinedPrefix(state), "Status: stable")
		observe(state, chatPayload(changed, []any{toolOf("read", "d")}, "u"), defaultScope())
		requireContains(t, joinedPrefix(state), "Status: changed")
	}
}

func TestPrefixEvictionIsBounded(t *testing.T) {
	state := createInstanceState(true)
	base := defaultScope()
	for i := 0; i < 9; i++ {
		identity := base
		modelID := "evict-" + itoa(i)
		identity.id = &modelID
		observe(state, chatPayload("S", []any{toolOf("read", "d")}, "u"), identity)
	}
	// evict-0 fell past the 8-entry cap, so it starts over at baseline.
	identity := base
	modelID := "evict-0"
	identity.id = &modelID
	observe(state, chatPayload("S", []any{toolOf("read", "d")}, "u"), identity)
	requireContains(t, joinedPrefix(state), "Status: baseline")

	// An in-cap scope keeps its snapshot: evict-8 is still stable.
	identity = base
	modelID = "evict-8"
	identity.id = &modelID
	observe(state, chatPayload("S", []any{toolOf("read", "d")}, "u"), identity)
	requireContains(t, joinedPrefix(state), "Status: stable")
}

func TestPrefixDisableAndReset(t *testing.T) {
	state := createInstanceState(true)
	observe(state, chatPayload("S", []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: baseline")
	disableRuntime(state)
	requireContains(t, joinedPrefix(state), "Status: disabled")
	enableRuntime(state)
	requireContains(t, joinedPrefix(state), "Status: n/a (no request observed)")
	observe(state, chatPayload("S", []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: baseline")

	// reset clears the comparison, so the next observation is a baseline.
	observe(state, chatPayload("S", []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: stable")
	resetRunState(state)
	requireContains(t, joinedPrefix(state), "Status: n/a (no request observed)")
	observe(state, chatPayload("S", []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: baseline")

	// The diagnostics env switch disables the feature.
	state = createInstanceState(false)
	observe(state, chatPayload("S", []any{toolOf("read", "d")}, "u"), defaultScope())
	requireContains(t, joinedPrefix(state), "Status: disabled")
	stats := joinedStats(state, nil)
	requireContains(t, stats, "Prefix: disabled")
}

func TestPrefixOutputNeverLeaksContent(t *testing.T) {
	sentinels := []string{
		"PRIV_PROMPT_CANARY_7f3a9c",
		"PRIV_TOOL_DESC_CANARY_9c1e",
		"PRIV_SESSION_ID_CANARY_bb",
		"PRIV_APIKEY_CANARY_sk-live",
		"PRIV_URL_QUERY_CANARY",
		"PRIV_USER_MSG_CANARY_dd",
	}
	state := createInstanceState(true)
	identity := scope("PRIV_SESSION_ID_CANARY_bb", "audit", "openai-completions", "offline",
		"https://example.invalid/v1/chat/completions?key=PRIV_URL_QUERY_CANARY")
	payload := chatPayload(
		"PRIV_PROMPT_CANARY_7f3a9c\n<project_instructions>PRIV_PROMPT_CANARY_7f3a9c</project_instructions>",
		[]any{toolOf("read", "PRIV_TOOL_DESC_CANARY_9c1e")},
		"PRIV_USER_MSG_CANARY_dd",
	)
	observe(state, payload, identity)
	prefix := joinedPrefix(state)
	stats := joinedStats(state, nil)
	for _, sentinel := range sentinels {
		requireNotContains(t, prefix, sentinel)
		requireNotContains(t, stats, sentinel)
	}
	requireContains(t, prefix, "project_rules: present chars=")
}

func TestEscapeForNotify(t *testing.T) {
	// C0/C1/DEL and the line separators are dropped.
	if got := escapeForNotify("a\u001bb]c\u0007d"); got != "ab]cd" {
		t.Fatalf("escapeForNotify dropped the wrong units: %q", got)
	}
	requireContains(t, escapeForNotify("audit\u009b\u001b[31m"), "audit[31m")
	requireNotContains(t, escapeForNotify("x\u2028y\u2029z"), "\u2028")
	requireNotContains(t, escapeForNotify("x\u2028y\u2029z"), "\u2029")

	// The output is capped at the notify budget plus the ellipsis.
	long := escapeForNotify(strings.Repeat("a", 400))
	if utf16Length(long) > prefixNotifyChars+1 {
		t.Fatalf("escaped length = %d, want <= %d", utf16Length(long), prefixNotifyChars+1)
	}
	if !strings.HasSuffix(long, "…") {
		t.Fatalf("escaped long text must end with an ellipsis: %q", long)
	}
}

func TestPrefixControlCharactersInStats(t *testing.T) {
	state := createInstanceState(true)
	state.unknown400["audit\u001b[31m/offline\u0007"] = true
	// The raw report carries the identity; the notify path sanitizes it.
	requireContains(t, strings.Join(statsLines(state, sessionAggregate{}), "\n"), "Unclassified 400 models")
	joined := notifyText(statsLines(state, sessionAggregate{}))
	for _, line := range statsLines(state, sessionAggregate{}) {
		for _, ch := range escapeForNotify(line) {
			if ch < 32 || ch == 127 || (ch >= 0x80 && ch <= 0x9f) {
				t.Fatalf("notified stats have control U+%04x in %q", ch, line)
			}
		}
	}
	requireContains(t, joined, "Unclassified 400 models")
	requireContains(t, joined, "audit[31m/offline")
}

// itoa is strconv.Itoa for the loop-shaped fixtures of this file.
func itoa(value int) string { return strconv.Itoa(value) }

// TestPadInt32Hex8MatchesJSVectors pins padInt32Hex8 against
// `(v|0).toString(16).padStart(8, "0")`. Every "want" below is what Node
// printed for the same input, so these are cross-language vectors.
//
// The defect this pins: padStart pads the *whole* signed string to eight
// characters, so a short negative keeps its sign and gains the zeros after it
// ((-1) -> "000000-1"), not a nine-character zero-extended magnitude
// ("-00000001") and not a bare "-1".
func TestPadInt32Hex8MatchesJSVectors(t *testing.T) {
	cases := []struct {
		value uint32
		want  string
	}{
		{0xffffffff, "000000-1"},  // (-1).toString(16) = "-1"
		{0xffffedcc, "000-1234"},  // (-0x1234).toString(16) = "-1234"
		{0x80000001, "-7fffffff"}, // already eight characters: no padding
		{0x80000000, "-80000000"}, // int32 min
		{0xfffffff0, "00000-10"},  // (-16).toString(16) = "-10"
		{0xffffff01, "00000-ff"},  // (-255).toString(16) = "-ff"
		{0x00000000, "00000000"},  // unchanged unsigned branch
		{0x00000001, "00000001"},
		{0x7fffffff, "7fffffff"},
	}
	for _, testCase := range cases {
		if got := padInt32Hex8(testCase.value); got != testCase.want {
			t.Errorf("padInt32Hex8(%#x) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}

// TestPadInt32Hex8IsPadStartNotZeroExtend makes the rule explicit: any 32-bit
// value whose signed hex form is shorter than eight characters is left-padded
// with zeros to exactly eight characters, sign included, and never to nine.
//
// The assertion is written so a regression reports, rather than panics on a
// short result: padStart only ever prepends zeros, so the result must equal
// zeros + the signed form and the signed form must be its suffix.
func TestPadInt32Hex8IsPadStartNotZeroExtend(t *testing.T) {
	for _, value := range []uint32{0xffffffff, 0xfffffffe, 0xffffedcc, 0xfffffff0, 0x80000013, 0x8000007f} {
		signed := strconv.FormatInt(int64(int32(value)), 16)
		if len(signed) >= 8 {
			continue
		}
		want := strings.Repeat("0", 8-len(signed)) + signed
		got := padInt32Hex8(value)
		if got != want {
			t.Errorf("padInt32Hex8(%#x) = %q (len %d), want %q (8 characters, sign included)", value, got, len(got), want)
		}
	}
}

// TestNumMatchesJSNumberToString pins num against String(v): num is the
// counter renderer of the stats lines, and a JavaScript template literal prints
// a Number through Number::toString. It used to print plain notation for the
// whole integral range, which diverged from JS at 1e21.
func TestNumMatchesJSNumberToString(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{1e21, "1e+21"},
		{1.5e21, "1.5e+21"},
		{-1e21, "-1e+21"},
		{1e20, "100000000000000000000"},
		{math.Pow(2, 63), "9223372036854776000"},
		{math.Pow(2, 62), "4611686018427388000"},
		{0, "0"},
		{1, "1"},
		{1234, "1234"},
		{0.5, "0.5"},
		{1e-7, "1e-7"},
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
	}
	for _, testCase := range cases {
		if got := num(testCase.value); got != testCase.want {
			t.Errorf("num(%v) = %q, want %q", testCase.value, got, testCase.want)
		}
		if got, want := num(testCase.value), jsNumberString(testCase.value); got != want {
			t.Errorf("num(%v) = %q, jsNumberString = %q: num must delegate", testCase.value, got, want)
		}
	}
}

// TestFingerprintMatchesTSFixedVectors cross-checks the Go fingerprint against
// fixed vectors produced by running the TypeScript implementation (the copy at
// /root/.pi/agent/extensions/cache-stats.ts, whose `fingerprint` is byte for
// byte the one ported here).
//
// The defect this pins: TS's last mixing step is `h2 ^= imul(...)`, a plain XOR
// that yields an int32, so `h2.toString(16)` prints a leading minus whenever bit
// 31 is set and the result is nine characters long. The Go port formatted h2 as
// a uint32, so every such fingerprint differed from TS in its last eight
// characters. Vectors were chosen to cover both branches (signed and unsigned
// h2) and the padding behaviour.
func TestFingerprintMatchesTSFixedVectors(t *testing.T) {
	zeroSalt := [16]byte{}
	seqSalt := [16]byte{}
	for i := range seqSalt {
		seqSalt[i] = byte(i)
	}
	cases := []struct {
		salt   [16]byte
		chunks []string
		want   string
	}{
		// Unsigned h2 (no sign bit): unchanged by the fix, kept as a control.
		{zeroSalt, nil, "69691905558c0ab3"},
		{zeroSalt, []string{""}, "37b3889e04690cdd"},
		{zeroSalt, []string{"system"}, "ffe2f829348456c9"},
		{zeroSalt, []string{"S", "3", "abc"}, "a3cdce8f1ca85e1a"},
		{zeroSalt, []string{"D", "100"}, "20d6a118683b58ea"},
		{zeroSalt, []string{"D", "1e-7"}, "cdfad4d04ec18e92"},
		{zeroSalt, []string{"D", "100000000000000000000"}, "fb1f584a0d95cd30"},
		// Signed h2 (bit 31 set): these are the ones the uint32 formatting broke.
		{zeroSalt, []string{"a"}, "eb875992-6c056e9f"},
		{zeroSalt, []string{"tools"}, "6f60a67a-97a6d34"},
		{zeroSalt, []string{"D", "1e+20"}, "135405f0-23c3b629"},
		{zeroSalt, []string{"xxxxxxxxxxxxxxxxxxxx"}, "4185eb8a-11243b8b"},
		{zeroSalt, []string{"", "", ""}, "eb458a1c-26c7c1bc"},
		{seqSalt, nil, "c8fff215-4c3d896d"},
		{seqSalt, []string{""}, "ba34e6ee-1b31a4b3"},
		{seqSalt, []string{"a"}, "2d2e08022caf41c1"},
		{seqSalt, []string{"system"}, "50625079-79f82667"},
		{seqSalt, []string{"S", "3", "abc"}, "abe6107f6c4e79da"},
		{seqSalt, []string{"D", "100"}, "41594408-5d1be9f6"},
		{seqSalt, []string{"tools"}, "ebfd3d2a1cd470cc"},
		{seqSalt, []string{"D", "1e+20"}, "8e606e60-73a74b49"},
		{seqSalt, []string{"D", "1e-7"}, "57bce440772fe8e2"},
		{seqSalt, []string{"xxxxxxxxxxxxxxxxxxxx"}, "ecff51ba0584b925"},
		{seqSalt, []string{"", "", ""}, "6b920bac73aecf74"},
	}
	for _, testCase := range cases {
		got := fingerprint(testCase.salt, testCase.chunks)
		if got != testCase.want {
			t.Errorf("fingerprint(%x, %q) = %q, want %q", testCase.salt[:2], testCase.chunks, got, testCase.want)
		}
		// TS's padStart(8, "0") never truncates, so a signed h2 keeps its minus
		// sign and the whole digest is 17 characters instead of 16.
		if len(got) != 16 && len(got) != 17 {
			t.Errorf("fingerprint length = %d, want 16 or 17: %q", len(got), got)
		}
	}
}

// TestFingerprintToolArgumentsNotAliased is the end-to-end regression for the
// jsNumberString overflow: two distinct large numeric tool arguments used to
// hash identically, so the prefix diagnosis reported "stable" while the tool
// definition had in fact changed.
func TestFingerprintToolArgumentsNotAliased(t *testing.T) {
	salt := [16]byte{1, 2, 3, 4}
	fp := func(limit float64) string {
		tools := []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "scale",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"limit": map[string]any{"type": "number", "const": limit},
					},
				},
			},
		}}
		section := toolsSection(salt, true, tools, &prefixBudget{})
		if section.fp == nil {
			t.Fatalf("tools section must fingerprint (limit %v), got state %q", limit, section.state)
		}
		return *section.fp
	}
	values := []float64{1e19, 2e19, 1e20, -1e20, 1e18}
	seen := map[string]float64{}
	for _, value := range values {
		digest := fp(value)
		if previous, dup := seen[digest]; dup {
			t.Fatalf("tool arguments %v and %v share fingerprint %q", previous, value, digest)
		}
		seen[digest] = value
	}
}

// TestNormalizeUsageTotalTokensMatchesTSFallback pins the `Number(selected) ||
// (input + output + cacheRead + cacheWrite)` semantics against the TypeScript
// implementation. Every expected value below was produced by running the TS
// normalizeUsage on the same payload, so a regression here means the Go port
// drifted from the source of truth rather than from the reviewer's prose.
//
// The historical defect: the Go port only fell back when the key was absent or
// NaN, so an explicit `totalTokens: 0` stayed 0 (TS reports the four-term sum),
// and the fallback summed the *raw* terms instead of the `Number(v) || 0`-normalized
// ones, so a NaN term could poison the total.
func TestNormalizeUsageTotalTokensMatchesTSFallback(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]any
		want  float64
	}{
		{"explicit zero falls back to the sum", usageMap("input", 10, "output", 5, "totalTokens", 0), 15},
		{"explicit zero with cache falls back", usageMap("input", 10, "cacheRead", 3, "totalTokens", 0), 13},
		{"string zero falls back (Number('0') is falsy)", usageMap("input", 10, "cacheRead", 3, "totalTokens", "0"), 13},
		{"negative is truthy in JS and is kept", usageMap("input", 10, "output", 5, "totalTokens", -3), -3},
		{"non-numeric string is NaN and falls back", usageMap("input", 10, "output", 5, "totalTokens", "abc"), 15},
		{"NaN falls back", usageMap("input", 10, "cacheRead", 3, "totalTokens", math.NaN()), 13},
		{"absent key falls back", usageMap("input", 10, "output", 5), 15},
		{"NaN term contributes 0 to the sum", usageMap("input", "x", "output", 5), 5},
		{"null key is skipped by ??", usageMap("input", 10, "totalTokens", nil, "total", 7), 7},
		{"first key wins", usageMap("input", 10, "totalTokens", 5, "total_tokens", 99), 5},
		{"truthy total is kept", usageMap("input", 10, "totalTokens", 42), 42},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			norm := normalizeUsage(testCase.usage)
			requireFloat(t, "totalTokens", norm.totalTokens, testCase.want)
			// total is the same value in TS (`total: totalTokens`).
			requireFloat(t, "total", norm.total, testCase.want)
		})
	}
}

// TestNormalizeUsageFallbackUsesNormalizedTerms guards the second half of the
// same defect: the fallback sum must use the `orZero`-normalized terms, so a
// payload whose cacheRead is a non-numeric string still sums to the numeric
// part instead of NaN.
func TestNormalizeUsageFallbackUsesNormalizedTerms(t *testing.T) {
	norm := normalizeUsage(usageMap("input", 100, "output", 50, "cacheRead", "not-a-number", "cacheWrite", 5))
	requireFloat(t, "input", norm.input, 100)
	requireFloat(t, "output", norm.output, 50)
	requireFloat(t, "cacheRead", norm.cacheRead, 0)
	requireFloat(t, "cacheWrite", norm.cacheWrite, 5)
	requireFloat(t, "totalTokens", norm.totalTokens, 155)
}
