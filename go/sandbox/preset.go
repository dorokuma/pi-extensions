package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ReviewerReportSubdir 是 ReadOnlyReviewer 在 workdir 下默认开放写权限的报告目录名。
const ReviewerReportSubdir = "reports"

const (
	envGoExecDirKey    = "PI_SANDBOX_GO_EXEC_DIR"
	envRealGoBinKey    = "PI_SANDBOX_REAL_GO"
	envInternalModeKey = "PI_SANDBOX_INTERNAL_MODE"
	envBrokerCfgKey    = "PI_SANDBOX_BROKER_CFG"
)

// ForbiddenExecNames 定义严禁在只读审查子代理中执行的嵌套代理与调度器二进制名称。
var ForbiddenExecNames = []string{
	"pig",
	"pi",
	"agy",
	"grok",
	"dispatch-agent",
}

// DefaultReviewerTools 定义只读审查子代理默认允许执行的常用只读检查与开发工具列表。
var DefaultReviewerTools = []string{
	// Shell 与基础命令包装
	"bash", "sh", "dash", "env", "true", "false",
	// 版本控制、Go 工具链、C 链接工具链与代码检索
	"git", "go", "rg", "grep", "egrep", "fgrep", "find", "xargs",
	"gcc", "cc", "clang", "ld", "as", "ar", "nm", "objcopy", "objdump", "readelf", "strip", "pkg-config",
	// 文本查看与只读分析工具
	"cat", "ls", "head", "tail", "less", "more", "wc", "nl",
	"sed", "awk", "gawk", "mawk", "cut", "tr", "sort", "uniq",
	"diff", "cmp", "comm", "stat", "file", "tree", "jq",
	// 路径与系统信息辅助工具
	"pwd", "dirname", "basename", "realpath", "readlink", "which",
	"date", "uname", "id", "whoami", "printf", "echo", "test",
	"sha256sum", "md5sum",
}

// standardBinDirs 是除 PATH 外用于查找白名单工具的标准系统二进制目录。
var standardBinDirs = []string{
	"/usr/local/bin",
	"/usr/bin",
	"/bin",
	"/usr/local/go/bin",
	"/usr/lib/go/bin",
}

