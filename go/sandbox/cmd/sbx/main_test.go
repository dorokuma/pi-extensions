package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dorokuma/pi-extensions/go/sandbox"
)

func TestParseArgsBasicAndFlags(t *testing.T) {
	cfg, err := parseArgs([]string{
		"--ro", "/usr,/bin",
		"--ro", "/etc",
		"--rw", "/tmp/work",
		"--exec", "/usr/bin/git",
		"--env", "FOO=bar",
		"--env", "BAZ=qux",
		"--deny-net",
		"--best-effort",
		"--",
		"git", "status", "-s",
	})
	if err != nil {
		t.Fatalf("parseArgs failed: %v", err)
	}

	wantRO := []string{"/usr", "/bin", "/etc"}
	if !reflect.DeepEqual(cfg.ReadOnly, wantRO) {
		t.Fatalf("ReadOnly = %v, want %v", cfg.ReadOnly, wantRO)
	}
	wantRW := []string{"/tmp/work"}
	if !reflect.DeepEqual(cfg.ReadWrite, wantRW) {
		t.Fatalf("ReadWrite = %v, want %v", cfg.ReadWrite, wantRW)
	}
	wantExec := []string{"/usr/bin/git"}
	if !reflect.DeepEqual(cfg.Exec, wantExec) {
		t.Fatalf("Exec = %v, want %v", cfg.Exec, wantExec)
	}
	wantEnv := []string{"FOO=bar", "BAZ=qux"}
	if !reflect.DeepEqual(cfg.Env, wantEnv) {
		t.Fatalf("Env = %v, want %v", cfg.Env, wantEnv)
	}
	if !cfg.DenyNet || !cfg.BestEffort || cfg.CheckOnly {
		t.Fatalf("unexpected bool flags: DenyNet=%v BestEffort=%v CheckOnly=%v", cfg.DenyNet, cfg.BestEffort, cfg.CheckOnly)
	}
	wantCmd := []string{"git", "status", "-s"}
	if !reflect.DeepEqual(cfg.Command, wantCmd) {
		t.Fatalf("Command = %v, want %v", cfg.Command, wantCmd)
	}
}

func TestParseArgsReviewerAliasEquivalence(t *testing.T) {
	workDir := t.TempDir()

	// 写法 1：兼容别名 `--reviewer WORKDIR -- cmd ...`
	cfgAlias, err := parseArgs([]string{
		"--reviewer", workDir,
		"--",
		"go", "test", "./...",
	})
	if err != nil {
		t.Fatalf("parseArgs(--reviewer) failed: %v", err)
	}

	// 写法 2：标准写法 `--preset reviewer --workdir DIR -- cmd ...`
	cfgPreset, err := parseArgs([]string{
		"--preset", "reviewer",
		"--workdir", workDir,
		"--",
		"go", "test", "./...",
	})
	if err != nil {
		t.Fatalf("parseArgs(--preset reviewer --workdir) failed: %v", err)
	}

	if !reflect.DeepEqual(cfgAlias, cfgPreset) {
		t.Fatalf("cliConfig mismatch between --reviewer and --preset reviewer --workdir:\nalias:  %+v\npreset: %+v", cfgAlias, cfgPreset)
	}

	polAlias, err := buildPolicy(cfgAlias)
	if err != nil {
		t.Fatalf("buildPolicy(cfgAlias) failed: %v", err)
	}
	defer cleanupPolicyTempDirs(polAlias)

	polPreset, err := buildPolicy(cfgPreset)
	if err != nil {
		t.Fatalf("buildPolicy(cfgPreset) failed: %v", err)
	}
	defer cleanupPolicyTempDirs(polPreset)

	if err := polAlias.Validate(); err != nil {
		t.Fatalf("polAlias.Validate() failed: %v", err)
	}
	if err := polPreset.Validate(); err != nil {
		t.Fatalf("polPreset.Validate() failed: %v", err)
	}

	if !reflect.DeepEqual(polAlias.ReadOnly, polPreset.ReadOnly) {
		t.Fatalf("ReadOnly mismatch: %v vs %v", polAlias.ReadOnly, polPreset.ReadOnly)
	}
	expectedReportDir := filepath.Join(workDir, sandbox.ReviewerReportSubdir)
	if len(polAlias.ReadWrite) < 2 || polAlias.ReadWrite[0] != expectedReportDir || polPreset.ReadWrite[0] != expectedReportDir {
		t.Fatalf("expected ReadWrite[0] to be %q, got alias=%v preset=%v", expectedReportDir, polAlias.ReadWrite, polPreset.ReadWrite)
	}
	if polAlias.DenyNet != polPreset.DenyNet || polAlias.BestEffort != polPreset.BestEffort {
		t.Fatalf("flags mismatch between policies")
	}
}

func TestParseArgsCheckAndErrors(t *testing.T) {
	cfgCheck, err := parseArgs([]string{"--check"})
	if err != nil {
		t.Fatalf("parseArgs(--check) failed: %v", err)
	}
	if !cfgCheck.CheckOnly {
		t.Fatal("expected CheckOnly=true")
	}

	// 缺少目标命令（且非 --check）应报错
	if _, err := parseArgs([]string{"--ro", "/usr"}); err == nil {
		t.Fatal("expected error when command is missing, got nil")
	}

	// 未知预设名应报错
	if _, err := parseArgs([]string{"--preset", "unknown-preset", "--", "true"}); err == nil {
		t.Fatal("expected error for unknown preset, got nil")
	}

	// --reviewer 与 --workdir 路径冲突应报错
	if _, err := parseArgs([]string{"--reviewer", "/tmp/a", "--workdir", "/tmp/b", "--", "true"}); err == nil {
		t.Fatal("expected error for conflicting --reviewer and --workdir, got nil")
	}

	// --reviewer 缺少有效目录参数（误吞 "--"）应报错
	if _, err := parseArgs([]string{"--reviewer", "--", "true"}); err == nil {
		t.Fatal("expected error when --reviewer is immediately followed by --, got nil")
	}
}

func TestMergeEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin:/bin", "HOME=/root", "TMPDIR=/tmp"}
	overrides := []string{"TMPDIR=/tmp/pi-reviewer-123", "GOTMPDIR=/tmp/pi-reviewer-123/go-tmp", "HOME=/home/reviewer"}

	merged := mergeEnv(base, overrides)
	want := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/reviewer",
		"TMPDIR=/tmp/pi-reviewer-123",
		"GOTMPDIR=/tmp/pi-reviewer-123/go-tmp",
	}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("mergeEnv = %v, want %v", merged, want)
	}
	// 确保原切片未被修改
	if base[2] != "TMPDIR=/tmp" {
		t.Fatalf("base slice was mutated: %v", base)
	}
}

func TestResolveCommandPathWithEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fakeGo := filepath.Join(dir, "go")
	if err := os.WriteFile(fakeGo, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fakeGo: %v", err)
	}

	resolved, err := resolveCommandPath("go", []string{"PATH=" + dir + ":/usr/bin:/bin"})
	if err != nil {
		t.Fatalf("resolveCommandPath failed: %v", err)
	}
	if resolved != fakeGo {
		t.Fatalf("resolveCommandPath = %q, want %q", resolved, fakeGo)
	}
}

func cleanupPolicyTempDirs(p sandbox.Policy) {
	for _, kv := range p.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if (k == "TMPDIR" && strings.HasPrefix(v, "/tmp/pi-reviewer-")) ||
			(k == "PI_SANDBOX_GO_EXEC_DIR" && strings.HasPrefix(v, "/tmp/pi-reviewer-exec-")) {
			_ = os.RemoveAll(v)
		}
	}
}
