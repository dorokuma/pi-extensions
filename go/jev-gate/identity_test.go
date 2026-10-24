package jevgate

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// identityFixture builds an IdentityEnv over a synthetic process chain. chain[0]
// is this process; QuerySocket returns shellPID (0 means "query failed").
func identityFixture(env map[string]string, chain []AncestorProcess, shellPID int) IdentityEnv {
	values := make(map[string]string, len(env))
	for key, value := range env {
		values[key] = value
	}
	ppids := make(map[int]int, len(chain))
	cmdlines := make(map[int]string, len(chain))
	for _, entry := range chain {
		ppids[entry.PID] = entry.PPID
		cmdlines[entry.PID] = entry.Cmdline
	}
	environ := make([]string, 0, len(values))
	for key, value := range values {
		environ = append(environ, key+"="+value)
	}
	return IdentityEnv{
		Getenv:      func(name string) string { return values[name] },
		Unsetenv:    func(name string) error { delete(values, name); return nil },
		PID:         chain[0].PID,
		SocketPath:  "/tmp/identity-fixture.sock",
		Timeout:     50 * time.Millisecond,
		ReadStat:    func(pid int) (int, bool) { ppid, ok := ppids[pid]; return ppid, ok },
		ReadCmdline: func(pid int) string { return cmdlines[pid] },
		Environ:     func() []string { return environ },
		QuerySocket: func(string, string, time.Duration) (int, bool) { return shellPID, shellPID > 0 },
		ArgvCache:   &ArgvCache{},
	}
}

// TestResolveAgentIdentityNegativeMarkers covers the launch markers.
func TestResolveAgentIdentityNegativeMarkers(t *testing.T) {
	chain := []AncestorProcess{{PID: 100, PPID: 1, Cmdline: "pi"}}
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"subagent_env", map[string]string{"PI_SUBAGENT": "1"}, nil, "launch marker: PI_SUBAGENT=1"},
		{"role_env", map[string]string{"PI_ROLE": "worker"}, nil, "launch marker: PI_ROLE is set"},
		{"name_role", nil, []string{"pi", "--name", "role-worker"}, "launch marker: argv has --name role-"},
		{"role_sessions", nil, []string{"pi", "/tmp/herdr-role-sessions/x.json"}, "launch marker: argv has /tmp/herdr-role-sessions"},
		{"dispatch", nil, []string{"pi", "/tmp/dispatch-abc"}, "launch marker: argv has /tmp/dispatch-"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := identityFixture(testCase.env, chain, 0)
			environment.Args = testCase.args
			isSubagent, reason := resolveAgentIdentityWithEnv(environment)
			if !isSubagent || !strings.Contains(reason, testCase.want) {
				t.Fatalf("isSubagent=%v reason=%q, want %q", isSubagent, reason, testCase.want)
			}
		})
	}
}

