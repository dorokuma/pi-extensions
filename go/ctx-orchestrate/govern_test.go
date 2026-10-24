package ctxorchestrate

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// Tests for the tool_result governance hook: the codemode text cap, its marker,
// the explicit details/structuredContent/isError/usage pass-through, and every
// case in which the hook must not touch a reply.

// cleanEnv clears the extension's environment so a test starts from the defaults.
func cleanEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CTX_ORCH_TEXT_CAP", "")
	t.Setenv("CTX_ORCH_GOVERN", "")
	t.Setenv("CTX_ORCH_DEBUG", "")
}

func textBlock(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

// contentText joins the text blocks of a content array.
func contentText(t *testing.T, content any) string {
	t.Helper()
	blocks, ok := content.([]any)
	if !ok {
		t.Fatalf("content is %T, want []any", content)
	}
	var texts []string
	for _, block := range blocks {
		if text, ok := textBlockText(block); ok {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func TestCapCodemodeResultCapsOversizedReply(t *testing.T) {
	cleanEnv(t)
	details := map[string]any{"fullOutputPath": "/tmp/out/full.txt", "calls": float64(3)}
	structured := map[string]any{"answer": float64(42)}
	usage := map[string]any{"input": float64(100)}

	result := CapCodemodeResult(map[string]any{
		"toolName":          "codemode",
		"content":           []any{textBlock(strings.Repeat("a", 3500))},
		"details":           details,
		"structuredContent": structured,
		"isError":           false,
		"usage":             usage,
	})

	if result == nil {
		t.Fatal("3500 characters must be governed")
	}
	text := contentText(t, result["content"])
	if units := utf16Len(text); units > DefaultTextCap {
		t.Fatalf("governed reply is %d units, want <= %d", units, DefaultTextCap)
	}
	for _, want := range []string{
		"[…ctx-orchestrate 治理] codemode 回传 3500 字符超过硬顶 3000",
		"丢弃约 680 字符", // 3500 - (3000-180)
		"完整输出已存至 /tmp/out/full.txt，用 read 精确取片段（无需重跑）。",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("marker missing %q in:\n%s", want, text)
		}
	}
	// The four members the host replaces must be passed through explicitly.
	if !reflect.DeepEqual(result["details"], details) {
		t.Fatalf("details = %+v, want the event's details", result["details"])
	}
	if !reflect.DeepEqual(result["structuredContent"], structured) {
		t.Fatalf("structuredContent = %+v, want the event's value", result["structuredContent"])
	}
	if value, ok := result["isError"]; !ok || value != false {
		t.Fatalf("isError = %v (present %t), want false", value, ok)
	}
	if !reflect.DeepEqual(result["usage"], usage) {
		t.Fatalf("usage = %+v, want the event's usage", result["usage"])
	}
}

func TestCapCodemodeResultMarkerWithoutFullOutputPath(t *testing.T) {
	cleanEnv(t)
	result := CapCodemodeResult(map[string]any{
		"toolName": "codemode",
		"content":  []any{textBlock(strings.Repeat("b", 4000))},
	})
	if result == nil {
		t.Fatal("4000 characters must be governed")
	}
	text := contentText(t, result["content"])
	if !strings.Contains(text, "请在脚本内自己收敛：只回摘要/计数/关键路径，需要全文就写入文件后用 read 精确取片段。") {
		t.Fatalf("fallback marker missing:\n%s", text)
	}
	if strings.Contains(text, "完整输出已存至") {
		t.Fatalf("marker must not name a path that does not exist:\n%s", text)
	}
	if _, ok := result["details"]; ok {
		t.Fatalf("an absent details member must stay absent, got %+v", result["details"])
	}
	if _, ok := result["structuredContent"]; ok {
		t.Fatalf("an absent structuredContent member must stay absent")
	}
}

func TestCapCodemodeResultDetailsWithoutPathMember(t *testing.T) {
	cleanEnv(t)
	// codemode always sets details (calls/…), but only some runs spool fullOutputPath.
	result := CapCodemodeResult(map[string]any{
		"toolName": "codemode",
		"content":  []any{textBlock(strings.Repeat("c", 4000))},
		"details":  map[string]any{"calls": []any{}},
	})
	if result == nil {
		t.Fatal("4000 characters must be governed")
	}
	if strings.Contains(contentText(t, result["content"]), "完整输出已存至") {
		t.Fatal("details without fullOutputPath must use the fallback sentence")
	}
}

func TestCapCodemodeResultLeavesThingsAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event map[string]any
		env   map[string]string
	}{
		{
			name:  "other tool",
			event: map[string]any{"toolName": "bash", "content": []any{textBlock(strings.Repeat("x", 5000))}},
		},
		{
			name:  "no tool name",
			event: map[string]any{"content": []any{textBlock(strings.Repeat("x", 5000))}},
		},
		{
			name:  "nested call",
			event: map[string]any{"toolName": "codemode", "parentToolCallId": "call-1", "content": []any{textBlock(strings.Repeat("x", 5000))}},
		},
		{
			name:  "no content member",
			event: map[string]any{"toolName": "codemode"},
		},
		{
			name:  "empty content",
			event: map[string]any{"toolName": "codemode", "content": []any{}},
		},
		{
			name:  "image only",
			event: map[string]any{"toolName": "codemode", "content": []any{map[string]any{"type": "image", "data": "AAAA"}}},
		},
		{
			name:  "exactly the trigger floor",
			event: map[string]any{"toolName": "codemode", "content": []any{textBlock(strings.Repeat("x", GovernMinTotal))}},
		},
		{
			name:  "just above the trigger floor",
			event: map[string]any{"toolName": "codemode", "content": []any{textBlock(strings.Repeat("x", GovernMinTotal+1))}},
		},
		{
			name:  "exactly the cap",
			event: map[string]any{"toolName": "codemode", "content": []any{textBlock(strings.Repeat("x", DefaultTextCap))}},
		},
		{
			name:  "governance disabled",
			event: map[string]any{"toolName": "codemode", "content": []any{textBlock(strings.Repeat("x", 5000))}},
			env:   map[string]string{"CTX_ORCH_GOVERN": "0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanEnv(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			if result := CapCodemodeResult(tc.event); result != nil {
				t.Fatalf("result = %+v, want nil (untouched)", result["content"])
			}
		})
	}
}

