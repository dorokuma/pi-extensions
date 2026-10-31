package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ReviewerReportSubdir 是 ReadOnlyReviewer 在 workdir 下默认开放写权限的报告子目录名。
const ReviewerReportSubdir = "reports"

// ForbiddenExecNames 定义严禁在只读审查子代理中执行的嵌套代理与调度器二进制名称。
var ForbiddenExecNames = []string{
	"pig",
	"pi",
	"agy",
	"grok",
	"dispatch-agent",
}

// DefaultReviewerTools 定义只读审查子代理默认允许执行的常用只读检查与开发工具名列表。
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

// BuildExecWhitelist 根据给定的工具名或路径列表构造去重后的可执行文件绝对路径白名单。
// 任何命中 ForbiddenExecNames 的二进制（或其符号链接目标）都会被自动过滤排除。
func BuildExecWhitelist(toolNames []string) []string {
	seen := make(map[string]struct{}, len(toolNames)*2)
	var out []string

	addCandidate := func(candidate string) {
		if candidate == "" || IsForbiddenExec(candidate) {
			return
		}
		norm, err := NormalizePath(candidate)
		if err != nil || IsForbiddenExec(norm) {
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
			if resolvedNorm, err := NormalizePath(resolved); err == nil && !IsForbiddenExec(resolvedNorm) {
				if _, ok := seen[resolvedNorm]; !ok {
					seen[resolvedNorm] = struct{}{}
					out = append(out, resolvedNorm)
				}
			}
		}
	}

	for _, name := range toolNames {
		name = strings.TrimSpace(name)
		if name == "" || IsForbiddenExec(name) {
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

// ReadOnlyReviewer 构造针对"只读审查员/体检员"子代理的预设沙盒策略：
//   - 整个根文件系统 "/" 只读；
//   - 仅开放 workdir 下的报告目录（<workdir>/reports）与 /tmp 下的私有子目录可写；
//   - 在私有临时目录下预创建 go-tmp 与 go-cache 子目录，并通过 Policy.Env 导出
//     TMPDIR、GOTMPDIR、GOCACHE，使 go build / go vet / go test 无需写全局 /tmp 或 ~/.cache；
//   - 允许执行常用基础工具白名单及私有临时目录中的测试产物（go test 编译的 *.test 二进制），
//     但严禁执行 pig、pi、agy、grok、dispatch-agent（且 <workdir>/reports 保持不可执行）。
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

	execList := BuildExecWhitelist(DefaultReviewerTools)
	// 允许执行私有临时目录（及 go-tmp）中由 go test 临时编译生成的测试二进制
	execList = append(execList, privateTmpDir, goTmpDir)

	envList := []string{
		"TMPDIR=" + privateTmpDir,
		"GOTMPDIR=" + goTmpDir,
		"GOCACHE=" + goCacheDir,
	}

	return Policy{
		ReadOnly:  []string{"/"},
		ReadWrite: []string{reportDir, privateTmpDir},
		Exec:      execList,
		DenyNet:   false,
		Env:       envList,
	}
}
