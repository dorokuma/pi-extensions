package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func makeTestRepo(t *testing.T, defaultDest string) (repoRoot string, hostedSrc string, magpieSrc string, fedScript string) {
	t.Helper()
	repoRoot = t.TempDir()
	hostedSrc = filepath.Join(repoRoot, "extensions", "auto-continue", "src", "auto-continue.ts")
	magpieSrc = filepath.Join(repoRoot, "extensions", "magpie-image", "src", "magpie-image.ts")
	fedScript = filepath.Join(repoRoot, "upstream-codegraph-install.sh")
	writeTestFile(t, hostedSrc, "export default function autoContinue() {}\n")
	writeTestFile(t, magpieSrc, "export default function magpieImage() {}\n")
	writeTestFile(t, fedScript, "#!/usr/bin/env bash\necho spy\n")

	reg := Registry{
		SchemaVersion:  1,
		DefaultDestDir: defaultDest,
		NeverWrite:     []string{"herdr-agent-state.ts"},
		Extensions: []Extension{
			{
				Name:     "auto-continue",
				Category: "hosted",
				DestFile: "auto-continue.ts",
				Authority: Authority{
					Kind:       "in-repo",
					SourcePath: &hostedSrc,
				},
			},
			{
				Name:     "magpie-image",
				Category: "hosted",
				DestFile: "magpie-image.ts",
				Authority: Authority{
					Kind:       "in-repo",
					SourcePath: &magpieSrc,
				},
			},
			{
				Name:     "codegraph-go",
				Category: "federated",
				DestFile: "codegraph-go.ts",
				Authority: Authority{
					Kind:           "host-repo-cascade",
					InstallScript:  &fedScript,
					InstallDestEnv: strPtr("PI_EXT_DEST"),
				},
			},
			{
				Name:     "pi-cache-stats",
				Category: "federated",
				DestFile: "cache-stats.ts",
				Authority: Authority{
					Kind:           "host-repo-cascade",
					InstallScript:  strPtr(filepath.Join(repoRoot, "missing-upstream-install.sh")),
					InstallDestEnv: strPtr("PI_EXT_DEST"),
				},
			},
			{
				Name:     "herdr-agent-state",
				Category: "external",
				DestFile: "herdr-agent-state.ts",
				ReadOnly: true,
				Authority: Authority{
					Kind: "out-of-band-herdr",
				},
			},
			{
				Name:      "auto-continue-go",
				Category:  "native",
				EntryKind: "go-module",
				DestName:  "auto-continue",
				Authority: Authority{
					Kind: "in-repo",
				},
			},
		},
	}

	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	writeTestFile(t, filepath.Join(repoRoot, "federated", "registry.json"), string(data))
	return repoRoot, hostedSrc, magpieSrc, fedScript
}

func strPtr(s string) *string { return &s }

func runApp(t *testing.T, repoRoot string, envMap map[string]string, runner func(string, string, string) ([]byte, error), args ...string) (int, string, string) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	a := &app{
		repoRoot:  repoRoot,
		stdout:    &outBuf,
		stderr:    &errBuf,
		runScript: runner,
		getenv: func(k string) string {
			if envMap != nil {
				if v, ok := envMap[k]; ok {
					return v
				}
			}
			return ""
		},
	}
	rc := a.run(args)
	return rc, outBuf.String(), errBuf.String()
}

func trySymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported in current environment: %v", err)
	}
}

func TestHostedIdempotentAndMtimePreserved(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "ext")
	repoRoot, hostedSrc, _, _ := makeTestRepo(t, dest)
	env := map[string]string{"PI_EXT_ALLOW_ZERO_NODE_TS": "1"}

	rc, out, errOut := runApp(t, repoRoot, env, nil, "--dest-dir", dest, "auto-continue")
	if rc != 0 {
		t.Fatalf("first install rc=%d stderr=%s", rc, errOut)
	}
	if !strings.Contains(out, "changed — installed") || !strings.Contains(out, "/reload") {
		t.Fatalf("unexpected first install output: %s", out)
	}

	destFile := filepath.Join(dest, "auto-continue.ts")
	sum := md5.Sum([]byte("export default function autoContinue() {}\n"))
	wantMD5 := hex.EncodeToString(sum[:])
	gotMD5, err := md5OfRegular(destFile)
	if err != nil || gotMD5 != wantMD5 {
		t.Fatalf("dest md5 = %q (%v), want %q", gotMD5, err, wantMD5)
	}

	past := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(destFile, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	beforeInfo, err := os.Stat(destFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	rc2, out2, _ := runApp(t, repoRoot, env, nil, "--dest-dir", dest, "auto-continue")
	if rc2 != 0 {
		t.Fatalf("second install rc=%d", rc2)
	}
	if !strings.Contains(out2, "unchanged") || strings.Contains(out2, "/reload") {
		t.Fatalf("expected unchanged without /reload, got: %s", out2)
	}
	afterInfo, _ := os.Stat(destFile)
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatalf("mtime changed on unchanged install: %v -> %v", beforeInfo.ModTime(), afterInfo.ModTime())
	}
	_ = hostedSrc
}