func TestCapCodemodeResultEnvCapOverride(t *testing.T) {
	t.Run("lower cap", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("CTX_ORCH_TEXT_CAP", "1000")
		if got := TextCap(); got != 1000 {
			t.Fatalf("TextCap = %d, want 1000", got)
		}
		result := CapCodemodeResult(map[string]any{
			"toolName": "codemode",
			"content":  []any{textBlock(strings.Repeat("y", 3500))},
		})
		if result == nil {
			t.Fatal("3500 characters must be governed at cap 1000")
		}
		text := contentText(t, result["content"])
		if units := utf16Len(text); units > 1000 {
			t.Fatalf("governed reply is %d units, want <= 1000", units)
		}
		if !strings.Contains(text, "硬顶 1000") {
			t.Fatalf("marker must name the effective cap:\n%s", text)
		}
	})

	t.Run("floor 200", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("CTX_ORCH_TEXT_CAP", "150")
		if got := TextCap(); got != MinTextCap {
			t.Fatalf("TextCap = %d, want the %d floor", got, MinTextCap)
		}
		result := CapCodemodeResult(map[string]any{
			"toolName": "codemode",
			"content":  []any{textBlock(strings.Repeat("y", 3500))},
		})
		if result == nil {
			t.Fatal("3500 characters must be governed at the 200 floor")
		}
		text := contentText(t, result["content"])
		if units := utf16Len(text); units > MinTextCap {
			t.Fatalf("governed reply is %d units, want <= %d", units, MinTextCap)
		}
	})

	t.Run("unparseable falls back to the default", func(t *testing.T) {
		cleanEnv(t)
		t.Setenv("CTX_ORCH_TEXT_CAP", "not-a-number")
		if got := TextCap(); got != DefaultTextCap {
			t.Fatalf("TextCap = %d, want %d", got, DefaultTextCap)
		}
	})

	t.Run("missing falls back to the default", func(t *testing.T) {
		cleanEnv(t)
		if got := TextCap(); got != DefaultTextCap {
			t.Fatalf("TextCap = %d, want %d", got, DefaultTextCap)
		}
		if !GovernEnabled() {
			t.Fatal("governance must default to enabled")
		}
	})

	t.Run("truthy spellings keep governance on", func(t *testing.T) {
		cleanEnv(t)
		for _, value := range []string{"1", "true", "TRUE", " on ", "yes"} {
			t.Setenv("CTX_ORCH_GOVERN", value)
			if !GovernEnabled() {
				t.Fatalf("CTX_ORCH_GOVERN=%q must keep governance on", value)
			}
		}
		t.Setenv("CTX_ORCH_GOVERN", "off")
		if GovernEnabled() {
			t.Fatal("CTX_ORCH_GOVERN=off must disable governance")
		}
	})
}

