package jevgate

import (
	"errors"
	"strings"
	"testing"
)

// TestIsLocalGatewayURL pins the loopback detection, including every spelling the
// WHATWG URL parser can produce for an IPv4-mapped / IPv4-translated address.
func TestIsLocalGatewayURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:3425/v1", true},
		{"http://127.1.2.3:1", true},
		{"https://127.0.0.1/v1", true},
		{"http://localhost:3425/v1", true},
		{"http://localhost.:3425/v1", true},
		{"http://LOCALHOST:3425/v1", true},
		{"http://[::1]:3425/v1", true},
		{"http://[::ffff:127.0.0.1]:3425/v1", true},
		{"http://[::ffff:7f00:1]:3425/v1", true},
		{"http://[::ffff:0:127.0.0.1]:3425/v1", true},
		{"http://[::ffff:0:7f00:1]:3425/v1", true},
		{"http://[0:0:0:0:ffff:0:127.0.0.1]:3425/v1", true},
		{"https://api.example.com/v1", false},
		{"http://169.254.169.254/v1", false},
		{"http://192.168.1.1/v1", false},
		{"http://0.0.0.0/v1", false},
		{"http://[::ffff:192.168.1.1]:1/v1", false},
		{"http://[::ffff:0:192.168.1.1]:1/v1", false},
		{"http://127.0.0.1.evil.com/v1", false},
		{"http://localhost.evil.com/v1", false},
		{"http://user:pw@127.0.0.1:3425/v1", false},
		{"http://user@127.0.0.1:3425/v1", false},
		{"ftp://127.0.0.1/v1", false},
		{"", false},
		{"not a url", false},
	}
	for _, testCase := range cases {
		if got := IsLocalGatewayURL(testCase.url); got != testCase.want {
			t.Errorf("IsLocalGatewayURL(%q) = %v, want %v", testCase.url, got, testCase.want)
		}
	}
}

// lookupFixture builds a KeyLookup over a fixed environment and file map.
func lookupFixture(t *testing.T, env map[string]string, files map[string]string) KeyLookup {
	t.Helper()
	return KeyLookup{
		Getenv: func(name string) string { return env[name] },
		ReadFile: func(path string) ([]byte, error) {
			content, ok := files[path]
			if !ok {
				return nil, errors.New("no such file")
			}
			return []byte(content), nil
		},
		HomeDir: func() (string, error) { return "/home/test", nil },
	}
}

// TestResolveAPIKeyOrder pins the three-step credential resolution.
func TestResolveAPIKeyOrder(t *testing.T) {
	local := "http://127.0.0.1:3425/v1"
	remote := "https://windhub.example/v1"
	settingsWithKey := `/home/test/.pi/agent/settings.json`
	settings := `{"env":{"MAGPIE_API_KEY":"from-settings"}}`

	if got := ResolveAPIKey(local, lookupFixture(t,
		map[string]string{"MAGPIE_API_KEY": " from-env "},
		map[string]string{settingsWithKey: settings})); !got.OK || got.Key != "from-env" {
		t.Fatalf("env must win: %+v", got)
	}
	if got := ResolveAPIKey(local, lookupFixture(t,
		map[string]string{"WINDHUB_API_KEY": "windhub-env"},
		map[string]string{settingsWithKey: settings})); !got.OK || got.Key != "windhub-env" {
		t.Fatalf("WINDHUB_API_KEY must be the second env candidate: %+v", got)
	}
	if got := ResolveAPIKey(remote, lookupFixture(t, nil, map[string]string{settingsWithKey: settings})); !got.OK || got.Key != "from-settings" {
		t.Fatalf("settings.json env block: %+v", got)
	}
	if got := ResolveAPIKey(local, lookupFixture(t, nil, map[string]string{settingsWithKey: `{"env":{}}`})); !got.OK || got.Key != LocalGatewayKey {
		t.Fatalf("loopback must fall back to the local gateway token: %+v", got)
	}
	if got := ResolveAPIKey(local, lookupFixture(t, nil, nil)); !got.OK || got.Key != LocalGatewayKey {
		t.Fatalf("unreadable settings on loopback: %+v", got)
	}

	missing := ResolveAPIKey(remote, lookupFixture(t, nil, map[string]string{settingsWithKey: `{"env":{}}`}))
	if missing.OK {
		t.Fatalf("remote gateway without a key must fail closed: %+v", missing)
	}
	if !strings.Contains(missing.Detail, "absent from") {
		t.Fatalf("detail = %q", missing.Detail)
	}

	unreadable := ResolveAPIKey(remote, lookupFixture(t, nil, nil))
	if unreadable.OK || !strings.Contains(unreadable.Detail, "cannot read") {
		t.Fatalf("unreadable settings on a remote gateway: %+v", unreadable)
	}

	corrupt := ResolveAPIKey(remote, lookupFixture(t, nil, map[string]string{settingsWithKey: `{not json`}))
	if corrupt.OK || !strings.Contains(corrupt.Detail, "cannot read") {
		t.Fatalf("corrupt settings on a remote gateway: %+v", corrupt)
	}
	if corrupt := ResolveAPIKey(local, lookupFixture(t, nil, map[string]string{settingsWithKey: `{not json`})); !corrupt.OK || corrupt.Key != LocalGatewayKey {
		t.Fatalf("corrupt settings on loopback must still use the local token: %+v", corrupt)
	}
}

// TestResolveAPIKeyAgentDir checks PI_CODING_AGENT_DIR and the home fallback.
func TestResolveAPIKeyAgentDir(t *testing.T) {
	remote := "https://windhub.example/v1"
	custom := "/tmp/agent-dir/settings.json"
	lookup := lookupFixture(t,
		map[string]string{"PI_CODING_AGENT_DIR": "/tmp/agent-dir"},
		map[string]string{custom: `{"env":{"WINDHUB_API_KEY":"dir-key"}}`})
	if got := ResolveAPIKey(remote, lookup); !got.OK || got.Key != "dir-key" {
		t.Fatalf("PI_CODING_AGENT_DIR: %+v", got)
	}

	brokenHome := lookupFixture(t, nil, nil)
	brokenHome.HomeDir = func() (string, error) { return "", errors.New("no home") }
	if got := ResolveAPIKey(remote, brokenHome); got.OK {
		t.Fatalf("no home directory and no key must fail closed: %+v", got)
	}
}

// TestResolveKeyOrError pins the fail-closed message shape.
func TestResolveKeyOrError(t *testing.T) {
	_, err := resolveKeyOrError("https://windhub.example/v1", lookupFixture(t, nil, nil))
	if err == nil {
		t.Fatal("missing credential must be an error")
	}
	message := err.Error()
	if !strings.HasPrefix(message, "missing MAGPIE_API_KEY/WINDHUB_API_KEY: absent from the process environment; ") ||
		!strings.HasSuffix(message, "(fail-closed, no fallback)") {
		t.Fatalf("message = %q", message)
	}
}
