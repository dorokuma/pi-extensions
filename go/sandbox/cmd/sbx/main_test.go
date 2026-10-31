package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/dorokuma/pi-extensions/go/sandbox"
)

const sbxHelperEnvKey = "PI_SBX_TEST_HELPER"

func TestParseArgsMultiValueAndRepeated(t *testing.T) {
	t.Parallel()

	// 1. 测试单标志后跟多个路径参数：`--ro DIR... --rw DIR... --exec BIN... --deny-net -- <cmd> [args]`
	args1 := []string{
		"--ro", "/", "/usr",
		"--rw", "/tmp/work", "/var/tmp",
		"--exec", "/bin/sh", "/usr/bin/git",
		"--deny-net",
		"--",
		"/usr/bin/git", "status", "-s",
	}
	cfg1, err := parseArgs(args1)
	if err != nil {
		t.Fatalf("parseArgs multi-value failed: %v", err)
	}
	if !reflect.DeepEqual(cfg1.policy.ReadOnly, []string{"/", "/usr"}) {
		t.Fatalf("ReadOnly: got %v, want [/ /usr]", cfg1.policy.ReadOnly)
	}
	if !reflect.DeepEqual(cfg1.policy.ReadWrite, []string{"/tmp/work", "/var/tmp"}) {
		t.Fatalf("ReadWrite: got %v, want [/tmp/work /var/tmp]", cfg1.policy.ReadWrite)
	}
	if !reflect.DeepEqual(cfg1.policy.Exec, []string{"/bin/sh", "/usr/bin/git"}) {
		t.Fatalf("Exec: got %v, want [/bin/sh /usr/bin/git]", cfg1.policy.Exec)
	}
	if !cfg1.policy.DenyNet {
		t.Fatal("expected DenyNet=true")
	}
	if cfg1.cmd != "/usr/bin/git" || !reflect.DeepEqual(cfg1.args, []string{"status", "-s"}) {
		t.Fatalf("cmd/args: got %q %v", cfg1.cmd, cfg1.args)
	}

	// 2. 测试重复标志与逗号分隔混合
	args2 := []string{
		"--ro=/",
		"--ro", "/opt",
		"--rw=/tmp/a,/tmp/b",
		"--exec", "/usr/bin/rg",
		"--best-effort",
		"--",
		"rg", "pattern",
	}
	cfg2, err := parseArgs(args2)
	if err != nil {
		t.Fatalf("parseArgs repeated failed: %v", err)
	}
	if !reflect.DeepEqual(cfg2.policy.ReadOnly, []string{"/", "/opt"}) {
		t.Fatalf("ReadOnly: got %v", cfg2.policy.ReadOnly)
	}
	if !reflect.DeepEqual(cfg2.policy.ReadWrite, []string{"/tmp/a", "/tmp/b"}) {
		t.Fatalf("ReadWrite: got %v", cfg2.policy.ReadWrite)
	}
	if !cfg2.policy.BestEffort {
		t.Fatal("expected BestEffort=true")
	}
	if cfg2.cmd != "rg" || !reflect.DeepEqual(cfg2.args, []string{"pattern"}) {
		t.Fatalf("cmd/args: got %q %v", cfg2.cmd, cfg2.args)
	}
}

func TestMergeEnv(t *testing.T) {
	t.Parallel()

	base := []string{"PATH=/usr/bin", "TMPDIR=/tmp", "HOME=/root"}
	overrides := []string{"TMPDIR=/tmp/pi-reviewer-123", "GOCACHE=/tmp/pi-reviewer-123/go-cache"}
	got := mergeEnv(base, overrides)
	want := []string{
		"PATH=/usr/bin",
		"TMPDIR=/tmp/pi-reviewer-123",
		"HOME=/root",
		"GOCACHE=/tmp/pi-reviewer-123/go-cache",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeEnv: got %v, want %v", got, want)
	}
}

func TestParseArgsErrors(t *testing.T) {
	t.Parallel()

	badCases := [][]string{
		{},
		{"--ro"},
		{"--ro", "--", "sh"},
		{"--rw", "--deny-net", "--", "sh"},
		{"--exec", "--", "sh"},
		{"--unknown-flag", "--", "sh"},
		{"--ro", "/", "--"},
	}
	for _, tc := range badCases {
		if _, err := parseArgs(tc); err == nil {
			t.Fatalf("expected error for args %v, got nil", tc)
		}
	}
}

func TestSbxApplyAndExecIntegration(t *testing.T) {
	if _, ok := sandbox.Supported(); !ok {
		t.Skip("Landlock is not supported on this kernel; skipping sbx exec integration test")
	}

	trueBin, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true binary not found in PATH")
	}
	if resolved, err := filepath.EvalSymlinks(trueBin); err == nil {
		trueBin = resolved
	}

	rwDir := t.TempDir()

	// 1. 目标命令在 --exec 白名单内：应成功执行并以 0 退出
	cmdOK := exec.Command(os.Args[0],
		"-test.run=^TestSbxSubprocessEntry$", "--",
		"--ro", "/",
		"--rw", rwDir,
		"--exec", trueBin,
		"--", trueBin,
	)
	cmdOK.Env = append(os.Environ(), sbxHelperEnvKey+"=1")
	if out, err := cmdOK.CombinedOutput(); err != nil {
		t.Fatalf("expected sbx allowed exec to succeed, got err=%v\noutput:\n%s", err, string(out))
	}

	// 2. 目标命令不在 --exec 白名单内：Apply 后 syscall.Exec 应因 EACCES 失败
	envBin, err := exec.LookPath("env")
	if err != nil {
		t.Skip("env binary not found in PATH")
	}
	if resolved, err := filepath.EvalSymlinks(envBin); err == nil {
		envBin = resolved
	}
	if envBin == trueBin {
		t.Skip("env and true point to the same multicall binary")
	}

	cmdDenied := exec.Command(os.Args[0],
		"-test.run=^TestSbxSubprocessEntry$", "--",
		"--ro", "/",
		"--rw", rwDir,
		"--exec", trueBin,
		"--", envBin,
	)
	cmdDenied.Env = append(os.Environ(), sbxHelperEnvKey+"=1")
	if err := cmdDenied.Run(); err == nil {
		t.Fatalf("expected sbx unlisted exec %s to fail, but it succeeded", envBin)
	}
}

func TestSbxSubprocessEntry(t *testing.T) {
	if os.Getenv(sbxHelperEnvKey) != "1" {
		return
	}
	var sbxArgs []string
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			sbxArgs = os.Args[i+1:]
			break
		}
	}
	cfg, err := parseArgs(sbxArgs)
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	binPath, err := resolveCommandPath(cfg.cmd)
	if err != nil {
		t.Fatalf("resolveCommandPath: %v", err)
	}
	if err := sandbox.Apply(cfg.policy); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	argv := append([]string{cfg.cmd}, cfg.args...)
	envv := mergeEnv(os.Environ(), cfg.policy.Env)
	err = syscall.Exec(binPath, argv, envv)
	t.Fatalf("syscall.Exec(%q) failed: %v", binPath, err)
}