func TestHostedAndFederatedNativeGoTakeover(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "ext")
	repoRoot, _, _, _ := makeTestRepo(t, dest)
	env := map[string]string{"PI_EXT_ALLOW_ZERO_NODE_TS": "1"}

	// Create native Go dir for auto-continue and stale .ts file.
	writeTestFile(t, filepath.Join(dest, "auto-continue", "go.mod"), "module example.com/ac\n")
	writeTestFile(t, filepath.Join(dest, "auto-continue.ts"), "stale\n")

	rc, out, errOut := runApp(t, repoRoot, env, nil, "--dest-dir", dest, "auto-continue")
	if rc != 0 {
		t.Fatalf("hosted native takeover rc=%d err=%s", rc, errOut)
	}
	if !strings.Contains(out, "skipped — native Go extension active") || !strings.Contains(out, "removed stale") {
		t.Fatalf("unexpected output: %s", out)
	}
	if pathExistsOrSymlink(filepath.Join(dest, "auto-continue.ts")) {
		t.Fatal("stale auto-continue.ts was not removed")
	}

	// Positive control: federated without native Go calls cascade script.
	destPos := filepath.Join(t.TempDir(), "ext-pos")
	called := 0
	spy := func(script, envName, destFile string) ([]byte, error) {
		called++
		return []byte("changed: installed\n"), nil
	}
	rcPos, outPos, _ := runApp(t, repoRoot, env, spy, "--dest-dir", destPos, "codegraph-go")
	if rcPos != 0 || called != 1 || !strings.Contains(outPos, "/reload") {
		t.Fatalf("federated positive control failed: rc=%d called=%d out=%s", rcPos, called, outPos)
	}

	// Negative: federated with native Go skips cascade and heals stale .ts.
	writeTestFile(t, filepath.Join(destPos, "codegraph-go", "go.mod"), "module example.com/cg\n")
	writeTestFile(t, filepath.Join(destPos, "codegraph-go.ts"), "stale\n")
	called = 0
	rcNeg, outNeg, errNeg := runApp(t, repoRoot, env, spy, "--dest-dir", destPos, "codegraph-go")
	if rcNeg != 0 || called != 0 || strings.Contains(outNeg+errNeg, "WARN") {
		t.Fatalf("federated native skip failed: rc=%d called=%d out=%s err=%s", rcNeg, called, outNeg, errNeg)
	}
	if pathExistsOrSymlink(filepath.Join(destPos, "codegraph-go.ts")) {
		t.Fatal("stale codegraph-go.ts was not removed")
	}

	// Missing upstream script + native Go active: install skips with zero WARN, list/audit merges status.
	writeTestFile(t, filepath.Join(destPos, "cache-stats", "go.mod"), "module example.com/cs\n")
	rcMiss, outMiss, errMiss := runApp(t, repoRoot, env, nil, "--dest-dir", destPos, "pi-cache-stats")
	if rcMiss != 0 || strings.Contains(outMiss+errMiss, "WARN") || !strings.Contains(outMiss, "skipped") {
		t.Fatalf("missing upstream + native Go install failed: rc=%d out=%s err=%s", rcMiss, outMiss, errMiss)
	}
	_, listOut, _ := runApp(t, repoRoot, env, nil, "--list", "--dest-dir", destPos)
	if !strings.Contains(listOut, "missing (upstream script not found; native Go active at") {
		t.Fatalf("list did not merge missing+native status: %s", listOut)
	}
	_, auditOut, _ := runApp(t, repoRoot, env, nil, "--audit", "--dest-dir", destPos)
	if !strings.Contains(auditOut, "MISMATCH — missing (upstream script not found; native Go active at") {
		t.Fatalf("audit did not emit MISMATCH for merged status: %s", auditOut)
	}
}

