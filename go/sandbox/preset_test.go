package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeUsageJSON 写 <dir>/usage.json 为指定 lastUsed（Unix 秒）。
func writeUsageJSON(t *testing.T, dir string, lastUsed int64) {
	t.Helper()
	data, err := json.Marshal(struct {
		LastUsed int64 `json:"lastUsed"`
	}{LastUsed: lastUsed})
	if err != nil {
		t.Fatalf("marshal usage.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), data, 0o644); err != nil {
		t.Fatalf("write usage.json: %v", err)
	}
}

// readUsageLastUsed 读回 <dir>/usage.json 的 lastUsed。
func readUsageLastUsed(t *testing.T, dir string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatalf("read usage.json: %v", err)
	}
	var usage struct {
		LastUsed int64 `json:"lastUsed"`
	}
	if err := json.Unmarshal(data, &usage); err != nil {
		t.Fatalf("unmarshal usage.json: %v", err)
	}
	return usage.LastUsed
}

// TestTouchUsageJSONSkipWindow 覆盖 touchUsageJSON 的 <5m 跳过重写窗口：
// <5m 只读短路不改写、>=5m（含恰 5m 边界）原子更新 lastUsed、未来时间戳短路不改写。
func TestTouchUsageJSONSkipWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0) // 固定基准，避免与真实时钟漂移

	cases := []struct {
		name         string
		lastUsed     int64
		wantRewrites bool
	}{
		{"fresh_4m_skip", now.Add(-4 * time.Minute).Unix(), false},
		{"boundary_5m_rewrite", now.Add(-5 * time.Minute).Unix(), true},
		{"stale_6m_rewrite", now.Add(-6 * time.Minute).Unix(), true},
		{"future_1h_skip", now.Add(time.Hour).Unix(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeUsageJSON(t, dir, tc.lastUsed)
			if err := touchUsageJSON(dir, now); err != nil {
				t.Fatalf("touchUsageJSON: %v", err)
			}
			got := readUsageLastUsed(t, dir)
			if tc.wantRewrites {
				if got != now.Unix() {
					t.Fatalf("expected lastUsed rewritten to %d, got %d", now.Unix(), got)
				}
			} else if got != tc.lastUsed {
				t.Fatalf("expected lastUsed unchanged %d, got %d", tc.lastUsed, got)
			}
		})
	}
}

// unsetEnv 保证 key 在本测试内不存在，并在 t.Cleanup 恢复原值（含原本未设置情形）。
// 与 sandbox_test.go 中同名 helper 语义一致，preset_test.go 属 package sandbox 需要自有副本。
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	orig, existed := os.LookupEnv(key)
	if existed {
		_ = os.Unsetenv(key)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// readExtensionsArray 解析注入后 settings.json 的 extensions 字段为 []string。
func readExtensionsArray(t *testing.T, data []byte) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("settings.json not a JSON object: %v", err)
	}
	var entries []string
	if err := json.Unmarshal(obj["extensions"], &entries); err != nil {
		t.Fatalf("extensions must be a string array: %s err=%v", obj["extensions"], err)
	}
	return entries
}

// TestInjectHostExtensionsSetting 覆盖 SBX-M3：
//   - 无 extensions 字段注入 [hostExtDir]；
//   - null / 非数组原样保留；
//   - 数组：相对本地路径条目相对 <pigHome>/agent 解析、+/-//~/URI/排除项正确处理，
//     未含等价 hostExtDir 且未显式排除时追加。
func TestInjectHostExtensionsSetting(t *testing.T) {
	tmp := t.TempDir()
	agentDir := filepath.Join(tmp, "agent")
	hostExtDir := filepath.Join(agentDir, "extensions")
	if err := os.MkdirAll(hostExtDir, 0o755); err != nil {
		t.Fatalf("mkdir hostExtDir: %v", err)
	}

	t.Run("inject when absent", func(t *testing.T) {
		out := injectHostExtensionsSetting([]byte(`{"theme":"dark"}`), hostExtDir)
		got := readExtensionsArray(t, out)
		if len(got) != 1 || got[0] != hostExtDir {
			t.Fatalf("extensions = %v, want [%s]", got, hostExtDir)
		}
	})

	t.Run("null preserved verbatim", func(t *testing.T) {
		src := []byte(`{"extensions":null,"theme":"dark"}`)
		if got := injectHostExtensionsSetting(src, hostExtDir); string(got) != string(src) {
			t.Fatalf("null extensions must be preserved verbatim, got %s", got)
		}
	})

	t.Run("non-array preserved verbatim", func(t *testing.T) {
		src := []byte(`{"extensions":"bogus","theme":"dark"}`)
		if got := injectHostExtensionsSetting(src, hostExtDir); string(got) != string(src) {
			t.Fatalf("non-array extensions must be preserved verbatim, got %s", got)
		}
	})

	t.Run("object preserved verbatim", func(t *testing.T) {
		src := []byte(`{"extensions":{"a":1}}`)
		if got := injectHostExtensionsSetting(src, hostExtDir); string(got) != string(src) {
			t.Fatalf("object extensions must be preserved verbatim, got %s", got)
		}
	})

	t.Run("array relative resolved and host appended", func(t *testing.T) {
		src := `{"theme":"dark","extensions":["/abs/ext","./rel-ext","sub/dir","npm:pkg","git:repo","https://x/y","~/.ext","+./plus-rel","!excluded"],"other":1}`
		out := injectHostExtensionsSetting([]byte(src), hostExtDir)
		got := readExtensionsArray(t, out)
		want := []string{
			"/abs/ext",
			filepath.Join(agentDir, "rel-ext"),
			filepath.Join(agentDir, "sub", "dir"),
			"npm:pkg",
			"git:repo",
			"https://x/y",
			"~/.ext",
			"+" + filepath.Join(agentDir, "plus-rel"),
			"!excluded",
			hostExtDir,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("extensions = %v, want %v", got, want)
		}
		// 原有键值保留
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		var theme string
		if err := json.Unmarshal(obj["theme"], &theme); err != nil || theme != "dark" {
			t.Fatalf("theme lost: %s err=%v", obj["theme"], err)
		}
	})

	t.Run("array already contains hostExtDir not duplicated", func(t *testing.T) {
		src := `{"extensions":["./a","` + hostExtDir + `"]}`
		got := readExtensionsArray(t, injectHostExtensionsSetting([]byte(src), hostExtDir))
		count := 0
		for _, e := range got {
			if e == hostExtDir {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("hostExtDir duplicated: %v", got)
		}
	})

	t.Run("explicit exclusion prevents append", func(t *testing.T) {
		for _, excl := range []string{"-" + hostExtDir, "!" + hostExtDir, "-extensions", "!extensions"} {
			src := `{"extensions":["` + excl + `"]}`
			got := readExtensionsArray(t, injectHostExtensionsSetting([]byte(src), hostExtDir))
			for _, e := range got {
				if e == hostExtDir {
					t.Fatalf("exclusion %q must prevent appending hostExtDir, got %v", excl, got)
				}
			}
		}
	})

	t.Run("equivalent host ext entries not duplicated", func(t *testing.T) {
		// "./extensions"/"extensions"/"+./extensions" 经相对 agentDir 解析后均等价
		// hostExtDir：必须先解析再判重，不得二次追加造成重复。
		for _, entry := range []string{"./extensions", "extensions", "+./extensions", "./extensions/"} {
			src := `{"extensions":["` + entry + `"]}`
			got := readExtensionsArray(t, injectHostExtensionsSetting([]byte(src), hostExtDir))
			count := 0
			for _, e := range got {
				if e == hostExtDir || e == "+"+hostExtDir {
					count++
				}
			}
			if count != 1 || len(got) != 1 {
				t.Fatalf("entry %q resolved to hostExtDir must not be duplicated, got %v", entry, got)
			}
		}
	})

	t.Run("relative exclusion rewritten to absolute and prevents append", func(t *testing.T) {
		// "-./extensions"/"!./extensions" 应识别为排除 hostExtDir；同时相对排除路径
		// 在私有 AgentDir 下会失配，必须重写为相对宿主 agentDir 的绝对排除路径。
		for _, tc := range []struct{ entry, want string }{
			{"-./extensions", "-" + hostExtDir},
			{"!./extensions", "!" + hostExtDir},
		} {
			src := `{"extensions":["` + tc.entry + `"]}`
			got := readExtensionsArray(t, injectHostExtensionsSetting([]byte(src), hostExtDir))
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("entry %q must be rewritten to %q and prevent append, got %v", tc.entry, tc.want, got)
			}
		}
	})

	t.Run("empty array gets host appended", func(t *testing.T) {
		got := readExtensionsArray(t, injectHostExtensionsSetting([]byte(`{"extensions":[]}`), hostExtDir))
		if len(got) != 1 || got[0] != hostExtDir {
			t.Fatalf("extensions = %v, want [%s]", got, hostExtDir)
		}
	})

	t.Run("invalid JSON returned verbatim", func(t *testing.T) {
		src := []byte(`{not json`)
		if got := injectHostExtensionsSetting(src, hostExtDir); string(got) != string(src) {
			t.Fatalf("invalid JSON must be returned verbatim, got %s", got)
		}
	})
}