// IsForbiddenExec 判断给定的命令名或路径是否属于禁止执行的代理/执行器。
// 同时检查原始路径的基名（basename）以及符号链接解析后的真实文件基名。
func IsForbiddenExec(pathOrName string) bool {
	trimmed := strings.TrimSpace(pathOrName)
	if trimmed == "" {
		return false
	}
	base := filepath.Base(trimmed)
	if isForbiddenBaseName(base) {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(trimmed); err == nil && resolved != "" {
		if isForbiddenBaseName(filepath.Base(resolved)) {
			return true
		}
	}
	return false
}

func isForbiddenBaseName(name string) bool {
	for _, forbidden := range ForbiddenExecNames {
		if name == forbidden {
			return true
		}
	}
	return false
}

// IsELFInterpreter 判断给定的命令名或路径是否属于系统 ELF 动态链接器（如 ld-linux、ld-musl、ld.so）。
// 动态链接器仅允许由内核在 execve 期间作为 PT_INTERP 解释器加载，严禁作为用户态命令直接调用以绕过执行白名单。
func IsELFInterpreter(pathOrName string) bool {
	trimmed := strings.TrimSpace(pathOrName)
	if trimmed == "" {
		return false
	}
	if isELFInterpreterBaseName(filepath.Base(trimmed)) {
		return true
	}
	if resolved, err := filepath.EvalSymlinks(trimmed); err == nil && resolved != "" {
		if isELFInterpreterBaseName(filepath.Base(resolved)) {
			return true
		}
	}
	return false
}

func isELFInterpreterBaseName(name string) bool {
	if name == "ld.so" || strings.HasPrefix(name, "ld.so.") {
		return true
	}
	if strings.HasPrefix(name, "ld-linux") || strings.HasPrefix(name, "ld-musl") || strings.HasPrefix(name, "ld64.so") {
		return true
	}
	if strings.HasPrefix(name, "ld-") && strings.Contains(name, ".so") {
		return true
	}
	return false
}

// BuildExecWhitelist 根据给定的工具名或路径列表构造去重后的可执行文件绝对路径白名单。
// 任何命中 ForbiddenExecNames 或 IsELFInterpreter 的二进制（或其符号链接目标）都会被自动过滤排除。
func BuildExecWhitelist(toolNames []string) []string {
	seen := make(map[string]struct{}, len(toolNames)*2)
	var out []string

	addCandidate := func(candidate string) {
		if candidate == "" || IsForbiddenExec(candidate) || IsELFInterpreter(candidate) {
			return
		}
		norm, err := NormalizePath(candidate)
		if err != nil || IsForbiddenExec(norm) || IsELFInterpreter(norm) {
			return
		}
		info, err := os.Stat(norm)
		if err != nil {
			return
		}
		// 仅接纳存在的可执行普通文件或内部工具目录
		if !info.IsDir() && info.Mode()&0o111 == 0 {
			return
		}
		if _, ok := seen[norm]; !ok {
			seen[norm] = struct{}{}
			out = append(out, norm)
		}
		if resolved, err := filepath.EvalSymlinks(norm); err == nil && resolved != "" {
			if resolvedNorm, err := NormalizePath(resolved); err == nil && !IsForbiddenExec(resolvedNorm) && !IsELFInterpreter(resolvedNorm) {
				if _, ok := seen[resolvedNorm]; !ok {
					seen[resolvedNorm] = struct{}{}
					out = append(out, resolvedNorm)
				}
			}
		}
	}

	for _, name := range toolNames {
		name = strings.TrimSpace(name)
		if name == "" || IsForbiddenExec(name) || IsELFInterpreter(name) {
			continue
		}
		if strings.ContainsRune(name, filepath.Separator) {
			addCandidate(name)
			continue
		}
		if lp, err := exec.LookPath(name); err == nil {
			addCandidate(lp)
		}
		for _, dir := range standardBinDirs {
			addCandidate(filepath.Join(dir, name))
		}

		// 针对 git、go、gcc，额外放行其内部子命令目录（如 git-core、go/pkg/tool、gcc libexec），
		// 确保 git status / git diff / go build / go vet / go test 等子进程调用正常运转。
		switch name {
		case "git":
			for _, gitCore := range []string{"/usr/lib/git-core", "/usr/libexec/git-core"} {
				if st, err := os.Stat(gitCore); err == nil && st.IsDir() {
					addCandidate(gitCore)
				}
			}
		case "go":
			for _, p := range resolveGoToolchainPaths() {
				addCandidate(p)
			}
		case "gcc", "cc":
			for _, gccDir := range []string{"/usr/lib/gcc", "/usr/libexec/gcc"} {
				if st, err := os.Stat(gccDir); err == nil && st.IsDir() {
					addCandidate(gccDir)
				}
			}
		}
	}

	return out
}

func resolveGoToolchainPaths() []string {
	var paths []string
	var goRoots []string

	if gr := runtime.GOROOT(); gr != "" {
		goRoots = append(goRoots, gr)
	}
	if envGR := os.Getenv("GOROOT"); envGR != "" {
		goRoots = append(goRoots, envGR)
	}
	if goBin, err := exec.LookPath("go"); err == nil {
		paths = append(paths, goBin)
		if resolved, err := filepath.EvalSymlinks(goBin); err == nil {
			paths = append(paths, resolved)
			goRoots = append(goRoots, filepath.Dir(filepath.Dir(resolved)))
		}
	}
	goRoots = append(goRoots, "/usr/local/go", "/usr/lib/go")
	if matches, err := filepath.Glob("/usr/lib/go-*"); err == nil {
		goRoots = append(goRoots, matches...)
	}
	if matches, err := filepath.Glob("/usr/share/go-*"); err == nil {
		goRoots = append(goRoots, matches...)
	}

	for _, root := range goRoots {
		if root == "" {
			continue
		}
		binGo := filepath.Join(root, "bin", "go")
		if st, err := os.Stat(binGo); err == nil && !st.IsDir() {
			paths = append(paths, binGo)
		}
		toolDir := filepath.Join(root, "pkg", "tool")
		if st, err := os.Stat(toolDir); err == nil && st.IsDir() {
			paths = append(paths, toolDir)
		}
	}

	return paths
}

func resolveRealGoBinary(skipDir string) string {
	cleanSkip := ""
	if skipDir != "" {
		if norm, err := NormalizePath(skipDir); err == nil {
			cleanSkip = norm
		} else {
			cleanSkip = filepath.Clean(skipDir)
		}
	}
	isSkipped := func(p string) bool {
		if cleanSkip == "" {
			return false
		}
		norm, err := NormalizePath(p)
		if err != nil {
			norm = filepath.Clean(p)
		}
		return norm == cleanSkip || strings.HasPrefix(norm, cleanSkip+string(filepath.Separator))
	}

	if envReal := strings.TrimSpace(os.Getenv(envRealGoBinKey)); envReal != "" && !isSkipped(envReal) {
		if st, err := os.Stat(envReal); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return envReal
		}
	}

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || isSkipped(dir) {
			continue
		}
		candidate := filepath.Join(dir, "go")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			if resolved, err := filepath.EvalSymlinks(candidate); err == nil && resolved != "" {
				return resolved
			}
			return candidate
		}
	}

	for _, dir := range []string{"/usr/local/go/bin", "/usr/lib/go/bin", "/usr/bin", "/bin"} {
		if isSkipped(dir) {
			continue
		}
		candidate := filepath.Join(dir, "go")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			if resolved, err := filepath.EvalSymlinks(candidate); err == nil && resolved != "" {
				return resolved
			}
			return candidate
		}
	}
	return ""
}