func TestDirWithoutGoModAndDryRun(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "ext")
	repoRoot, _, _, _ := makeTestRepo(t, dest)
	env := map[string]string{"PI_EXT_ALLOW_ZERO_NODE_TS": "1"}

	// Dir without go.mod must NOT trigger native Go takeover.
	if err := os.MkdirAll(filepath.Join(dest, "auto-continue"), 0o755); err != nil {
		t.Fatal(err)
	}
	rc, out, _ := runApp(t, repoRoot, env, nil, "--dest-dir", dest, "auto-continue")
	if rc != 0 || !strings.Contains(out, "changed — installed") {
		t.Fatalf("expected normal install when go.mod absent, got rc=%d out=%s", rc, out)
	}

	// Dry-run must not remove stale .ts or invoke federated script.
	destDry := filepath.Join(t.TempDir(), "ext-dry")
	writeTestFile(t, filepath.Join(destDry, "auto-continue", "go.mod"), "module x\n")
	writeTestFile(t, filepath.Join(destDry, "auto-continue.ts"), "stale\n")
	called := 0
	spy := func(script, envName, destFile string) ([]byte, error) {
		called++
		return nil, nil
	}
	rcDry, outDry, _ := runApp(t, repoRoot, env, spy, "--dry-run", "--dest-dir", destDry, "auto-continue", "codegraph-go")
	if rcDry != 0 || called != 0 || !strings.Contains(outDry, "[dry-run]") || !pathExistsOrSymlink(filepath.Join(destDry, "auto-continue.ts")) {
		t.Fatalf("dry-run mutated state: rc=%d called=%d out=%s", rcDry, called, outDry)
	}
}

func TestAuditDriftSameMD5IsMismatch(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "ext")
	repoRoot, hostedSrc, _, _ := makeTestRepo(t, dest)
	env := map[string]string{"PI_EXT_ALLOW_ZERO_NODE_TS": "1"}

	writeTestFile(t, filepath.Join(dest, "auto-continue", "go.mod"), "module x\n")
	srcBytes, err := os.ReadFile(hostedSrc)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dest, "auto-continue.ts"), string(srcBytes))

	rc, out, _ := runApp(t, repoRoot, env, nil, "--audit", "--dest-dir", dest)
	if rc != 0 {
		t.Fatalf("audit rc=%d", rc)
	}
	if !strings.Contains(out, "MISMATCH — DRIFT (stale .ts alongside native Go ext") {
		t.Fatalf("expected MISMATCH — DRIFT, got:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "MISMATCH — DRIFT") && strings.Contains(line, "MATCH — DRIFT") {
			t.Fatalf("audit emitted illegal MATCH — DRIFT: %s", line)
		}
	}
}

func TestNeverWriteThreeDefenseLayers(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "ext")
	repoRoot, _, _, _ := makeTestRepo(t, dest)
	env := map[string]string{"PI_EXT_ALLOW_ZERO_NODE_TS": "1"}

	herdrDest := filepath.Join(dest, "herdr-agent-state.ts")
	writeTestFile(t, herdrDest, "original herdr content")

	// Layer 1: explicit install of external category fails.
	rc, _, errOut := runApp(t, repoRoot, env, nil, "--dest-dir", dest, "herdr-agent-state")
	if rc != 1 || !strings.Contains(errOut, "refusing to install external/herdr-managed") {
		t.Fatalf("expected external refuse, rc=%d err=%s", rc, errOut)
	}

	// Tamper registry: change herdr-agent-state category to hosted (bypassing Layer 1).
	regPath := filepath.Join(repoRoot, "federated", "registry.json")
	raw, _ := os.ReadFile(regPath)
	var reg Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	fakeSrc := filepath.Join(repoRoot, "fake-herdr.ts")
	writeTestFile(t, fakeSrc, "tampered")
	for i := range reg.Extensions {
		if reg.Extensions[i].Name == "herdr-agent-state" {
			reg.Extensions[i].Category = "hosted"
			reg.Extensions[i].Authority.Kind = "in-repo"
			reg.Extensions[i].Authority.SourcePath = &fakeSrc
		}
	}
	tampered, _ := json.Marshal(reg)
	writeTestFile(t, regPath, string(tampered))

	// Layer 2: --all filters out herdr-agent-state.ts via hardNeverWrite.
	rcAll, outAll, _ := runApp(t, repoRoot, env, func(string, string, string) ([]byte, error) {
		return nil, nil
	}, "--all", "--dest-dir", dest)
	if rcAll != 0 || strings.Contains(outAll, "herdr-agent-state") {
		t.Fatalf("--all leaked tampered neverWrite entry: rc=%d out=%s", rcAll, outAll)
	}
	got, _ := os.ReadFile(herdrDest)
	if string(got) != "original herdr content" {
		t.Fatal("herdr-agent-state.ts was modified under --all")
	}

	// Layer 3: explicit install is still blocked by refuseIfNeverWrite.
	rcExp, _, errExp := runApp(t, repoRoot, env, nil, "--dest-dir", dest, "herdr-agent-state")
	if rcExp != 1 || !strings.Contains(errExp, "refusing to write neverWrite target") {
		t.Fatalf("Layer 3 failed: rc=%d err=%s", rcExp, errExp)
	}
}