// TestInjectPigCodingAgentDirPiDirs 覆盖 SBX-S1：PIG_USE_PI_DIRS=1（宿主环境或 Policy.Env）
// 且 PI_CODING_AGENT_DIR 未设置时，对称注入 PI_CODING_AGENT_DIR 指向私有目录。
func TestInjectPigCodingAgentDirPiDirs(t *testing.T) {
	private := "/tmp/pi-reviewer-test-xyz/pig-agent"

	baseEnv := []string{"TMPDIR=/tmp/pi-reviewer-test-xyz"}

	t.Run("PIG_USE_PI_DIRS host env injects PI_CODING_AGENT_DIR", func(t *testing.T) {
		t.Setenv("PIG_USE_PI_DIRS", "1")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		got := injectPigCodingAgentDir(append([]string(nil), baseEnv...), private)
		m := parseEnvSlice(got)
		if m["PIG_CODING_AGENT_DIR"] != private {
			t.Fatalf("PIG_CODING_AGENT_DIR = %q", m["PIG_CODING_AGENT_DIR"])
		}
		if m["PI_CODING_AGENT_DIR"] != private {
			t.Fatalf("expected PI_CODING_AGENT_DIR injected to %q, env=%v", private, got)
		}
	})

	t.Run("PIG_USE_PI_DIRS in Policy.Env injects PI_CODING_AGENT_DIR", func(t *testing.T) {
		unsetEnv(t, "PIG_USE_PI_DIRS")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		env := append(append([]string(nil), baseEnv...), "PIG_USE_PI_DIRS=1")
		got := injectPigCodingAgentDir(env, private)
		m := parseEnvSlice(got)
		if m["PI_CODING_AGENT_DIR"] != private {
			t.Fatalf("expected PI_CODING_AGENT_DIR injected via Policy.Env flag, env=%v", got)
		}
	})

	t.Run("PI_CODING_AGENT_DIR already set in env not overridden", func(t *testing.T) {
		t.Setenv("PIG_USE_PI_DIRS", "1")
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		env := append(append([]string(nil), baseEnv...), "PI_CODING_AGENT_DIR=/custom/pi-agent")
		got := injectPigCodingAgentDir(env, private)
		m := parseEnvSlice(got)
		if m["PI_CODING_AGENT_DIR"] != "/custom/pi-agent" {
			t.Fatalf("PI_CODING_AGENT_DIR must be preserved, got %q", m["PI_CODING_AGENT_DIR"])
		}
	})

	t.Run("PI_CODING_AGENT_DIR host env not overridden", func(t *testing.T) {
		t.Setenv("PIG_USE_PI_DIRS", "1")
		t.Setenv("PI_CODING_AGENT_DIR", "/host/pi-agent")
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		got := injectPigCodingAgentDir(append([]string(nil), baseEnv...), private)
		m := parseEnvSlice(got)
		if m["PI_CODING_AGENT_DIR"] != "" {
			t.Fatalf("PI_CODING_AGENT_DIR must not be injected when host env set, got %q", m["PI_CODING_AGENT_DIR"])
		}
		if m["PIG_CODING_AGENT_DIR"] != private {
			t.Fatalf("PIG_CODING_AGENT_DIR = %q", m["PIG_CODING_AGENT_DIR"])
		}
	})

	t.Run("PIG_USE_PI_DIRS unset no symmetric injection", func(t *testing.T) {
		unsetEnv(t, "PIG_USE_PI_DIRS")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		got := injectPigCodingAgentDir(append([]string(nil), baseEnv...), private)
		m := parseEnvSlice(got)
		if m["PI_CODING_AGENT_DIR"] != "" {
			t.Fatalf("PI_CODING_AGENT_DIR must not be injected without PIG_USE_PI_DIRS, got %v", got)
		}
		if m["PIG_CODING_AGENT_DIR"] != private {
			t.Fatalf("PIG_CODING_AGENT_DIR = %q", m["PIG_CODING_AGENT_DIR"])
		}
	})

	t.Run("PIG_CODING_AGENT_DIR set short-circuits both", func(t *testing.T) {
		t.Setenv("PIG_USE_PI_DIRS", "1")
		t.Setenv("PIG_CODING_AGENT_DIR", "/custom/agent")
		got := injectPigCodingAgentDir(append([]string(nil), baseEnv...), private)
		m := parseEnvSlice(got)
		if m["PIG_CODING_AGENT_DIR"] != "" || m["PI_CODING_AGENT_DIR"] != "" {
			t.Fatalf("explicit PIG_CODING_AGENT_DIR must short-circuit, env=%v", got)
		}
	})
}