func TestCapCodemodeResultMultipleBlocks(t *testing.T) {
	cleanEnv(t)
	image := map[string]any{"type": "image", "data": "AAAA", "mimeType": "image/png"}
	result := CapCodemodeResult(map[string]any{
		"toolName": "codemode",
		"content": []any{
			textBlock(strings.Repeat("a", 100)),
			image,
			textBlock(strings.Repeat("b", 3500)),
		},
	})
	if result == nil {
		t.Fatal("the reply must be governed")
	}
	blocks, ok := result["content"].([]any)
	if !ok || len(blocks) != 3 {
		t.Fatalf("blocks = %+v, want 3", result["content"])
	}
	first, _ := textBlockText(blocks[0])
	if first != strings.Repeat("a", 100) {
		t.Fatalf("the first block must stay whole, got %d units", utf16Len(first))
	}
	if !reflect.DeepEqual(blocks[1], image) {
		t.Fatalf("the image block must be kept as it is, got %+v", blocks[1])
	}
	last, _ := textBlockText(blocks[2])
	if !strings.Contains(last, "[…ctx-orchestrate 治理]") {
		t.Fatal("the marker must be appended to the last text block")
	}
	if strings.Contains(first, "[…ctx-orchestrate 治理]") {
		t.Fatal("the marker must not land in an earlier block")
	}
	total := utf16Len(first) + utf16Len(last)
	if total > DefaultTextCap {
		t.Fatalf("governed text is %d units, want <= %d", total, DefaultTextCap)
	}
	// budget 2820: 100 for the first block, 2720 for the last, so 780 dropped.
	if !strings.Contains(last, "丢弃约 780 字符") {
		t.Fatalf("dropped count wrong:\n%s", last)
	}
}

func TestCapCodemodeResultDropsTrailingTextBlocks(t *testing.T) {
	cleanEnv(t)
	result := CapCodemodeResult(map[string]any{
		"toolName": "codemode",
		"content":  []any{textBlock(strings.Repeat("a", 5000)), textBlock(strings.Repeat("b", 100))},
	})
	if result == nil {
		t.Fatal("the reply must be governed")
	}
	blocks := result["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want the trailing block dropped", len(blocks))
	}
	text, _ := textBlockText(blocks[0])
	// 5000-2820 = 2180 dropped from the first, 0 of the 100 kept, plus the whole 100.
	if !strings.Contains(text, "丢弃约 2280 字符") {
		t.Fatalf("dropped count wrong:\n%s", text)
	}
	if utf16Len(text) > DefaultTextCap {
		t.Fatalf("governed text is %d units", utf16Len(text))
	}
}

func TestCapCodemodeResultCountsUTF16Units(t *testing.T) {
	cleanEnv(t)
	// 1500 astral runes are 3000 UTF-16 units: exactly the cap, so untouched.
	if result := CapCodemodeResult(map[string]any{
		"toolName": "codemode",
		"content":  []any{textBlock(strings.Repeat("😀", 1500))},
	}); result != nil {
		t.Fatal("3000 UTF-16 units are exactly the cap and must stay untouched")
	}
	// 1600 of them are 3200 units: governed, and never cut mid-rune.
	result := CapCodemodeResult(map[string]any{
		"toolName": "codemode",
		"content":  []any{textBlock(strings.Repeat("😀", 1600))},
	})
	if result == nil {
		t.Fatal("3200 UTF-16 units must be governed")
	}
	text := contentText(t, result["content"])
	if units := utf16Len(text); units > DefaultTextCap {
		t.Fatalf("governed reply is %d units", units)
	}
	if !utf8.ValidString(text) {
		t.Fatal("truncation split a rune")
	}
	before, _, _ := strings.Cut(text, "\n[…ctx-orchestrate")
	if strings.ContainsRune(before, utf8.RuneError) {
		t.Fatal("truncation produced a replacement character")
	}
	if utf16Len(before) != 2820 {
		t.Fatalf("kept body = %d units, want the 2820 budget", utf16Len(before))
	}
}