func TestZeroNodeDualAnchorsAndOverrides(t *testing.T) {
	base := t.TempDir()
	fakeHome := filepath.Join(base, "fake_home")
	fakeDest := filepath.Join(fakeHome, ".pig", "agent", "extensions")
	if err := os.MkdirAll(fakeDest, 0o755); err != nil {
		t.Fatal(err)
	}
	repoRoot, _, _, _ := makeTestRepo(t, fakeDest)
	env := map[string]string{
		"HOME":                     fakeHome,
		"PI_EXT_TEST_PATH_NO_NODE": "1",
	}

	// --all: skips magpie-image & federated, heals stale magpie-image.ts.
	writeTestFile(t, filepath.Join(fakeDest, "auto-continue", "go.mod"), "module x\n")
	writeTestFile(t, filepath.Join(fakeDest, "magpie-image.ts"), "stale\n")
	called := 0
	spy := func(script, envName, destFile string) ([]byte, error) {
		called++
		return nil, nil
	}
	rcAll, outAll, _ := runApp(t, repoRoot, env, spy, "--all")
	if rcAll != 0 || called != 0 || !strings.Contains(outAll, "skipped — zero-node host") || pathExistsOrSymlink(filepath.Join(fakeDest, "magpie-image.ts")) {
		t.Fatalf("zero-node --all failed: rc=%d called=%d out=%s", rcAll, called, outAll)
	}

	// Explicit install hosted & federated refused.
	for _, target := range []string{"magpie-image", "codegraph-go"} {
		rc, _, errOut := runApp(t, repoRoot, env, spy, target)
		if rc != 1 || !strings.Contains(errOut, "refusing to install TypeScript extension") {
			t.Fatalf("expected zero-node refuse for %s, rc=%d err=%s", target, rc, errOut)
		}
	}

	// Path equivalence bypasses: trailing slash, dotdot, env without HOME, PI_EXT_DEST_DIR.
	cases := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{
			name: "trailing-slash",
			env:  env,
			args: []string{"--dest-dir", fakeDest + string(filepath.Separator), "magpie-image"},
		},
		{
			name: "dotdot",
			env:  env,
			args: []string{"--dest-dir", filepath.Join(fakeHome, ".pig", "agent", "..", "agent", "extensions"), "magpie-image"},
		},
		{
			name: "no-home-defaultDestDir-anchor",
			env:  map[string]string{"PI_EXT_TEST_PATH_NO_NODE": "1"},
			args: []string{"--dest-dir", fakeDest + string(filepath.Separator), "magpie-image"},
		},
		{
			name: "env-PI_EXT_DEST_DIR",
			env: map[string]string{
				"HOME":                     fakeHome,
				"PI_EXT_TEST_PATH_NO_NODE": "1",
				"PI_EXT_DEST_DIR":          fakeDest + string(filepath.Separator),
			},
			args: []string{"magpie-image"},
		},
	}
	for _, tc := range cases {
		rc, _, errOut := runApp(t, repoRoot, tc.env, nil, tc.args...)
		if rc != 1 || !strings.Contains(errOut, "refusing to install TypeScript extension") {
			t.Fatalf("bypass %s was not blocked: rc=%d err=%s", tc.name, rc, errOut)
		}
	}

	// Override with PI_EXT_ALLOW_ZERO_NODE_TS=1 succeeds.
	envAllow := map[string]string{
		"HOME":                      fakeHome,
		"PI_EXT_TEST_PATH_NO_NODE":  "1",
		"PI_EXT_ALLOW_ZERO_NODE_TS": "1",
	}
	rcOk, _, errOk := runApp(t, repoRoot, envAllow, nil, "magpie-image")
	if rcOk != 0 || !pathExistsOrSymlink(filepath.Join(fakeDest, "magpie-image.ts")) {
		t.Fatalf("override failed: rc=%d err=%s", rcOk, errOk)
	}

	// Explicit --dest-dir to another staging dir succeeds.
	otherDir := filepath.Join(base, "staging")
	rcOther, _, errOther := runApp(t, repoRoot, env, nil, "--dest-dir", otherDir, "magpie-image")
	if rcOther != 0 || !pathExistsOrSymlink(filepath.Join(otherDir, "magpie-image.ts")) {
		t.Fatalf("staging dir install failed: rc=%d err=%s", rcOther, errOther)
	}
}