// parseEnvSlice 将 KEY=VAL 切片转 map（包内 helper，测试专用）。
func parseEnvSlice(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// TestResolvePolicyPathWithin 覆盖 SBX-M2 的软链/逃逸防线：
// 软链本体被拒、逃逸出 root 的被拒、指向 Exec 中路径的被拒。
func TestResolvePolicyPathWithin(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	inside := filepath.Join(root, "state")
	outside := filepath.Join(tmp, "outside")
	execDir := filepath.Join(tmp, "execdir")
	for _, d := range []string{inside, outside, execDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	p := &Policy{Exec: []string{filepath.Join(execDir, "bin")}}

	t.Run("plain dir inside root accepted", func(t *testing.T) {
		if got := resolvePolicyPathWithin(inside, root, p); got != inside {
			t.Fatalf("resolvePolicyPathWithin(%q) = %q, want %q", inside, got, inside)
		}
	})

	t.Run("symlink entry rejected", func(t *testing.T) {
		link := filepath.Join(root, "link")
		if err := os.Symlink(inside, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if got := resolvePolicyPathWithin(link, root, p); got != "" {
			t.Fatalf("symlink %q must be rejected, got %q", link, got)
		}
	})

	t.Run("symlink escaping root rejected", func(t *testing.T) {
		link := filepath.Join(root, "escape")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if got := resolvePolicyPathWithin(link, root, p); got != "" {
			t.Fatalf("escaping symlink %q must be rejected, got %q", link, got)
		}
	})

	t.Run("root itself rejected", func(t *testing.T) {
		if got := resolvePolicyPathWithin(root, root, p); got != "" {
			t.Fatalf("root dir itself must not be granted, got %q", got)
		}
	})

	t.Run("dir resolving into Exec path rejected", func(t *testing.T) {
		// 构造 root 内普通目录但其内容与 p.Exec 中路径同名重合的场景靠路径本身判断；
		// 这里直接验证 execDir 不在 root 内已被拒，且 root 内路径等于 execDir 解析目标时被拒。
		p2 := &Policy{Exec: []string{inside}}
		if got := resolvePolicyPathWithin(inside, root, p2); got != "" {
			t.Fatalf("dir equal to Exec entry must be rejected, got %q", got)
		}
	})

	t.Run("nonexistent rejected", func(t *testing.T) {
		if got := resolvePolicyPathWithin(filepath.Join(root, "missing"), root, p); got != "" {
			t.Fatalf("nonexistent dir must be rejected, got %q", got)
		}
	})

	t.Run("non-dir rejected", func(t *testing.T) {
		f := filepath.Join(root, "file")
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got := resolvePolicyPathWithin(f, root, p); got != "" {
			t.Fatalf("non-dir must be rejected, got %q", got)
		}
	})

	t.Run("symlinked root ancestor accepted, mid-level symlink rejected", func(t *testing.T) {
		// root 经祖先软链传入（如 macOS /var -> /private/var）：root 内部真实
		// 子目录仍应放行，但 root 内部任意层级的中间软链必须被拒。
		realParent := filepath.Join(tmp, "real-parent")
		realRoot := filepath.Join(realParent, "root")
		realInside := filepath.Join(realRoot, "sub")
		if err := os.MkdirAll(realInside, 0o755); err != nil {
			t.Fatalf("mkdir realInside: %v", err)
		}
		symParent := filepath.Join(tmp, "sym-parent")
		if err := os.Symlink(realParent, symParent); err != nil {
			t.Fatalf("symlink sym-parent: %v", err)
		}
		symRoot := filepath.Join(symParent, "root")
		if got := resolvePolicyPathWithin(filepath.Join(symRoot, "sub"), symRoot, p); got != realInside {
			t.Fatalf("dir under symlinked root ancestor must be accepted as %q, got %q", realInside, got)
		}

		// root 内部中间层软链：<root>/sym-mid -> <realParent>/midtarget，
		// candidate <root>/sym-mid/sub 虽解析后仍在 root 的真实父树内，
		// 但中途经 root 内部软链必须被拒。
		midTarget := filepath.Join(realParent, "midtarget")
		midSub := filepath.Join(midTarget, "sub")
		if err := os.MkdirAll(midSub, 0o755); err != nil {
			t.Fatalf("mkdir midSub: %v", err)
		}
		if err := os.Symlink(midTarget, filepath.Join(realRoot, "sym-mid")); err != nil {
			t.Fatalf("symlink sym-mid: %v", err)
		}
		if got := resolvePolicyPathWithin(filepath.Join(realRoot, "sym-mid", "sub"), symRoot, p); got != "" {
			t.Fatalf("mid-level symlink %q inside root must be rejected, got %q", filepath.Join(realRoot, "sym-mid", "sub"), got)
		}
	})

	t.Run("dir overlapping Exec path as parent or child rejected", func(t *testing.T) {
		// resolved 是某个 Exec 文件的父目录，或位于某个 Exec 目录之下时，
		// 都会造成 W^X 写穿，必须被拒（不再仅判 resolved == ep）。
		execRoot := filepath.Join(root, "execroot")
		execParent := filepath.Join(execRoot, "parent")
		execFile := filepath.Join(execParent, "runner")
		if err := os.MkdirAll(execParent, 0o755); err != nil {
			t.Fatalf("mkdir execParent: %v", err)
		}
		if err := os.WriteFile(execFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write runner: %v", err)
		}
		underExec := filepath.Join(execParent, "sub")
		if err := os.MkdirAll(underExec, 0o755); err != nil {
			t.Fatalf("mkdir underExec: %v", err)
		}

		pExec := &Policy{Exec: []string{execFile}}
		if got := resolvePolicyPathWithin(execParent, root, pExec); got != "" {
			t.Fatalf("parent dir of Exec file must be rejected, got %q", got)
		}
		pExecDir := &Policy{Exec: []string{execParent}}
		if got := resolvePolicyPathWithin(underExec, root, pExecDir); got != "" {
			t.Fatalf("dir under Exec dir must be rejected, got %q", got)
		}
		if got := resolvePolicyPathWithin(execParent, root, pExecDir); got != "" {
			t.Fatalf("dir equal to Exec dir must be rejected, got %q", got)
		}
	})
}

// TestAppendCtxmodeStateReadWriteRejectsSymlink 覆盖 SBX-M2：
// 把 home 下 ctxmode 兜底路径中的目录替换为逃逸软链，验证不进 ReadWrite。
func TestAppendCtxmodeStateReadWriteRejectsSymlink(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	ctxTarget := filepath.Join(tmp, "outside-ctx")
	if err := os.MkdirAll(ctxTarget, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	share := filepath.Join(home, ".local", "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatalf("mkdir share: %v", err)
	}
	ctxmodePath := filepath.Join(share, "ctxmode")
	if err := os.Symlink(ctxTarget, ctxmodePath); err != nil {
		t.Fatalf("symlink ctxmode: %v", err)
	}

	// 模拟 homeDir() 返回伪造 home：appendCtxmodeStateReadWrite 内部使用
	// dedupeStrings([]string{homeDir(), "/root", "/root/.agy-pro-home"})。
	// 这里直接以伪造 home 作为 root 调用 resolvePolicyPathWithin 做单元级验证，
	// 并确认软链目录不会被加进 ReadWrite。
	p := &Policy{}
	if got := resolvePolicyPathWithin(ctxmodePath, home, p); got != "" {
		t.Fatalf("ctxmode symlink must be rejected, got %q", got)
	}
	if len(p.ReadWrite) != 0 {
		t.Fatalf("ReadWrite must stay empty, got %v", p.ReadWrite)
	}

	// 正常目录在 root 内可放行。
	real := filepath.Join(share, "realctx")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	if got := resolvePolicyPathWithin(real, home, p); got != real {
		t.Fatalf("real ctxmode dir rejected unexpectedly")
	}
}

// TestAppendCellCachePolicySymlinkEscape 覆盖 SBX-M2：cells/<lang>/.locks 为逃逸
// 软链时不得进 ReadWrite；正常 .locks 进 ReadWrite，且不与 Exec 重合。
func TestAppendCellCachePolicySymlinkEscape(t *testing.T) {
	tmp := t.TempDir()
	pigHome := filepath.Join(tmp, "pig")
	cacheRoot := filepath.Join(pigHome, "cache")
	cellsGo := filepath.Join(cacheRoot, "cells", "go")
	extDir := filepath.Join(cacheRoot, "ext")
	outside := filepath.Join(tmp, "outside")
	for _, d := range []string{cellsGo, extDir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// cells/go/.locks 为指向外部的软链 → 必须被拒。
	if err := os.Symlink(outside, filepath.Join(cellsGo, ".locks")); err != nil {
		t.Fatalf("symlink .locks: %v", err)
	}
	// ext/.locks 为正常目录 → 必须放行。
	if err := os.MkdirAll(filepath.Join(extDir, ".locks"), 0o755); err != nil {
		t.Fatalf("mkdir ext/.locks: %v", err)
	}

	p := &Policy{}
	appendCellCachePolicy(p, pigHome)

	locksLink := filepath.Join(cellsGo, ".locks")
	locksReal := filepath.Join(extDir, ".locks")
	foundLink, foundReal := false, false
	for _, rw := range p.ReadWrite {
		if rw == locksLink || rw == outside {
			foundLink = true
		}
		if rw == locksReal {
			foundReal = true
		}
		if strings.HasPrefix(rw, outside) {
			t.Fatalf("outside dir leaked into ReadWrite: %q", rw)
		}
	}
	if foundLink {
		t.Fatalf("symlink .locks must not be in ReadWrite, got %v", p.ReadWrite)
	}
	if !foundReal {
		t.Fatalf("real ext/.locks must be in ReadWrite, got %v", p.ReadWrite)
	}
}

// TestOverrideEnvKeyDeduplicates 覆盖 overrideEnvKey 同名键去重加固：
// 无同名键追加、单同名键保序原地替换、多同名键合并为唯一首位置条目，
// 且不修改原输入切片。
func TestOverrideEnvKeyDeduplicates(t *testing.T) {
	t.Run("no existing key appends", func(t *testing.T) {
		env := []string{"A=1", "B=2"}
		got := overrideEnvKey(env, "XDG_RUNTIME_DIR", "/rt")
		want := []string{"A=1", "B=2", "XDG_RUNTIME_DIR=/rt"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("single key replaced in place", func(t *testing.T) {
		env := []string{"A=1", "XDG_RUNTIME_DIR=/run/user/0", "B=2"}
		got := overrideEnvKey(env, "XDG_RUNTIME_DIR", "/rt")
		want := []string{"A=1", "XDG_RUNTIME_DIR=/rt", "B=2"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("duplicate keys collapse to first position", func(t *testing.T) {
		env := []string{"A=1", "XDG_RUNTIME_DIR=/run/user/0", "B=2", "XDG_RUNTIME_DIR=/dup"}
		orig := append([]string(nil), env...)
		got := overrideEnvKey(env, "XDG_RUNTIME_DIR", "/rt")
		want := []string{"A=1", "XDG_RUNTIME_DIR=/rt", "B=2"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		// 输入切片不被修改（新分配输出）。
		if !reflect.DeepEqual(env, orig) {
			t.Fatalf("input env mutated: %v -> %v", orig, env)
		}
		// 输出中同名键仅剩一个。
		count := 0
		for _, kv := range got {
			if strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected exactly one XDG_RUNTIME_DIR entry, got %d in %v", count, got)
		}
	})
}

// ---------------------------------------------------------------------------
// 宿主态 Go 扩展 cell 预热（MaybeWarmGoExtensionCells）测试
// ---------------------------------------------------------------------------

// warmTestFixture 是预热测试的假 PIG_HOME 布局。
type warmTestFixture struct {
	pigHome   string
	cacheRoot string // <pigHome>/cache
	fakePig   string
	extDir    string // <pigHome>/agent/extensions/fake-ext 符号链接（生产布局）
	extReal   string // 软链目标的真实源码目录
	logPath   string
}

// newWarmTestFixture 构造假 PIG_HOME：
//   - fakeBin/pig：可执行脚本（pigScript 为空时由调用方稍后写入），测试
//     经包内未导出钩子 warmGoCellHook.PigBin 显式注入该路径——生产路径
//     resolvePigEntryBinary 只在固定可信目录解析 pig，绝不读调用方 PATH；
//   - agent/extensions/fake-ext：指向 pigHome 之外真实源码目录的目录
//     符号链接（与生产布局 /root/.pig/agent/extensions/<ext> ->
//     /root/workspace/pi-extensions/go/<ext> 完全一致），内含 go.mod + main.go；
//   - withSDK 为 true 时预建 state/pigsdk/sdk-test（含 go.mod）；
//     false 时留空以覆盖 state/pigsdk 缺失的真实冷启动。
//
// 显式 AgentDir 相关环境变量一律剥离（unsetEnv），保证测试封闭：
// 宿主 PIG_CODING_AGENT_DIR / PI_CODING_AGENT_DIR / PIG_USE_PI_DIRS 若泄漏
// 进测试，resolveSourcePigAgentDir 会把源 AgentDir 指离夹具而跳过预热。
func newWarmTestFixture(t *testing.T, pigScript string, withSDK bool) *warmTestFixture {
	t.Helper()
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	unsetEnv(t, "PI_CODING_AGENT_DIR")
	unsetEnv(t, "PIG_USE_PI_DIRS")
	root := t.TempDir()
	fx := &warmTestFixture{
		pigHome:   filepath.Join(root, "pig-home"),
		cacheRoot: filepath.Join(root, "pig-home", "cache"),
		extDir:    filepath.Join(root, "pig-home", "agent", "extensions", "fake-ext"),
		extReal:   filepath.Join(root, "ext-src", "fake-ext"),
		logPath:   filepath.Join(root, "warm.log"),
	}
	fakeBin := filepath.Join(root, "fake-bin")
	fx.fakePig = filepath.Join(fakeBin, "pig")

	dirs := []string{
		fakeBin, fx.pigHome, filepath.Dir(fx.extDir), fx.extReal,
	}
	if withSDK {
		dirs = append(dirs, filepath.Join(fx.pigHome, "state", "pigsdk", "sdk-test"))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	if pigScript != "" {
		fx.setPigScript(t, pigScript)
	}
	// fake-ext 必须是目录符号链接：复刻生产环境扩展布局，
	// 覆盖 WalkDir 对顶层软链不递归的关键路径。
	if err := os.Symlink(fx.extReal, fx.extDir); err != nil {
		t.Fatalf("Symlink %s -> %s: %v", fx.extDir, fx.extReal, err)
	}
	writeFile := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}
	writeFile(filepath.Join(fx.extReal, "go.mod"), "module fake/ext\n\ngo 1.22\n")
	writeFile(filepath.Join(fx.extReal, "main.go"), "package main\n")
	if withSDK {
		writeFile(filepath.Join(fx.pigHome, "state", "pigsdk", "sdk-test", "go.mod"), "module sdk\n")
	}

	t.Setenv("PIG_HOME", fx.pigHome)
	return fx
}

// setPigScript 覆写假 pig 可执行脚本。
func (f *warmTestFixture) setPigScript(t *testing.T, script string) {
	t.Helper()
	if err := os.WriteFile(f.fakePig, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake pig: %v", err)
	}
}

// warmEnvLogScript 返回记录环境到 logPath 的假 pig 脚本：
// cellSnippet 创建 cache/cells/go 合包或 cache/ext 单扩展产物，
// extraSnippet 追加自定义逻辑（如落盘 SDK / 源码漂移）。
func (f *warmTestFixture) warmEnvLogScript(cellSnippet, extraSnippet string) string {
	return fmt.Sprintf(`#!/bin/sh
{
  echo "ARGS=$*"
  echo "CWD=$(pwd)"
  echo "HOME=${HOME-<unset>}"
  echo "PATH=${PATH-<unset>}"
  echo "GOFLAGS=${GOFLAGS-<unset>}"
  echo "LD_PRELOAD=${LD_PRELOAD-<unset>}"
  echo "PIG_CODING_AGENT_DIR=${PIG_CODING_AGENT_DIR-<unset>}"
  echo "PI_CODING_AGENT_DIR=${PI_CODING_AGENT_DIR-<unset>}"
  echo "PIG_HOME=${PIG_HOME-<unset>}"
  echo "GOPROXY=${GOPROXY-<unset>}"
  echo "GOWORK=${GOWORK-<unset>}"
} >> '%[1]s'
%[2]s
%[3]s
exit 0
`, f.logPath, cellSnippet, extraSnippet)
}

// bundledCellSnippet 创建合包形态产物 cache/cells/go/<digest>/runner
// （language 显式 "go"，与真实 pig 落盘的 ready.json 一致）。
const bundledCellSnippet = `cell="$PIG_HOME/cache/cells/go/digestwarm0001"
mkdir -p "$cell"
printf '#!/bin/sh\nexit 0\n' > "$cell/runner"
chmod 755 "$cell/runner"
sz=$(wc -c < "$cell/runner" | tr -d ' ')
printf '{"inputDigest":"digestwarm0001","artifact":"runner","size":%s,"created":1700000000,"language":"go"}\n' "$sz" > "$cell/ready.json"`

// extCellSnippet 创建单扩展形态产物 cache/ext/<name>-<digest>/<artifact>
// （cache/ext 语言门要求 ready.json 显式 "language":"go"）。
const extCellSnippet = `cell="$PIG_HOME/cache/ext/fake-ext-digestext01"
mkdir -p "$cell"
printf '#!/bin/sh\nexit 0\n' > "$cell/bin"
chmod 755 "$cell/bin"
sz=$(wc -c < "$cell/bin" | tr -d ' ')
printf '{"inputDigest":"digestext01","artifact":"bin","size":%s,"created":1700000000,"language":"go"}\n' "$sz" > "$cell/ready.json"`

// useWarmHook 安装包级测试钩子并在测试结束时还原。
func useWarmHook(t *testing.T, h *warmGoCellTestHook) {
	t.Helper()
	restore := warmGoCellHook
	warmGoCellHook = h
	t.Cleanup(func() { warmGoCellHook = restore })
}

// warmLog 返回假 pig 调用日志原文；文件不存在返回空串。
func (f *warmTestFixture) warmLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("ReadFile warm.log: %v", err)
	}
	return string(data)
}

// warmInvocations 返回假 pig 被调用次数（每次调用追加一段以 ARGS= 开头的记录）。
func (f *warmTestFixture) warmInvocations(t *testing.T) int {
	t.Helper()
	return strings.Count(f.warmLog(t), "ARGS=")
}

// warmLogEnv 返回最近一次调用记录中 KEY= 行的值；缺失返回 ""。
func (f *warmTestFixture) warmLogEnv(t *testing.T, key string) string {
	t.Helper()
	got := ""
	for _, l := range strings.Split(f.warmLog(t), "\n") {
		if strings.HasPrefix(l, key+"=") {
			got = strings.TrimPrefix(l, key+"=")
		}
	}
	return got
}

// stampPath 返回指纹墓碑路径 <pigHome>/cache/.locks/sbx-warm.sha256。
func (f *warmTestFixture) stampPath() string {
	return filepath.Join(f.cacheRoot, ".locks", warmGoCellStampName)
}

// removePresetTempDirs 清理 ReviewerAgentWithEnv 生成的私有临时目录
// （pi-reviewer-* / pi-reviewer-exec-*），防止测试泄漏到 /tmp。
func removePresetTempDirs(t *testing.T, p Policy) {
	t.Helper()
	drop := func(path string) {
		base := filepath.Base(filepath.Clean(path))
		if strings.HasPrefix(base, "pi-reviewer-") {
			_ = os.RemoveAll(path)
		}
	}
	for _, rw := range p.ReadWrite {
		drop(rw)
	}
	for _, ex := range p.Exec {
		drop(ex)
	}
}

// TestWarmGoExtensionCellsBundledColdStart 验证合包（cache/cells/go）形态冷启动：
// 假 pig 被调用时处于干净最小环境（剥离调用方注入的 GOFLAGS/LD_PRELOAD/
// 外部 PIG_CODING_AGENT_DIR）、cwd 为私有临时目录、AgentDir 指向
// <sbx-warm-*>/pig-agent 私有副本而非 <pigHome>/agent；
// 预热生成的 <digest>/runner 经 appendCellCachePolicy 进入 policy.Exec；
// 指纹墓碑写入 cache/.locks 后二次调用零子进程短路。
func TestWarmGoExtensionCellsBundledColdStart(t *testing.T) {
	// 恶意/泄漏变量先植入宿主环境，预热子进程必须彻底剥离。
	t.Setenv("GOFLAGS", "-tags=evil")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")

	fx := newWarmTestFixture(t, "", true)
	// 显式自定义 AgentDir 同样走私有副本隔离：宿主变量指向夹具内有效
	// AgentDir（含同一 Go 扩展），预热必须正常进行且子进程 agent dir 指向私有副本。
	customAgent := filepath.Join(t.TempDir(), "custom-agent")
	customExt := filepath.Join(customAgent, "extensions", "fake-ext")
	if err := os.MkdirAll(filepath.Dir(customExt), 0o755); err != nil {
		t.Fatalf("MkdirAll custom ext parent: %v", err)
	}
	if err := os.Symlink(fx.extReal, customExt); err != nil {
		t.Fatalf("Symlink custom ext: %v", err)
	}
	t.Setenv("PIG_CODING_AGENT_DIR", customAgent)
	t.Setenv("PI_CODING_AGENT_DIR", "/evil/agent")
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, ""))
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig})

	// 夹具必须复刻生产布局：agent/extensions/fake-ext 是目录符号链接。
	if st, err := os.Lstat(fx.extDir); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("fixture ext dir must be a symlink (production layout): %v", err)
	}

	// 收窄触发：非 reviewer-agent preset / 非 pig 命令均不得调用 pig。
	MaybeWarmGoExtensionCells("reviewer", []string{"pig"}, nil)
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"ls", "-la"}, nil)
	// 无原生 Go 扩展的假目录同样零开销返回（临时摘除 custom AgentDir 的 go.mod 验证）。
	goMod := filepath.Join(customExt, "go.mod")
	goModBak := goMod + ".bak"
	if err := os.Rename(goMod, goModBak); err != nil {
		t.Fatalf("Rename go.mod: %v", err)
	}
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if err := os.Rename(goModBak, goMod); err != nil {
		t.Fatalf("restore go.mod: %v", err)
	}
	if n := fx.warmInvocations(t); n != 0 {
		t.Fatalf("narrow gate: pig invoked unexpectedly (%d calls)", n)
	}

	workdir := t.TempDir()
	if code := MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil); code != 0 {
		t.Fatalf("warm-up returned non-zero exit code %d", code)
	}
	if n := fx.warmInvocations(t); n == 0 {
		t.Fatal("cold start: fake pig was not invoked")
	}

	if v := fx.warmLogEnv(t, "GOFLAGS"); v != "<unset>" {
		t.Fatalf("GOFLAGS leaked: %q", v)
	}
	if v := fx.warmLogEnv(t, "LD_PRELOAD"); v != "<unset>" {
		t.Fatalf("LD_PRELOAD leaked: %q", v)
	}
	// AgentDir 必须指向私有临时副本（与沙盒内扩展发现视图同构），
	// 绝不直接指向宿主 AgentDir；预热结束后随 warmTmp 一并清理。
	for _, key := range []string{"PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR"} {
		agentDir := fx.warmLogEnv(t, key)
		if filepath.Base(agentDir) != "pig-agent" ||
			!strings.Contains(filepath.Base(filepath.Dir(agentDir)), "sbx-warm-") {
			t.Fatalf("%s not pinned to private warm agent dir: %q", key, agentDir)
		}
		if _, err := os.Lstat(agentDir); !os.IsNotExist(err) {
			t.Fatalf("private agent dir should be removed with warmTmp, Lstat err=%v", err)
		}
	}
	if v := fx.warmLogEnv(t, "PIG_HOME"); v != fx.pigHome {
		t.Fatalf("PIG_HOME mismatch: %q want %q", v, fx.pigHome)
	}
	// 子进程 HOME 必须取自 os/user（可信），不得是调用方环境 HOME。
	if v := fx.warmLogEnv(t, "HOME"); v != trustedHomeDir() {
		t.Fatalf("HOME not trusted home: %q want %q", v, trustedHomeDir())
	}
	if v := fx.warmLogEnv(t, "GOPROXY"); v != "off" {
		t.Fatalf("GOPROXY not isolated: %q", v)
	}
	if v := fx.warmLogEnv(t, "GOWORK"); v != "off" {
		t.Fatalf("GOWORK not isolated: %q", v)
	}
	// 子进程 PATH 必须完全受控：调用方 PATH 不得泄漏进预热环境，
	// 仅允许固定可信目录常量。
	childPath := fx.warmLogEnv(t, "PATH")
	if childPath == "" || strings.Contains(childPath, filepath.Dir(fx.fakePig)) {
		t.Fatalf("untrusted host PATH leaked into warm subprocess: %q", childPath)
	}
	for _, seg := range strings.Split(childPath, string(os.PathListSeparator)) {
		switch seg {
		case "/usr/local/go/bin", "/usr/lib/go/bin", "/root/.local/bin",
			"/usr/local/bin", "/usr/bin", "/bin":
		default:
			t.Fatalf("unexpected dir in controlled warm PATH: %q (full %q)", seg, childPath)
		}
	}
	cwd := fx.warmLogEnv(t, "CWD")
	if cwd == "" || cwd == workdir || !strings.Contains(filepath.Base(filepath.Dir(cwd)), "sbx-warm-") {
		t.Fatalf("warm cwd not private tmp dir: %q (workdir=%q)", cwd, workdir)
	}

	// 预热生成的合法 cell runner 应被紧随其后的策略构造纳入 Exec 白名单。
	var p Policy
	appendCellCachePolicy(&p, fx.pigHome)
	wantRunner := filepath.Join(fx.cacheRoot, "cells", "go", "digestwarm0001", "runner")
	found := false
	for _, ep := range p.Exec {
		if filepath.Clean(ep) == wantRunner {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("runner %q not in policy.Exec: %v", wantRunner, p.Exec)
	}

	// 指纹墓碑写入 cache/.locks；热态二次调用零子进程短路。
	if _, err := os.Stat(fx.stampPath()); err != nil {
		t.Fatalf("stamp file missing: %v", err)
	}
	before := fx.warmInvocations(t)
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if got := fx.warmInvocations(t); got != before {
		t.Fatalf("warm start should be zero-subprocess, invocations %d -> %d", before, got)
	}
}

// TestWarmGoExtensionCellsExtCacheColdStart 验证单扩展（cache/ext）产物形态：
// 合包 cells/go 完全缺失时预热仍有效（产物计数同时检查两种形态），
// 产物删除后墓碑防护失效并重新预热。
func TestWarmGoExtensionCellsExtCacheColdStart(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	fx.setPigScript(t, fx.warmEnvLogScript(extCellSnippet, ""))
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig})

	if code := MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil); code != 0 {
		t.Fatalf("warm-up returned non-zero exit code %d", code)
	}
	if n := fx.warmInvocations(t); n != 1 {
		t.Fatalf("ext cache cold start: want 1 invocation, got %d", n)
	}
	artifact := filepath.Join(fx.cacheRoot, "ext", "fake-ext-digestext01", "bin")
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("ext artifact missing: %v", err)
	}
	if _, err := os.Stat(fx.stampPath()); err != nil {
		t.Fatalf("stamp file missing after ext-form warm-up: %v", err)
	}
	var p Policy
	appendCellCachePolicy(&p, fx.pigHome)
	found := false
	for _, ep := range p.Exec {
		if filepath.Clean(ep) == artifact {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ext artifact %q not in policy.Exec: %v", artifact, p.Exec)
	}
	// 热短路。
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 1 {
		t.Fatalf("warm start should be zero-subprocess, invocations=%d", n)
	}
	// 产物删除 → 墓碑防护失效 → 重新预热。
	if err := os.Remove(artifact); err != nil {
		t.Fatalf("Remove ext artifact: %v", err)
	}
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 2 {
		t.Fatalf("ext artifact removal should re-trigger warm-up, invocations=%d", n)
	}
}