// ReadOnlyReviewer 构造针对"只读审查员/体检员"子代理的预设沙盒策略：
//   - 整个根文件系统 "/" 只读；
//   - 仅开放 workdir 下的报告目录 (<workdir>/reports) 与 /tmp 下的私有子目录 (/tmp/pi-reviewer-*) 可写；
//   - 严格实施 W^X：所有可写目录（含 <workdir>/reports、TMPDIR、GOTMPDIR、GOCACHE）均不授予执行权限，
//     彻底封堵将可读 ELF（如 agy、pi、dispatch-agent）拷贝至可写临时目录后再执行的绕过路径；
//   - 建立独立的只读可执行目录 (/tmp/pi-reviewer-exec-*) 并注入受控的 go 包装器，
//     使 go build / go vet / go test 在受控的构建域中生成和运行测试二进制，且测试二进制运行前自动降权回只读审查域。
func ReadOnlyReviewer(workdir string) Policy {
	if strings.TrimSpace(workdir) == "" {
		workdir = "."
	}
	absWorkdir, err := NormalizePath(workdir)
	if err != nil {
		absWorkdir = filepath.Clean(workdir)
	}

	reportDir := filepath.Join(absWorkdir, ReviewerReportSubdir)
	// 若 workdir 已存在于磁盘，预先创建报告子目录以便 Landlock O_PATH 打开绑定规则。
	if st, err := os.Stat(absWorkdir); err == nil && st.IsDir() {
		_ = os.MkdirAll(reportDir, 0o755)
	}

	privateTmpDir, err := os.MkdirTemp("/tmp", "pi-reviewer-*")
	if err != nil {
		privateTmpDir, err = os.MkdirTemp("", "pi-reviewer-*")
		if err != nil {
			privateTmpDir = filepath.Join(os.TempDir(), fmt.Sprintf("pi-reviewer-%d", os.Getpid()))
			_ = os.MkdirAll(privateTmpDir, 0o700)
		}
	}

	goTmpDir := filepath.Join(privateTmpDir, "go-tmp")
	goCacheDir := filepath.Join(privateTmpDir, "go-cache")
	_ = os.MkdirAll(goTmpDir, 0o700)
	_ = os.MkdirAll(goCacheDir, 0o700)

	// 创建与可写 privateTmpDir 分离的受控执行目录 goExecDir：
	// 在审查员 Landlock 规则集中，goExecDir 仅具只读+执行权限（无写权限），审查员无法向其中写入或拷贝任何文件。
	goExecDir, err := os.MkdirTemp("/tmp", "pi-reviewer-exec-*")
	if err != nil {
		goExecDir, err = os.MkdirTemp("", "pi-reviewer-exec-*")
		if err != nil {
			goExecDir = filepath.Join(os.TempDir(), fmt.Sprintf("pi-reviewer-exec-%d", os.Getpid()))
			_ = os.MkdirAll(goExecDir, 0o700)
		}
	}
	goExecBinDir := filepath.Join(goExecDir, "bin")
	goExecWorkDir := filepath.Join(goExecDir, "work")
	goExecCacheDir := filepath.Join(goExecDir, "cache")
	_ = os.MkdirAll(goExecBinDir, 0o755)
	_ = os.MkdirAll(goExecWorkDir, 0o700)
	_ = os.MkdirAll(goExecCacheDir, 0o700)

	realGoBin := resolveRealGoBinary(goExecBinDir)

	execList := BuildExecWhitelist(DefaultReviewerTools)
	execList = append(execList, goExecDir)

	if selfExe, err := os.Executable(); err == nil && selfExe != "" {
		if resolvedExe, err := filepath.EvalSymlinks(selfExe); err == nil && resolvedExe != "" {
			selfExe = resolvedExe
		}
		if normExe, err := NormalizePath(selfExe); err == nil && !IsForbiddenExec(normExe) && !IsELFInterpreter(normExe) {
			execList = append(execList, normExe)
			_ = os.Symlink(normExe, filepath.Join(goExecBinDir, "go"))
			_ = os.Symlink(normExe, filepath.Join(goExecBinDir, "pi-test-runner"))
		}
	}

	curPath := os.Getenv("PATH")
	newPath := goExecBinDir
	if curPath != "" {
		newPath = goExecBinDir + string(os.PathListSeparator) + curPath
	}

	envList := []string{
		"TMPDIR=" + privateTmpDir,
		"GOTMPDIR=" + goTmpDir,
		"GOCACHE=" + goCacheDir,
		"PATH=" + newPath,
		envGoExecDirKey + "=" + goExecDir,
	}
	if realGoBin != "" {
		envList = append(envList, envRealGoBinKey+"="+realGoBin)
	}

	return Policy{
		ReadOnly:  []string{"/"},
		ReadWrite: []string{reportDir, privateTmpDir},
		Exec:      execList,
		DenyNet:   false,
		Env:       envList,
	}
}

// ReviewerAgentTools 在 DefaultReviewerTools 基础上补充代理运行期常用的文件辅助命令。
var ReviewerAgentTools = append(append([]string{}, DefaultReviewerTools...),
	"mkdir", "cp", "touch",
)

// agentLookupDirs 是解析 agy/pig/pi 时除 PATH 与 standardBinDirs 外额外搜索的目录。
var agentLookupDirs = []string{
	"/root/.local/bin",
	"/root/.pig/bin",
}

// entryAgentPathSet 将 EntryAgents 路径规范化（含符号链接目标）为集合，供 Validate / seccomp 使用。
func entryAgentPathSet(paths []string) map[string]struct{} {
	out := make(map[string]struct{}, len(paths)*2)
	for _, raw := range paths {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		norm, err := NormalizePath(raw)
		if err != nil {
			continue
		}
		out[norm] = struct{}{}
		if resolved, err := filepath.EvalSymlinks(norm); err == nil && resolved != "" {
			if rn, err := NormalizePath(resolved); err == nil {
				out[rn] = struct{}{}
			}
		}
	}
	return out
}

// IsEntryAgentPath 判断 pathOrName 是否属于给定的 EntryAgents 列表（含符号链接解析与 basename 匹配）。
func IsEntryAgentPath(entryAgents []string, pathOrName string) bool {
	trimmed := strings.TrimSpace(pathOrName)
	if trimmed == "" || len(entryAgents) == 0 {
		return false
	}
	set := entryAgentPathSet(entryAgents)
	if norm, err := NormalizePath(trimmed); err == nil {
		if _, ok := set[norm]; ok {
			return true
		}
		if resolved, err := filepath.EvalSymlinks(norm); err == nil && resolved != "" {
			if rn, err := NormalizePath(resolved); err == nil {
				if _, ok := set[rn]; ok {
					return true
				}
			}
		}
	}
	if !strings.ContainsRune(trimmed, filepath.Separator) {
		for p := range set {
			if filepath.Base(p) == trimmed {
				return true
			}
		}
	}
	return false
}

// agyHookBinaryNames 是 agy 生命周期钩子（SessionStart/PreInvocation/PreToolUse/PostToolUse/
// PreCompress/Stop）会 exec 的钩子二进制名。来源：/root/.gemini/config/hooks.json、
// /root/.agy-pro-home/.gemini/config/hooks.json 及插件 hooks.json（agy-recall）。
// herdsman 是 bash 包装脚本，exec 到 $HOME/.local/lib/herdsman-go/herdsman（ELF 本体）。
// 严禁整目录放行 ~/.local/bin（含 herdr、dispatch-agent），按名字逐个点名。
var agyHookBinaryNames = []string{
	"herdsman",
	"agy-rules-hook",
	"moshi-hook",
	"agy-recover-400",
	"agy-recall",
}

