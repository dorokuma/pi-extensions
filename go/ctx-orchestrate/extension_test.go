package ctxorchestrate

import (
	"os"
	"testing"
)

// TestMain points diagnostics at a throwaway directory: the extension's default
// log path is <home>/.pi/agent/logs, which a test run must not create or touch.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ctx-orchestrate-diag")
	if err != nil {
		panic("create diagnostics dir: " + err.Error())
	}
	if err := os.Setenv("CTX_ORCH_DIAG_DIR", dir); err != nil {
		panic("set CTX_ORCH_DIAG_DIR: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestExtensionRegistersWithoutHost checks the factory itself: the tool
// definition and the tool_result handler must register with no host connection
// and no panic (the SDK validates the parameter schema at registration).
func TestExtensionRegistersWithoutHost(t *testing.T) {
	t.Setenv("CTX_ORCH_TEXT_CAP", "")
	t.Setenv("CTX_ORCH_GOVERN", "")

	extension := Extension()
	if extension == nil {
		t.Fatal("Extension() returned nil")
	}
	if name := extension.Name(); name != "ctx-orchestrate" {
		t.Fatalf("extension name = %q", name)
	}
}

// TestToolDefinitionShape pins the registration fields the TS version sets, so a
// drift from codemode exposure / namespace / read-only annotation is caught.
func TestToolDefinitionShape(t *testing.T) {
	if got := namespace().Name; got != NamespaceName {
		t.Fatalf("namespace name = %q", got)
	}
	if namespace().Instructions == "" {
		t.Fatal("namespace instructions must carry the describeNamespace() guidance")
	}

	schema := searchDigestSchema()
	if schema["type"] != "object" {
		t.Fatalf("parameter schema type = %v", schema["type"])
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, name := range []string{"patterns", "path", "glob", "max_hits", "include_hidden", "ignore_case", "context", "literal"} {
		if _, ok := properties[name]; !ok {
			t.Fatalf("parameter %q missing from the schema", name)
		}
	}
	patterns := properties["patterns"].(map[string]any)
	if patterns["type"] != "array" || patterns["items"].(map[string]any)["type"] != "string" {
		t.Fatalf("patterns must be an array of strings: %+v", patterns)
	}
	required, ok := schema["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "patterns" {
		t.Fatalf("required = %+v, want [patterns]", schema["required"])
	}

	output := searchDigestOutputSchema()
	outputProperties, _ := output["properties"].(map[string]any)
	for _, name := range []string{"patterns", "files", "summary", "errors"} {
		if _, ok := outputProperties[name]; !ok {
			t.Fatalf("output member %q missing", name)
		}
	}
	files := outputProperties["files"].(map[string]any)
	items, _ := files["items"].(map[string]any)
	hitProperties, _ := items["properties"].(map[string]any)
	for _, name := range []string{"path", "hits"} {
		if _, ok := hitProperties[name]; !ok {
			t.Fatalf("file member %q missing", name)
		}
	}
}