// TestWarmGoExtensionCellsNoSDKColdStart 验证 state/pigsdk 初始缺失的真实
// 冷启动：预热不再因 sdkDirs 为空提前返回；假 pig 首次运行落盘 sdk-* 后
// pre/post 指纹漂移，第 1 轮重验收敛并写入墓碑。
func TestWarmGoExtensionCellsNoSDKColdStart(t *testing.T) {
	fx := newWarmTestFixture(t, "", false)
	dropSDK := `sdk="$PIG_HOME/state/pigsdk/sdk-dropped"
if [ ! -d "$sdk" ]; then
  mkdir -p "$sdk"
  printf 'module sdk\n' > "$sdk/go.mod"
fi`
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, dropSDK))
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig})

	if code := MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil); code != 0 {
		t.Fatalf("warm-up returned non-zero exit code %d", code)
	}
	// 第 0 轮预热后 SDK 落盘导致 pre/post 漂移 → 第 1 轮重验收敛：共 2 次调用。
	if n := fx.warmInvocations(t); n != 2 {
		t.Fatalf("no-SDK cold start should converge after 2 attempts, invocations=%d", n)
	}
	if _, err := os.Stat(fx.stampPath()); err != nil {
		t.Fatalf("stamp file missing after retry convergence: %v", err)
	}
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 2 {
		t.Fatalf("warm start should be zero-subprocess, invocations=%d", n)
	}
}