// TestResolveAgentIdentityProofChain walks S1/S2/S3/S4.
func TestResolveAgentIdentityProofChain(t *testing.T) {
	subprocess := []AncestorProcess{
		{PID: 100, PPID: 200, Cmdline: "/opt/ext/jev-gate"},
		{PID: 200, PPID: 300, Cmdline: "/usr/local/bin/pi --print"},
		{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
	}
	t.Run("S1_missing_pane", func(t *testing.T) {
		environment := identityFixture(map[string]string{"PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "S1: HERDR_PANE_ID") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S2_query_failed", func(t *testing.T) {
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 0)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "S2:") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S3_not_in_ancestry", func(t *testing.T) {
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 999)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "is not in this process ancestry") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S3_no_runtime_ancestor", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 300, Cmdline: "/opt/ext/jev-gate"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, chain, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "no agent-runtime ancestor") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("S4_intermediate_runtime", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "/opt/ext/jev-gate"},
			{PID: 200, PPID: 250, Cmdline: "/usr/local/bin/pi"},
			{PID: 250, PPID: 300, Cmdline: "/usr/local/bin/ctxmode run"},
			{PID: 300, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, chain, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "S4: agent runtime ancestor pid=250") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("main_subprocess", func(t *testing.T) {
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("packed_cell_member_is_subprocess_form", func(t *testing.T) {
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1", "PIG_EXT_SOCKET_JEV_GATE": "/tmp/x"}, subprocess, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("fused_main", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "/usr/local/bin/pig"},
			{PID: 200, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 200)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("fused_unrecognisable_self", func(t *testing.T) {
		chain := []AncestorProcess{
			{PID: 100, PPID: 200, Cmdline: "/usr/bin/mytool --serve"},
			{PID: 200, PPID: 1, Cmdline: "/bin/bash"},
		}
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, chain, 200)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "fused form but self pid=100") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("pane_id_without_p1_suffix_still_main", func(t *testing.T) {
		// Pane naming is not a criterion in the Go family (see README).
		environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w32:p23", "PIG_EXT_SOCKET": "/tmp/x"}, subprocess, 300)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if isSubagent || !strings.Contains(reason, "S1-S4 hold") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
}

// TestResolveAgentIdentityUnreadableAncestry pins the unreadable /proc branch.
func TestResolveAgentIdentityUnreadableAncestry(t *testing.T) {
	environment := identityFixture(map[string]string{"HERDR_PANE_ID": "w1:p1"}, []AncestorProcess{{PID: 100, PPID: 1, Cmdline: "pi"}}, 300)
	environment.ReadStat = func(int) (int, bool) { return 0, false }
	isSubagent, reason := resolveAgentIdentityWithEnv(environment)
	if !isSubagent || !strings.Contains(reason, "ancestry for this process is unreadable") {
		t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
	}
}

// TestDetectSubprocessForm covers every transport variable.
func TestDetectSubprocessForm(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		want  bool
		proof string
	}{
		{"isolated", map[string]string{"PIG_EXT_SOCKET": "/tmp/s"}, true, "PIG_EXT_SOCKET"},
		{"packed_cell", map[string]string{"PIG_EXT_PACKED_CELL": "cell"}, true, "PIG_EXT_PACKED_CELL"},
		{"active_members", map[string]string{"PIG_EXT_ACTIVE_MEMBERS": "a,b"}, true, "PIG_EXT_ACTIVE_MEMBERS"},
		{"member_socket", map[string]string{"PIG_EXT_SOCKET_JEV_GATE": "/tmp/s"}, true, "PIG_EXT_SOCKET_JEV_GATE"},
		{"empty_member_socket_is_not_proof", map[string]string{"PIG_EXT_SOCKET_JEV_GATE": ""}, false, ""},
		{"fused", nil, false, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			environ := []string{}
			for key, value := range testCase.env {
				environ = append(environ, key+"="+value)
			}
			getenv := func(name string) string { return testCase.env[name] }
			got, proof := detectSubprocessForm(getenv, func() []string { return environ })
			if got != testCase.want || proof != testCase.proof {
				t.Fatalf("detectSubprocessForm = %v,%q want %v,%q", got, proof, testCase.want, testCase.proof)
			}
		})
	}
}

// TestLoadEffectiveArgs covers the harness argv bridge.
func TestLoadEffectiveArgs(t *testing.T) {
	t.Run("unset_falls_back_to_os_args", func(t *testing.T) {
		args, err := loadEffectiveArgs(func(string) string { return "" }, func(string) error { return nil }, []string{"pig"}, &ArgvCache{})
		if err != nil || strings.Join(args, " ") != "pig" {
			t.Fatalf("args=%v err=%v", args, err)
		}
	})
	t.Run("file_is_merged_cached_and_unset", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "argv.json")
		if err := os.WriteFile(path, []byte(`["--name","role-worker"]`), 0o600); err != nil {
			t.Fatalf("write argv file: %v", err)
		}
		values := map[string]string{"PIG_HARNESS_ARGV_FILE": path}
		getenv := func(name string) string { return values[name] }
		unsetenv := func(name string) error { delete(values, name); return nil }
		cache := &ArgvCache{}
		args, err := loadEffectiveArgs(getenv, unsetenv, []string{"pig"}, cache)
		if err != nil {
			t.Fatalf("loadEffectiveArgs: %v", err)
		}
		if strings.Join(args, " ") != "pig --name role-worker" {
			t.Fatalf("args = %v", args)
		}
		if _, stillSet := values["PIG_HARNESS_ARGV_FILE"]; stillSet {
			t.Fatal("PIG_HARNESS_ARGV_FILE must be unset after the read")
		}
		again, err := loadEffectiveArgs(getenv, unsetenv, []string{"pig"}, cache)
		if err != nil || strings.Join(again, " ") != "pig --name role-worker" {
			t.Fatalf("cached args=%v err=%v", again, err)
		}
	})
	t.Run("cached_argv_is_authoritative", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "argv.json")
		if err := os.WriteFile(path, []byte(`["--name","role-worker"]`), 0o600); err != nil {
			t.Fatalf("write argv file: %v", err)
		}
		values := map[string]string{"PIG_HARNESS_ARGV_FILE": path}
		getenv := func(name string) string { return values[name] }
		unsetenv := func(name string) error { delete(values, name); return nil }
		cache := &ArgvCache{}
		if _, err := loadEffectiveArgs(getenv, unsetenv, []string{"pig"}, cache); err != nil {
			t.Fatalf("first load: %v", err)
		}
		// A caller that finds the variable set again (re-exported, or pointing at
		// an unreadable path) must get the argv cached by the first load: the load
		// is one process-wide critical section, not a per-caller file read.
		values["PIG_HARNESS_ARGV_FILE"] = filepath.Join(t.TempDir(), "absent.json")
		args, err := loadEffectiveArgs(getenv, unsetenv, []string{"pig"}, cache)
		if err != nil || strings.Join(args, " ") != "pig --name role-worker" {
			t.Fatalf("cached args=%v err=%v", args, err)
		}
	})
	t.Run("missing_file_fails_closed", func(t *testing.T) {
		values := map[string]string{"PIG_HARNESS_ARGV_FILE": filepath.Join(t.TempDir(), "absent.json")}
		_, err := loadEffectiveArgs(func(name string) string { return values[name] }, func(string) error { return nil }, nil, &ArgvCache{})
		if err == nil {
			t.Fatal("a missing argv file must fail closed")
		}
	})
	t.Run("invalid_json_fails_closed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "argv.json")
		if err := os.WriteFile(path, []byte(`{"not":"an array"}`), 0o600); err != nil {
			t.Fatalf("write argv file: %v", err)
		}
		values := map[string]string{"PIG_HARNESS_ARGV_FILE": path}
		_, err := loadEffectiveArgs(func(name string) string { return values[name] }, func(string) error { return nil }, nil, &ArgvCache{})
		if err == nil {
			t.Fatal("invalid argv JSON must fail closed")
		}
	})
	t.Run("bridge_failure_resolves_as_subagent", func(t *testing.T) {
		environment := identityFixture(map[string]string{"PIG_HARNESS_ARGV_FILE": filepath.Join(t.TempDir(), "absent.json")},
			[]AncestorProcess{{PID: 100, PPID: 1, Cmdline: "pi"}}, 100)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.HasPrefix(reason, "argv bridge failed:") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
	t.Run("bridge_marker_resolves_as_subagent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "argv.json")
		if err := os.WriteFile(path, []byte(`["--name","role-verifier"]`), 0o600); err != nil {
			t.Fatalf("write argv file: %v", err)
		}
		environment := identityFixture(map[string]string{"PIG_HARNESS_ARGV_FILE": path},
			[]AncestorProcess{{PID: 100, PPID: 1, Cmdline: "pi"}}, 100)
		isSubagent, reason := resolveAgentIdentityWithEnv(environment)
		if !isSubagent || !strings.Contains(reason, "launch marker: argv has --name role-") {
			t.Fatalf("isSubagent=%v reason=%q", isSubagent, reason)
		}
	})
}

// TestLoadEffectiveArgsConcurrent pins the process-wide critical section of the
// argv bridge: every concurrent caller must see the bridged argv. Without the
// lock a caller could read PIG_HARNESS_ARGV_FILE after a first caller unset it
// but before that caller cached the argv, and would silently fall back to the
// base argv (empty for a PiG subprocess), losing every negative launch marker.
// Run with -race.
func TestLoadEffectiveArgsConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "argv.json")
	if err := os.WriteFile(path, []byte(`["--name","role-worker"]`), 0o600); err != nil {
		t.Fatalf("write argv file: %v", err)
	}
	const (
		goroutines = 8
		rounds     = 200
	)
	for round := 0; round < rounds; round++ {
		var envMu sync.Mutex
		values := map[string]string{"PIG_HARNESS_ARGV_FILE": path}
		getenv := func(name string) string {
			envMu.Lock()
			defer envMu.Unlock()
			return values[name]
		}
		unsetenv := func(name string) error {
			envMu.Lock()
			defer envMu.Unlock()
			delete(values, name)
			return nil
		}
		cache := &ArgvCache{}
		results := make([][]string, goroutines)
		errs := make([]error, goroutines)
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(goroutines)
		for index := 0; index < goroutines; index++ {
			go func(index int) {
				defer done.Done()
				<-start
				results[index], errs[index] = loadEffectiveArgs(getenv, unsetenv, []string{"pig"}, cache)
			}(index)
		}
		close(start)
		done.Wait()
		for index, args := range results {
			if errs[index] != nil {
				t.Fatalf("round %d caller %d: %v", round, index, errs[index])
			}
			if joined := strings.Join(args, " "); joined != "pig --name role-worker" {
				t.Fatalf("round %d caller %d args = %q, want the bridged argv \"pig --name role-worker\"", round, index, joined)
			}
		}
		if _, stillSet := values["PIG_HARNESS_ARGV_FILE"]; stillSet {
			t.Fatalf("round %d: PIG_HARNESS_ARGV_FILE must be unset after the read", round)
		}
	}
}