func TestZeroNodeSymlinkedHomeBypass(t *testing.T) {
	base := t.TempDir()
	fakeHome := filepath.Join(base, "real_home")
	fakeDest := filepath.Join(fakeHome, ".pig", "agent", "extensions")
	if err := os.MkdirAll(fakeDest, 0o755); err != nil {
		t.Fatal(err)
	}
	symHome := filepath.Join(base, "sym_home")
	trySymlinkOrSkip(t, fakeHome, symHome)

	repoRoot, _, _, _ := makeTestRepo(t, filepath.Join(base, "unrelated-default"))
	env := map[string]string{
		"HOME":                     symHome,
		"PI_EXT_TEST_PATH_NO_NODE": "1",
	}
	rc, _, errOut := runApp(t, repoRoot, env, nil, "--dest-dir", fakeDest, "magpie-image")
	if rc != 1 || !strings.Contains(errOut, "refusing to install TypeScript extension") {
		t.Fatalf("symlinked HOME bypass not blocked: rc=%d err=%s", rc, errOut)
	}
}

func TestAuditUnregisteredKindsAndUpstreamFailureWarn(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "ext")
	repoRoot, _, _, _ := makeTestRepo(t, dest)
	env := map[string]string{"PI_EXT_ALLOW_ZERO_NODE_TS": "1"}

	// Create unregistered entries across categories.
	writeTestFile(t, filepath.Join(dest, "rogue.ts"), "x")
	writeTestFile(t, filepath.Join(dest, "no-tables.ts.bak-2026"), "bak")
	writeTestFile(t, filepath.Join(dest, "notes.txt"), "txt")
	writeTestFile(t, filepath.Join(dest, "__tests__", "unit.ts"), "test")
	writeTestFile(t, filepath.Join(dest, "custom", "nested.TS"), "nested")
	writeTestFile(t, filepath.Join(dest, "custom", "data.json"), "{}")

	rc, out, _ := runApp(t, repoRoot, env, nil, "--audit", "--dest-dir", dest)
	if rc != 0 {
		t.Fatalf("audit rc=%d", rc)
	}
	for _, want := range []string{
		"other *.ts",
		"historical backup",
		"other-file",
		"__tests__ dir",
		"__tests__ file",
		"nested *.ts",
		"dir [merged: dir=1 + __tests__ dir=1]: 2",
		"other [merged: other-file=1 + other=0]: 1",
		"nested non-.ts not listed: 1",
		"unregistered *.ts in dest-dir: rogue.ts custom/nested.TS",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit missing %q in output:\n%s", want, out)
		}
	}

	// Federated script failure is tolerated with WARN SUMMARY and exit 0.
	failRunner := func(string, string, string) ([]byte, error) {
		return []byte("boom"), errors.New("exit status 2")
	}
	rcFed, outFed, errFed := runApp(t, repoRoot, env, failRunner, "--dest-dir", dest, "codegraph-go")
	if rcFed != 0 || !strings.Contains(outFed, "WARN SUMMARY (1)") || !strings.Contains(errFed, "upstream install.sh failed") {
		t.Fatalf("federated failure handling wrong: rc=%d out=%s err=%s", rcFed, outFed, errFed)
	}
}
