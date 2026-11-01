package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