// TestWarmGoExtensionCellsInvalidation 验证指纹或产物失效时重新触发预热：
// 修改软链目标目录内的 .go 文件 → 指纹漂移 → 再预热；删除全部 runner →
// 墓碑防护失效 → 再预热。
func TestWarmGoExtensionCellsInvalidation(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, ""))
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig})

	resolved, err := filepath.EvalSymlinks(fx.extDir)
	if err != nil || resolved != fx.extReal {
		t.Fatalf("ext dir should resolve to %q, got %q (%v)", fx.extReal, resolved, err)
	}

	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	count := fx.warmInvocations(t)
	if count == 0 {
		t.Fatal("initial warm-up did not run")
	}

	// 修改软链目标目录内的扩展源文件 → 指纹必须漂移 → 重新预热。
	if err := os.WriteFile(filepath.Join(fx.extReal, "main.go"), []byte("package main\n\n// changed\n"), 0o644); err != nil {
		t.Fatalf("WriteFile main.go in symlink target: %v", err)
	}
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if got := fx.warmInvocations(t); got <= count {
		t.Fatalf("source change should re-trigger warm-up, invocations %d -> %d", count, got)
	}
	count = fx.warmInvocations(t)

	// 删除全部可执行 runner → 即便指纹未变也应重新预热（墓碑防护）。
	if err := os.Remove(filepath.Join(fx.cacheRoot, "cells", "go", "digestwarm0001", "runner")); err != nil {
		t.Fatalf("Remove runner: %v", err)
	}
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if got := fx.warmInvocations(t); got <= count {
		t.Fatalf("runner removal should re-trigger warm-up, invocations %d -> %d", count, got)
	}
}

// TestWarmGoExtensionCellsFingerprintDriftRetry 验证 R1 有界重试：
// 预热期间源码 A->B 持续漂移时，最多 2 轮子进程后告警跳过写戳，
// 绝不把未构建的新源码指纹标为热态；源码稳定后收敛写戳。
func TestWarmGoExtensionCellsFingerprintDriftRetry(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	// 每次调用都对源码做一次确定性漂移（追加注释行改变 mtime/size）。
	drift := fmt.Sprintf(`cnt=$(cat '%[1]s' 2>/dev/null || echo 0)
cnt=$((cnt + 1))
echo "$cnt" > '%[1]s'
printf '\n// drift %%s\n' "$cnt" >> '%[2]s'`,
		filepath.Join(fx.pigHome, "drift.counter"), filepath.Join(fx.extReal, "main.go"))
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, drift))

	var warnBuf strings.Builder
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig, Stderr: &warnBuf})

	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != warmGoCellMaxAttempts {
		t.Fatalf("drifting source should exhaust %d attempts, invocations=%d", warmGoCellMaxAttempts, n)
	}
	if _, err := os.Lstat(fx.stampPath()); !os.IsNotExist(err) {
		t.Fatalf("stamp must not be written while fingerprint drifts, err=%v", err)
	}
	if !strings.Contains(warnBuf.String(), "fingerprint drifted") {
		t.Fatalf("expected drift warning, got %q", warnBuf.String())
	}

	// 源码稳定后收敛：预热成功并写入墓碑。
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, ""))
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if _, err := os.Stat(fx.stampPath()); err != nil {
		t.Fatalf("stamp file missing after stable warm-up: %v", err)
	}
}

// TestResolvePigEntryBinaryRejectsUntrustedPath 验证 F1 信任边界：
// 生产路径 resolvePigEntryBinary 只在固定可信目录解析 pig，
// 绝不调用 exec.LookPath / 读取调用方 PATH——PATH 前置目录与
// 任意非可信路径下的伪造 pig 一律拒绝。
func TestResolvePigEntryBinaryRejectsUntrustedPath(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, ""))
	useWarmHook(t, &warmGoCellTestHook{})

	// 调用方 PATH 前置伪造目录：生产解析不得命中 fakePig。
	t.Setenv("PATH", filepath.Dir(fx.fakePig)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if got := resolvePigEntryBinary(fx.fakePig); got != "" {
		t.Fatalf("untrusted explicit path must be rejected, got %q", got)
	}
	if got := resolvePigEntryBinary("pig"); got == fx.fakePig {
		t.Fatalf("fake pig resolved via caller PATH: %q", got)
	}

	// MaybeWarm 层同样不得执行伪造 pig：fakePig 不在可信解析集，
	// 无论真实 pig 是否解析成功，假脚本的日志都必须为空。
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if log := fx.warmLog(t); log != "" {
		t.Fatalf("untrusted fake pig was executed, log=%q", log)
	}
}

// TestWarmGoExtensionCellsRejectsUnsafePigHome 验证 PIG_HOME 预热前校验：
// 符号链接、非目录、0777 公共可写目录一律拒绝，不产生任何子进程。
func TestWarmGoExtensionCellsRejectsUnsafePigHome(t *testing.T) {
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	unsetEnv(t, "PI_CODING_AGENT_DIR")
	unsetEnv(t, "PIG_USE_PI_DIRS")

	var warnBuf strings.Builder
	useWarmHook(t, &warmGoCellTestHook{Stderr: &warnBuf})

	root := t.TempDir()
	realHome := filepath.Join(root, "real-pig-home")
	linkHome := filepath.Join(root, "link-pig-home")
	if err := os.MkdirAll(realHome, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// 符号链接 PIG_HOME 拒绝。
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	t.Setenv("PIG_HOME", linkHome)
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if !strings.Contains(warnBuf.String(), "refusing symlink PIG_HOME") {
		t.Fatalf("expected symlink PIG_HOME refusal, got %q", warnBuf.String())
	}

	// 0777 公共可写目录拒绝。
	if err := os.Chmod(realHome, 0o777); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Setenv("PIG_HOME", realHome)
	warnBuf.Reset()
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if !strings.Contains(warnBuf.String(), "group/world-writable") {
		t.Fatalf("expected world-writable PIG_HOME refusal, got %q", warnBuf.String())
	}
}

// TestWarmGoExtensionCellsEnvPigHome 验证 F5 对齐：cfgEnv（--env 注入）中
// 最后一项 PIG_HOME 同时决定预热指纹/缓存根与 ReviewerAgentWithEnv
// 策略构造引用的 PIG_HOME，两处绝无分裂。
func TestWarmGoExtensionCellsEnvPigHome(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	fx.setPigScript(t, fx.warmEnvLogScript(extCellSnippet, ""))
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig})

	// 第二 PIG_HOME：含同一 Go 扩展（复用真实源码目录的目录软链）。
	pigHome2 := filepath.Join(t.TempDir(), "pig-home-2")
	extDir2 := filepath.Join(pigHome2, "agent", "extensions", "fake-ext")
	if err := os.MkdirAll(filepath.Dir(extDir2), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(fx.extReal, extDir2); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// cfgEnv 最后一项 PIG_HOME 键生效（尾部非空值即停）。
	cfgEnv := []string{
		"PIG_HOME=" + fx.pigHome,
		"PIG_HOME=" + pigHome2,
	}
	if got := resolvePigHomeDir(cfgEnv); got != pigHome2 {
		t.Fatalf("resolvePigHomeDir = %q, want %q", got, pigHome2)
	}

	if code := MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, cfgEnv); code != 0 {
		t.Fatalf("warm-up returned non-zero exit code %d", code)
	}
	if v := fx.warmLogEnv(t, "PIG_HOME"); v != pigHome2 {
		t.Fatalf("warm subprocess PIG_HOME = %q, want cfgEnv value %q", v, pigHome2)
	}
	// 产物与墓碑落在 pigHome2 而非宿主 $PIG_HOME。
	if _, err := os.Stat(filepath.Join(pigHome2, "cache", ".locks", warmGoCellStampName)); err != nil {
		t.Fatalf("stamp must land in cfgEnv PIG_HOME: %v", err)
	}
	if _, err := os.Lstat(fx.stampPath()); !os.IsNotExist(err) {
		t.Fatalf("stamp must not land in env PIG_HOME when cfgEnv overrides, err=%v", err)
	}

	// ReviewerAgentWithEnv 引用的缓存根同样是 pigHome2：
	// 其中已构建的 ext 产物进 Exec，env PIG_HOME 下的（空）缓存不产生条目；
	// Policy.Env 固定注入 PIG_HOME=pigHome2（防 --env 空覆盖/子进程 HOME 漂移）。
	workdir := t.TempDir()
	p := ReviewerAgentWithEnv(workdir, cfgEnv)
	t.Cleanup(func() { removePresetTempDirs(t, p) })
	if got := lookupEnvValue(p.Env, "PIG_HOME"); got != pigHome2 {
		t.Fatalf("Policy.Env must pin PIG_HOME=%q, got %q (env=%v)", pigHome2, got, p.Env)
	}
	wantArtifact := filepath.Join(pigHome2, "cache", "ext", "fake-ext-digestext01", "bin")
	found := false
	for _, ep := range p.Exec {
		if filepath.Clean(ep) == wantArtifact {
			found = true
		}
		if strings.HasPrefix(filepath.Clean(ep), filepath.Join(fx.cacheRoot, "ext")+string(filepath.Separator)) ||
			strings.HasPrefix(filepath.Clean(ep), filepath.Join(fx.cacheRoot, "cells")+string(filepath.Separator)) {
			t.Fatalf("artifact from env PIG_HOME cache leaked into policy.Exec: %q", ep)
		}
	}
	if !found {
		t.Fatalf("cfgEnv PIG_HOME artifact %q not in policy.Exec", wantArtifact)
	}
}