// agyHookLookupDirs 是在 standardBinDirs/agentLookupDirs 之外为 agy 钩子二进制
// 追加搜索的宿主目录，覆盖 homeDir() 与硬编码 /root 两种 HOME 形态。
func agyHookLookupDirs() []string {
	seen := make(map[string]struct{}, 2)
	var out []string
	add := func(d string) {
		d = filepath.Clean(strings.TrimSpace(d))
		if d == "" || d == "." {
			return
		}
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	add(filepath.Join(homeDir(), ".local", "bin"))
	add("/root/.local/bin")
	return out
}

// ResolveAgyHookPaths 解析 agy 生命周期钩子可执行路径：逐个在 PATH、standardBinDirs、
// agentLookupDirs、agyHookLookupDirs() 下查找 agyHookBinaryNames，校验存在、非目录、可执行，
// 并自动 EvalSymlinks 把符号链接与真实目标一并纳入（如 agy-recall → plugins/agy-recall/bin/agy-recall）。
// 对 herdsman 额外纳入其 bash exec 目标 ~/.local/lib/herdsman-go/herdsman 及 HERDSMAN_GO_BIN 覆盖值。
// 不放行整个 ~/.local/bin，避免 herdr/dispatch-agent 被连带放行。
func ResolveAgyHookPaths() []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		norm, err := NormalizePath(candidate)
		if err != nil {
			return
		}
		info, err := os.Stat(norm)
		if err != nil || info.IsDir() {
			return
		}
		if info.Mode()&0o111 == 0 {
			return
		}
		if _, ok := seen[norm]; !ok {
			seen[norm] = struct{}{}
			out = append(out, norm)
		}
		if resolved, err := filepath.EvalSymlinks(norm); err == nil && resolved != "" {
			if rn, err := NormalizePath(resolved); err == nil {
				if st, err := os.Stat(rn); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
					if _, ok := seen[rn]; !ok {
						seen[rn] = struct{}{}
						out = append(out, rn)
					}
				}
			}
		}
	}

	for _, name := range agyHookBinaryNames {
		if lp, err := exec.LookPath(name); err == nil {
			add(lp)
		}
		for _, dir := range standardBinDirs {
			add(filepath.Join(dir, name))
		}
		for _, dir := range agentLookupDirs {
			add(filepath.Join(dir, name))
		}
		for _, dir := range agyHookLookupDirs() {
			add(filepath.Join(dir, name))
		}
	}

	// herdsman 包装脚本 exec 到 herdsman-go 真实二进制；HERDSMAN_GO_BIN 优先于默认路径。
	// 覆盖 homeDir() 与 /root 两种 HOME，确保 HOME=/root/.agy-pro-home 下同样解析到本体。
	addHerdsmanGoBin := func(root string) {
		add(filepath.Join(root, ".local", "lib", "herdsman-go", "herdsman"))
	}
	if envBin := strings.TrimSpace(os.Getenv("HERDSMAN_GO_BIN")); envBin != "" {
		add(envBin)
	}
	addHerdsmanGoBin(homeDir())
	addHerdsmanGoBin("/root")

	return out
}

// mcpServerBinaryNames 是 pig / agy MCP 生态中作为独立进程被代理直接 exec 的服务二进制名。
// 它们通过 PATH 或软链调用，经 resolveExecutableCandidate 把软链与 ELF 本体一并放行。
// 注意：严禁整目录放行 ~/.local/bin（含 herdr/dispatch-agent/dispatch-agent-go/grok），按名字逐个点名。
var mcpServerBinaryNames = []string{
	"ctxmode",
	"codegraph-go",
	"jev-mcp",
	"jev-mcp-go",
	"agy-recall",
}

// resolveExecutableCandidate 校验 candidate 为存在的可执行普通文件，并把符号链接
// 解析目标一并纳入输出（去重，保持首次出现顺序）。
func resolveExecutableCandidate(candidate string, seen map[string]struct{}, out *[]string) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return
	}
	norm, err := NormalizePath(candidate)
	if err != nil {
		return
	}
	info, err := os.Stat(norm)
	if err != nil || info.IsDir() {
		return
	}
	if info.Mode()&0o111 == 0 {
		return
	}
	if _, ok := seen[norm]; !ok {
		seen[norm] = struct{}{}
		*out = append(*out, norm)
	}
	if resolved, err := filepath.EvalSymlinks(norm); err == nil && resolved != "" {
		if rn, err := NormalizePath(resolved); err == nil {
			if st, err := os.Stat(rn); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				if _, ok := seen[rn]; !ok {
					seen[rn] = struct{}{}
					*out = append(*out, rn)
				}
			}
		}
	}
}

// ResolveMCPServerPaths 解析 pig / agy MCP 生态的服务二进制及其包装脚本目标：
// ctxmode、codegraph-go、jev-mcp、jev-mcp-go、agy-recall，逐个在 PATH、
// standardBinDirs、agentLookupDirs、agyHookLookupDirs()（覆盖 homeDir() 与 /root 两种 HOME）
// 下查找，软链与 ELF 本体一并放行（去重）。不放行整个 ~/.local/bin。
func ResolveMCPServerPaths() []string {
	seen := make(map[string]struct{}, len(mcpServerBinaryNames)*2)
	var out []string
	for _, name := range mcpServerBinaryNames {
		if lp, err := exec.LookPath(name); err == nil {
			resolveExecutableCandidate(lp, seen, &out)
		}
		for _, dir := range standardBinDirs {
			resolveExecutableCandidate(filepath.Join(dir, name), seen, &out)
		}
		for _, dir := range agentLookupDirs {
			resolveExecutableCandidate(filepath.Join(dir, name), seen, &out)
		}
		for _, dir := range agyHookLookupDirs() {
			resolveExecutableCandidate(filepath.Join(dir, name), seen, &out)
		}
	}
	return out
}

