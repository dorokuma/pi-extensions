package herdsmanpi

// TestMain keeps every test hermetic: the diagnostic logger writes under
// HERDSMAN_HOME, so a test-run must never touch the real ~/.herdsman (and,
// for the same reason, never the production daemon: the extension is driven
// through fake clients except in daemonclient_test.go, which binds isolated
// Unix sockets under /tmp).

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "hpi-log-home-")
	if err != nil {
		panic(err)
	}
	previous, hadPrevious := os.LookupEnv("HERDSMAN_HOME")
	if err := os.Setenv("HERDSMAN_HOME", home); err != nil {
		panic(err)
	}
	code := m.Run()
	if hadPrevious {
		_ = os.Setenv("HERDSMAN_HOME", previous)
	} else {
		_ = os.Unsetenv("HERDSMAN_HOME")
	}
	_ = os.RemoveAll(home)
	os.Exit(code)
}