// TestWarmGoExtensionCellsTrackedPISymlinkSwitch 验证 F2 追踪目标：
// 包装脚本 pig 本体不变，仅切换被追踪 pi 软链目标（升级真实 ELF）时
// 墓碑指纹失效并重新预热。
func TestWarmGoExtensionCellsTrackedPISymlinkSwitch(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, ""))

	realDir := t.TempDir()
	piV1 := filepath.Join(realDir, "pi-v1")
	piV2 := filepath.Join(realDir, "pi-v2")
	for _, p := range []string{piV1, piV2} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("WriteFile %s: %v", p, err)
		}
	}
	piLink := filepath.Join(fx.pigHome, "bin", "pi")
	if err := os.MkdirAll(filepath.Dir(piLink), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(piV1, piLink); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig, ExtraTrackedPaths: []string{piLink}})

	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 1 {
		t.Fatalf("initial warm-up did not run, invocations=%d", n)
	}
	if _, err := os.Stat(fx.stampPath()); err != nil {
		t.Fatalf("stamp file missing: %v", err)
	}

	// 切换 pi 软链目标 v1 -> v2（包装脚本 pig 不动）→ 指纹失效重热。
	if err := os.Remove(piLink); err != nil {
		t.Fatalf("Remove piLink: %v", err)
	}
	if err := os.Symlink(piV2, piLink); err != nil {
		t.Fatalf("Symlink retarget: %v", err)
	}
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 2 {
		t.Fatalf("pi symlink retarget should invalidate stamp and re-warm, invocations=%d", n)
	}
}

// TestWarmGoExtensionCellsFakeSuccessRejected 验证 P3b/F3：真实 pig --help
// 在扩展编译失败时仍退 0（stderr 输出失败标记），预热必须拦截假成功：
// 返回错误、告警降级且绝不写入指纹墓碑。
func TestWarmGoExtensionCellsFakeSuccessRejected(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	// 即使产物已生成，stderr 命中失败标记同样判定预热失败。
	script := fx.warmEnvLogScript(bundledCellSnippet,
		`echo "Failed to load extension fake-ext: extension build failed" >&2`)
	fx.setPigScript(t, script)

	var warnBuf strings.Builder
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig, Stderr: &warnBuf})

	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 1 {
		t.Fatalf("fake-success run should execute pig exactly once, invocations=%d", n)
	}
	if _, err := os.Lstat(fx.stampPath()); !os.IsNotExist(err) {
		t.Fatalf("stamp must not be written on fake zero-exit, err=%v", err)
	}
	if !strings.Contains(warnBuf.String(), "pig warm-up failed") {
		t.Fatalf("expected warm-up failure warning, got %q", warnBuf.String())
	}
}

// TestStderrScanWriterSplitMarker 验证 stderr 失败标记的跨块扫描：
// 标记被多次 Write 切分时仍能命中（不受 4KB 环形缓冲边界影响）。
func TestStderrScanWriterSplitMarker(t *testing.T) {
	w := newStderrScanWriter()
	parts := []string{"noise line\nFailed to load ex", "tension: x\n", "more noise\n"}
	for _, p := range parts {
		if _, err := w.Write([]byte(p)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if hit, _ := w.Snapshot(); hit != "Failed to load extension" {
		t.Fatalf("split marker not detected, hit=%q", hit)
	}
	// 跨 4KB 写边界（先写大量噪声再写标记）同样命中。
	w2 := newStderrScanWriter()
	noise := strings.Repeat("n", 6000)
	if _, err := w2.Write([]byte(noise[:5990] + "extension buil")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := w2.Write([]byte("d failed: boom\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if hit, _ := w2.Snapshot(); hit != "extension build failed" {
		t.Fatalf("marker across large write boundary not detected, hit=%q", hit)
	}
}

// TestWarmGoExtensionCellsSignalInterrupt 验证 F4/P5b：预热中途收到
// SIGINT 时取消子进程 ctx、SIGKILL 整个进程组，让函数正常退栈跑完
// 所有 defer（/tmp/sbx-warm-* 零残留、释放锁），返回 130 给 runMain 退出。
func TestWarmGoExtensionCellsSignalInterrupt(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	hangScript := fmt.Sprintf(`#!/bin/sh
echo $$ > '%[1]s.pid'
sleep 60 &
echo $! >> '%[1]s.pid'
sleep 60
`, fx.logPath)
	fx.setPigScript(t, hangScript)

	var warnBuf strings.Builder
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig, Stderr: &warnBuf})

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	}()
	// 等待假 pig 真正启动（pid 文件落盘）再发信号，避免信号早于
	// signal.Notify 注册窗口导致测试自身被 SIGINT 终止。
	pidFile := fx.logPath + ".pid"
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake pig pid file never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("self SIGINT: %v", err)
	}

	var code int
	select {
	case code = <-codeCh:
	case <-time.After(15 * time.Second):
		t.Fatal("warm-up did not return after SIGINT")
	}
	if code != 130 {
		t.Fatalf("signal-interrupted warm-up must return 130, got %d", code)
	}
	if !strings.Contains(warnBuf.String(), "sbx-warm") {
		t.Fatalf("expected degradation warning, got %q", warnBuf.String())
	}

	// /tmp/sbx-warm-* 零残留：函数正常退栈跑完 defer 清理。
	if left, err := filepath.Glob(filepath.Join(os.TempDir(), "sbx-warm-*")); err != nil || len(left) != 0 {
		t.Fatalf("sbx-warm temp dirs leaked: %v (err=%v)", left, err)
	}

	// 父进程与其派生的孙子进程（同一进程组）都应已被 SIGKILL 清理。
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("ReadFile pid log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		// SIGKILL 送达后 cmd 只 waitpid 直接子进程；孙子进程过继给
		// PID 1，在其异步 wait4 回收僵尸前 kill(pid,0) 仍返回 0，
		// 故轮询等待，僵尸(Z)态视为已清理。
		if err := waitPIDReaped(pid, 2*time.Second); err != nil {
			t.Fatalf("pid %d still alive after signal kill: %v", pid, err)
		}
	}
}

// TestWarmGoExtensionCellsTimeout 验证假 pig 挂起时：
// 预热子进程连同进程组被 SIGKILL 清理，sbx 仅告警优雅降级不卡死。
func TestWarmGoExtensionCellsTimeout(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	hangScript := fmt.Sprintf(`#!/bin/sh
echo $$ > '%[1]s.pid'
sleep 60 &
echo $! >> '%[1]s.pid'
sleep 60
`, fx.logPath)
	fx.setPigScript(t, hangScript)

	var warnBuf strings.Builder
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig, Timeout: 500 * time.Millisecond, Stderr: &warnBuf})

	start := time.Now()
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	elapsed := time.Since(start)
	if elapsed > 15*time.Second {
		t.Fatalf("warm-up did not degrade gracefully, elapsed=%v", elapsed)
	}
	if !strings.Contains(warnBuf.String(), "sbx-warm") {
		t.Fatalf("expected degradation warning, got %q", warnBuf.String())
	}

	data, err := os.ReadFile(fx.logPath + ".pid")
	if err != nil {
		t.Fatalf("ReadFile pid log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d", &pid); err != nil || pid <= 0 {
			continue
		}
		if err := waitPIDReaped(pid, 2*time.Second); err != nil {
			t.Fatalf("pid %d still alive after timeout kill: %v", pid, err)
		}
	}
}