// ResolveEntryAgentPaths 解析宿主上的 agy / pig / pi 及其符号链接目标，供 ReviewerAgent 顶层启动。
// 不把 dispatch-agent / grok 纳入；嵌套禁止无法靠 Landlock 一次性放行实现，见 ReviewerAgent 注释。
func ResolveEntryAgentPaths() []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		norm, err := NormalizePath(candidate)
		if err != nil {
			return
		}
		info, err := os.Stat(norm)
		if err != nil || info.IsDir() {
			return
		}
		if info.Mode()&0o111 == 0 {
			return
		}
		if _, ok := seen[norm]; !ok {
			seen[norm] = struct{}{}
			out = append(out, norm)
		}
		if resolved, err := filepath.EvalSymlinks(norm); err == nil && resolved != "" {
			if rn, err := NormalizePath(resolved); err == nil {
				if st, err := os.Stat(rn); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
					if _, ok := seen[rn]; !ok {
						seen[rn] = struct{}{}
						out = append(out, rn)
					}
				}
			}
		}
	}

	names := []string{"agy", "pig", "pi"}
	for _, name := range names {
		if lp, err := exec.LookPath(name); err == nil {
			add(lp)
		}
		for _, dir := range standardBinDirs {
			add(filepath.Join(dir, name))
		}
		for _, dir := range agentLookupDirs {
			add(filepath.Join(dir, name))
		}
	}
	if matches, err := filepath.Glob("/root/.local/opt/pig/*/pig"); err == nil {
		for _, m := range matches {
			add(m)
		}
	}
	// agy stub (/root/.local/bin/agy) 会再 exec 到 antigravity-cli/bin/agy（动态链接真实本体）
	agyStateBin := filepath.Join(homeDir(), ".gemini", "antigravity-cli", "bin")
	for _, name := range []string{"agy", "agentapi", "webm_encoder"} {
		add(filepath.Join(agyStateBin, name))
	}
	return out
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}

// agyProHomeStateDir 是 agy 二进制硬编码 HOME（/root/.agy-pro-home）下的 antigravity-cli 状态根。
// agy 不跟随 $HOME / UserHomeDir；仅放行 homeDir()/.gemini/... 时，派活路径会因写 log/oauth token 失败。
const agyProHomeStateDir = "/root/.agy-pro-home/.gemini/antigravity-cli"

// agyStateDataSubdirs 是 antigravity-cli 下需要可写的数据子目录（不含 bin/）。
var agyStateDataSubdirs = []string{
	"conversations", "cache", "scratch", "log", "crashes",
	"annotations", "presence", "updater", "brain", "plugin_data",
	"knowledge", "implicit", "mcp",
}