func TestCapCodemodeResultPreservesOtherBlockMembers(t *testing.T) {
	cleanEnv(t)
	block := map[string]any{
		"type":         "text",
		"text":         strings.Repeat("z", 4000),
		"cacheControl": map[string]any{"type": "ephemeral"},
	}
	result := CapCodemodeResult(map[string]any{"toolName": "codemode", "content": []any{block}})
	if result == nil {
		t.Fatal("the reply must be governed")
	}
	governed := result["content"].([]any)[0].(map[string]any)
	if governed["cacheControl"] == nil {
		t.Fatal("block members other than text must survive")
	}
	if block["text"] != strings.Repeat("z", 4000) {
		t.Fatal("the event's own block must not be edited in place")
	}
}

// TestTextCapRejectsUnusableValues pins the totality of CTX_ORCH_TEXT_CAP: a
// value that cannot describe a usable cap must fall back to DefaultTextCap, not
// leak a JavaScript float into the Go int conversion (int(NaN) and int(1e19) are
// implementation-defined and used to wrap to the minimum int, i.e. the 200 floor).
func TestTextCapRejectsUnusableValues(t *testing.T) {
	for _, raw := range []string{
		"",             // unset
		"   ",          // blank (empty after no trim: unparseable)
		"not-a-number", // unparseable
		"12px",
		"NaN", "nan", // JavaScript's Number() keeps these as a non-finite float
		"Inf", "+Inf", "-Inf", "infinity", "-infinity",
		"0", "0.0", "-1", "-0.5", // <= 0 is not a cap
		"1e19", "9.3e18", "1e308", // beyond math.MaxInt (2^63-1) / float64 range
		"9223372036854775807", // math.MaxInt itself rounds to 2^63 as a float64
	} {
		t.Run(raw, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("CTX_ORCH_TEXT_CAP", raw)
			if got := TextCap(); got != DefaultTextCap {
				t.Fatalf("TextCap(CTX_ORCH_TEXT_CAP=%q) = %d, want the %d default", raw, got, DefaultTextCap)
			}
		})
	}
}

// TestTextCapAcceptsFinitePositiveValues pins the other half: a finite positive
// value inside the int range is floored and only then floored again at MinTextCap.
func TestTextCapAcceptsFinitePositiveValues(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"1000", 1000},
		{"3000.9", 3000},
		{"250.7", 250},
		{"199.9", MinTextCap},
		{"9223372036854774784", 9223372036854774784}, // 2^63-1024: still an int
	} {
		t.Run(tc.raw, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("CTX_ORCH_TEXT_CAP", tc.raw)
			if got := TextCap(); got != tc.want {
				t.Fatalf("TextCap(CTX_ORCH_TEXT_CAP=%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestCapCodemodeResultWithUnusableCapFallsBack keeps the two layers honest: a
// nonsense CTX_ORCH_TEXT_CAP must not only read as the default, the hook must
// also govern at the default rather than at the 200 floor the wrapped int implied.
func TestCapCodemodeResultWithUnusableCapFallsBack(t *testing.T) {
	for _, raw := range []string{"NaN", "1e19", "0"} {
		t.Run(raw, func(t *testing.T) {
			cleanEnv(t)
			t.Setenv("CTX_ORCH_TEXT_CAP", raw)
			result := CapCodemodeResult(map[string]any{
				"toolName": "codemode",
				"content":  []any{textBlock(strings.Repeat("q", 3500))},
			})
			if result == nil {
				t.Fatalf("3500 characters must be governed at the fallback cap (CTX_ORCH_TEXT_CAP=%q)", raw)
			}
			text := contentText(t, result["content"])
			if units := utf16Len(text); units > DefaultTextCap {
				t.Fatalf("governed reply is %d units, want <= %d", units, DefaultTextCap)
			}
			if !strings.Contains(text, "硬顶 3000") {
				t.Fatalf("the marker must name the effective %d cap:\n%s", DefaultTextCap, text)
			}
			if strings.Contains(text, "硬顶 200") {
				t.Fatalf("an unusable cap must not degrade to the 200 floor:\n%s", text)
			}
		})
	}
}
