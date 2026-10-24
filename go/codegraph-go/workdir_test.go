package codegraphgo

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
func isolated(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CODEGRAPH_CONFIG", "")
	t.Setenv("CODEGRAPH_GO_WORKDIR", "")
	t.Setenv("CODEGRAPH_GO_ALLOW_BROAD", "")
	t.Setenv("CODEGRAPH_GO_DEBUG", "")
	t.Chdir(dir)
	return dir
}
func TestPaths(t *testing.T) {
	h := isolated(t)
	t.Setenv("ROOT_VAR", h)
	t.Setenv("UNSET_PATH", "")
	for _, tc := range []struct{ in, want string }{{"", ""}, {"~", h}, {"~/a", filepath.Join(h, "a")}, {"$ROOT_VAR/${UNSET_PATH}/x", h + "//x"}, {"~other", "~other"}, {"$UNSET_PATH", ""}} {
		if got := expandPath(tc.in); got != tc.want {
			t.Errorf("expand %q = %q", tc.in, got)
		}
	}
	p := filepath.Join(h, "project")
	if err := os.Mkdir(p, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if normPath(link) != p || normPath("missing") != filepath.Join(h, "missing") {
		t.Fatal("canonical/fallback")
	}
	for _, r := range []string{"/", "/home", "/Users"} {
		if !isFilesystemRoot(r) {
			t.Errorf("root %s", r)
		}
	}
	if isFilesystemRoot(h) || !isHomeDir(h) || !isHomeDir("/root") || isHomeDir(p) {
		t.Fatal("home/root")
	}
	if !isWithinAllowlist(p, []string{h}) || !isWithinAllowlist(link, []string{p}) || !isWithinAllowlist(h, []string{h}) || isWithinAllowlist(h+"-other", []string{h}) || isWithinAllowlist(h, []string{}) {
		t.Fatal("segment allowlist")
	}
	if len(projectMarkers) != 20 {
		t.Fatal("markers")
	}
	for _, v := range []string{"1", " TRUE ", "yes", "on"} {
		t.Setenv("FLAG", v)
		if !envFlag("FLAG") {
			t.Fatal(v)
		}
	}
	t.Setenv("FLAG", "false")
	if envFlag("FLAG") {
		t.Fatal("false")
	}
}
func TestConfigParser(t *testing.T) {
	h := isolated(t)
	p := filepath.Join(h, "c.yaml")
	cases := []struct {
		name, body string
		want       []string
	}{
		{"block", "workdirs:\n  - '/a'\n- /b # comment\nnext: x\n  - /ignored\n", []string{"/a", "/b"}},
		{"flow", "workdirs: [/a, \"/b\", ''] # c\n - /c\n", []string{"/a", "/b", "/c"}},
		{"quotes before comments", "workdirs:\n - '/a' # c\n", []string{"/a'"}},
		{"quoted with comment", "workdirs:\n - \"/root\" # comment\n", []string{"/root\""}},
		{"quoted only", "workdirs:\n - \"/root\"\n", []string{"/root"}},
		{"js trim before quotes", "workdirs:\n - \ufeff\"/root\"\n", []string{"/root"}},
		{"empty", "workdirs: []", []string{}}, {"anchor", "workdirs: &roots\n - /a", []string{}}, {"alias", "workdirs:\n - *roots", []string{}}, {"multidoc", "---\nworkdirs: [/a]", []string{}}, {"scalar", "workdirs:\n |+\n - /a", []string{}}, {"mapping", "workdirs: [/a, {b: c}]", []string{}}, {"no key", "other: yes", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeFixture(t, p, tc.body)
			if got := parseConfigWorkdirs(p); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	if got := parseConfigWorkdirs(filepath.Join(h, "absent")); len(got) != 0 {
		t.Fatal(got)
	}
}
func TestConfigLookupAndAllowlist(t *testing.T) {
	h := isolated(t)
	local := filepath.Join(h, "codegraph-config.yaml")
	global := filepath.Join(h, ".config", "codegraph", "config.yaml")
	writeFixture(t, global, "workdirs: [/opt]")
	if configFilePath() != global {
		t.Fatal("global")
	}
	writeFixture(t, local, "workdirs: [~/p, $PROJECT, ~/p]")
	t.Setenv("PROJECT", filepath.Join(h, "q"))
	t.Setenv("CODEGRAPH_CONFIG", filepath.Join(h, "dead"))
	if configFilePath() != local {
		t.Fatal("dead env did not fall through")
	}
	if got := allowedRoots(); !reflect.DeepEqual(got, []string{filepath.Join(h, "p"), filepath.Join(h, "q")}) {
		t.Fatal(got)
	}
	t.Setenv("CODEGRAPH_CONFIG", global)
	if configFilePath() != global {
		t.Fatal("env")
	}
	writeFixture(t, global, "workdirs: []")
	if !reflect.DeepEqual(allowedRoots(), []string{h}) {
		t.Fatal("empty fallback")
	}
	writeFixture(t, global, "workdirs: [$NOT_SET_ROOT]")
	t.Setenv("NOT_SET_ROOT", "")
	if len(allowedRoots()) != 0 {
		t.Fatal("expanded-empty must fail closed")
	}
	t.Setenv("HOME", "")
	t.Setenv("CODEGRAPH_CONFIG", "")
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	if len(allowedRoots()) != 0 {
		t.Fatal("missing HOME must fail closed")
	}
}
func TestWorkdirDecision(t *testing.T) {
	h := isolated(t)
	p := filepath.Join(h, "project")
	sub := filepath.Join(p, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(p, "go.mod"), "module fixture\n")
	if d := resolveWorkdir(sub); !d.OK || d.Workdir != p || d.Note != "项目标记目录" {
		t.Fatal(d)
	}
	// A minimal local git repository needs no git mutation command: rev-parse
	// recognizes HEAD/config/objects/refs. No checkout, add or commit is used.
	writeFixture(t, filepath.Join(p, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFixture(t, filepath.Join(p, ".git", "config"), "[core]\n repositoryformatversion = 0\n bare = false\n")
	for _, d := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(p, ".git", d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal(err)
	}
	if d := resolveWorkdir(sub); d.Workdir != p || d.Note != "git 仓库根" {
		t.Fatal(d)
	}
	cfg := filepath.Join(h, "config.yaml")
	t.Setenv("CODEGRAPH_CONFIG", cfg)
	writeFixture(t, cfg, "workdirs: [$EMPTY_ROOT, ~]\n")
	t.Setenv("EMPTY_ROOT", "")
	if d := makeWorkdirDecision(sub); !d.OK || d.Workdir != h || !strings.HasPrefix(d.Note, "config workdirs[0]") {
		t.Fatal(d)
	}
	t.Setenv("CODEGRAPH_GO_WORKDIR", p)
	if d := makeWorkdirDecision(sub); !d.OK || d.Workdir != p || d.Note != "CODEGRAPH_GO_WORKDIR" {
		t.Fatal(d)
	}
	writeFixture(t, cfg, "workdirs: [/opt]")
	if d := makeWorkdirDecision(sub); d.OK || !strings.Contains(d.Reason, "不在 codegraph 授权根内") {
		t.Fatal(d)
	}
	t.Setenv("CODEGRAPH_GO_WORKDIR", "/")
	if d := resolveWorkdir(sub); d.OK || !strings.Contains(d.Reason, "CODEGRAPH_GO_WORKDIR=/") {
		t.Fatal(d)
	}
	t.Setenv("CODEGRAPH_GO_ALLOW_BROAD", "yes")
	if d := resolveWorkdir(sub); !d.OK || d.Workdir != "/" {
		t.Fatal(d)
	}
	t.Setenv("CODEGRAPH_GO_WORKDIR", "")
	t.Setenv("CODEGRAPH_GO_ALLOW_BROAD", "")
	writeFixture(t, cfg, "workdirs: [/home]")
	if d := resolveWorkdir(sub); d.OK {
		t.Fatal(d)
	}
	writeFixture(t, cfg, "workdirs: []")
	if d := resolveWorkdir(h); !d.OK || !strings.HasPrefix(d.Note, "家目录模式") {
		t.Fatal(d)
	}
	plain := filepath.Join(h, "plain")
	if err := os.Mkdir(plain, 0755); err != nil {
		t.Fatal(err)
	}
	if d := resolveWorkdir(plain); !d.OK || d.Note != "cwd" {
		t.Fatal(d)
	}
	if d := resolveWorkdir("/Users"); d.OK {
		t.Fatal(d)
	}
	if findMarkerRoot(h) != "" || findGitRoot(filepath.Join(h, "absent")) != "" {
		t.Fatal("fallback")
	}
}
func TestListProjects(t *testing.T) {
	h := isolated(t)
	for _, n := range []string{"zeta", "alpha", ".hidden", "go", "node_modules", "code_references"} {
		writeFixture(t, filepath.Join(h, n, "package.json"), "{}")
	}
	if err := os.Mkdir(filepath.Join(h, "plain"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := listProjects(h); !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Fatal(got)
	}
	if len(listProjects(filepath.Join(h, "absent"))) != 0 {
		t.Fatal("missing")
	}
}
