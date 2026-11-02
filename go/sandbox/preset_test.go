package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