// paneSocket starts a unix socket server that replies with reply for every
// connection and returns its path.
func paneSocket(t *testing.T, reply string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			buffer := make([]byte, 4096)
			_, _ = conn.Read(buffer)
			if reply != "" {
				_, _ = conn.Write([]byte(reply))
			}
			_ = conn.Close()
		}
	}()
	return path
}

// TestQueryPaneShellPid covers the socket protocol branches.
func TestQueryPaneShellPid(t *testing.T) {
	ok := `{"id":"pi-identity","result":{"process_info":{"pane_id":"w1:p1","shell_pid":4321}}}`
	cases := []struct {
		name    string
		reply   string
		paneID  string
		wantPID int
		wantOK  bool
	}{
		{"ok", ok, "w1:p1", 4321, true},
		{"pane_mismatch", ok, "w1:p2", 0, false},
		{"wrong_request_id", `{"id":"other","result":{"process_info":{"pane_id":"w1:p1","shell_pid":7}}}`, "w1:p1", 0, false},
		{"zero_pid", `{"id":"pi-identity","result":{"process_info":{"pane_id":"w1:p1","shell_pid":0}}}`, "w1:p1", 0, false},
		{"fractional_pid", `{"id":"pi-identity","result":{"process_info":{"pane_id":"w1:p1","shell_pid":4.5}}}`, "w1:p1", 0, false},
		{"bad_json", `not json`, "w1:p1", 0, false},
		{"empty_reply", "", "w1:p1", 0, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := paneSocket(t, testCase.reply)
			pid, ok := queryPaneShellPid(path, testCase.paneID, time.Second)
			if ok != testCase.wantOK || pid != testCase.wantPID {
				t.Fatalf("queryPaneShellPid = %d,%v want %d,%v", pid, ok, testCase.wantPID, testCase.wantOK)
			}
		})
	}

	t.Run("no_socket", func(t *testing.T) {
		if _, ok := queryPaneShellPid(filepath.Join(t.TempDir(), "absent.sock"), "w1:p1", 50*time.Millisecond); ok {
			t.Fatal("a missing socket must fail closed")
		}
	})
}

// TestQueryPaneShellPidTimeout pins the bounded query budget.
func TestQueryPaneShellPidTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		time.Sleep(2 * time.Second)
		_ = conn.Close()
	}()
	start := time.Now()
	if _, ok := queryPaneShellPid(path, "w1:p1", 40*time.Millisecond); ok {
		t.Fatal("a silent server must fail closed")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("query took %s, the budget must bound it", elapsed)
	}
}

// TestReadProcSelf exercises the real /proc readers on this process.
func TestReadProcSelf(t *testing.T) {
	pid := os.Getpid()
	ppid, ok := readProcPpid(pid)
	if !ok || ppid <= 0 {
		t.Fatalf("readProcPpid(%d) = %d,%v", pid, ppid, ok)
	}
	if cmdline := readProcCmdline(pid); cmdline == "" {
		t.Fatal("readProcCmdline returned nothing for this process")
	}
	chain := readAncestorChain(pid, identityAncestorMaxHops, readProcPpid, readProcCmdline)
	if len(chain) == 0 || chain[0].PID != pid {
		t.Fatalf("chain = %+v", chain)
	}
	if _, ok := readProcPpid(-1); ok {
		t.Fatal("an invalid pid must not resolve a ppid")
	}
}
