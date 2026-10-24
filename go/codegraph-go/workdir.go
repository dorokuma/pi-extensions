package codegraphgo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var projectMarkers = []string{".git", "go.mod", "package.json", "pyproject.toml", "Cargo.toml", "pom.xml", "build.gradle", "build.gradle.kts", "composer.json", "Gemfile", "mix.exs", "CMakeLists.txt", "Makefile", "Cargo.lock", "pnpm-workspace.yaml", "setup.py", "requirements.txt", "Pipfile", "build.sbt", "Package.swift"}

type WorkdirDecision struct {
	OK      bool
	Workdir string
	Note    string
	Reason  string
}

func envFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
func homeDir() string { h, _ := os.UserHomeDir(); return h }
func normPath(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if c, err := filepath.EvalSymlinks(a); err == nil {
		return c
	}
	return a
}

var envPathRE = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*|\{[^}]*\})`)

func expandPath(p string) string {
	if h := homeDir(); h != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		if p == "~" {
			p = h
		} else {
			p = filepath.Join(h, p[2:])
		}
	}
	return envPathRE.ReplaceAllStringFunc(p, func(m string) string { return os.Getenv(strings.Trim(m[1:], "{}")) })
}
func isFilesystemRoot(dir string) bool {
	d := normPath(dir)
	return d == string(filepath.Separator) || d == "/home" || d == "/Users"
}
func isHomeDir(dir string) bool {
	d := normPath(dir)
	h := homeDir()
	return d == "/root" || (h != "" && d == normPath(h))
}
func exists(p string) bool { _, err := os.Stat(p); return err == nil }
func hasProjectMarker(dir string) bool {
	for _, m := range projectMarkers {
		if exists(filepath.Join(dir, m)) {
			return true
		}
	}
	return false
}
func findGitRoot(start string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "git", "-C", start, "rev-parse", "--show-toplevel").Output()
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return ""
	}
	return normPath(strings.TrimSpace(string(b)))
}
func findMarkerRoot(start string) string {
	h := homeDir()
	if h != "" {
		h = normPath(h)
	}
	cur := normPath(start)
	for i := 0; i < 24; i++ {
		if cur == h || cur == string(filepath.Separator) || cur == "/root" {
			return ""
		}
		if hasProjectMarker(cur) {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
	return ""
}
func configFilePath() string {
	if e := os.Getenv("CODEGRAPH_CONFIG"); e != "" {
		if exists(e) {
			return e
		}
		diagLog(fmt.Sprintf("$CODEGRAPH_CONFIG=%s 不存在，跳过并继续默认查找（与二进制 ConfigPath 一致）", e))
	}
	local := normPath("./codegraph-config.yaml")
	if exists(local) {
		return local
	}
	if h := homeDir(); h != "" {
		p := filepath.Join(h, ".config", "codegraph", "config.yaml")
		if exists(p) {
			return p
		}
	}
	return ""
}

var unsupportedYAML = regexp.MustCompile(`(?m)&[A-Za-z0-9_-]+|\*[A-Za-z0-9_-]+|^---\s*$|^[ \t]*[|>][-+]?\s*$`)
var flowWorkdirs = regexp.MustCompile(`^workdirs\s*:\s*\[(.*)\]\s*(?:#.*)?$`)
var blockWorkdirs = regexp.MustCompile(`^workdirs\s*:`)
var listWorkdir = regexp.MustCompile(`^\s*-\s+(.+)$`)
var pathComment = regexp.MustCompile(`\s+#.*$`)

func configItem(s string) string {
	// JS `item.trim()` is the isJSSpace set, not Go's TrimSpace (which trims
	// U+0085 and keeps U+FEFF). Then the TS order: edge quotes first, then a
	// trailing comment — so '"/root" # comment' keeps its inner quote.
	s = strings.TrimFunc(s, isJSSpace)
	if strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "'") {
		s = s[1:]
	}
	if strings.HasSuffix(s, "\"") || strings.HasSuffix(s, "'") {
		s = s[:len(s)-1]
	}
	return pathComment.ReplaceAllString(s, "")
}
func parseConfigWorkdirs(filePath string) []string {
	roots := []string{}
	b, err := os.ReadFile(filePath)
	if err != nil {
		diagLog(fmt.Sprintf("无法读取 config %s（%v），授权根回退到 $HOME", filePath, err))
		return roots
	}
	data := string(b)
	if unsupportedYAML.MatchString(data) {
		diagLog(fmt.Sprintf("config %s 含轻量解析器无法处理的 YAML 构造（锚点/别名/多文档/块标量），授权根按「config 不可解析」回退到 $HOME（与二进制 WorkdirAllowlist 语义一致）", filePath))
		return roots
	}
	inWorkdirs := false
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimRightFunc(raw, isJSSpace)
		if !inWorkdirs {
			if flow := flowWorkdirs.FindStringSubmatch(line); flow != nil {
				if strings.ContainsAny(flow[1], "{}") {
					diagLog(fmt.Sprintf("config %s 的 workdirs 含 flow mapping（{…}），轻量解析器不支持，回退到 $HOME", filePath))
					return []string{}
				}
				inWorkdirs = true
				for _, item := range strings.Split(flow[1], ",") {
					if p := configItem(item); p != "" {
						roots = append(roots, p)
					}
				}
				continue
			}
			if blockWorkdirs.MatchString(line) {
				inWorkdirs = true
			}
			continue
		}
		if line != "" && !isJSSpace([]rune(line)[0]) && !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "-\t") {
			inWorkdirs = false
			continue
		}
		if m := listWorkdir.FindStringSubmatch(line); m != nil {
			if p := configItem(m[1]); p != "" {
				roots = append(roots, p)
			}
		}
	}
	return roots
}
func allowedRoots() []string {
	if cfg := configFilePath(); cfg != "" {
		parsed := parseConfigWorkdirs(cfg)
		if len(parsed) > 0 {
			roots := []string{}
			seen := map[string]bool{}
			for _, r := range parsed {
				if e := expandPath(r); e != "" {
					c := normPath(e)
					if c != "" && !seen[c] {
						seen[c] = true
						roots = append(roots, c)
					}
				}
			}
			return roots
		}
		diagLog(fmt.Sprintf("config %s 未解析出 workdirs，授权根回退到 $HOME（与二进制语义一致）", cfg))
	}
	if h := homeDir(); h != "" {
		return []string{normPath(h)}
	}
	return []string{}
}
func isWithinAllowlist(cand string, roots []string) bool {
	c := normPath(cand)
	for _, r := range roots {
		rel, err := filepath.Rel(normPath(r), c)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}
func resolveWorkdir(cwd string) WorkdirDecision {
	broad := envFlag("CODEGRAPH_GO_ALLOW_BROAD")
	if forced := strings.TrimSpace(os.Getenv("CODEGRAPH_GO_WORKDIR")); forced != "" {
		w := normPath(forced)
		if !broad && isFilesystemRoot(w) {
			return WorkdirDecision{Reason: fmt.Sprintf("CODEGRAPH_GO_WORKDIR=%s 是系统根目录。请换成用户目录或项目路径，或设 CODEGRAPH_GO_ALLOW_BROAD=1", w)}
		}
		note := "CODEGRAPH_GO_WORKDIR"
		if isHomeDir(w) {
			note += "（家目录模式：只索引像项目的一级目录）"
		}
		return WorkdirDecision{OK: true, Workdir: w, Note: note}
	}
	if cfg := configFilePath(); cfg != "" {
		for _, r := range parseConfigWorkdirs(cfg) {
			e := expandPath(r)
			if e == "" {
				continue
			}
			w := normPath(e)
			if !broad && isFilesystemRoot(w) {
				return WorkdirDecision{Reason: fmt.Sprintf("拒绝 workdir=%s（系统根）。请用家目录或项目路径。", w)}
			}
			note := "config workdirs[0]"
			if isHomeDir(w) {
				note += "（家目录模式：只索引像项目的一级目录）"
			}
			return WorkdirDecision{OK: true, Workdir: w, Note: note}
		}
	}
	start := normPath(cwd)
	gitRoot, markerRoot := findGitRoot(start), findMarkerRoot(start)
	candidate, note := start, "cwd"
	if gitRoot != "" && !isHomeDir(gitRoot) && !isFilesystemRoot(gitRoot) {
		candidate, note = gitRoot, "git 仓库根"
	} else if markerRoot != "" && !isHomeDir(markerRoot) && !isFilesystemRoot(markerRoot) {
		candidate, note = markerRoot, "项目标记目录"
	}
	if !broad && isFilesystemRoot(candidate) {
		return WorkdirDecision{Reason: fmt.Sprintf("拒绝 workdir=%s（系统根）。请用家目录或项目路径。", candidate)}
	}
	if isHomeDir(candidate) {
		if note == "cwd" {
			note = "家目录模式（只索引带 go.mod/package.json/.git 等标记的一级目录）"
		} else {
			note += " + 家目录模式"
		}
	}
	return WorkdirDecision{OK: true, Workdir: candidate, Note: note}
}
func makeWorkdirDecision(cwd string) WorkdirDecision {
	d := resolveWorkdir(cwd)
	if !d.OK {
		return d
	}
	roots := allowedRoots()
	if !isWithinAllowlist(d.Workdir, roots) {
		declared := strings.Join(roots, ", ")
		if declared == "" {
			declared = "无"
		}
		return WorkdirDecision{Reason: fmt.Sprintf("workdir %s 不在 codegraph 授权根内（config 声明：%s）", d.Workdir, declared)}
	}
	return d
}
func listProjects(workdir string) []string {
	out := []string{}
	entries, err := os.ReadDir(workdir)
	if err != nil {
		return out
	}
	// os.ReadDir returns lexical order, matching the specified Go port contract.
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || strings.HasPrefix(n, ".") || n == "go" || n == "node_modules" || n == "code_references" {
			continue
		}
		if hasProjectMarker(filepath.Join(workdir, n)) {
			out = append(out, n)
		}
	}
	return out
}
