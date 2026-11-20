package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
		got := injectPigCodingAgentDir(append([]string(nil), baseEnv...), private, nil)
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
		got := injectPigCodingAgentDir(env, private, nil)
		m := parseEnvSlice(got)
		if m["PI_CODING_AGENT_DIR"] != private {
			t.Fatalf("expected PI_CODING_AGENT_DIR injected via Policy.Env flag, env=%v", got)
		}
	})

	t.Run("PI_CODING_AGENT_DIR stale env value is overridden by private dir", func(t *testing.T) {
		// 宿主残留/非 cfgEnv 显式声明的 PI_CODING_AGENT_DIR（如被塞进
		// Policy.Env 的旧值或宿主环境变量）不得被 pig 包装器后置的
		// export PIG_USE_PI_DIRS=1 激活，必须无条件回钉私有目录。
		t.Setenv("PIG_USE_PI_DIRS", "1")
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		env := append(append([]string(nil), baseEnv...), "PI_CODING_AGENT_DIR=/custom/pi-agent")
		got := injectPigCodingAgentDir(env, private, nil)
		m := parseEnvSlice(got)
		if m["PI_CODING_AGENT_DIR"] != private {
			t.Fatalf("stale PI_CODING_AGENT_DIR must be pinned to private, got %q", m["PI_CODING_AGENT_DIR"])
		}
	})

	t.Run("PI_CODING_AGENT_DIR host env overridden by private dir", func(t *testing.T) {
		// 宿主环境 PI_CODING_AGENT_DIR（未在 cfgEnv 显式声明、未激活
		// PIG_USE_PI_DIRS 自定义语境）会被包装器后来启用 PIG_USE_PI_DIRS
		// 时激活，必须回钉为私有目录。
		t.Setenv("PIG_USE_PI_DIRS", "1")
		t.Setenv("PI_CODING_AGENT_DIR", "/host/pi-agent")
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		got := injectPigCodingAgentDir(append([]string(nil), baseEnv...), private, nil)
		m := parseEnvSlice(got)
		if m["PI_CODING_AGENT_DIR"] != private {
			t.Fatalf("host PI_CODING_AGENT_DIR must be pinned to private, got %q", m["PI_CODING_AGENT_DIR"])
		}
		if m["PIG_CODING_AGENT_DIR"] != private {
			t.Fatalf("PIG_CODING_AGENT_DIR = %q", m["PIG_CODING_AGENT_DIR"])
		}
	})

	t.Run("PIG_USE_PI_DIRS unset both pinned to private dir", func(t *testing.T) {
		// 新口径：未设 PIG_USE_PI_DIRS 时默认同时注入双变量，均回钉私有
		// 目录——防止 pig 包装器后置 export PIG_USE_PI_DIRS=1 激活宿主残留。
		unsetEnv(t, "PIG_USE_PI_DIRS")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		got := injectPigCodingAgentDir(append([]string(nil), baseEnv...), private, nil)
		m := parseEnvSlice(got)
		if m["PI_CODING_AGENT_DIR"] != private {
			t.Fatalf("PI_CODING_AGENT_DIR must be injected to private by default, env=%v", got)
		}
		if m["PIG_CODING_AGENT_DIR"] != private {
			t.Fatalf("PIG_CODING_AGENT_DIR = %q", m["PIG_CODING_AGENT_DIR"])
		}
	})

	t.Run("PIG_CODING_AGENT_DIR host env overridden; cfgEnv explicit custom preserved", func(t *testing.T) {
		// 宿主 PIG_CODING_AGENT_DIR 属残留面：非 cfgEnv 显式声明时一律
		// 回钉私有目录（防宿主残留被包装器后置 PIG_USE_PI_DIRS=1 激活）。
		t.Setenv("PIG_USE_PI_DIRS", "1")
		t.Setenv("PIG_CODING_AGENT_DIR", "/custom/agent")
		got := injectPigCodingAgentDir(append([]string(nil), baseEnv...), private, nil)
		m := parseEnvSlice(got)
		if m["PIG_CODING_AGENT_DIR"] != private {
			t.Fatalf("host PIG_CODING_AGENT_DIR must be pinned to private, got %q", m["PIG_CODING_AGENT_DIR"])
		}
		// cfgEnv 显式声明非空 PIG_CODING_AGENT_DIR 时保持其优先级（可信自定义）。
		got2 := injectPigCodingAgentDir(append([]string(nil), baseEnv...), private, []string{"PIG_CODING_AGENT_DIR=/custom/agent"})
		m2 := parseEnvSlice(got2)
		if m2["PIG_CODING_AGENT_DIR"] != "" {
			t.Fatalf("cfgEnv explicit PIG_CODING_AGENT_DIR must not be overridden by inject, got %q", m2["PIG_CODING_AGENT_DIR"])
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
  echo "GOCMD=$(command -v go 2>/dev/null || echo none)"
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
	// 受控 PATH 白名单：仅固定可信目录 + 首个条目允许为沙盒对齐的真实 go
	// 所在目录（resolveRealGoBinary 与沙盒同口径选出，保证 cell 摘要命中）。
	realGoDir := ""
	if rg := resolveSandboxRealGoBinary(""); rg != "" {
		realGoDir = filepath.Dir(rg)
	}
	for i, seg := range strings.Split(childPath, string(os.PathListSeparator)) {
		if i == 0 && realGoDir != "" && seg == realGoDir {
			continue
		}
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
	// Policy.Env 固定注入 PIG_HOME=<privateTmpDir>/pig-home 私有根（沙盒内 pig
	// 经私有根访问投影的宿主产物；宿主 pigHome2 产物仍逐文件枚举进 Exec）。
	workdir := t.TempDir()
	p := ReviewerAgentWithEnv(workdir, cfgEnv)
	t.Cleanup(func() { removePresetTempDirs(t, p) })
	tmpdir := lookupEnvValue(p.Env, "TMPDIR")
	wantPrivatePigHome := filepath.Join(tmpdir, "pig-home")
	if got := lookupEnvValue(p.Env, "PIG_HOME"); got != wantPrivatePigHome {
		t.Fatalf("Policy.Env must pin PIG_HOME=%q (private root), got %q (env=%v)", wantPrivatePigHome, got, p.Env)
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
	// 隔离 TMPDIR：预热临时目录走 os.MkdirTemp("", "sbx-warm-*") 遵循
	// os.TempDir()，用独立目录避免宿主 /tmp 中历史诊断文件
	// （如 sbx-warm-scratch-repair-*.txt）污染零残留 glob 断言。
	t.Setenv("TMPDIR", t.TempDir())
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

// TestPreparePrivatePigHomeProjection 验证私有 PIG_HOME 投影结构：
// 宿主有效缓存单元经同名软链投影进私有 cells/<lang>/ext（私有目录本身可写
// 承载 scratch/locks）；每个投影 artifact 经软链解析后的宿主真实路径须已入
// 传入 execSet，否则记入 missing。无效单元（无 ready.json）不投影。
func TestPreparePrivatePigHomeProjection(t *testing.T) {
	hostPigHome := t.TempDir()
	hostCache := filepath.Join(hostPigHome, "cache")
	writeFakeCellEntry(t, filepath.Join(hostCache, "cells", "go", "deadbeef0001"), "deadbeef0001", "runner", "")
	writeFakeCellEntry(t, filepath.Join(hostCache, "ext", "fake-ext-cafe0001"), "cafe0001", "bin", "go")
	// 无效单元（无 ready.json）：不投影。
	if err := os.MkdirAll(filepath.Join(hostCache, "cells", "go", "no-ready-9999"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 宿主 bin/docs/state/toolchains。
	for _, sub := range []string{"bin", "docs", "state"} {
		if err := os.MkdirAll(filepath.Join(hostPigHome, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(hostCache, "toolchains"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostCache, "toolchains", "tc-1.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// execSet：含两个有效单元的 artifact 解析后真实路径（模拟 appendCellCachePolicy
	// 已把宿主产物枚举进 Policy.Exec）。
	execSet := map[string]struct{}{}
	for _, rel := range []string{
		filepath.Join("cells", "go", "deadbeef0001", "runner"),
		filepath.Join("ext", "fake-ext-cafe0001", "bin"),
	} {
		hostArtifact := filepath.Join(hostCache, rel)
		if norm, err := NormalizePath(hostArtifact); err == nil {
			execSet[filepath.Clean(norm)] = struct{}{}
		}
	}

	privateTmpDir := t.TempDir()
	privatePigHome, missing := preparePrivatePigHome(privateTmpDir, hostPigHome, execSet)
	if len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
	if filepath.Base(privatePigHome) != "pig-home" {
		t.Fatalf("private pig-home path = %q", privatePigHome)
	}
	// bin/docs/state 是软链→宿主。
	for _, sub := range []string{"bin", "docs", "state"} {
		linkPath := filepath.Join(privatePigHome, sub)
		li, err := os.Lstat(linkPath)
		if err != nil || li.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s must be symlink to host, err=%v mode=%v", sub, err, li.Mode())
		}
		if tgt, _ := os.Readlink(linkPath); tgt != filepath.Join(hostPigHome, sub) {
			t.Fatalf("%s -> %q, want %q", sub, tgt, filepath.Join(hostPigHome, sub))
		}
	}
	// cells/go/<digest> 与 ext/<entry> 是软链→宿主对应单元；cells/go 目录本身可写。
	for _, rel := range []string{
		filepath.Join("cache", "cells", "go", "deadbeef0001"),
		filepath.Join("cache", "ext", "fake-ext-cafe0001"),
	} {
		li, err := os.Lstat(filepath.Join(privatePigHome, rel))
		if err != nil || li.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s must be projected symlink, err=%v", rel, err)
		}
	}
	if li, err := os.Lstat(filepath.Join(privatePigHome, "cache", "cells", "go")); err != nil || !li.IsDir() || li.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("cells/go must be real writable dir, err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(privatePigHome, "cache", "cells", "go", ".locks")); err != nil {
		t.Fatalf("cells/go/.locks must exist for build locks: %v", err)
	}
	// 无效单元不投影。
	if _, err := os.Lstat(filepath.Join(privatePigHome, "cache", "cells", "go", "no-ready-9999")); !os.IsNotExist(err) {
		t.Fatalf("invalid cell entry must not be projected, err=%v", err)
	}
	// toolchains 条目投影 + 目录可写。
	if _, err := os.Lstat(filepath.Join(privatePigHome, "cache", "toolchains", "tc-1.json")); err != nil {
		t.Fatalf("toolchains record must be projected: %v", err)
	}

	// 反向用例：execSet 缺某 artifact → missing 指名该项（供启动前 fail-closed）。
	privateTmpDir2 := t.TempDir()
	execSetMissing := map[string]struct{}{}
	if norm, err := NormalizePath(filepath.Join(hostCache, "cells", "go", "deadbeef0001", "runner")); err == nil {
		execSetMissing[filepath.Clean(norm)] = struct{}{}
	}
	_, missing2 := preparePrivatePigHome(privateTmpDir2, hostPigHome, execSetMissing)
	if len(missing2) == 0 {
		t.Fatalf("expected missing entry for ext artifact not in execSet")
	}
	found := false
	for _, m := range missing2 {
		if strings.Contains(m, "fake-ext-cafe0001") {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing must name the uncovered ext entry, got %v", missing2)
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

	t.Run("PI via cfg dir but host-only PIG_USE_PI_DIRS is not CfgEnv", func(t *testing.T) {
		// resolveAgentDirSelection 收紧：PIG_USE_PI_DIRS=1 仅来自宿主环境时，
		// cfgEnv 显式 PI_CODING_AGENT_DIR 不得归入 CfgEnv（trustedCustom）；
		// HostEnv 层要求 PI 键未在 cfgEnv 出现（!piInCfg 不成立），同样不
		// 命中——直接回落 Default 层（私有回钉）。宿主开关残留是旧口径下
		// 显式受信语义被越权激活的混淆通道。
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		t.Setenv("PIG_USE_PI_DIRS", "1")
		custom := filepath.Join(t.TempDir(), "pi-agent")
		if err := os.MkdirAll(custom, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(custom, 0o700); err != nil {
			t.Fatal(err)
		}
		sel := resolveAgentDirSelection(fakeHome, []string{"PI_CODING_AGENT_DIR=" + custom})
		if sel.origin == agentDirOriginCfgEnv || sel.trustedCustom {
			t.Fatalf("host-only PIG_USE_PI_DIRS=1 must not promote cfgEnv PI dir to CfgEnv/trustedCustom: sel=%+v", sel)
		}
		// cfgEnv 已显式声明 PI 键，HostEnv 层（要求键未在 cfg 出现）同样
		// 不命中——既非 CfgEnv 受信也非 HostEnv 复制源，落 Default 回钉
		// 私有目录：宿主开关残留无法借显式 PI 值冒认受信自定义，也不能
		// 把显式 PI 值静默降级成宿主复制源。
		if sel.origin != agentDirOriginDefault {
			t.Fatalf("host-only switch + cfg PI dir must fall to Default (private pin): sel=%+v want origin=Default", sel)
		}
	})

	t.Run("ReviewerAgentWithEnv pins resolved PIG_HOME into Policy.Env", func(t *testing.T) {
		fx := newWarmTestFixture(t, "", true)
		cfgEnv := []string{"PIG_HOME=" + fx.pigHome}
		workdir := t.TempDir()
		p := ReviewerAgentWithEnv(workdir, cfgEnv)
		t.Cleanup(func() { removePresetTempDirs(t, p) })
		// 私有 PIG_HOME 重定向：Policy.Env["PIG_HOME"] 指向 <TMPDIR>/pig-home
		// 私有根；宿主 fx.pigHome 经 resolvePigHomeDir 参与预热对齐与产物投影。
		tmpdir := lookupEnvValue(p.Env, "TMPDIR")
		wantPrivate := filepath.Join(tmpdir, "pig-home")
		if got := lookupEnvValue(p.Env, "PIG_HOME"); got != wantPrivate {
			t.Fatalf("Policy.Env must pin private PIG_HOME=%q, got %q", wantPrivate, got)
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

// TestWarmGoExtensionCellsGoToolchainAlign 是 .build-* EACCES 授权缺口的回归：
// 沙盒内 pig 经 goExecDir 包装器由 resolveRealGoBinary 解析真实 go（宿主
// PATH 顺序）；预热子进程原先只用 controlledWarmPath 的固定目录顺序，在
// /usr/local/go 优先而宿主 PATH 先命中 /usr/bin 的机器上解析出不同 go——
// goPackedCellHash 折进 toolchain 的 commandVersion/goroot/compiler，两侧
// toolchain 不一致则 cell 摘要错配，沙盒内 pig 被迫在 cells/go 下建
// .build-* scratch 而撞 W^X 只读（15 个 Go 扩展全加载失败、exit 1）。
// 本测试在工作树内放一个「假 go」并把宿主 PATH 前置到它，断言预热子进程
// command -v go 仍解析到可信的真实 go（不得前置该不可信父目录），同时断言
// 可信解析与沙盒侧 resolveRealGoBinary 在多版本工具链下仍对齐同一实体。
func TestWarmGoExtensionCellsGoToolchainAlign(t *testing.T) {
	// 在工作树（不可信父目录）内植入假 go：resolveSandboxRealGoBinary 必须
	// 拒绝把它（或其所在不可信父目录）前置进预热 PATH——旧实现只按宿主 PATH
	// 顺序取首个可执行 go，预热子进程在 Landlock 生效前就会以宿主权限执行它。
	untrustedDir := t.TempDir()
	fakeGo := filepath.Join(untrustedDir, "go")
	if err := os.WriteFile(fakeGo, []byte("#!/bin/sh\necho untrusted-fake-go\n"), 0o755); err != nil {
		t.Fatalf("WriteFile fake go: %v", err)
	}
	// 同一污染现场再叠加 PI_SANDBOX_REAL_GO 污染（resolveRealGoBinary 的
	// Env 覆盖优先于 PATH）：伪造 go 不得成为预热/策略共用的受信实体。
	t.Setenv("PI_SANDBOX_REAL_GO", fakeGo)
	t.Setenv("PATH", untrustedDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// GOROOT 别名反例（先建壳）：伪造 GOROOT 的 bin/go 是指向受信 go 的软链，
	// 旧实现据此把原始 envGR 加入 goRoots 并直接放行 envGR/pkg/tool——伪造
	// pkg/tool 随别名根混进 Policy.Exec。修复后只接纳「已解析真实 go 的
	// 规范化可信根」。bin/go 软链在下方 realGo 解析出后补建。
	fakeGOROOT := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fakeGOROOT, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeToolDir := filepath.Join(fakeGOROOT, "pkg", "tool")
	if err := os.MkdirAll(fakeToolDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fx := newWarmTestFixture(t, "", true)
	fx.setPigScript(t, fx.warmEnvLogScript(bundledCellSnippet, ""))
	useWarmHook(t, &warmGoCellTestHook{PigBin: fx.fakePig})

	if code := MaybeWarmGoExtensionCells("reviewer-agent", []string{"pig"}, nil); code != 0 {
		t.Fatalf("warm-up returned non-zero exit code %d", code)
	}
	if n := fx.warmInvocations(t); n == 0 {
		t.Fatal("cold start: fake pig was not invoked")
	}

	// 受信解析：resolveSandboxRealGoBinary 跳过不可信 PATH 前置与
	// PI_SANDBOX_REAL_GO 污染的假 go，命中固定可信工具链前缀内的真实 go。
	wantRealGo := resolveSandboxRealGoBinary("")
	if wantRealGo == "" {
		t.Skip("no trusted real go resolvable on host; cannot assert toolchain alignment")
	}
	wantGoDir := filepath.Dir(wantRealGo)
	gotGo := fx.warmLogEnv(t, "GOCMD")
	if gotGo == "" || gotGo == "none" {
		t.Fatalf("warm subprocess did not resolve any go: %q", gotGo)
	}
	// command -v go 返回 PATH 中首个 go；其所在目录必须是受信真实 go 目录，
	// 不得是不可信父目录 untrustedDir（旧实现会把整个不可信目录前置进 PATH）。
	if filepath.Dir(gotGo) == untrustedDir {
		t.Fatalf("warm subprocess resolved untrusted fake go at %q (dir %q); untrusted parent dir must not be prepended to warm PATH", gotGo, untrustedDir)
	}
	if gotDir := filepath.Dir(gotGo); gotDir != wantGoDir {
		t.Fatalf("warm subprocess resolved go at %q (dir %q), want trusted real go dir %q; cell digest would mismatch and force .build-* rebuild under read-only cells/go", gotGo, gotDir, wantGoDir)
	}
	// 多版本工具链对齐能力保留：预热命中的真实 go（EvalSymlinks 后）必须与
	// resolveSandboxRealGoBinary 选中的是同一个真实 go 实体。
	resolvedWarm, err := filepath.EvalSymlinks(gotGo)
	if err != nil || resolvedWarm == "" {
		resolvedWarm = gotGo
	}
	resolvedWant, err := filepath.EvalSymlinks(wantRealGo)
	if err != nil || resolvedWant == "" {
		resolvedWant = wantRealGo
	}
	if filepath.Clean(resolvedWarm) != filepath.Clean(resolvedWant) {
		t.Fatalf("warm go %q (resolved %q) and trusted resolver %q (resolved %q) must be the same real go for cell digest alignment", gotGo, resolvedWarm, wantRealGo, resolvedWant)
	}
	// 受信前缀断言：选中 go 的真实路径位于可信工具链前缀内（回归点本身）。
	if !pathWithinPrefixes(resolvedWant, trustedGoToolchainPrefixes()) {
		t.Fatalf("trusted resolver returned %q outside trusted toolchain prefixes", resolvedWant)
	}

	// GOROOT 别名反例落成：<fakeGOROOT>/bin/go 软链指向受信 go（旧实现会
	// 误判整根可信），pkg/tool 为伪造目录。同一污染现场调用
	// ReadOnlyReviewer：policy 注入的 PI_SANDBOX_REAL_GO 必须与预热同一
	// 受信实体，且 Policy.Exec 绝不含伪造 GOROOT/pkg/tool 与不可信 go。
	if err := os.Symlink(wantRealGo, filepath.Join(fakeGOROOT, "bin", "go")); err != nil {
		t.Fatalf("symlink fake GOROOT/bin/go -> trusted go: %v", err)
	}
	t.Setenv("GOROOT", fakeGOROOT)
	policy := ReadOnlyReviewer(t.TempDir())
	t.Cleanup(func() { removePresetTempDirs(t, policy) })
	if got := lookupEnvValue(policy.Env, "PI_SANDBOX_REAL_GO"); got != wantRealGo {
		t.Fatalf("policy PI_SANDBOX_REAL_GO = %q, want same trusted entity as warm resolver %q", got, wantRealGo)
	}
	execSet := make(map[string]struct{}, len(policy.Exec))
	for _, ex := range policy.Exec {
		execSet[filepath.Clean(ex)] = struct{}{}
		if resolved, rerr := filepath.EvalSymlinks(ex); rerr == nil && resolved != "" {
			execSet[filepath.Clean(resolved)] = struct{}{}
		}
	}
	for _, bad := range []string{fakeToolDir, fakeGo, untrustedDir} {
		if _, hit := execSet[filepath.Clean(bad)]; hit {
			t.Fatalf("untrusted path %q must not enter Policy.Exec (GOROOT alias / PATH pollution)", bad)
		}
	}
	// 正向锚点：受信真实 go 本体仍须在 Exec（防整列被清空后的空转假绿）。
	if _, hit := execSet[filepath.Clean(wantRealGo)]; !hit {
		t.Fatalf("trusted real go %q missing from Policy.Exec", wantRealGo)
	}

	// trustedRealGoCandidateWithPrefixes 边界分支（t.TempDir() 前缀注入）：
	// 常规可执行 go 通过；perm&0o022!=0（组可写）拒绝；父目录 0777 拒绝；
	// 非 go 基名拒绝。
	prefix := t.TempDir()
	good := filepath.Join(prefix, "bin", "go")
	if err := os.MkdirAll(filepath.Dir(good), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if resolved, ok := trustedRealGoCandidateWithPrefixes(good, []string{prefix}); !ok || resolved == "" {
		t.Fatalf("regular go under temp prefix must be accepted: resolved=%q ok=%v", resolved, ok)
	}
	groupWritable := filepath.Join(prefix, "bin", "go-gw")
	if err := os.WriteFile(groupWritable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// WriteFile 的 mode 受 umask 影响；显式 Chmod 0777 确保 perm&0o022!=0。
	if err := os.Chmod(groupWritable, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, ok := trustedRealGoCandidateWithPrefixes(groupWritable, []string{prefix}); ok {
		t.Fatalf("group/world-writable go must be rejected (perm&0o022!=0)")
	}
	worldWritableParent := filepath.Join(prefix, "wdir")
	if err := os.MkdirAll(worldWritableParent, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(worldWritableParent, 0o777); err != nil {
		t.Fatal(err)
	}
	innerGo := filepath.Join(worldWritableParent, "go")
	if err := os.WriteFile(innerGo, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := trustedRealGoCandidateWithPrefixes(innerGo, []string{prefix}); ok {
		t.Fatalf("go under 0777 parent dir must be rejected (parent dir untrusted)")
	}
	notGo := filepath.Join(prefix, "bin", "notgo")
	if err := os.WriteFile(notGo, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := trustedRealGoCandidateWithPrefixes(notGo, []string{prefix}); ok {
		t.Fatalf("non-go basename must be rejected")
	}
}

// TestTrustedToolDirPkgTool 是 trustedToolDir 前缀误拒的回归：pkg/tool 位于
// 工具链根而非 bin 前缀之下，旧实现复用 trustedGoToolchainPrefixes（仅含
// <root>/bin、/usr/lib、/usr/share、*.local/opt 与 standardBinDirs）会把
// 合规的 /usr/local/go/pkg/tool 误拒出 Policy.Exec。修复后 trustedToolDir
// 走专用 trustedGoToolDirPrefixes：
//   - 正向：本机存在且合规（真实目录、perm&0o022==0、属主 0 或 euid）的
//     /usr/local/go/pkg/tool、/usr/lib/go/pkg/tool 必须通过 trustedToolDir
//     并被 resolveGoToolchainPaths 收录；
//   - 反向：软链 pkg/tool、组/全局可写目录、可信前缀外的外部目录仍一律拒绝，
//     trustedRealGoCandidate 的 bin 前缀未被放宽（见本文件既有校验）。
func TestTrustedToolDirPkgTool(t *testing.T) {
	// 正向：固定可信 pkg/tool 根，存在且合规时必须放行并被收录——生产
	// trustedToolDir 返回解析后的规范真实路径且非空。
	checked := 0
	for _, toolDir := range []string{"/usr/local/go/pkg/tool", "/usr/lib/go/pkg/tool"} {
		st, err := os.Stat(toolDir)
		if err != nil || !st.IsDir() || st.Mode().Perm()&0o022 != 0 {
			continue // 本机无此根或不合规（如自定义安装权限位），跳过该锚点
		}
		checked++
		resolved, ok := trustedToolDir(toolDir)
		if !ok || resolved == "" {
			t.Fatalf("trustedToolDir must accept compliant %q (resolved=%q ok=%v)", toolDir, resolved, ok)
		}
		// resolveGoToolchainPaths 只收录解析后的规范真实路径（如
		// /usr/lib/go-1.24/pkg/tool），不含祖先软链拼写（/usr/lib/go 是
		// 软链）：与 trustedToolDir 返回的 resolved 逐一对齐断言。
		found := false
		for _, p := range resolveGoToolchainPaths() {
			if filepath.Clean(p) == filepath.Clean(resolved) {
				found = true
			}
		}
		if !found {
			t.Fatalf("resolveGoToolchainPaths must include resolved %q (from compliant %q)", resolved, toolDir)
		}
	}
	if checked == 0 {
		t.Skip("no compliant /usr/local/go/pkg/tool or /usr/lib/go/pkg/tool on host; positive anchor unverifiable")
	}

	// trustedToolDirWithPrefixes（t.TempDir() 前缀注入）：真实覆盖
	// 0777 组/全局可写拒绝分支（perm&0o022!=0，前缀检查已通过）与
	// 本体软链拒绝分支（含指向前缀内真实目录与指向本机
	// /usr/local/go/pkg/tool 的软链）——生产 trustedToolDir 走固定前缀
	// 无法覆盖这些拒绝分支（前缀检查先短路），必须走注入版本（SF-2）。
	prefix := t.TempDir()
	prefixes := []string{prefix}

	// 正向：前缀内合规目录命中并返回解析后规范路径。
	goodTool := filepath.Join(prefix, "pkg", "tool")
	if err := os.MkdirAll(goodTool, 0o755); err != nil {
		t.Fatal(err)
	}
	if resolved, ok := trustedToolDirWithPrefixes(goodTool, prefixes); !ok || filepath.Clean(resolved) != filepath.Clean(goodTool) {
		t.Fatalf("trustedToolDirWithPrefixes must accept compliant %q (resolved=%q ok=%v)", goodTool, resolved, ok)
	}

	// 反向：0777 组/全局可写目录拒绝（覆盖 Perm()&0o022!=0 分支）。
	badPerm := filepath.Join(prefix, "bad-perm")
	if err := os.MkdirAll(badPerm, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(badPerm, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, ok := trustedToolDirWithPrefixes(badPerm, prefixes); ok {
		t.Fatalf("trustedToolDirWithPrefixes must reject 0777 group/world-writable %q", badPerm)
	}

	// 反向：软链本体拒绝——指向「前缀内」真实目录的软链同样拒绝
	//（Lstat 口径优先于 EvalSymlinks/前缀检查）。
	linkInside := filepath.Join(prefix, "link-inside")
	if err := os.Symlink(goodTool, linkInside); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, ok := trustedToolDirWithPrefixes(linkInside, prefixes); ok {
		t.Fatalf("trustedToolDirWithPrefixes must reject symlink %q (-> prefix-internal real dir)", linkInside)
	}

	// 反向：软链本体拒绝——指向本机 /usr/local/go/pkg/tool 的软链：
	// 目标恰是固定可信前缀内的真实目录，Lstat 软链拒绝必须先于「目标
	// 合规」判定生效（别名拼写不得获执行授权）。
	hostTool := "/usr/local/go/pkg/tool"
	if st, err := os.Stat(hostTool); err == nil && st.IsDir() {
		linkHost := filepath.Join(prefix, "link-host")
		if err := os.Symlink(hostTool, linkHost); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, ok := trustedToolDirWithPrefixes(linkHost, prefixes); ok {
			t.Fatalf("trustedToolDirWithPrefixes must reject symlink %q (-> %s)", linkHost, hostTool)
		}
	} else {
		t.Logf("%s absent on host; skip symlink-to-host-tool case", hostTool)
	}

	// 反向：生产 trustedToolDir（固定前缀）对 t.TempDir() 下合规目录
	// 必须拒绝（前缀失配）——注入前缀不是外部开启入口。
	if _, ok := trustedToolDir(goodTool); ok {
		t.Fatalf("trustedToolDir must reject %q outside trusted prefixes", goodTool)
	}

	// 反向：不存在路径必须拒绝。
	if _, ok := trustedToolDirWithPrefixes(filepath.Join(prefix, "missing"), prefixes); ok {
		t.Fatal("trustedToolDirWithPrefixes must reject nonexistent dir")
	}
}

// TestResolveGoToolchainPathsGOROOTAlias 是 MF-1 回归：GOROOT 指向伪造根
// fake-goroot，其 pkg 为指向真实 GOROOT pkg 的中间软链（fake/pkg ->
// <realGoRoot>/pkg），伪造根的 bin/go 缺失或只是普通文件。
// 断言 resolveGoToolchainPaths() 与 ReadOnlyReviewer 的 Policy.Exec 绝不
// 含 <fake>/pkg/tool 拼写（别名不得获执行授权），同时本机合规
// /usr/local/go/pkg/tool 仍以规范真实路径收录。
func TestResolveGoToolchainPathsGOROOTAlias(t *testing.T) {
	// 受信真实 go 必须可解析：其真实根下的 pkg 即中间软链目标。
	realGo := resolveSandboxRealGoBinary("")
	if realGo == "" {
		t.Skip("no trusted real go resolvable on host")
	}
	resolvedReal, err := filepath.EvalSymlinks(realGo)
	if err != nil || resolvedReal == "" {
		t.Fatalf("EvalSymlinks(%q): %v", realGo, err)
	}
	realGoRoot := filepath.Dir(filepath.Dir(resolvedReal))
	hostTool := filepath.Join(realGoRoot, "pkg", "tool")
	if st, err := os.Stat(hostTool); err != nil || !st.IsDir() {
		t.Skipf("host tool dir %s not usable on this host", hostTool)
	}

	// 伪造 GOROOT：bin/go 缺失（未通过 trustedRealGoCandidate），pkg
	// 中间软链指向受信根 pkg——Lstat(<fake>/pkg/tool) 是真实目录（跟随
	// 中间软链）且解析后位于可信前缀内，旧实现据此把未解析的
	// <fake>/pkg/tool 写进 Policy.Exec（MF-1 逃逸形态）。
	fakeRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fakeRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(realGoRoot, "pkg"), filepath.Join(fakeRoot, "pkg")); err != nil {
		t.Fatalf("symlink fake pkg -> host pkg: %v", err)
	}
	aliasTool := filepath.Join(fakeRoot, "pkg", "tool")
	if st, err := os.Lstat(aliasTool); err != nil || !st.IsDir() {
		t.Fatalf("fixture broken: %q via intermediate symlink is not a dir: %v", aliasTool, err)
	}
	t.Setenv("GOROOT", fakeRoot)

	paths := resolveGoToolchainPaths()
	for _, p := range paths {
		cp := filepath.Clean(p)
		if cp == filepath.Clean(aliasTool) {
			t.Fatalf("unresolved alias %q must not enter resolveGoToolchainPaths output", aliasTool)
		}
		if strings.HasPrefix(cp, filepath.Clean(fakeRoot)+string(filepath.Separator)) {
			t.Fatalf("path %q under untrusted fake GOROOT must not be trusted", cp)
		}
	}

	// ReadOnlyReviewer 的 Policy.Exec 同口径：绝不含别名拼写或未解析路径。
	policy := ReadOnlyReviewer(t.TempDir())
	t.Cleanup(func() { removePresetTempDirs(t, policy) })
	for _, ex := range policy.Exec {
		cp := filepath.Clean(ex)
		if cp == filepath.Clean(aliasTool) {
			t.Fatalf("unresolved alias %q must not enter Policy.Exec", aliasTool)
		}
		if strings.HasPrefix(cp, filepath.Clean(fakeRoot)+string(filepath.Separator)) {
			t.Fatalf("Policy.Exec entry %q under untrusted fake GOROOT", cp)
		}
	}

	// 正向：本机合规 /usr/local/go/pkg/tool 仍以规范真实路径被收录
	//（存在且合规时；缺该根则断言真实根 pkg/tool 被收录）。
	wantTool := filepath.Clean(hostTool)
	if st, err := os.Stat("/usr/local/go/pkg/tool"); err == nil && st.IsDir() && st.Mode().Perm()&0o022 == 0 {
		wantTool = "/usr/local/go/pkg/tool"
	}
	foundTool := false
	for _, p := range paths {
		if filepath.Clean(p) == wantTool {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("compliant tool dir %q must be included in resolveGoToolchainPaths output: %v", wantTool, paths)
	}
}

// TestReviewerAgentUntrustedPigHomeFailClosed 是 SF-1（方案 1A）回归：
// PIG_HOME 为软链或 0777 公共可写目录时，ReviewerAgentWithEnv 不得
// 读取其 <pigHome>/agent、不得改写其 usage.json、不得把其缓存产物
// 枚举进 Policy.Exec、不得建指向它的宿主投影；同时
// reviewerAgentCellTargets 记录错误——ReviewerAgentCellTargetError()
// 返回非 nil，真实 pig 入口启动前 fail-closed，非 pig 命令以空私有根放行。
func TestReviewerAgentUntrustedPigHomeFailClosed(t *testing.T) {
	workdir := t.TempDir()

	makeHostPigHome := func(t *testing.T) (string, string, string) {
		t.Helper()
		host := t.TempDir()
		// 构造宿主 <pigHome>/cache/cells/go/<digest>：目录名必须与
		// ready.json inputDigest 绑定（validCellEntryDir 口径），含
		// usage.json（lastUsed=1 过期，若被刷新会改写为近期 now）与带
		// 执行位且大小一致的 artifact。
		entry := filepath.Join(host, "cache", "cells", "go", "digest0123")
		if err := os.MkdirAll(entry, 0o755); err != nil {
			t.Fatal(err)
		}
		runnerBody := []byte("#!/bin/sh\n")
		runner := filepath.Join(entry, "runner")
		if err := os.WriteFile(runner, runnerBody, 0o755); err != nil {
			t.Fatal(err)
		}
		ready := fmt.Sprintf(`{"inputDigest":"digest0123","artifact":"runner","size":%d,"created":1,"language":"go"}`, len(runnerBody))
		if err := os.WriteFile(filepath.Join(entry, "ready.json"), []byte(ready), 0o644); err != nil {
			t.Fatal(err)
		}
		writeUsageJSON(t, entry, 1)
		// 宿主 agent 目录（不可信时不得被读/作扩展源）。
		if err := os.MkdirAll(filepath.Join(host, "agent"), 0o755); err != nil {
			t.Fatal(err)
		}
		return host, entry, runner
	}

	assertFailClosed := func(t *testing.T, cfgEnv []string, entry, runner, forbidden string) {
		t.Helper()
		policy := ReviewerAgentWithEnv(workdir, cfgEnv)
		t.Cleanup(func() { removePresetTempDirs(t, policy) })

		// usage.json 不得被改写：lastUsed 仍为夹具值 1。
		if got := readUsageLastUsed(t, entry); got != 1 {
			t.Fatalf("untrusted PIG_HOME usage.json must not be refreshed: lastUsed=%d", got)
		}
		// 不可信根的 artifact 不得进 Policy.Exec（含解析后形式）。
		for _, ex := range policy.Exec {
			cp := filepath.Clean(ex)
			if cp == filepath.Clean(runner) || cp == filepath.Clean(forbidden) {
				t.Fatalf("untrusted artifact %q must not enter Policy.Exec", cp)
			}
			if resolved, rerr := filepath.EvalSymlinks(cp); rerr == nil && filepath.Clean(resolved) == filepath.Clean(runner) {
				t.Fatalf("untrusted artifact %q (as %q) must not enter Policy.Exec", runner, cp)
			}
		}
		// reviewerAgentCellTargets 记录错误：ReviewerAgentCellTargetError 非 nil。
		if err := ReviewerAgentCellTargetError(); err == nil {
			t.Fatal("ReviewerAgentCellTargetError must be non-nil for untrusted PIG_HOME")
		}
		// PIG_HOME env 仍回钉到空私有根（<privateTmpDir>/pig-home）。
		ph := lookupEnvValue(policy.Env, "PIG_HOME")
		priv := lookupEnvValue(policy.Env, "TMPDIR")
		if ph == "" || priv == "" || !strings.HasPrefix(ph, priv) || !strings.HasSuffix(ph, "pig-home") {
			t.Fatalf("PIG_HOME env = %q, want private pig-home under TMPDIR=%q", ph, priv)
		}
	}

	t.Run("symlink_pig_home", func(t *testing.T) {
		host, entry, runner := makeHostPigHome(t)
		linkDir := filepath.Join(t.TempDir(), "pig-link")
		if err := os.Symlink(host, linkDir); err != nil {
			t.Fatal(err)
		}
		for _, env := range []string{"PIG_HOME", "PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR", "PIG_USE_PI_DIRS"} {
			unsetEnv(t, env)
		}
		assertFailClosed(t, []string{"PIG_HOME=" + linkDir}, entry, runner, linkDir)
	})

	t.Run("world_writable_pig_home", func(t *testing.T) {
		host, entry, runner := makeHostPigHome(t)
		if err := os.Chmod(host, 0o777); err != nil {
			t.Fatal(err)
		}
		for _, env := range []string{"PIG_HOME", "PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR", "PIG_USE_PI_DIRS"} {
			unsetEnv(t, env)
		}
		assertFailClosed(t, []string{"PIG_HOME=" + host}, entry, runner, host)
	})
}

// TestAppendCellCachePolicyNoLocksUnderSymlinkRoots 是 SF-1(c) 回归：
// 合规 PIG_HOME 下 cache/ext 与 cache/cells 为指向外部空目录的软链时，
// appendCellCachePolicy 绝不在外部目录内创建 .locks（旧实现在
// resolvePolicyPathWithin 拒软链前先 MkdirAll，跟随软链在外部目录凭空
// 建 .locks）。
func TestAppendCellCachePolicyNoLocksUnderSymlinkRoots(t *testing.T) {
	pigHome := t.TempDir()
	cacheRoot := filepath.Join(pigHome, "cache")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	extTarget := filepath.Join(external, "ext-target")
	cellsTarget := filepath.Join(external, "cells-target")
	for _, d := range []string{extTarget, cellsTarget} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// cache/ext 与 cache/cells 本体为指向外部空目录的软链。
	if err := os.Symlink(extTarget, filepath.Join(cacheRoot, "ext")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cellsTarget, filepath.Join(cacheRoot, "cells")); err != nil {
		t.Fatal(err)
	}

	var p Policy
	appendCellCachePolicy(&p, pigHome)

	for _, d := range []string{extTarget, cellsTarget} {
		if _, err := os.Lstat(filepath.Join(d, ".locks")); !os.IsNotExist(err) {
			t.Fatalf(".locks must not be created under symlinked root target %q (err=%v)", filepath.Join(d, ".locks"), err)
		}
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

	t.Run("root_symlinks_skipped", func(t *testing.T) {
		// 隔离 HOME=t.TempDir() 覆盖根级软链与软链子目录被跳过：
		// agyStateDir 根级符号链接（指向文件或目录）一律不进 ReadWrite/
		// OptionalReadWrite——放行软链会与 Exec 重合叠出 RWX 提权面。
		t.Setenv("HOME", t.TempDir())
		stateRoot := filepath.Join(homeDir(), ".gemini", "antigravity-cli")
		if err := os.MkdirAll(stateRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		// 根级软链→普通文件：绝不被枚举为根级状态文件。
		victimFile := filepath.Join(t.TempDir(), "victim.db")
		if err := os.WriteFile(victimFile, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		fileLink := filepath.Join(stateRoot, "linked.db")
		if err := os.Symlink(victimFile, fileLink); err != nil {
			t.Fatal(err)
		}
		// 根级软链→目录：DirEntry.IsDir 对软链为 false，必须按
		// Type()&ModeSymlink 显式排除（不得进根级文件枚举）。
		victimDir := t.TempDir()
		dirLink := filepath.Join(stateRoot, "linked-dir")
		if err := os.Symlink(victimDir, dirLink); err != nil {
			t.Fatal(err)
		}
		// 对照：根级普通状态文件照常进 ReadWrite/OptionalReadWrite。
		regular := filepath.Join(stateRoot, "regular.json")
		if err := os.WriteFile(regular, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}

		var p Policy
		appendAgyStateReadWrite(&p)
		all := append(append([]string(nil), p.ReadWrite...), p.OptionalReadWrite...)
		for _, bad := range []string{fileLink, dirLink, victimFile, victimDir} {
			for _, entry := range all {
				if entry == bad {
					t.Fatalf("symlink (or its target) %q must not enter ReadWrite/OptionalReadWrite", bad)
				}
				if resolved, rerr := filepath.EvalSymlinks(entry); rerr == nil && resolved == bad {
					t.Fatalf("symlink target %q reached policy list via EvalSymlinks(%q)", bad, entry)
				}
			}
		}
		normReg, err := NormalizePath(regular)
		if err != nil {
			t.Fatalf("NormalizePath: %v", err)
		}
		foundRW, foundORW := false, false
		for _, entry := range p.ReadWrite {
			if entry == normReg || entry == regular {
				foundRW = true
			}
		}
		for _, entry := range p.OptionalReadWrite {
			if entry == normReg || entry == regular {
				foundORW = true
			}
		}
		if !foundRW || !foundORW {
			t.Fatalf("regular root state file %q must enter ReadWrite(%v) and OptionalReadWrite(%v)", regular, foundRW, foundORW)
		}
	})
}

// ── export_test 段：供外部测试包 sandbox_test 使用 ────────────────────────
// sandbox_test.go 属 package sandbox_test，无法访问包内未导出符号。本段
// 仅存在于测试二进制中（非生产 API）：resolveEntryAgentPaths 的 hostHome
// 参数取代 trustedHomeDir()，使「隔离 HOME 下宿主受信安装根/收录面」相关
// 断言能用 t.TempDir() 夹具确定性实跑，摆脱宿主真实 claude/agy 安装状态。
var ResolveEntryAgentPathsForTest = resolveEntryAgentPaths

// ---------------------------------------------------------------------------
// N7：受信自定义目录判定不得误丢合法值——解析后的规范化路径与 cfg 原始
// 字符串相等判定会把通过校验的相对路径/冗余分隔/点段写法丢弃；明确 PI
// 自定义入口（PIG_USE_PI_DIRS=1 + PI_CODING_AGENT_DIR）同样必须保留。
// 断言对象：ReviewerAgentWithEnv 产出的 Policy.Env 有效 AgentDir 键。

// trustedCustomDirFixture 建一个 0700 属主=euid 的可信自定义目录（通过
// validateTrustedPigDir），返回其规范化绝对路径与另一种拼写形式。
func trustedCustomDirFixture(t *testing.T, join ...string) (canon, spelled string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, j := range join {
		dir = filepath.Join(dir, j)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	canon = filepath.Clean(dir)
	return canon, dir
}

// unsetAgentDirEnv 隔离全部 AgentDir 相关宿主变量。
func unsetAgentDirEnv(t *testing.T) {
	t.Helper()
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	unsetEnv(t, "PI_CODING_AGENT_DIR")
	unsetEnv(t, "PIG_USE_PI_DIRS")
	t.Setenv("PIG_HOME", t.TempDir())
}

func TestTrustedCustomAgentDirSpellingsPreserved(t *testing.T) {
	unsetAgentDirEnv(t)
	workdir := t.TempDir()

	cases := []struct {
		name    string
		cfgEnv  func(canon, spelled string) []string
		wantDir func(canon, spelled string) string
	}{
		{
			name: "relative_path",
			// 相对路径经 absCleanPath 规范化后必须保留，不得因
			// 「规范化值 != cfg 原始串」被丢弃。
			cfgEnv: func(canon, spelled string) []string {
				return []string{"PIG_CODING_AGENT_DIR=" + spelled}
			},
			wantDir: func(canon, spelled string) string { return canon },
		},
		{
			name: "redundant_separator",
			cfgEnv: func(canon, spelled string) []string {
				return []string{"PIG_CODING_AGENT_DIR=" + spelled}
			},
			wantDir: func(canon, spelled string) string { return canon },
		},
		{
			name: "dot_segments",
			cfgEnv: func(canon, spelled string) []string {
				return []string{"PIG_CODING_AGENT_DIR=" + spelled}
			},
			wantDir: func(canon, spelled string) string { return canon },
		},
		{
			name: "pi_explicit_custom_entry",
			// 明确 PI 自定义入口：PIG_USE_PI_DIRS=1 + PI_CODING_AGENT_DIR
			// 须保留为有效目录，不再被 PIG 预值遮蔽。
			cfgEnv: func(canon, spelled string) []string {
				return []string{"PIG_USE_PI_DIRS=1", "PI_CODING_AGENT_DIR=" + canon}
			},
			wantDir: func(canon, spelled string) string { return canon },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unsetAgentDirEnv(t)
			canon, spelled := trustedCustomDirFixture(t, "custom-agent")
			switch tc.name {
			case "relative_path":
				// 构造相对 cwd 的拼写：把 t.TempDir 子目录指为相对路径。
				parent := filepath.Dir(canon)
				if rel, err := filepath.Rel(mustGetwd(t), canon); err == nil && !filepath.IsAbs(rel) {
					spelled = rel
				} else {
					// 兜底：./<base> 相对 parent 的 cwd 写法不可控，改用 ./ 前缀。
					spelled = "." + string(filepath.Separator) + filepath.Join(parent, filepath.Base(canon))
				}
			case "redundant_separator":
				spelled = filepath.Dir(canon) + string(filepath.Separator) + "." + string(filepath.Separator) + filepath.Base(canon)
			case "dot_segments":
				spelled = canon + string(filepath.Separator) + "sub" + string(filepath.Separator) + ".."
			}
			p := ReviewerAgentWithEnv(workdir, tc.cfgEnv(canon, spelled))
			t.Cleanup(func() { removePresetTempDirs(t, p) })
			want := tc.wantDir(canon, spelled)
			gotPig := lookupEnvValue(p.Env, "PIG_CODING_AGENT_DIR")
			gotPi := lookupEnvValue(p.Env, "PI_CODING_AGENT_DIR")
			if gotPig != want {
				t.Fatalf("PIG_CODING_AGENT_DIR = %q, want trusted custom %q (spelling %q)", gotPig, want, spelled)
			}
			if gotPi != want {
				t.Fatalf("PI_CODING_AGENT_DIR = %q, want %q (spelling %q)", gotPi, want, spelled)
			}
		})
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	return wd
}

// TestReviewerAgentCellTargetErrorNodeRuntimeGate 覆盖 oracle FM-2C：
// reviewer-agent cell 目标集 fail-closed 必须按 language=go 且可执行产物
// 过滤——宿主 cache 仅有 node-runtime-*（language:"node"）产物而 Go 扩展
// 存在时，total 不得被 node cell 充数假通过，ReviewerAgentCellTargetError
// 必须返回错误（冷缓存下 Go cell 缺失应 fail-closed）。
func TestReviewerAgentCellTargetErrorNodeRuntimeGate(t *testing.T) {
	// 隔离宿主 pig home：构造仅含 Go 扩展源码 + node-runtime cell 的假 PIG_HOME。
	fakePigHome := t.TempDir()
	agentDir := filepath.Join(fakePigHome, "agent")
	goExtDir := filepath.Join(agentDir, "extensions", "fake-go-ext")
	if err := os.MkdirAll(goExtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goExtDir, "go.mod"), []byte("module fake\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Join(fakePigHome, "cache")
	// 仅 node-runtime 产物（language:"node"），无任何 Go cell。
	writeFakeCellEntry(t, filepath.Join(cacheRoot, "ext", "node-runtime-deadbeef01"), "deadbeef01", "bin", "node")

	t.Setenv("PIG_HOME", fakePigHome)
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	unsetEnv(t, "PI_CODING_AGENT_DIR")
	unsetEnv(t, "PIG_USE_PI_DIRS")

	workdir := t.TempDir()
	policy := ReviewerAgentWithEnv(workdir, nil)
	// 手动清理 pi-reviewer-* 私有临时目录（内部包无 cleanupReviewerTempDirs）。
	t.Cleanup(func() {
		for _, kv := range policy.Env {
			if k, v, ok := strings.Cut(kv, "="); ok && k == "TMPDIR" && strings.Contains(v, "pi-reviewer-") {
				_ = os.RemoveAll(v)
			}
			if k, v, ok := strings.Cut(kv, "="); ok && k == "PI_SANDBOX_GO_EXEC_DIR" && strings.Contains(v, "pi-reviewer-exec-") {
				_ = os.RemoveAll(v)
			}
		}
	})

	// hasGoExt 必须为 true（Go 扩展存在）；total 必须是 0（node cell 不计入）。
	hasGoExt, total, _ := ReviewerAgentCellTargets()
	if !hasGoExt {
		t.Fatalf("hasGoExt = false, want true (Go extension present)")
	}
	if total != 0 {
		t.Fatalf("total = %d, want 0: node-runtime cell must not count toward Go target set", total)
	}
	// fail-closed：有 Go 扩展但无有效 Go 产物 → 返回错误拒绝启动。
	if err := ReviewerAgentCellTargetError(); err == nil {
		t.Fatalf("ReviewerAgentCellTargetError = nil, want error: node-runtime cell must not satisfy Go cell completeness gate")
	}
}

// TestReviewerAgentCellTargetSymlinkCellsLangGate 覆盖 oracle N4：cell 目标
// 枚举必须单一化——countValidExecutableGoArtifacts 的 total 不得再直接
// ReadDir(cells/go) 跟随语言根软链而投影/Exec 侧跳过它。构造宿主
// cells/go 为指向「含有效可执行 Go cell 的外部目录」的符号链接，并混入
// .tombstone-* 合法墓碑目录：旧实现 total>0 而投影为空、missing 为空，
// 完整性门禁零投影假放行；修复后 total 同样跳过该软链，
// ReviewerAgentCellTargetError 返回明确错误拒绝启动。
func TestReviewerAgentCellTargetSymlinkCellsLangGate(t *testing.T) {
	fakePigHome := t.TempDir()
	agentDir := filepath.Join(fakePigHome, "agent")
	goExtDir := filepath.Join(agentDir, "extensions", "fake-go-ext")
	if err := os.MkdirAll(goExtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goExtDir, "go.mod"), []byte("module fake\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 宿主 cache/cells/go 是指向「含有效 Go cell 的外部目录」的符号链接。
	hostCells := filepath.Join(fakePigHome, "cache", "cells")
	if err := os.MkdirAll(hostCells, 0o755); err != nil {
		t.Fatal(err)
	}
	externalCellsGo := filepath.Join(t.TempDir(), "cells-go-target")
	writeFakeCellEntry(t, filepath.Join(externalCellsGo, "deadbeef0001"), "deadbeef0001", "runner", "")
	// .tombstone-* 合法墓碑目录（旧实现 projection 接纳但 total 跳过）。
	writeFakeCellEntry(t, filepath.Join(externalCellsGo, ".tombstone-aaaa0001"), "aaaa0001", "runner", "")
	if err := os.Symlink(externalCellsGo, filepath.Join(hostCells, "go")); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PIG_HOME", fakePigHome)
	unsetEnv(t, "PIG_CODING_AGENT_DIR")
	unsetEnv(t, "PI_CODING_AGENT_DIR")
	unsetEnv(t, "PIG_USE_PI_DIRS")

	workdir := t.TempDir()
	policy := ReviewerAgentWithEnv(workdir, nil)
	t.Cleanup(func() {
		for _, kv := range policy.Env {
			if k, v, ok := strings.Cut(kv, "="); ok && k == "TMPDIR" && strings.Contains(v, "pi-reviewer-") {
				_ = os.RemoveAll(v)
			}
			if k, v, ok := strings.Cut(kv, "="); ok && k == "PI_SANDBOX_GO_EXEC_DIR" && strings.Contains(v, "pi-reviewer-exec-") {
				_ = os.RemoveAll(v)
			}
		}
	})

	hasGoExt, total, missing := ReviewerAgentCellTargets()
	if !hasGoExt {
		t.Fatalf("hasGoExt = false, want true (Go extension present)")
	}
	// 软链语言根不得被 total 单独计入（枚举口径与投影/Exec 一致）。
	if total != 0 {
		t.Fatalf("total = %d, want 0: cells/go symlink root must not be counted toward Go target set", total)
	}
	// 完整性校验必须拒绝放行（零投影假通过）并给出明确错误。
	if err := ReviewerAgentCellTargetError(); err == nil {
		t.Fatalf("ReviewerAgentCellTargetError = nil, want error: cells/go symlink projection must not be allowed to satisfy completeness gate")
	} else if !strings.Contains(err.Error(), "no valid Go extension cell artifact") && len(missing) == 0 {
		t.Fatalf("ReviewerAgentCellTargetError = %v, want fail-closed error naming missing/empty projection", err)
	}

	// cleanupReviewerAgentPolicy 是 ReviewerAgentWithEnv 产出策略的预设定
	// 时目录清理辅助（本文件多个 cell 目标用例共用）。
	cleanupPolicy := func(p Policy) {
		t.Helper()
		for _, kv := range p.Env {
			if k, v, ok := strings.Cut(kv, "="); ok && k == "TMPDIR" && strings.Contains(v, "pi-reviewer-") {
				_ = os.RemoveAll(v)
			}
			if k, v, ok := strings.Cut(kv, "="); ok && k == "PI_SANDBOX_GO_EXEC_DIR" && strings.Contains(v, "pi-reviewer-exec-") {
				_ = os.RemoveAll(v)
			}
		}
	}

	t.Run("ext_root_symlink_to_external_cells_skipped", func(t *testing.T) {
		// cache/ext 根本体为指向外部目录（内含有效可执行 Go cell）的软链
		// 时：isRealDirNoSymlink 整体拒枚举——total==0、
		// ReviewerAgentCellTargetError 非 nil、外部 usage.json 零改写、
		// 外部产物绝不进 Policy.Exec。改回跟随软链的枚举本用例即红。
		pigHome := t.TempDir()
		extLinkTarget := t.TempDir()
		extCell := filepath.Join(extLinkTarget, "ext-entry-aaaa0001")
		writeFakeCellEntry(t, extCell, "aaaa0001", "bin", "go")
		writeUsageJSON(t, extCell, 1700000000)
		if err := os.MkdirAll(filepath.Join(pigHome, "cache"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(extLinkTarget, filepath.Join(pigHome, "cache", "ext")); err != nil {
			t.Fatal(err)
		}
		goExt := filepath.Join(pigHome, "agent", "extensions", "fake-go-ext")
		if err := os.MkdirAll(goExt, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(goExt, "go.mod"), []byte("module fake\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PIG_HOME", pigHome)
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_USE_PI_DIRS")

		p := ReviewerAgentWithEnv(t.TempDir(), nil)
		t.Cleanup(func() { cleanupPolicy(p) })

		_, total, _ := ReviewerAgentCellTargets()
		if total != 0 {
			t.Fatalf("total = %d, want 0: ext symlink root must not be enumerated", total)
		}
		if err := ReviewerAgentCellTargetError(); err == nil {
			t.Fatalf("ReviewerAgentCellTargetError = nil, want fail-closed error (ext symlink -> external Go cells)")
		}
		if got := readUsageLastUsed(t, extCell); got != 1700000000 {
			t.Fatalf("external usage.json lastUsed = %d, want 1700000000 unchanged (refresh must not follow ext symlink)", got)
		}
		externalBin := filepath.Join(extCell, "bin")
		for _, ex := range p.Exec {
			if ex == externalBin {
				t.Fatalf("external ext artifact %q must not enter Exec whitelist", externalBin)
			}
			if resolved, rerr := filepath.EvalSymlinks(ex); rerr == nil && resolved == externalBin {
				t.Fatalf("external ext artifact %q reached Exec via symlink resolution", externalBin)
			}
		}
	})

	t.Run("cells_go_entry_symlink_and_hidden_skipped", func(t *testing.T) {
		// 真实 cells/go 根下：条目级软链与 .hidden-* 目录经
		// cellEntryHiddenOrSymlink 排除——不计入 total、不入 Exec；
		// .tombstone-* 合法墓碑目录正常计入。改坏该谓词本用例即红。
		pigHome := t.TempDir()
		hostCellsGo := filepath.Join(pigHome, "cache", "cells", "go")
		if err := os.MkdirAll(hostCellsGo, 0o755); err != nil {
			t.Fatal(err)
		}
		externalCell := filepath.Join(t.TempDir(), "external-entry-bbbb0002")
		writeFakeCellEntry(t, externalCell, "bbbb0002", "runner", "")
		if err := os.Symlink(externalCell, filepath.Join(hostCellsGo, "bbbb0002")); err != nil {
			t.Fatal(err)
		}
		writeFakeCellEntry(t, filepath.Join(hostCellsGo, ".hidden-cccc0003"), "cccc0003", "runner", "")
		tombstone := filepath.Join(hostCellsGo, ".tombstone-dddd0004")
		writeFakeCellEntry(t, tombstone, "dddd0004", "runner", "")
		goExt := filepath.Join(pigHome, "agent", "extensions", "fake-go-ext")
		if err := os.MkdirAll(goExt, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(goExt, "go.mod"), []byte("module fake\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PIG_HOME", pigHome)
		unsetEnv(t, "PIG_CODING_AGENT_DIR")
		unsetEnv(t, "PI_CODING_AGENT_DIR")
		unsetEnv(t, "PIG_USE_PI_DIRS")

		p := ReviewerAgentWithEnv(t.TempDir(), nil)
		t.Cleanup(func() { cleanupPolicy(p) })

		_, total, _ := ReviewerAgentCellTargets()
		if total != 1 {
			t.Fatalf("total = %d, want 1: only .tombstone-* entry counts; entry symlink and .hidden-* must be skipped", total)
		}
		execSet := make(map[string]struct{}, len(p.Exec))
		for _, ex := range p.Exec {
			execSet[filepath.Clean(ex)] = struct{}{}
			if resolved, rerr := filepath.EvalSymlinks(ex); rerr == nil && resolved != "" {
				execSet[filepath.Clean(resolved)] = struct{}{}
			}
		}
		for _, bad := range []string{
			filepath.Join(externalCell, "runner"),
			filepath.Join(hostCellsGo, ".hidden-cccc0003", "runner"),
		} {
			if _, hit := execSet[filepath.Clean(bad)]; hit {
				t.Fatalf("excluded cell artifact %q must not enter Exec whitelist", bad)
			}
		}
		if _, hit := execSet[filepath.Join(tombstone, "runner")]; !hit {
			t.Fatalf("tombstone cell artifact %s/runner missing from Exec (tombstone entry must count normally)", tombstone)
		}
	})

	t.Run("verify_projected_missing_when_projection_absent", func(t *testing.T) {
		// verifyProjectedGoCellTargets 负例：宿主存在有效可执行 Go 产物，
		// 但私有投影根下缺失对应软链投影时，必须返回非空 missing 点名——
		// 枚举/投影口径错配绝不假放行。
		hostCache := filepath.Join(t.TempDir(), "cache")
		writeFakeCellEntry(t, filepath.Join(hostCache, "ext", "fake-ext-eeee0005"), "eeee0005", "bin", "go")
		privCacheRoot := filepath.Join(t.TempDir(), "priv-cache")
		if err := os.MkdirAll(privCacheRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		missing := verifyProjectedGoCellTargets(hostCache, privCacheRoot)
		if len(missing) == 0 {
			t.Fatalf("verifyProjectedGoCellTargets = %v, want non-empty missing for unprojected host Go artifact", missing)
		}
		if !strings.Contains(missing[0], "ext/fake-ext-eeee0005") {
			t.Fatalf("missing entry %q must name the unprojected ext entry", missing[0])
		}
	})

}

// ---------------------------------------------------------------------------
// SF-1 回归（oracle9）：validateTrustedPigDir 祖先链逐级校验 +
// appendCellCachePolicy 以 EvalSymlinks 规范真实路径入 Policy.Exec。
//
// 覆盖：PIG_HOME 祖先软链（末级存在 / 末级不存在）、非粘滞 0777 父目录、
// 未解析软链 artifactPath 不得原样进 Exec（必须收敛为 EvalSymlinks 真实路径，
// 且落在 resolvedCacheRoot 内）。

// makeSF1HostPigHome 复刻 O8 SF-1 探针夹具：<host>/cache/cells/go/<digest>/
// 内含 runner/ready.json/usage.json（lastUsed=1），以及空 cache/ext 与
// agent/extensions/fake/go.mod。返回 (host, entry, runner)。
func makeSF1HostPigHome(t *testing.T, host string) (entry, runner string) {
	t.Helper()
	entry = filepath.Join(host, "cache", "cells", "go", "digest0123")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	runnerBody := []byte("#!/bin/sh\n")
	runner = filepath.Join(entry, "runner")
	if err := os.WriteFile(runner, runnerBody, 0o755); err != nil {
		t.Fatal(err)
	}
	ready := `{"inputDigest":"digest0123","artifact":"runner","size":` +
		strconv.Itoa(len(runnerBody)) + `,"created":1,"language":"go"}`
	if err := os.WriteFile(filepath.Join(entry, "ready.json"), []byte(ready), 0o644); err != nil {
		t.Fatal(err)
	}
	writeUsageJSON(t, entry, 1)
	if err := os.MkdirAll(filepath.Join(host, "cache", "ext"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(host, "agent", "extensions", "fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "agent", "extensions", "fake", "go.mod"), []byte("module fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return entry, runner
}

// assertPigHomeFailClosedSF1 断言不可信 PIG_HOME 下 ReviewerAgentWithEnv
// 未越界写 usage.json、未在外部建 .locks、未把未解析 runner 入 Exec，
// 且 ReviewerAgentCellTargetError 非 nil（pig 入口启动前 fail-closed）。
func assertPigHomeFailClosedSF1(t *testing.T, cfgEnv []string, entry, runner, pigHomePath string) {
	t.Helper()
	workdir := t.TempDir()
	policy := ReviewerAgentWithEnv(workdir, cfgEnv)
	t.Cleanup(func() { removePresetTempDirs(t, policy) })

	if got := readUsageLastUsed(t, entry); got != 1 {
		t.Fatalf("untrusted PIG_HOME usage.json must not be refreshed: lastUsed=%d", got)
	}
	// 不得在外部 cache/ext 下创建 .locks。
	if _, err := os.Lstat(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(entry))), "ext", ".locks")); !os.IsNotExist(err) {
		t.Fatalf(".locks must not be created under untrusted ext root (err=%v)", err)
	}
	for _, ex := range policy.Exec {
		cp := filepath.Clean(ex)
		if cp == filepath.Clean(runner) || cp == filepath.Clean(pigHomePath) {
			t.Fatalf("untrusted artifact %q must not enter Policy.Exec", cp)
		}
		if resolved, rerr := filepath.EvalSymlinks(cp); rerr == nil && filepath.Clean(resolved) == filepath.Clean(runner) {
			t.Fatalf("untrusted artifact %q (resolved as %q) must not enter Policy.Exec", runner, cp)
		}
	}
	if err := ReviewerAgentCellTargetError(); err == nil {
		t.Fatal("ReviewerAgentCellTargetError must be non-nil for untrusted PIG_HOME")
	}
}

// TestValidateTrustedPigDirAncestorChain 是 SF-1 的直接单元回归：
// 祖先软链、非粘滞 0777 父目录、末级不存在但祖先不安全的场景必须拒；
// 正常 t.TempDir()（/tmp 01777 root 粘滞根下）与属主=euid 的目录必须放行。
func TestValidateTrustedPigDirAncestorChain(t *testing.T) {
	euid := os.Geteuid()

	mkHost := func(t *testing.T) (extParent, host string) {
		t.Helper()
		extParent = t.TempDir()
		host = filepath.Join(extParent, "host")
		if err := os.MkdirAll(host, 0o755); err != nil {
			t.Fatal(err)
		}
		return extParent, host
	}

	t.Run("ancestor symlink existing leaf", func(t *testing.T) {
		extParent, host := mkHost(t)
		link := filepath.Join(t.TempDir(), "pig-link")
		if err := os.Symlink(extParent, link); err != nil {
			t.Fatal(err)
		}
		if validateTrustedPigDir(filepath.Join(link, "host"), "PIG_HOME") {
			t.Fatalf("ancestor symlink %q must be rejected (host=%q)", link, host)
		}
	})

	t.Run("ancestor symlink missing leaf", func(t *testing.T) {
		extParent, _ := mkHost(t)
		link := filepath.Join(t.TempDir(), "pig-link")
		if err := os.Symlink(extParent, link); err != nil {
			t.Fatal(err)
		}
		missing := filepath.Join(link, "new-sub")
		if validateTrustedPigDir(missing, "PIG_HOME") {
			t.Fatalf("missing leaf under ancestor symlink %q must be rejected", missing)
		}
	})

	t.Run("non-sticky 0777 ancestor", func(t *testing.T) {
		worldParent := filepath.Join(t.TempDir(), "world-parent")
		if err := os.MkdirAll(worldParent, 0o777); err != nil {
			t.Fatal(err)
		}
		// 显式去掉 sticky（TempDir 默认 0700，这里造 0777 非粘滞）。
		if err := os.Chmod(worldParent, 0o777); err != nil {
			t.Fatal(err)
		}
		host := filepath.Join(worldParent, "host")
		if err := os.MkdirAll(host, 0o755); err != nil {
			t.Fatal(err)
		}
		if validateTrustedPigDir(host, "PIG_HOME") {
			t.Fatalf("host under non-sticky 0777 ancestor %q must be rejected", worldParent)
		}
	})

	t.Run("trusted existing dir accepted", func(t *testing.T) {
		if euid == 0 {
			// /tmp 下 t.TempDir()（root 01777 粘滞根 + euid 拥有的目录）应放行。
			dir := t.TempDir()
			if !validateTrustedPigDir(dir, "PIG_HOME") {
				t.Fatalf("legit TempDir %q must be accepted", dir)
			}
		}
	})

	t.Run("missing leaf under trusted ancestors accepted", func(t *testing.T) {
		// 末级不存在但整条已存在祖先链可信：交由后续 MkdirAll 报真实错误，
		// 校验本身放行（与旧 ENOENT 语义一致但只放行可信祖先链）。
		parent := t.TempDir()
		missing := filepath.Join(parent, "new-pig-home")
		if !validateTrustedPigDir(missing, "PIG_HOME") {
			t.Fatalf("missing leaf under trusted ancestor %q must be accepted", missing)
		}
	})

	t.Run("sticky 01777 leaf rejected", func(t *testing.T) {
		// 末级 sticky 回归：/tmp、/var/tmp 这类 01777 顶层粘滞目录不得被
		// 当作可信 PIG_HOME——sticky 豁免只适用于真正的祖先级临时根，
		// 末级本体 0o022 可写一律拒（oracle 应修 #1）。
		if euid == 0 {
			for _, dir := range []string{"/tmp", "/var/tmp"} {
				if st, err := os.Lstat(dir); err == nil && st.IsDir() &&
					st.Mode()&os.ModeSticky != 0 && st.Mode().Perm()&0o022 != 0 {
					if validateTrustedPigDir(dir, "PIG_HOME") {
						t.Fatalf("sticky world-writable leaf %q must be rejected as PIG_HOME", dir)
					}
				}
			}
		}
		// 本地构造等价夹具（非 root/他属主场景同样覆盖末级 sticky 拒绝）。
		stickyLeaf := filepath.Join(t.TempDir(), "sticky-home")
		if err := os.MkdirAll(stickyLeaf, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(stickyLeaf, 0o1777); err != nil {
			t.Fatal(err)
		}
		if validateTrustedPigDir(stickyLeaf, "PIG_HOME") {
			t.Fatalf("sticky world-writable leaf %q must be rejected as PIG_HOME", stickyLeaf)
		}
	})

	// 顶层 uid==0 软链目标链（oracle 应修 #4 红绿对照）：经
	// validateTrustedPigDirWithRoot 的 sysRoot 注入 seam 构造假根
	// sysRoot/top -> sysRoot/<target>，Linux 上确定性覆盖「祖先链顶层
	// 软链不跳回 / 提前结束、跟随目标继续逐级校验」的分支。
	// euid 必须匹配顶层软链属主判定（仅 root 跑时有意义）。
	if euid == 0 {
		t.Run("top symlink to compliant dir accepted", func(t *testing.T) {
			sysRoot := t.TempDir()
			world := filepath.Join(sysRoot, "world")
			if err := os.MkdirAll(world, 0o755); err != nil {
				t.Fatal(err)
			}
			top := filepath.Join(sysRoot, "top")
			if err := os.Symlink(world, top); err != nil {
				t.Fatal(err)
			}
			if !validateTrustedPigDirWithRoot(filepath.Join(top, "host"), "PIG_HOME", sysRoot) {
				t.Fatalf("top-level symlink to compliant dir under sysRoot must be accepted")
			}
		})

		t.Run("top symlink to non-sticky 0777 rejected", func(t *testing.T) {
			sysRoot := t.TempDir()
			world := filepath.Join(sysRoot, "world")
			if err := os.MkdirAll(world, 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(world, 0o777); err != nil {
				t.Fatal(err)
			}
			top := filepath.Join(sysRoot, "top")
			if err := os.Symlink(world, top); err != nil {
				t.Fatal(err)
			}
			if validateTrustedPigDirWithRoot(filepath.Join(top, "host"), "PIG_HOME", sysRoot) {
				t.Fatalf("top-level symlink to non-sticky 0777 dir must be rejected")
			}
		})

		t.Run("top symlink to nested second-level link rejected", func(t *testing.T) {
			// 顶层软链目标链内含二级（非顶层）嵌套软链：跟随到目标后
			// 中间级软链仍属拒绝项（sysRoot 直属豁免不适用于更深层）。
			sysRoot := t.TempDir()
			realWorld := filepath.Join(sysRoot, "real-world")
			if err := os.MkdirAll(realWorld, 0o755); err != nil {
				t.Fatal(err)
			}
			real2 := filepath.Join(sysRoot, "real2")
			if err := os.MkdirAll(real2, 0o755); err != nil {
				t.Fatal(err)
			}
			// <sysRoot>/real-world/sub 是指向 real2 的第二级软链（其父级
			// realWorld 不直属 sysRoot，不满足顶层豁免）。
			secondLink := filepath.Join(realWorld, "sub")
			if err := os.Symlink(real2, secondLink); err != nil {
				t.Fatal(err)
			}
			top := filepath.Join(sysRoot, "top")
			if err := os.Symlink(secondLink, top); err != nil {
				t.Fatal(err)
			}
			if validateTrustedPigDirWithRoot(filepath.Join(top, "host"), "PIG_HOME", sysRoot) {
				t.Fatalf("top-level symlink chain containing second-level symlink must be rejected")
			}
		})

		t.Run("dangling top symlink rejected", func(t *testing.T) {
			sysRoot := t.TempDir()
			top := filepath.Join(sysRoot, "top")
			if err := os.Symlink(filepath.Join(sysRoot, "no-such-target"), top); err != nil {
				t.Fatal(err)
			}
			if validateTrustedPigDirWithRoot(filepath.Join(top, "host"), "PIG_HOME", sysRoot) {
				t.Fatalf("dangling top-level symlink must be rejected")
			}
		})
	}

	t.Run("untrusted default agent dir falls back to private", func(t *testing.T) {
		// 默认 <pigHome>/agent 为软链时 sel.sourceTrusted=false：配置
		// 复制源回退空私有 pig-agent，宿主 settings.json 不得被拷入
		//（闭合默认层校验，oracle 应修 #2）。
		pigHome := t.TempDir()
		realAgent := t.TempDir()
		if err := os.WriteFile(filepath.Join(realAgent, "settings.json"), []byte(`{"extensions":["evil"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realAgent, filepath.Join(pigHome, "agent")); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(t.TempDir(), "pig-agent")
		sel := resolveAgentDirSelection(pigHome, nil)
		if sel.sourceTrusted {
			t.Fatalf("default <pigHome>/agent symlink must be untrusted")
		}
		src := sel.sourceDir
		if sel.sourceTrusted == false {
			src = dst // 与 ReviewerAgentWithEnv 同口径回退私有
		}
		preparePrivatePigAgentDirFromAgent(dst, src)
		if _, err := os.Lstat(filepath.Join(dst, "settings.json")); !os.IsNotExist(err) {
			t.Fatalf("host settings.json must not be copied from untrusted default agent dir")
		}
	})

	t.Run("world-writable default agent dir falls back to private", func(t *testing.T) {
		pigHome := t.TempDir()
		agentDir := filepath.Join(pigHome, "agent")
		if err := os.MkdirAll(agentDir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(agentDir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte(`{"extensions":["evil"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		sel := resolveAgentDirSelection(pigHome, nil)
		if sel.sourceTrusted {
			t.Fatalf("world-writable default <pigHome>/agent must be untrusted")
		}
	})

	t.Run("symlink config file not copied", func(t *testing.T) {
		// 单配置文件为软链时不得越界拷入私有 pig-agent
		//（preparePrivatePigAgentDirFromAgent 的 Lstat 口径，oracle 应修 #2）。
		src := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside-settings.json")
		if err := os.WriteFile(outside, []byte(`{"extensions":["evil"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(src, "settings.json")); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(t.TempDir(), "pig-agent")
		preparePrivatePigAgentDirFromAgent(dst, src)
		if _, err := os.Lstat(filepath.Join(dst, "settings.json")); !os.IsNotExist(err) {
			t.Fatalf("symlinked settings.json must not be copied into private pig-agent")
		}
	})

	t.Run("symlink extensions dir not projected", func(t *testing.T) {
		// <agent>/extensions 本体为软链时不得被认作宿主扩展目录注入
		// settings.json（Lstat 口径）。
		src := t.TempDir()
		outsideExt := t.TempDir()
		if err := os.Symlink(outsideExt, filepath.Join(src, "extensions")); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(t.TempDir(), "pig-agent")
		preparePrivatePigAgentDirFromAgent(dst, src)
		if _, err := os.Lstat(filepath.Join(dst, "settings.json")); !os.IsNotExist(err) {
			t.Fatalf("symlinked extensions dir must not trigger settings.json injection")
		}
	})
}

// TestReviewerAgentPigHomeAncestorSymlinkFailClosed 端到端断言：PIG_HOME
// 经祖先软链指向外部目录时 ReviewerAgentWithEnv 不越界写 usage.json、
// 不建外部 .locks、不把 runner 入 Exec，且 CellTargetError 非 nil。
func TestReviewerAgentPigHomeAncestorSymlinkFailClosed(t *testing.T) {
	workdir := t.TempDir()
	extParent := t.TempDir()
	host := filepath.Join(extParent, "host")
	if err := os.MkdirAll(host, 0o755); err != nil {
		t.Fatal(err)
	}
	entry, runner := makeSF1HostPigHome(t, host)
	link := filepath.Join(t.TempDir(), "pig-link")
	if err := os.Symlink(extParent, link); err != nil {
		t.Fatal(err)
	}
	pigHomeAlias := filepath.Join(link, "host")

	for _, env := range []string{"PIG_HOME", "PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR", "PIG_USE_PI_DIRS"} {
		unsetEnv(t, env)
	}
	assertPigHomeFailClosedSF1(t, []string{"PIG_HOME=" + pigHomeAlias}, entry, runner, pigHomeAlias)
	_ = workdir
}

// TestAppendCellCachePolicyResolvesArtifactRealPath 断言：appendCellCachePolicy
// 对经祖先软链别名传入的 PIG_HOME 严格落真实路径——合规产物必须以
// EvalSymlinks 后的规范真实路径入 Policy.Exec 且绝不写未解析别名；
// 0777 全局可写产物与指向 cacheRoot 外的软链产物一律不入列。
// 旧版仅写未解析路径的实现在①②断言下必 FAIL（红绿对照）。
func TestAppendCellCachePolicyResolvesArtifactRealPath(t *testing.T) {
	realParent := t.TempDir()
	realPigHome := filepath.Join(realParent, "host")
	if err := os.MkdirAll(realPigHome, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Join(realPigHome, "cache")
	// 祖先软链别名：parentLink -> realParent，pigHomeAlias 经软链拼写。
	parentLink := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(realParent, parentLink); err != nil {
		t.Fatal(err)
	}
	pigHomeAlias := filepath.Join(parentLink, "host")

	execContains := func(p *Policy, path string) bool {
		want := filepath.Clean(path)
		for _, ex := range p.Exec {
			if filepath.Clean(ex) == want {
				return true
			}
		}
		return false
	}

	// ① 合规 0755 产物：必须以 EvalSymlinks 解析后的真实路径入列，
	// 且未解析的别名拼写绝不入列。
	entryGood := filepath.Join(cacheRoot, "cells", "go", "digest0001")
	writeFakeCellEntry(t, entryGood, "digest0001", "runner", "go")
	realRunnerGood := filepath.Join(entryGood, "runner")
	resolvedGood, err := filepath.EvalSymlinks(realRunnerGood)
	if err != nil {
		t.Fatalf("EvalSymlinks good runner: %v", err)
	}
	aliasRunnerGood := filepath.Join(pigHomeAlias, "cache", "cells", "go", "digest0001", "runner")

	p := &Policy{}
	appendCellCachePolicy(p, pigHomeAlias)
	if !execContains(p, resolvedGood) {
		t.Fatalf("resolved artifact %q must enter Policy.Exec, got %v", resolvedGood, p.Exec)
	}
	if execContains(p, aliasRunnerGood) {
		t.Fatalf("unresolved alias %q must not enter Policy.Exec", aliasRunnerGood)
	}

	// ② 0777 全局可写产物：绝不放行执行（权限收紧后不入列）。
	entryWorld := filepath.Join(cacheRoot, "cells", "go", "digest0002")
	writeFakeCellEntry(t, entryWorld, "digest0002", "runner", "go")
	worldRunner := filepath.Join(entryWorld, "runner")
	if err := os.Chmod(worldRunner, 0o777); err != nil {
		t.Fatal(err)
	}
	resolvedWorld, err := filepath.EvalSymlinks(worldRunner)
	if err != nil {
		t.Fatalf("EvalSymlinks world runner: %v", err)
	}
	p2 := &Policy{}
	appendCellCachePolicy(p2, pigHomeAlias)
	if execContains(p2, resolvedWorld) {
		t.Fatalf("world-writable artifact %q must not enter Policy.Exec", resolvedWorld)
	}

	// ③ 指向 cacheRoot 外的软链产物：软链与外部目标均不入列。
	outsideDir := t.TempDir()
	outsideRunner := filepath.Join(outsideDir, "outside-runner")
	if err := os.WriteFile(outsideRunner, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	entryLink := filepath.Join(cacheRoot, "cells", "go", "digest0003")
	if err := os.MkdirAll(entryLink, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRunner := filepath.Join(entryLink, "runner")
	if err := os.Symlink(outsideRunner, linkRunner); err != nil {
		t.Fatal(err)
	}
	// digest0003 的 ready.json：size 按 linkRunner 的 Lstat（软链本体）
	// 大小写入，保证 validCellEntryDir 能命中该条目（其 Lstat 口径读到
	// 的是软链本体）；软链产物在 appendCellCachePolicy 的 Lstat+解析
	// 校验处被拒之门外。
	lst, err := os.Lstat(linkRunner)
	if err != nil {
		t.Fatal(err)
	}
	ready := fmt.Sprintf(`{"inputDigest":"digest0003","artifact":"runner","size":%d,"created":1,"language":"go"}`, lst.Size())
	if err := os.WriteFile(filepath.Join(entryLink, "ready.json"), []byte(ready), 0o644); err != nil {
		t.Fatal(err)
	}

	p3 := &Policy{}
	appendCellCachePolicy(p3, pigHomeAlias)
	resolvedLink, err := filepath.EvalSymlinks(linkRunner)
	if err != nil {
		t.Fatalf("EvalSymlinks link runner: %v", err)
	}
	if execContains(p3, linkRunner) || execContains(p3, resolvedLink) || execContains(p3, outsideRunner) {
		t.Fatalf("symlink artifact %q (resolved %q / outside %q) must not enter Policy.Exec",
			linkRunner, resolvedLink, outsideRunner)
	}
}