// agyAntigravityStateDirs 返回需要开放写权限的 antigravity-cli 状态根（去重）。
// 同时覆盖 UserHomeDir 路径与 agy 硬编码的 /root/.agy-pro-home 路径。
func agyAntigravityStateDirs() []string {
	seen := make(map[string]struct{}, 2)
	var out []string
	add := func(d string) {
		d = filepath.Clean(strings.TrimSpace(d))
		if d == "" || d == "." {
			return
		}
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	add(filepath.Join(homeDir(), ".gemini", "antigravity-cli"))
	add(agyProHomeStateDir)
	return out
}

// appendAgyStateReadWrite 为每个 antigravity-cli 状态根开放数据子目录与根级状态文件写权限（不含 bin/）。
func appendAgyStateReadWrite(p *Policy) {
	for _, agyStateDir := range agyAntigravityStateDirs() {
		for _, sub := range agyStateDataSubdirs {
			d := filepath.Join(agyStateDir, sub)
			_ = os.MkdirAll(d, 0o755)
			p.ReadWrite = append(p.ReadWrite, d)
		}
		if entries, err := os.ReadDir(agyStateDir); err == nil {
			for _, ent := range entries {
				name := ent.Name()
				// bin/ 必须保持只读+Exec；agy-pro-home 下 bin 常为指向真实 ELF 目录的符号链接，
				// DirEntry.IsDir 对符号链接为 false，须按名字显式跳过，避免 W^X 写穿。
				if ent.IsDir() || name == "bin" {
					continue
				}
				// 根级状态文件（settings.json / history.jsonl / antigravity-oauth-token / *.db 等）
				p.ReadWrite = append(p.ReadWrite, filepath.Join(agyStateDir, name))
			}
		}
	}
}

// ReviewerAgent 构造「只读审查员代理」预设：在 ReadOnlyReviewer 基础上允许顶层启动 agy/pig，
// 并开放代理会话状态目录写权限。
//
// 额外能力：
//   - Exec：agy、pig、pi（及符号链接目标）+ mkdir/cp/touch；
//     agy 生命周期钩子二进制（herdsman 及其 herdsman-go ELF 本体、agy-rules-hook、moshi-hook、
//     agy-recover-400、agy-recall，含软链目标），否则 SessionStart/PreInvocation/PreToolUse/
//     PostToolUse/PreCompress/Stop 钩子在沙盒内被 Landlock 拦截，只读子代理无法向 Herdsman
//     注册会话/上报 agent.turn.completed；
//   - Exec：pig / agy MCP 生态服务二进制（ctxmode / codegraph-go / jev-mcp / jev-mcp-go /
//     agy-recall，含软链解析目标），仍不整目录放行 ~/.local/bin；
//   - ReadWrite：/tmp/herdr-role-sessions（pig --session-dir）；agy 状态为 ~/.gemini/antigravity-cli
//     与 /root/.agy-pro-home/.gemini/antigravity-cli 下数据子目录 + 根目录已有状态文件
//     （不含 bin/，避免对可执行文件 W^X 写穿）；以及 /dev/tty（dispatch-agent-go 对 agy 使用
//     --prompt-interactive / -i，bubbletea 需要打开控制终端）；
//   - ReadWrite（ctxmode/MCP 数据隔离）：CTXMODE_DB 未显式指定时注入
//     <privateTmpDir>/ctxmode/context_mode.db（沙盒私有 0700）；宿主兜底仅精确放行
//     ~/.local/share/ctxmode 与 ~/.gemini/config/plugins/agy-recall/state（含
//     /root/.agy-pro-home 对应目录），绝不放宽到 ~/.local/share 或 gemini-web；
//   - ReadWrite（pig AgentDir 私有隔离）：宿主 /root/.pig/agent 保持 100% 只读；
//     进入 Landlock 前把必要配置文件 0600 拷入 <privateTmpDir>/pig-agent（extensions
//     软链指向只读宿主目录），并注入 PIG_CODING_AGENT_DIR；
//   - ReadWrite/Exec（Cell 缓存 W^X）：宿主态预刷新 <pigHome>/cache 下缓存单元 usage.json
//     （TouchUsage <1h 只读短路）；仅 cells/<lang>/.locks、ext/.locks 与 toolchains 锁文件
//     可写，仅已构建 <digest>/runner 与 cache/ext 下可执行产物可执行；
//   - DenyNet 仍为 false（LLM 走本机 Magpie）；
//   - CTXMODE_* / HERDR_* 由 sbx 继承 os.Environ 透传，本预设不剥离。
//
// 注意：herdsman 访问 /root/.herdsman（runtime.json/herdsman.sock）保持只读；
// /root/.herdsman 绝不加入 ReadWrite，HOME=/root/.agy-pro-home 时经软链指向同一目录亦然。
//
// 嵌套限制（如实）：Landlock 按路径授予 EXECUTE，顶层放行后子进程通常也能再 exec 同一路径；
// 当前实现没有 execve 计数 / 一次性 ticket。沙盒内再起 agy/pig 在 OS 层仍可能成功。
// 兜底：只读角色本身无 bash/run_command，工具层难以再派发；dispatch-agent/grok 仍不在白名单。
// 原 ReadOnlyReviewer / --preset reviewer 行为不变。
func ReviewerAgent(workdir string) Policy {
	p := ReadOnlyReviewer(workdir)

	extraTools := BuildExecWhitelist([]string{"mkdir", "cp", "touch"})
	agents := ResolveEntryAgentPaths()
	hooks := ResolveAgyHookPaths()
	p.EntryAgents = agents
	p.Exec = append(p.Exec, extraTools...)
	p.Exec = append(p.Exec, agents...)
	p.Exec = append(p.Exec, hooks...)

	sessionRoot := "/tmp/herdr-role-sessions"
	_ = os.MkdirAll(sessionRoot, 0o700)
	p.ReadWrite = append(p.ReadWrite, sessionRoot)

	// agy 状态：只开放数据子目录，不开放 bin/（真实 ELF 在 bin/agy，须保持只读+Exec，避免 W^X 被写穿）。
	// 同时覆盖 homeDir() 与 agy 硬编码 HOME（/root/.agy-pro-home），否则派活路径写 log/token 会 EACCES。
	appendAgyStateReadWrite(&p)

	// dispatch-agent-go reviewer 对 agy 使用 -i（--prompt-interactive），bubbletea 需要 /dev/tty。
	// 仅本预设放行；不改全局 defaultNullDevices，避免扩大其他预设攻击面。
	p.ReadWrite = append(p.ReadWrite, "/dev/tty")

	// privateTmpDir 由 ReadOnlyReviewer 创建并记录在 Policy.Env 的 TMPDIR 中。
	privateTmpDir := lookupEnvValue(p.Env, "TMPDIR")

	// 安全细则一：pig / agy MCP 生态的服务二进制（ctxmode / codegraph-go / jev-mcp /
	// jev-mcp-go / agy-recall）按名字逐个点名放行，含软链解析目标。
	p.Exec = append(p.Exec, ResolveMCPServerPaths()...)

	// 安全细则一：为沙盒内 ctxmode 提供私有数据库目录并注入 CTXMODE_DB，
	// 避免写宿主 ~/.local/share/ctxmode（gemini-web chrome-profile 同父目录，绝不放行）。
	privateCtxDir := preparePrivateCtxmodeDir(privateTmpDir)
	if strings.TrimSpace(os.Getenv("CTXMODE_DB")) == "" {
		p.Env = append(p.Env, "CTXMODE_DB="+filepath.Join(privateCtxDir, "context_mode.db"))
	}
	// 宿主兜底：仅精确放行 ctxmode 数据目录与 agy-recall 的状态目录；
	// 绝不允许 ~/.local/share 整体可写（会连带 ~/.local/share/gemini-web/chrome-profile）。
	appendCtxmodeStateReadWrite(&p)

	// 安全细则二：pig AgentDir 私有隔离。宿主 /root/.pig/agent 保持 100% 只读，
	// 沙盒内 pig 经 PIG_CODING_AGENT_DIR 指向 privateTmpDir 下的私有副本。
	preparePrivatePigAgentDir(privateTmpDir)
	p.Env = injectPigCodingAgentDir(p.Env, filepath.Join(privateTmpDir, "pig-agent"))

	// 安全细则三：宿主态预刷新缓存单元 usage.json（沙盒内 TouchUsage 命中 <1h 只读短路），
	// 只放行锁目录与已构建产物，严守 W^X。
	refreshCellUsageMetadata(pigHomeDir(), time.Now())
	appendCellCachePolicy(&p, pigHomeDir())

	return p
}

// pigHomeDir 返回宿主 PIG_HOME：$PIG_HOME 优先，其次 homeDir()/.pig，最后 /root/.pig。
func pigHomeDir() string {
	if v := strings.TrimSpace(os.Getenv("PIG_HOME")); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return filepath.Clean(abs)
		}
		return filepath.Clean(v)
	}
	return filepath.Join(homeDir(), ".pig")
}

