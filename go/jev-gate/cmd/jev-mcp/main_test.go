package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestVersionFlag verifies --version prints the injected/dev version and
// returns before entering the MCP stdio read loop.
func TestVersionFlag(t *testing.T) {
	if version == "" {
		t.Fatal("version must be non-empty (default \"dev\" or ldflags-injected)")
	}
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	os.Stdout = w
	defer func() { os.Stdout = old }()

	oldArgs := os.Args
	os.Args = []string{"jev-mcp", "--version"}
	defer func() { os.Args = oldArgs }()

	main()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != version {
		t.Fatalf("--version output = %q, want %q", got, version)
	}
}