// waitPIDReaped 轮询等待 pid 退出（ESRCH）或进入僵尸态（Z，待 PID 1 回收），
// 最多等 timeout，间隔 10ms；超时仍存活且非僵尸返回错误。
func waitPIDReaped(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return nil
		}
		if err == nil || err == syscall.EPERM {
			if state := procState(pid); state == "Z" {
				return nil
			}
		} else {
			return nil // 其它错误（如 ESRCH 已覆盖外的异常）按已消失处理
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still alive (state=%q)", procState(pid))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// procState 读 /proc/<pid>/stat 的进程状态字符；读不到返回空串。
func procState(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// comm 字段可能含空格/括号，状态字符在最后一个 ')' 之后。
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// ---------------------------------------------------------------------------
// 第 2 轮门禁回归测试（reviewer/oracle 复审项）
// ---------------------------------------------------------------------------

// writeFakeCellEntry 在 <cacheRoot> 下构造带执行位 artifact + 合法 ready.json
// 的缓存单元目录（language 为空串时省略字段），返回单元目录路径。
func writeFakeCellEntry(t *testing.T, dir, inputDigest, artifact, language string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	artifactPath := filepath.Join(dir, artifact)
	if err := os.WriteFile(artifactPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile artifact: %v", err)
	}
	meta := map[string]any{
		"inputDigest": inputDigest,
		"artifact":    artifact,
		"size":        int64(len("#!/bin/sh\nexit 0\n")),
		"created":     1700000000,
	}
	if language != "" {
		meta["language"] = language
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal ready.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ready.json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile ready.json: %v", err)
	}
}

// TestCountValidExecutableGoArtifactsLanguageGate 验证 M1 语言门：
// cache/ext/* 必须 ready.json 显式 language=="go"（宿主 node-runtime-* 等
// 非 Go 产物不计入）；cache/cells/go/* 允许 language 缺省或 "go"。
func TestCountValidExecutableGoArtifactsLanguageGate(t *testing.T) {
	cacheRoot := filepath.Join(t.TempDir(), "cache")

	// ext 形态：language:"node" / language 缺省 一律不计。
	writeFakeCellEntry(t, filepath.Join(cacheRoot, "ext", "node-runtime-deadbeef01"), "deadbeef01", "bin", "node")
	writeFakeCellEntry(t, filepath.Join(cacheRoot, "ext", "fake-ext-nolang001"), "nolang001", "bin", "")
	if n := countValidExecutableGoArtifacts(cacheRoot); n != 0 {
		t.Fatalf("non-Go / language-less ext entries must not count, got %d", n)
	}

	// ext 形态：language:"go" 计入；cells/go 形态：language 缺省计入。
	writeFakeCellEntry(t, filepath.Join(cacheRoot, "ext", "fake-ext-aabbcc0011"), "aabbcc0011", "bin", "go")
	writeFakeCellEntry(t, filepath.Join(cacheRoot, "cells", "go", "ddeeff001122"), "ddeeff001122", "runner", "")
	if n := countValidExecutableGoArtifacts(cacheRoot); n != 2 {
		t.Fatalf("go ext + language-less cells/go entries must count, got %d", n)
	}
}

// TestWarmGoExtensionCellsEmbeddedAssetInvalidation 验证 M2：扩展内非 .go
// 常规文件（//go:embed 静态资产，如 tools.json）变更同样使墓碑指纹失效重热。
func TestWarmGoExtensionCellsEmbeddedAssetInvalidation(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, ""))
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig})

	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 1 {
		t.Fatalf("initial warm-up did not run, invocations=%d", n)
	}
	if _, err := os.Stat(fx.stampPath()); err != nil {
		t.Fatalf("stamp file missing: %v", err)
	}

	// 新增嵌入资产文件（非 .go 常规文件）→ 指纹必须漂移 → 重新预热。
	if err := os.WriteFile(filepath.Join(fx.extReal, "tools.json"), []byte(`{"tools":[]}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile tools.json: %v", err)
	}
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 2 {
		t.Fatalf("embedded asset change should invalidate stamp and re-warm, invocations=%d", n)
	}
}

// TestWarmGoExtensionCellsFlockWaitSignalInterrupt 验证 reviewer 项 1：
// 另一进程持有 sbx-warm.lock 期间本进程阻塞在 flock 等待，收到 SIGINT 必须
// 在 500ms 内退出并返回 128+SIGINT=130（acquireWarmFlock 非阻塞轮询 + select）。
func TestWarmGoExtensionCellsFlockWaitSignalInterrupt(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, ""))
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig, Stderr: &strings.Builder{}})

	lockDir := filepath.Join(fx.cacheRoot, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("MkdirAll lockDir: %v", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(lockDir, warmGoCellLockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("OpenFile lock: %v", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("Flock LOCK_EX: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	}()
	// 预热先完成指纹枚举再注册 signal.Notify 并进入 flock 等待；
	// 留出足够时间进入等待循环后再发信号，避免早于 Notify 注册窗口。
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("self SIGINT: %v", err)
	}

	start := time.Now()
	select {
	case code := <-codeCh:
		elapsed := time.Since(start)
		if code != 130 {
			t.Fatalf("flock wait interrupted by SIGINT must return 130, got %d", code)
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("flock wait did not abort promptly on signal, elapsed=%v", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("warm-up blocked in flock wait did not return after SIGINT")
	}
	if n := fx.warmInvocations(t); n != 0 {
		t.Fatalf("fake pig must not be invoked while waiting for lock, invocations=%d", n)
	}
}

// TestResolvePigHomeAndSourceAgentDirSemantics 验证 reviewer 项 2 + oracle S1：
//   - cfgEnv 最后一个 PIG_HOME 键命中即停：非空取绝对路径，显式空串回退默认
//     <trustedHome>/.pig（不回扫被覆盖条目、不读宿主 PIG_HOME）；仅 cfgEnv
//     完全不含该键时才读宿主环境变量；
//   - resolveSourcePigAgentDir 与 injectPigCodingAgentDir 同优先级解析显式
//     AgentDir（PIG_CODING_AGENT_DIR 优先，有效 PIG_USE_PI_DIRS=1 时
//     PI_CODING_AGENT_DIR；--env KEY= 空覆盖屏蔽宿主同名变量）。
func TestResolvePigHomeAndSourceAgentDirSemantics(t *testing.T) {
	dflt := filepath.Join(trustedHomeDir(), ".pig")
	fakeHome := filepath.Join(t.TempDir(), "fake-home")

	t.Run("empty override falls back to default ignoring host env", func(t *testing.T) {
		t.Setenv("PIG_HOME", fakeHome)
		cfgEnv := []string{"PIG_HOME=/nonexistent-a", "PIG_HOME="}
		if got := resolvePigHomeDir(cfgEnv); got != dflt {
			t.Fatalf("resolvePigHomeDir = %q, want default %q", got, dflt)
		}
	})

	t.Run("last non-empty wins", func(t *testing.T) {
		t.Setenv("PIG_HOME", fakeHome)
		cfgEnv := []string{"PIG_HOME=", "PIG_HOME=/somewhere/else"}
		if got := resolvePigHomeDir(cfgEnv); got != "/somewhere/else" {
			t.Fatalf("resolvePigHomeDir = %q, want /somewhere/else", got)
		}
	})

	t.Run("no key reads host env", func(t *testing.T) {
		t.Setenv("PIG_HOME", fakeHome)
		if got := resolvePigHomeDir(nil); got != fakeHome {
			t.Fatalf("resolvePigHomeDir(nil) = %q, want host env %q", got, fakeHome)
		}
	})

	t.Run("explicit PIG_CODING_AGENT_DIR aligned", func(t *testing.T) {
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_USE_PI_DIRS")
		custom := filepath.Join(t.TempDir(), "custom-agent")
		if dir, c := resolveSourcePigAgentDir(fakeHome, []string{"PIG_CODING_AGENT_DIR=" + custom}); dir != custom || !c {
			t.Fatalf("resolveSourcePigAgentDir = (%q,%v), want (%q,true)", dir, c, custom)
		}
		// 显式空串覆盖屏蔽宿主变量 → 回退默认 <pigHome>/agent。
		t.Setenv("PIG_CODING_AGENT_DIR", "/host/agent")
		if dir, c := resolveSourcePigAgentDir(fakeHome, []string{"PIG_CODING_AGENT_DIR="}); c || dir != filepath.Join(fakeHome, "agent") {
			t.Fatalf("empty override must shadow host env: (%q,%v)", dir, c)
		}
	})

	t.Run("PI_CODING_AGENT_DIR via PIG_USE_PI_DIRS", func(t *testing.T) {
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_USE_PI_DIRS")
		custom := filepath.Join(t.TempDir(), "pi-agent")
		cfgEnv := []string{"PIG_USE_PI_DIRS=1", "PI_CODING_AGENT_DIR=" + custom}
		if dir, c := resolveSourcePigAgentDir(fakeHome, cfgEnv); dir != custom || !c {
			t.Fatalf("resolveSourcePigAgentDir = (%q,%v), want (%q,true)", dir, c, custom)
		}
		// PIG_USE_PI_DIRS 非 1 时忽略 PI_CODING_AGENT_DIR。
		if dir, c := resolveSourcePigAgentDir(fakeHome, []string{"PI_CODING_AGENT_DIR=" + custom}); c || dir != filepath.Join(fakeHome, "agent") {
			t.Fatalf("PI_CODING_AGENT_DIR without PIG_USE_PI_DIRS=1 must be ignored: (%q,%v)", dir, c)
		}
	})

	t.Run("ReviewerAgentWithEnv pins resolved PIG_HOME into Policy.Env", func(t *testing.T) {
		fx := newWarmTestFixture(t, "", true)
		cfgEnv := []string{"PIG_HOME=" + fx.pigHome}
		workdir := t.TempDir()
		p := ReviewerAgentWithEnv(workdir, cfgEnv)
		t.Cleanup(func() { removePresetTempDirs(t, p) })
		if got := lookupEnvValue(p.Env, "PIG_HOME"); got != fx.pigHome {
			t.Fatalf("Policy.Env must pin PIG_HOME=%q, got %q", fx.pigHome, got)
		}
	})
}

// TestWarmGoExtensionCellsSettingsFingerprint 验证 reviewer 项 3：
// <srcAgentDir>/settings.json 内容变更（如启用/禁用扩展）使墓碑失效重热。
func TestWarmGoExtensionCellsSettingsFingerprint(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, ""))
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig})

	settingsPath := filepath.Join(fx.pigHome, "agent", "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"dark"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile settings.json: %v", err)
	}
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 1 {
		t.Fatalf("initial warm-up did not run, invocations=%d", n)
	}
	if _, err := os.Stat(fx.stampPath()); err != nil {
		t.Fatalf("stamp file missing: %v", err)
	}
	// 修改 settings.json → 指纹失效重热。
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"light"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile settings.json: %v", err)
	}
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 2 {
		t.Fatalf("settings.json change should invalidate stamp and re-warm, invocations=%d", n)
	}
}

// TestWarmGoExtensionCellsSettingsExternalExtPath 验证 reviewer 项 3：
// settings.json 显式声明的宿主外本地 Go 扩展路径被纳入指纹——其文件变更
// 触发墓碑失效重热，同时默认 extensions 目录仍参与（未显式排除）。
func TestWarmGoExtensionCellsSettingsExternalExtPath(t *testing.T) {
	fx := newWarmTestFixture(t, "", true)
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, ""))
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig})

	outside := filepath.Join(t.TempDir(), "outside-ext")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("MkdirAll outside-ext: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "go.mod"), []byte("module outside/ext\n"), 0o644); err != nil {
		t.Fatalf("WriteFile go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("WriteFile main.go: %v", err)
	}
	settingsPath := filepath.Join(fx.pigHome, "agent", "settings.json")
	settings := fmt.Sprintf(`{"extensions":[%q]}`, outside)
	if err := os.WriteFile(settingsPath, []byte(settings), 0o644); err != nil {
		t.Fatalf("WriteFile settings.json: %v", err)
	}

	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 1 {
		t.Fatalf("initial warm-up did not run, invocations=%d", n)
	}
	// 外部扩展文件变更 → 指纹失效重热。
	if err := os.WriteFile(filepath.Join(outside, "main.go"), []byte("package main\n// v2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile main.go: %v", err)
	}
	MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil)
	if n := fx.warmInvocations(t); n != 2 {
		t.Fatalf("external settings-declared extension change should re-warm, invocations=%d", n)
	}
}

// TestExecWarmCommandWaitDelayStderrRace 验证 oracle S2：子孙进程在父进程
// 退出后仍持有 stderr fd 时，cmd.WaitDelay 到期使 Run 返回但后台管道拷贝
// 协程仍在向 stderrScanWriter 写入；Snapshot 加锁读取消除 data race（-race）。
func TestExecWarmCommandWaitDelayStderrRace(t *testing.T) {
	fakeDir := t.TempDir()
	fakePig := filepath.Join(fakeDir, "pig")
	// 子进程立即退出；孙子进程持续向 stderr 写噪声约 3s，令管道 EOF 迟迟不至。
	script := `#!/bin/sh
(sleep 0.1; i=0; while [ $i -lt 300 ]; do echo noise >&2; sleep 0.01; i=$((i+1)); done) &
exit 0
`
	if err := os.WriteFile(fakePig, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile fake pig: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := execWarmCommand(ctx, fakePig, fakeDir, []string{"PATH=/bin:/usr/bin"})
	if err == nil {
		t.Fatal("execWarmCommand must return error when WaitDelay expires on held stderr")
	}
}

// TestPreparePrivateClaudeConfigDirProjectsSymlink 覆盖 claude 私有配置目录的
// projects 共享软链：<privateTmpDir>/claude 仍是 0700 每会话私有目录，但其下
// projects 是指向 claudeSharedProjectsRoot 的符号链接（共享根预建 0700），
// 经软链创建的 <project-slug>/ 子目录与写入的 .jsonl 普通文件实际落盘到共享根，
// 而根级配置文件（.claude.json / settings.json）仍留在私有目录、不进共享根。
// 测试把共享根临时指向 t.TempDir()，不污染真实 /tmp/herdr-role-sessions。
func TestPreparePrivateClaudeConfigDirProjectsSymlink(t *testing.T) {
	privateTmp := t.TempDir()
	sharedRoot := filepath.Join(t.TempDir(), "claude-projects")

	orig := claudeSharedProjectsRoot
	claudeSharedProjectsRoot = sharedRoot
	t.Cleanup(func() { claudeSharedProjectsRoot = orig })

	dir := preparePrivateClaudeConfigDir(privateTmp)
	if dir != filepath.Join(privateTmp, "claude") {
		t.Fatalf("preparePrivateClaudeConfigDir = %q, want %q", dir, filepath.Join(privateTmp, "claude"))
	}

	// 私有配置目录本体仍是真实目录 0700。
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("private claude config dir missing: %v", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("private claude config dir bad state: mode=%v", info.Mode())
	}

	// 共享根已预建为真实目录 0700。
	sharedInfo, err := os.Stat(sharedRoot)
	if err != nil {
		t.Fatalf("shared projects root missing: %v", err)
	}
	if !sharedInfo.IsDir() || sharedInfo.Mode().Perm() != 0o700 {
		t.Fatalf("shared projects root bad state: mode=%v", sharedInfo.Mode())
	}

	// <dir>/projects 是指向共享根的符号链接。
	projectsLink := filepath.Join(dir, "projects")
	linkInfo, err := os.Lstat(projectsLink)
	if err != nil {
		t.Fatalf("Lstat %s: %v", projectsLink, err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s must be a symlink, mode=%v", projectsLink, linkInfo.Mode())
	}
	if target, err := os.Readlink(projectsLink); err != nil || target != sharedRoot {
		t.Fatalf("projects symlink target = %q err=%v, want %q", target, err, sharedRoot)
	}

	// 经软链创建项目子目录并写入 .jsonl 普通文件，实际落盘到共享根。
	projectDir := filepath.Join(projectsLink, "-proj-slug")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir via projects symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sharedRoot, "-proj-slug")); err != nil {
		t.Fatalf("project dir must land in shared root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "session-uuid.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write jsonl via projects symlink: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(sharedRoot, "-proj-slug", "session-uuid.jsonl")); err != nil || string(data) != "{}\n" {
		t.Fatalf("jsonl must land in shared root: data=%q err=%v", data, err)
	}

	// 根级配置文件仍每会话私有：写 <dir>/.claude.json 与 settings.json
	// 均不泄漏进共享根。
	for _, name := range []string{".claude.json", "settings.json"} {
		priv := filepath.Join(dir, name)
		if err := os.WriteFile(priv, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", priv, err)
		}
		if _, err := os.Lstat(filepath.Join(sharedRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("%s must not leak into shared root, err=%v", name, err)
		}
	}

	// 共享根被预置为指向外部目录的符号链接时（os.MkdirAll 对指向目录的软链
	// 返回 nil，必须靠 Lstat 守卫识别本体），不跟随软链修改外部目标权限，
	// 也不预置 <dir>/projects 软链（容忍降级）。
	externalDir := filepath.Join(t.TempDir(), "victim-dir")
	if err := os.MkdirAll(externalDir, 0o755); err != nil {
		t.Fatalf("mkdir external dir: %v", err)
	}
	if err := os.Chmod(externalDir, 0o755); err != nil {
		t.Fatalf("chmod external dir: %v", err)
	}
	linkRoot := filepath.Join(t.TempDir(), "claude-projects")
	if err := os.Symlink(externalDir, linkRoot); err != nil {
		t.Fatalf("symlink shared root to external dir: %v", err)
	}
	claudeSharedProjectsRoot = linkRoot
	privateTmpLink := t.TempDir()
	preparePrivateClaudeConfigDir(privateTmpLink)
	if fi, statErr := os.Stat(externalDir); statErr != nil || fi.Mode().Perm() != 0o755 {
		var mode os.FileMode
		if fi != nil {
			mode = fi.Mode().Perm()
		}
		t.Fatalf("external dir perm = %v err=%v, want 0755 unchanged", mode, statErr)
	}
	if _, lerr := os.Lstat(filepath.Join(privateTmpLink, "claude", "projects")); !os.IsNotExist(lerr) {
		t.Fatalf("projects symlink must not be created when shared root is a symlink, err=%v", lerr)
	}

	// 共享根的父目录（模拟 herdrRoleSessionsRoot）被预置为符号链接时，
	// 不容忍降级跟随父软链在外部目标下创建 claude-projects 子目录，
	// 也不预置 <dir>/projects 软链，与 ReviewerAgentWithEnv 对
	// sessionRoot 拒绝软链的 ReadWrite 白名单口径一致。
	externalTarget := filepath.Join(t.TempDir(), "external-target")
	if err := os.MkdirAll(externalTarget, 0o755); err != nil {
		t.Fatalf("mkdir external target: %v", err)
	}
	parentLink := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(externalTarget, parentLink); err != nil {
		t.Fatalf("symlink parent dir to external target: %v", err)
	}
	claudeSharedProjectsRoot = filepath.Join(parentLink, "claude-projects")
	privateTmpParentLink := t.TempDir()
	dir = preparePrivateClaudeConfigDir(privateTmpParentLink)
	if _, lerr := os.Lstat(filepath.Join(externalTarget, "claude-projects")); !os.IsNotExist(lerr) {
		t.Fatalf("claude-projects must not be created under external target via parent symlink, err=%v", lerr)
	}
	if _, lerr := os.Lstat(filepath.Join(dir, "projects")); !os.IsNotExist(lerr) {
		t.Fatalf("projects symlink must not be created when shared root parent is a symlink, err=%v", lerr)
	}
}

// TestAppendAgyStateReadWriteMarksRootFilesOptional 覆盖 [pattern:verifier-transient-fail] 修复的
// 标记口径：appendAgyStateReadWrite 快照枚举的根级状态文件（含 SQLite 瞬态
// *.db-shm/*.db-wal/*.db-journal 与临时原子写文件）必须同时进 ReadWrite
// （授权不变）与 OptionalReadWrite（Apply 时 ENOENT 容忍）；数据子目录、
// bin/ 软链绝不进 OptionalReadWrite——保持严格 fail-closed ENOENT 报错。
func TestAppendAgyStateReadWriteMarksRootFilesOptional(t *testing.T) {
	const fixture = "conversation_summaries.db-shm"
	var roots []string
	var fixturePaths []string
	for _, dir := range agyAntigravityStateDirs() {
		if st, err := os.Lstat(dir); err == nil && st.IsDir() {
			roots = append(roots, dir)
			fp := filepath.Join(dir, fixture)
			fixturePaths = append(fixturePaths, fp)
			if err := os.WriteFile(fp, []byte("x"), 0o600); err != nil {
				t.Fatalf("write fixture %s: %v", fp, err)
			}
			t.Cleanup(func() { _ = os.Remove(fp) })
		}
	}
	if len(roots) == 0 {
		t.Skip("host has no antigravity-cli state root")
	}

	var p Policy
	appendAgyStateReadWrite(&p)

	rwSet := make(map[string]struct{}, len(p.ReadWrite))
	for _, rp := range p.ReadWrite {
		if n, err := NormalizePath(rp); err == nil {
			rwSet[n] = struct{}{}
		}
	}
	orwSet := make(map[string]struct{}, len(p.OptionalReadWrite))
	for _, op := range p.OptionalReadWrite {
		if n, err := NormalizePath(op); err == nil {
			orwSet[n] = struct{}{}
		}
	}

	for _, fp := range fixturePaths {
		n, err := NormalizePath(fp)
		if err != nil {
			t.Fatalf("NormalizePath %q: %v", fp, err)
		}
		if _, ok := rwSet[n]; !ok {
			t.Fatalf("snapshot-enumerated root file %q missing from ReadWrite", fp)
		}
		if _, ok := orwSet[n]; !ok {
			t.Fatalf("snapshot-enumerated root file %q missing from OptionalReadWrite", fp)
		}
	}
	// agyStateDataSubdirs 数据子目录只进 ReadWrite、绝不进 OptionalReadWrite（fail-closed 不变）。
	for _, root := range roots {
		for _, sub := range agyStateDataSubdirs {
			n, err := NormalizePath(filepath.Join(root, sub))
			if err != nil {
				t.Fatalf("NormalizePath subdir: %v", err)
			}
			if _, ok := rwSet[n]; !ok {
				t.Fatalf("data subdir %s/%s missing from ReadWrite", root, sub)
			}
			if _, ok := orwSet[n]; ok {
				t.Fatalf("data subdir %s/%s must NOT be in OptionalReadWrite", root, sub)
			}
		}
	}
}

// ── export_test 段：供外部测试包 sandbox_test 使用 ────────────────────────
// sandbox_test.go 属 package sandbox_test，无法访问包内未导出符号。本段
// 仅存在于测试二进制中（非生产 API）：resolveEntryAgentPaths 的 hostHome
// 参数取代 trustedHomeDir()，使「隔离 HOME 下宿主受信安装根/收录面」相关
// 断言能用 t.TempDir() 夹具确定性实跑，摆脱宿主真实 claude/agy 安装状态。
var ResolveEntryAgentPathsForTest = resolveEntryAgentPaths