// preparePrivateCtxmodeDir 创建 <privateTmpDir>/ctxmode（0700）并返回路径。
// 与 CTXMODE_DB 注入配合：ctxmode 对自定义 CTXMODE_DB 的父目录只做 MkdirAll 不再 chmod，
// 所以这里先预建为 0700。
func preparePrivateCtxmodeDir(privateTmpDir string) string {
	dir := filepath.Join(privateTmpDir, "ctxmode")
	if err := os.MkdirAll(dir, 0o700); err == nil {
		_ = os.Chmod(dir, 0o700)
	}
	return dir
}

// appendCtxmodeStateReadWrite 为宿主态 ctxmode / agy-recall 状态目录做精确兜底放行：
//   - <home>/.local/share/ctxmode：CTXMODE_DB 显式指定指回宿主时的最小写域；
//   - <home>/.gemini/config/plugins/agy-recall/state（覆盖 homeDir() 与 /root/.agy-pro-home）。
// 绝不加入 ~/.local/share 本身或其兄弟目录（特别是 gemini-web/chrome-profile）。
func appendCtxmodeStateReadWrite(p *Policy) {
	seen := make(map[string]struct{}, 4)
	add := func(d string, mkdir bool) {
		d = filepath.Clean(strings.TrimSpace(d))
		if d == "" || d == "." {
			return
		}
		if _, ok := seen[d]; ok {
			return
		}
		seen[d] = struct{}{}
		if mkdir {
			_ = os.MkdirAll(d, 0o700)
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			p.ReadWrite = append(p.ReadWrite, d)
		}
	}
	for _, root := range dedupeStrings([]string{homeDir(), "/root", "/root/.agy-pro-home"}) {
		add(filepath.Join(root, ".local", "share", "ctxmode"), false)
		add(filepath.Join(root, ".gemini", "config", "plugins", "agy-recall", "state"), true)
	}
}

// pigAgentConfigFiles 是拷贝进私有 pig-agent 目录的必要配置文件白名单。
// 绝不拷贝 sessions/、logs/、crash-output/、.lock/ 目录或其它非清单文件。
var pigAgentConfigFiles = []string{
	"settings.json",
	"models.json",
	"models-store.json",
	"auth.json",
	"keybindings.json",
	"trusted-projects.json",
}

// preparePrivatePigAgentDir 在 <privateTmpDir>/pig-agent（0700）下构建私有 AgentDir：
// 仅当宿主 AgentDir 下对应普通文件存在时才以 0600 拷入白名单配置文件，
// 并把 extensions 软链指向只读的宿主扩展目录。返回目录路径。
func preparePrivatePigAgentDir(privateTmpDir string) string {
	dst := filepath.Join(privateTmpDir, "pig-agent")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return dst
	}
	_ = os.Chmod(dst, 0o700)

	src := filepath.Join(pigHomeDir(), "agent")
	for _, name := range pigAgentConfigFiles {
		srcFile := filepath.Join(src, name)
		info, err := os.Stat(srcFile)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(srcFile)
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(dst, name), data, 0o600)
	}

	// extensions 软链指向宿主只读目录（宿主 /root/.pig/agent/extensions 严禁改动，
	// 沙盒内经只读根规则保持只读）。已存在的普通目录/文件不覆盖。
	dstExt := filepath.Join(dst, "extensions")
	srcExt := filepath.Join(src, "extensions")
	if _, err := os.Stat(srcExt); err == nil {
		if _, err := os.Lstat(dstExt); os.IsNotExist(err) {
			_ = os.Symlink(srcExt, dstExt)
		}
	}
	return dst
}

// injectPigCodingAgentDir 在 env 中注入 PIG_CODING_AGENT_DIR=<privatePigAgentDir>：
// 仅当调用方未显式将 PIG_CODING_AGENT_DIR（或 Policy.Env 同名键）指向其它可写目录时生效。
func injectPigCodingAgentDir(env []string, privatePigAgentDir string) []string {
	if strings.TrimSpace(os.Getenv("PIG_CODING_AGENT_DIR")) != "" {
		return env
	}
	if lookupEnvValue(env, "PIG_CODING_AGENT_DIR") != "" {
		return env
	}
	return append(env, "PIG_CODING_AGENT_DIR="+privatePigAgentDir)
}

// cellReadyMeta 是缓存单元 ready.json 中校验所需的字段。
type cellReadyMeta struct {
	InputDigest string `json:"inputDigest"`
	Artifact    string `json:"artifact"`
	Size        int64  `json:"size"`
	Created     int64  `json:"created"`
}

// validCellEntryDir 判断 dir 是否为含有效 ready.json 的缓存单元目录：
// ready.json 可解析、Created>0、目录名与 InputDigest 绑定（<digest> 或 <name>-<digest>）、
// artifact 名为单路径元素且对应普通文件大小一致。与 PiG runtimecell.readReadyMetadata 对齐。
func validCellEntryDir(dir string) (cellReadyMeta, string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "ready.json"))
	if err != nil {
		return cellReadyMeta{}, "", false
	}
	var meta cellReadyMeta
	if json.Unmarshal(data, &meta) != nil || meta.Created <= 0 {
		return cellReadyMeta{}, "", false
	}
	base := filepath.Base(dir)
	if meta.InputDigest == "" || (base != meta.InputDigest && !strings.HasSuffix(base, "-"+meta.InputDigest)) {
		return cellReadyMeta{}, "", false
	}
	name := meta.Artifact
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || filepath.Base(name) != name {
		return cellReadyMeta{}, "", false
	}
	artifactPath := filepath.Join(dir, name)
	info, err := os.Lstat(artifactPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != meta.Size {
		return cellReadyMeta{}, "", false
	}
	return meta, artifactPath, true
}

// touchUsageJSON 与 PiG runtimecell.TouchUsage 同语义：<1h 或未来时间戳只读短路，
// 否则原子写 <dir>/usage.json（tmp + rename，0644）。宿主态调用，沙盒内不会执行。
func touchUsageJSON(entryDir string, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	if data, err := os.ReadFile(filepath.Join(entryDir, "usage.json")); err == nil {
		var current struct {
			LastUsed int64 `json:"lastUsed"`
		}
		if json.Unmarshal(data, &current) == nil && current.LastUsed > 0 {
			last := time.Unix(current.LastUsed, 0)
			if last.After(now) || now.Sub(last) < time.Hour {
				return nil
			}
		}
	}
	data, err := json.Marshal(struct {
		LastUsed int64 `json:"lastUsed"`
	}{LastUsed: now.Unix()})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(entryDir, ".usage-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(entryDir, "usage.json"))
}

// cellCacheEntryDirs 返回缓存根下全部缓存单元目录（ext/<entry> 与 cells/<lang>/<entry>）。
// 与 PiG runtimecell.cacheEntryPaths 同构：跳过点开头目录，tombstone 除外。
func cellCacheEntryDirs(cacheRoot string) []string {
	var paths []string
	addEntries := func(root string) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			name := ent.Name()
			if strings.HasPrefix(name, ".") && !strings.HasPrefix(name, ".tombstone-") {
				continue
			}
			paths = append(paths, filepath.Join(root, name))
		}
	}
	addEntries(filepath.Join(cacheRoot, "ext"))
	cellsRoot := filepath.Join(cacheRoot, "cells")
	if langs, err := os.ReadDir(cellsRoot); err == nil {
		for _, lang := range langs {
			if !lang.IsDir() || strings.HasPrefix(lang.Name(), ".") {
				continue
			}
			addEntries(filepath.Join(cellsRoot, lang.Name()))
		}
	}
	return paths
}

// refreshCellUsageMetadata 在进入 Landlock 前（宿主态）把 <pigHome>/cache 下
// 所有含有效 ready.json 的缓存单元 usage.json 原子刷新为 now.Unix()，
// 确保沙盒内 pig 的 TouchUsage 命中 <1h 纯只读短路、不向缓存目录写临时文件。
// 返回实际刷新的单元数；调用方应将 now 取在进入 Landlock 前不久。
func refreshCellUsageMetadata(pigHome string, now time.Time) int {
	cacheRoot := filepath.Join(pigHome, "cache")
	refreshed := 0
	for _, dir := range cellCacheEntryDirs(cacheRoot) {
		if _, _, ok := validCellEntryDir(dir); !ok {
			continue
		}
		if err := touchUsageJSON(dir, now); err == nil {
			refreshed++
		}
	}
	return refreshed
}

// appendCellCachePolicy 为 pig 缓存追加最小权限：
//   - ReadWrite：仅锁目录 cells/<lang>/.locks、ext/.locks（宿主态预建 0755）
//     与 toolchains 版本记录锁文件（*.json.lock 所在目录，非可执行目录）；
//   - Exec：已构建缓存单元的 artifact 文件（cells/<lang>/<digest>/runner、cache/ext/<entry>/<artifact>）。
// 缓存单元目录本身保持只读；锁目录与可执行文件分离，不破坏 W^X。
func appendCellCachePolicy(p *Policy, pigHome string) {
	cacheRoot := filepath.Join(pigHome, "cache")
	if st, err := os.Stat(cacheRoot); err != nil || !st.IsDir() {
		return
	}

	seenRW := make(map[string]struct{}, 4)
	addRW := func(d string) {
		d = filepath.Clean(d)
		if _, ok := seenRW[d]; ok {
			return
		}
		seenRW[d] = struct{}{}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return
		}
		p.ReadWrite = append(p.ReadWrite, d)
	}

	// 锁目录：cells/<lang>/.locks 与 ext/.locks。
	addRW(filepath.Join(cacheRoot, "ext", ".locks"))
	cellsRoot := filepath.Join(cacheRoot, "cells")
	if langs, err := os.ReadDir(cellsRoot); err == nil {
		for _, lang := range langs {
			if !lang.IsDir() || strings.HasPrefix(lang.Name(), ".") {
				continue
			}
			addRW(filepath.Join(cellsRoot, lang.Name(), ".locks"))
		}
	}
	// toolchains 版本记录的锁文件 (*.json.lock)：锁文件与记录同目录，
	// 仅当目录下已有记录或锁文件时预建并放行（不创建空的可写目录）。
	toolchainsDir := filepath.Join(cacheRoot, "toolchains")
	if entries, err := os.ReadDir(toolchainsDir); err == nil {
		hasRecords := false
		for _, ent := range entries {
			name := ent.Name()
			if !ent.IsDir() && (strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".json.lock")) {
				hasRecords = true
				break
			}
		}
		if hasRecords {
			addRW(toolchainsDir)
		}
	}

	// 已构建产物：artifact 普通文件进 Exec（cells/<digest>/runner、ext/<entry>/bin、
	// ext/node-runtime 的 content.sha256 除外——其虽带执行位但不是 ELF，按统一规则放行也无执行面）。
	seenExec := make(map[string]struct{}, 8)
	for _, dir := range cellCacheEntryDirs(cacheRoot) {
		meta, artifactPath, ok := validCellEntryDir(dir)
		if !ok {
			continue
		}
		_ = meta
		info, err := os.Stat(artifactPath)
		if err != nil || info.IsDir() {
			continue
		}
		// 缓存产物按普通文件处理：仅放行带执行位的真实文件。
		if info.Mode()&0o111 == 0 {
			continue
		}
		norm, err := NormalizePath(artifactPath)
		if err != nil {
			continue
		}
		if _, ok := seenExec[norm]; !ok {
			seenExec[norm] = struct{}{}
			p.Exec = append(p.Exec, norm)
		}
	}
}

// dedupeStrings 去重并保持首次出现顺序。
func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
