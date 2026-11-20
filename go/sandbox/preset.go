package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// ReviewerReportSubdir 是 ReadOnlyReviewer 在 workdir 下默认开放写权限的报告目录名。
const ReviewerReportSubdir = "reports"

// herdrRoleSessionsRoot 是 herdr 角色会话的共享可写根，在 ReviewerAgent 预设
// 的 ReadWrite 白名单内（见 ReviewerAgentWithEnv）。
const herdrRoleSessionsRoot = "/tmp/herdr-role-sessions"

// herdrRoleReportsRoot 是 dispatch-agent 派发报告文档（DISPATCH_REPORT_PATH）
// 的共享可写根，同样在 ReviewerAgent 预设的 ReadWrite 白名单内
// （见 ReviewerAgentWithEnv）；与 herdrRoleSessionsRoot 并列独立，
// 会话目录与报告目录分根管理。
const herdrRoleReportsRoot = "/tmp/herdr-role-reports"

// claudeSharedProjectsRoot 是 Claude Code 项目会话日志（projects/）的共享根。
// 落在 herdrRoleSessionsRoot 之下，随其 ReadWrite 白名单天然可写，沙盒内
// claude 经 <CLAUDE_CONFIG_DIR>/projects/<project-slug>/<uuid>.jsonl 写入的
// 会话转录跨会话汇聚于此。提取为包级变量供包内测试覆盖根路径，避免单测
// 污染真实 /tmp/herdr-role-sessions。
var claudeSharedProjectsRoot = filepath.Join(herdrRoleSessionsRoot, "claude-projects")

const (
	envGoExecDirKey = "PI_SANDBOX_GO_EXEC_DIR"
	// envPresetTempDirsKey 登记预设构造阶段实际创建的私有临时目录清单
	//（JSON 字符串数组序列化，绝不使用 os.PathListSeparator 分隔——
	// 备用根 os.TempDir() 可经 TMPDIR 设为含 ":" 的目录，冒号分隔
	// 序列化会把含冒号路径拆成无效段，导致 isPresetTempDir 校验全部
	// 失败、非空清单短路返回不再回退兜底，两个真实目录（含私有凭据
	// 副本）残留泄漏）。ReadOnlyReviewer 在 privateTmpDir/goExecDir
	// 创建完成后把两条登记路径写入本键；sbx 在 buildPolicy 完成后
	// 将该键从最终 env 删除并转交 cleanupPresetTempDirsFromPolicy——
	// 清理集合必须来自本次创建的登记清单，绝不从 Policy.Exec/Env
	// 用父目录前缀反推（调用方经 --exec /tmp/pi-reviewer-exec-manual/bin
	// 传入的形似路径不属于本预设创建，推断式清理会递归误删其整个
	// 父目录）。该键是内部实现细节，不进入受限子进程最终 env，不属
	// 调用方可控输入。
	envPresetTempDirsKey = "PI_SANDBOX_PRESET_TMP_DIRS"
	// envGoTestWorkDirKey 仅在 broker 启动 go test 时由 helper 注入，让
	// pi-test-runner 能把 <goExecDir>/work（.test 产物所在，helper 可写域）
	// 经 cfg.Exec 单独放行执行；审查员进程侧绝不持有该键——Policy.Exec
	// 只放行 <goExecDir>/bin（MF-1）。
	envGoTestWorkDirKey = "PI_SANDBOX_GO_TEST_WORK"
	envRealGoBinKey     = "PI_SANDBOX_REAL_GO"
	envInternalModeKey  = "PI_SANDBOX_INTERNAL_MODE"
	envBrokerCfgKey     = "PI_SANDBOX_BROKER_CFG"
)

// ForbiddenExecNames 定义严禁在只读审查子代理中执行的嵌套代理与调度器二进制名称。
// 除顶层 wrapper 名外同时包含 ELF 本体名（dispatch-agent-go / herdr / herdr-core /
// herdr-mobile-bridge）：顶层 IsForbiddenExec 与 seccomp shouldDenyExecMmap 都按
// resolved basename 精确匹配 +「禁名+"-"/"."」前缀派生判定，名单不含本体名
// 会让本体二进制穿透三层拦截（Policy.Validate / BuildExecWhitelist 过滤、
// Landlock Exec 白名单、seccomp PROT_EXEC 守卫）；前缀派生仅对长度 ≥ 5 的
// 禁名生效（见 forbiddenNameDeriveMinLen），且只覆盖分隔符起始的形态（如
// dispatch-agent-go.bak-*、herdr-core），故 agy-recall / pig-agent /
// pi-reviewer-* 这类合法短名前缀工件不受影响。
// 本体名只列宿主实测存在的部署形态，不泛化；既有成员语义只增不删。
var ForbiddenExecNames = []string{
	"pig",
	"pi",
	"agy",
	"grok",
	"dispatch-agent",
	// 本体名均为宿主实测存在形态（ls /root/.local/bin + file 复核，均为静态 ELF）：
	// /root/.local/bin/dispatch-agent-go、herdr（herdr 本体）、herdr-core、
	// herdr-mobile-bridge。不存在的派生名（如 herdr-go）不入名单。
	"dispatch-agent-go",
	"herdr",
	"herdr-core",
	"herdr-mobile-bridge",
	// claude 衍生本体形态：裸 "claude" 不加（否则顶层 `sbx -- claude` 启动
	// 会与 EntryAgents 白名单对 claude 的放行形成自锁）；只列精确派生名。
	"claude-pty",
	"claude-code",
}

// DefaultReviewerTools 定义只读审查子代理默认允许执行的常用只读检查与开发工具列表。
var DefaultReviewerTools = []string{
	// Shell 与基础命令包装
	"bash", "sh", "dash", "env", "true", "false",
	// 版本控制、Go 工具链、C 链接工具链、远端只读探查（ssh）与代码检索
	"git", "go", "rg", "grep", "egrep", "fgrep", "find", "xargs", "ssh",
	"gcc", "cc", "clang", "ld", "as", "ar", "nm", "objcopy", "objdump", "readelf", "strip", "pkg-config",
	// 文本查看与只读分析工具
	"cat", "ls", "head", "tail", "less", "more", "wc", "nl",
	"sed", "awk", "gawk", "mawk", "cut", "tr", "sort", "uniq",
	"diff", "cmp", "comm", "stat", "file", "tree", "jq",
	// 路径与系统信息辅助工具
	"pwd", "dirname", "basename", "realpath", "readlink", "which",
	"date", "uname", "id", "whoami", "printf", "echo", "test",
	"sha256sum", "md5sum",
	// 只读角色回报通道
	"dispatch-report",
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
// 同时检查原始路径的基名（basename）以及符号链接解析后的真实文件基名；
// 基名命中按 isForbiddenBaseName：等于禁名即中，长度 ≥ 5 的禁名再以
// 「禁名+"-"」「禁名+"."」前缀派生（如 dispatch-agent-go.bak-*）也算命中。
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

// forbiddenNameDeriveMinLen 是参与「禁名+"-"/"."」前缀派生判定的禁名最短长度。
// 短禁名（pi/agy/pig/grok）是大量合法工件名的前缀——pi-reviewer-* 受控
// 执行目录、pig-agent 私有配置目录、agy-recall / agy-rules-hook 等——若对
// 短名也做前缀派生，这些合法 Exec 路径会被误判禁名而击穿全部预设。
const forbiddenNameDeriveMinLen = 5

func isForbiddenBaseName(name string) bool {
	for _, forbidden := range ForbiddenExecNames {
		if name == forbidden {
			return true
		}
		if len(forbidden) >= forbiddenNameDeriveMinLen &&
			(strings.HasPrefix(name, forbidden+"-") || strings.HasPrefix(name, forbidden+".")) {
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
		if name != "go" {
			// go 单独走受信 resolveGoToolchainPaths：绝不 exec.LookPath
			// 任意 PATH 前置的「假 go」或 standardBinDirs 下未校验 go 混入
			// Policy.Exec（oracle MF-2）。其它工具保持原 LookPath 契约。
			if lp, err := exec.LookPath(name); err == nil {
				addCandidate(lp)
			}
			for _, dir := range standardBinDirs {
				addCandidate(filepath.Join(dir, name))
			}
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

	// 受信真实 go 根：与预热/策略/broker 共用 resolveSandboxRealGoBinary
	// 的同一实体——其真实根 <goroot> 下的 bin/go 与 pkg/tool 才可放行，
	// 绝不因宿主 GOROOT/bin/go 软链指向受信 go 就放行原任意 GOROOT/pkg/tool
	//（别名反例：伪造 GOROOT 的 bin/go 软链→受信 go，pkg/tool 仍是伪造内容）。
	if realGo := resolveSandboxRealGoBinary(""); realGo != "" {
		paths = append(paths, realGo)
		goRoots = append(goRoots, filepath.Dir(filepath.Dir(realGo)))
	}
	// 固定系统根兜底：同样要求 <root>/bin/go 通过受信校验。刻意不再纳入
	// runtime.GOROOT()——Go 标准库 GOROOT() 优先返回进程启动时的 GOROOT
	// 环境变量原始值，伪造 GOROOT 会经它绕过下方 envGR 校验直接混入
	// goRoots，其 <root>/pkg/tool 中间软链别名即可逃逸进 Policy.Exec
	//（oracle8 MF-1）；编译期 GOROOT 已由上面的受信实体根与固定系统根覆盖。
	goRoots = append(goRoots, "/usr/local/go", "/usr/lib/go")
	if matches, err := filepath.Glob("/usr/lib/go-*"); err == nil {
		goRoots = append(goRoots, matches...)
	}
	if matches, err := filepath.Glob("/usr/share/go-*"); err == nil {
		goRoots = append(goRoots, matches...)
	}

	// os.Getenv("GOROOT") 仅当其 <GOROOT>/bin/go 通过受信校验（真实常规
	// 可执行文件、非组/全局可写、属主合规、固定前缀、父目录合规）才接纳——
	// 但绝不把原始 envGR 加入 goRoots：伪造 GOROOT 的 bin/go 可以是指向受信
	// go 的软链，此时伪造的 <GOROOT>/pkg/tool 仍会随原根混入 Policy.Exec
	//（GOROOT 别名漏洞）。只接纳 trustedRealGoCandidate 返回的已解析真实
	// go 推导出的规范化可信根 filepath.Dir(filepath.Dir(trusted))。
	if envGR := strings.TrimSpace(os.Getenv("GOROOT")); envGR != "" {
		if trusted, ok := trustedRealGoCandidate(filepath.Join(envGR, "bin", "go")); ok {
			goRoots = append(goRoots, filepath.Dir(filepath.Dir(trusted)))
		}
	}

	seen := make(map[string]struct{}, len(paths)+8)
	for _, p := range paths {
		seen[p] = struct{}{}
	}
	for _, root := range goRoots {
		if root == "" {
			continue
		}
		// <root>/bin/go 未通过受信校验直接跳过该根：绝不对未认证根收录
		// pkg/tool——伪造根的 bin/go 缺失/悬链时，其 pkg/tool 目录（或
		// 中间软链别名 pkg -> 受信根的 pkg）不得混进 Policy.Exec
		//（oracle8 MF-1）。
		binGo := filepath.Join(root, "bin", "go")
		trusted, ok := trustedRealGoCandidate(binGo)
		if !ok {
			continue
		}
		if _, dup := seen[trusted]; !dup {
			seen[trusted] = struct{}{}
			paths = append(paths, trusted)
		}
		// pkg/tool 只基于「解析后的真实 go 推导出的规范真实根」收录：
		// bin/go 是软链时 trusted 已解析到真实工具链根，绝不使用原始
		// root 拼接 pkg/tool——伪造根 bin/go 软链→受信 go 时，伪造根
		// 下 pkg -> 受信根 pkg 的中间软链别名会让 <root>/pkg/tool 指向
		// 受信内容但路径拼写属不可信域，必须以解析后真实根准入
		//（GOROOT 中间软链别名，oracle8 MF-1）。
		realRoot := filepath.Dir(filepath.Dir(trusted))
		toolDir := filepath.Join(realRoot, "pkg", "tool")
		if resolvedToolDir, ok := trustedToolDir(toolDir); ok {
			// 写入 Policy.Exec 的只能是解析后的规范真实路径，绝不写未解析
			// 的原始 toolDir（中间软链别名拼写）。
			if _, dup := seen[resolvedToolDir]; !dup {
				seen[resolvedToolDir] = struct{}{}
				paths = append(paths, resolvedToolDir)
			}
		}
	}

	return paths
}

// trustedToolDir 校验 Go 工具链 pkg/tool 目录本体可信：必须是真实目录
// （Lstat 拒绝本体软链——软链 pkg/tool 会把外部目录整体混入 Policy.Exec）；
// 其已解析真实路径必须位于固定可信工具目录前缀之下（专用
// trustedGoToolDirPrefixes——pkg/tool 位于工具链根而非 bin 前缀之下，
// 与 trustedRealGoCandidate 的 bin 前缀刻意不同界）；权限 perm&0o022==0
// 且属主为 0 或当前 euid——组/全局可写或异属主的 pkg/tool 目录内可被
// 塞入任意可执行文件，绝不能整目录放行执行。
func trustedToolDir(dir string) (string, bool) {
	return trustedToolDirWithPrefixes(dir, trustedGoToolDirPrefixes())
}

// trustedToolDirWithPrefixes 与 trustedToolDir 同口径，但可信前缀由调用方
// 显式给定——仅供同包单测注入 t.TempDir() 前缀覆盖权限/软链拒绝分支；
// 生产一律经 trustedToolDir 走固定前缀，零外部开启入口（与
// trustedRealGoCandidateWithPrefixes 同模式）。命中返回解析后的规范真实
// 路径（调用方只把该路径写进 paths/Policy.Exec，绝不写未解析原始 dir），
// 否则返回 ("", false)。
func trustedToolDirWithPrefixes(dir string, prefixes []string) (string, bool) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" || dir == "." {
		return "", false
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved == "" {
		return "", false
	}
	resolved = filepath.Clean(resolved)
	// 已解析真实路径必须位于固定可信前缀之下；同时 dir 不得借外部
	// 不可信软链别名解析出真实路径——EvalSymlinks 解析出的真实路径
	// 与调用方拼写经同一解析必须严格一致（调用点只把 resolved 写进
	// paths/Policy.Exec），前缀失配一律拒绝。
	if !pathWithinPrefixes(resolved, prefixes) {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return "", false
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
			return "", false
		}
	}
	return resolved, true
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

// resolveSandboxRealGoBinary 是 reviewer-agent 预热（含后续任何把所选真实
// go 目录前置进受控 PATH 的用途）共用的可信 go 解析：绝不把由调用方可控
// 输入（宿主 PATH 前置目录、PI_SANDBOX_REAL_GO）解析出的二进制直接以宿主
// 权限喂给预热子进程——工作树/任意目录内放个「假 go」+ 宿主 PATH 前置，旧
// 实现在 Landlock 生效前就已在宿主权限下把它前置进预热 PATH 被执行
// （controlledWarmPath 刻意不读调用方 PATH 的既有防线由此被打穿）。
//
// 可信判定分两步：先经 resolveRealGoBinary(skipDir) 做常规「非目录且有执行位」
// 解析（Env 覆盖 → 调用方 PATH → 固定目录回退）得到沙盒侧将选中的同一个
// go（cell 摘要对齐的来源），再对其做受信校验——EvalSymlinks 后的真实
// 路径必须位于固定可信工具链前缀（trustedGoToolchainPrefixes）之下。命中
// 不可信前缀时**不再继续探测下一候选**，直接回退固定可信目录顺序解析：
// 预热子进程 PATH 本就以 /usr/local/go/bin 等固定目录起头，回退到固定目录
// 首个可信 go 仍与沙盒侧同口径对齐（两侧均落同一真实工具链根），而「跳过
// PATH 命中改用 PATH 内下一目录」反而会让预热与沙盒内包装器（resolveRealGoBinary
// 仍按宿主 PATH 命中同一不可信目录）解析出不同 go——cell 摘要错配。于是
// 折中：PATH 命中不可信时放弃 PATH 对齐、回退固定可信目录首个 go（该目录
// 同样在沙盒 resolveRealGoBinary 的固定目录回退列表内，兜底一致），同时
// 杜绝不可信父目录被前置进预热 PATH。
//
// skipDir 与 resolveRealGoBinary 同语义（沙盒内 go 包装器跳过 goExecBinDir
// 自身）。
func resolveSandboxRealGoBinary(skipDir string) string {
	// 第一步：与沙盒同口径 resolveRealGoBinary 解析（PI_SANDBOX_REAL_GO →
	// 调用方 PATH → 固定目录回退），得到沙盒侧将选中的同一个 go。
	if realGo := resolveRealGoBinary(skipDir); realGo != "" {
		if trusted, ok := trustedRealGoCandidate(realGo); ok {
			return trusted
		}
		// PATH/Env 命中不可信：放弃该候选，继续回退固定可信目录，
		// 不把不可信父目录前置进预热 PATH。
	}
	// 回退：固定可信工具链目录顺序，与 controlledWarmPath 前缀一致。
	// 同步应用 skipDir（沙盒内 go 包装器须跳过 goExecBinDir 自身）。
	cleanSkip := ""
	if skipDir != "" {
		if norm, err := NormalizePath(skipDir); err == nil {
			cleanSkip = norm
		} else {
			cleanSkip = filepath.Clean(skipDir)
		}
	}
	for _, dir := range []string{"/usr/local/go/bin", "/usr/lib/go/bin", "/usr/bin", "/bin"} {
		if cleanSkip != "" {
			if norm, err := NormalizePath(dir); err == nil && (norm == cleanSkip || strings.HasPrefix(norm, cleanSkip+string(filepath.Separator))) {
				continue
			}
		}
		if trusted, ok := trustedRealGoCandidate(filepath.Join(dir, "go")); ok {
			return trusted
		}
	}
	return ""
}

// trustedGoToolchainPrefixes 返回真实 go（EvalSymlinks 后）允许落入的固定
// 可信工具链前缀：版本化/系统 Go 安装根 bin 目录与 trustedPigSearchDirs
// 同源的标准 bin 目录。绝不纳入调用方可控 PATH / TMPDIR / 工作树目录。
func trustedGoToolchainPrefixes() []string {
	trustedHome := trustedHomeDir()
	prefixes := []string{
		"/usr/local/go/bin",
		"/usr/lib/go/bin",
		"/usr/lib",
		"/usr/share",
		filepath.Join(trustedHome, ".local", "opt"),
		"/root/.local/opt",
	}
	return append(prefixes, standardBinDirs...)
}

// trustedGoToolDirPrefixes 返回 trustedToolDir 专用的固定可信工具目录前缀：
// Go 工具链的 pkg/tool 位于工具链根之下（<root>/pkg/tool），而非
// trustedGoToolchainPrefixes 的 bin 前缀之下——直接复用后者会把合法的
// /usr/local/go/pkg/tool、/usr/lib/go/pkg/tool 误拒出 Policy.Exec。本集合
// 显式纳入两固定 pkg/tool 根与既有的 /usr/lib、/usr/share、
// <trustedHome>/.local/opt、/root/.local/opt 版本化安装根；刻意不含
// standardBinDirs（/usr/bin、/bin 等整机 bin 目录绝非 pkg/tool 的合法
// 安装根），也绝不放宽 trustedRealGoCandidate 的 bin 前缀边界。
func trustedGoToolDirPrefixes() []string {
	trustedHome := trustedHomeDir()
	return []string{
		"/usr/local/go/pkg/tool",
		"/usr/lib/go/pkg/tool",
		"/usr/lib",
		"/usr/share",
		filepath.Join(trustedHome, ".local", "opt"),
		"/root/.local/opt",
	}
}

// trustedRealGoCandidate 校验单个 go 候选（生产前缀固定为
// trustedGoToolchainPrefixes）：真实常规可执行文件、非组/全局可写、属主
// 为 0 或当前 euid，且 EvalSymlinks 后的真实路径位于固定可信工具链前缀
// 之下。命中返回解析后的真实路径，否则返回 ("", false)。
func trustedRealGoCandidate(candidate string) (string, bool) {
	return trustedRealGoCandidateWithPrefixes(candidate, trustedGoToolchainPrefixes())
}

// trustedRealGoCandidateWithPrefixes 与 trustedRealGoCandidate 同口径，但可信
// 前缀由调用方显式给定——仅供同包单测注入 t.TempDir() 前缀覆盖文件/父目录
// 权限拒绝分支；生产一律经 trustedRealGoCandidate 走固定前缀，零外部开启入口。
func trustedRealGoCandidateWithPrefixes(candidate string, prefixes []string) (string, bool) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || resolved == "" {
		// EvalSymlinks 失败（悬链/自引用/不存在）：拒绝，绝不回退原始
		// candidate（旧兜底会把未解析的不可信路径当作真实路径放行）。
		return "", false
	}
	// 基名校验：解析后真实文件名必须是 go 本体（go 或 go<版本>/go-* 形态），
	// 杜绝借 PI_SANDBOX_REAL_GO=<受信根>/<其它二进制> 混入非 go 可执行文件。
	base := filepath.Base(filepath.Clean(resolved))
	if base != "go" && !strings.HasPrefix(base, "go-") && !strings.HasPrefix(base, "go1.") {
		return "", false
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return "", false
	}
	if st.Mode().Perm()&0o022 != 0 {
		return "", false
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); ok {
		if stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
			return "", false
		}
	}
	if !pathWithinPrefixes(resolved, prefixes) {
		return "", false
	}
	// 将被前置进预热 PATH 的父目录同样校验：组/全局可写或非合规属主的
	// 目录内可被掉包塞入同名可执行文件，绝不可信。
	parentDir := filepath.Dir(resolved)
	if pst, err := os.Stat(parentDir); err != nil || !pst.IsDir() ||
		pst.Mode().Perm()&0o022 != 0 {
		return "", false
	} else if pstat, ok := pst.Sys().(*syscall.Stat_t); ok {
		if pstat.Uid != 0 && pstat.Uid != uint32(os.Geteuid()) {
			return "", false
		}
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	return resolved, true
}

// ReadOnlyReviewer 构造针对"只读审查员/体检员"子代理的预设沙盒策略：
//   - 整个根文件系统 "/" 只读；
//   - 仅开放 /tmp 下的私有子目录 (/tmp/pi-reviewer-*) 可写；报告统一走
//     /tmp/herdr-role-reports（ReviewerAgentWithEnv 共享根），绝不在
//     <workdir> 下创建 reports/ 子目录污染目标 Git 工作树（oracle SF-2）；
//   - 严格实施 W^X：所有可写目录（TMPDIR、GOTMPDIR、GOCACHE）均不授予执行权限，
//     彻底封堵将可读 ELF（如 agy、pi、dispatch-agent）拷贝至可写临时目录后再执行的绕过路径；
//   - 建立独立的只读可执行目录 (/tmp/pi-reviewer-exec-*) 并注入受控的 go 包装器，
//     使 go build / go vet / go test 在受控的构建域中生成和运行测试二进制，且测试二进制运行前自动降权回只读审查域。
func ReadOnlyReviewer(workdir string) Policy {
	if strings.TrimSpace(workdir) == "" {
		workdir = "."
	}
	// workdir 不再用于创建 <workdir>/reports：报告根已统一收敛到
	// /tmp/herdr-role-reports（ReviewerAgentWithEnv 共享根），避免在目标
	// Git 工作树内凭空创建 reports/ 子目录、弄脏 git status 或使只读角色
	// 在源仓内拥有可写子目录（oracle SF-2）。保留 workdir 参数维持 API。

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

	// 与预热 resolveSandboxRealGoBinary 共用同一次受信解析：策略注入的
	// PI_SANDBOX_REAL_GO 与预热前置进 PATH 的真实 go 必须是同一实体
	//（杜绝 PATH/PI_SANDBOX_REAL_GO 污染导致的 cell toolchain 摘要错配）。
	realGoBin := resolveSandboxRealGoBinary(goExecBinDir)

	execList := BuildExecWhitelist(DefaultReviewerTools)
	// MF-1：审查员 Policy.Exec 只放行 <goExecDir>/bin（go 包装器、
	// pi-test-runner、hc 受限客户端），绝不放行整个 goExecDir——其下
	// work/ 是 broker 可写域，整目录放行等于授予沙盒内进程执行任何被
	// 写入 work/ 的 ELF 的权利（含 objcopy 拷入的 ForbiddenExecNames）。
	// .test 产物不走审查员侧 Exec：pi-test-runner 自身位于 bin/ 内可执行，
	// 它先叠加审查员规则集再 exec <goExecDir>/work 下 .test 目标，目标
	// 授权来自 helper 侧 helperPolicy 的 Exec 放行（helper Exec 经
	// startHelperProcess 继承 cfg.Exec，对应规则授予路径级执行权）；
	// 审查员进程自己 exec work/*.test 不再被 Landlock 放行——W^X 收口。
	execList = append(execList, goExecBinDir)

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
	// 创建清单登记（见 envPresetTempDirsKey）：本次预设实际创建的
	// privateTmpDir/goExecDir 绝对路径，是清理动作唯一可信的身份来源。
	// JSON 字符串数组序列化（不用 PathListSeparator 冒号分隔）：备用根
	// os.TempDir() 可含 ":"，冒号序列化会被 Split 拆成无效段造成清单
	// 校验全失败、真实目录泄漏。两绝对路径的 json.Marshal 不会失败，
	// 仍按规范处理返回值：失败则不登记，清理走既有兜底而非携带
	// 损坏清单。
	// UTF-8 合法性前置校验：JSON 字符串类型无法无损承载任意字节路径——
	// encoding/json 对字符串内非法 UTF-8 字节静默替换为 U+FFFD 且不报错
	// （encode.go appendString）。备用根 os.TempDir() 经 TMPDIR 可含非 UTF-8
	// 字节（Linux 合法路径），其下 privateTmpDir/goExecDir 若被有损登记，
	// 消费侧还原出的路径与真实目录不符，isPresetTempDir 前缀比对全失败
	// 且非空清单短路返回阻断 env/ReadWrite 兜底，两个真实目录（含私有
	// 凭据副本）残留泄漏。登记侧 fail-safe 为「不登记」而非「有损登记」——
	// 不登记时清理走保留原始字节的既有 env/ReadWrite 兜底。
	if utf8.ValidString(privateTmpDir) && utf8.ValidString(goExecDir) {
		if raw, err := json.Marshal([]string{privateTmpDir, goExecDir}); err == nil {
			envList = append(envList, envPresetTempDirsKey+"="+string(raw))
		}
	}
	if realGoBin != "" {
		envList = append(envList, envRealGoBinKey+"="+realGoBin)
	}

	return Policy{
		ReadOnly:  []string{"/"},
		ReadWrite: []string{privateTmpDir},
		Exec:      execList,
		DenyNet:   false,
		Env:       envList,
	}
}

// ReviewerAgentTools 在 DefaultReviewerTools 基础上补充代理运行期常用的文件辅助命令。
var ReviewerAgentTools = append(append([]string{}, DefaultReviewerTools...),
	"mkdir", "cp", "touch",
)

// dedupeTrustedHomes 归并受信 home 根（去重、保持顺序）：空串、"." 与 "/"
// 一律跳过（"/" 是 trustedHomeDir 不可解析时的兜底，作 home 根会 join 出
// /.local/bin 等无意义路径）。
//
// 信任边界口径：安装根前缀含 homeDir() 系既有信任口径（软链目标只当其
// 位于这些根之下才可经「含 /<查找名>/ 路径段」判定放行），维持不改；
// 二进制查找/收录面（agentLookupDirs / agyHookLookupDirs / herdsman-go bin /
// 版本化 pig glob / agyStateBin）一律改用 trustedLookupHomes()，不读 $HOME——
// $HOME 是调用方可控输入，不得作为二进制收录的信任边界输入（与 warm.go
// trustedHomeDir 注释口径一致）。
func dedupeTrustedHomes(candidates []string) []string {
	var homes []string
	seen := make(map[string]struct{}, len(candidates))
	for _, home := range candidates {
		home = filepath.Clean(strings.TrimSpace(home))
		if home == "" || home == "." || home == "/" {
			continue
		}
		if _, ok := seen[home]; ok {
			continue
		}
		seen[home] = struct{}{}
		homes = append(homes, home)
	}
	return homes
}

// trustedAgentHomesFor 返回代理安装根前缀判定专用的受信 home 根列表
// （去重、保持顺序）：homeDir()（当前 $HOME）在前、调用方显式给定的宿主 home
// 在后（生产始终传 trustedHomeDir()；测试传入夹具宿主 home，使安装根判定
// 可在无真实 claude/agy 的干净 CI 上确定性覆盖）。
func trustedAgentHomesFor(hostHome string) []string {
	return dedupeTrustedHomes([]string{homeDir(), hostHome})
}

// trustedLookupHomes 返回二进制查找/收录面专用的受信 home 根列表
// （去重、保持顺序）：trustedHomeDir()（/etc/passwd 解析的宿主真实 HOME，
// 被隔离覆盖的 $HOME 下仍解析到宿主）、硬编码 /root（agy/pig 本体不跟随
// $HOME 的既有约定）与 /root/.agy-pro-home（agy 硬编码 HOME 约定，显式替代
// 原先靠 homeDir() 覆盖 .agy-pro-home 的合法场景）。绝不读 $HOME/homeDir()：
// $HOME 是调用方可控输入，不得作为二进制收录的信任边界输入（与 warm.go
// trustedHomeDir 注释口径一致）。trustedHomeDir 不可解析时的兜底 "/" 不作
// home 根（否则 join 出 /.local/bin 等无意义路径），跳过。
//
// 仅就 agentLookupDirs 与版本化 pig glob 两面，相对修复前枚举是超集
// （修复前这两面仅 /root 两项，现含 passwd-HOME + /root + .agy-pro-home），
// 不会拒掉修复前能收录的合法项；这两面唯一撤掉的是本 diff 新加的 $HOME 项。
// 其余三面（agyHookLookupDirs / herdsman-go bin / agyStateBin）修复前本就经
// homeDir() 枚举环境 $HOME，收紧后同样撤销该项——非受信 $HOME 下原本可经
// homeDir() 收录的合法路径不再收录，此为用户批准的收紧方案一部分。
func trustedLookupHomes() []string {
	return trustedLookupHomesFor(trustedHomeDir())
}

// trustedLookupHomesFor 与 trustedLookupHomes 同口径，但 trustedHomeDir() 位置
// 由调用方显式给定（生产始终传 trustedHomeDir()；测试传入夹具宿主 home，
// 使二进制收录面枚举可在无真实 claude/agy 的干净 CI 上确定性覆盖）。
func trustedLookupHomesFor(hostHome string) []string {
	return dedupeTrustedHomes([]string{hostHome, "/root", "/root/.agy-pro-home"})
}

// agentLookupDirs 是解析 agy/pig/pi/claude 时除 PATH 与 standardBinDirs 外额外搜索的目录。
// 覆盖全部 trustedLookupHomes() 的受信宿主查找根（.local/bin、.pig/bin），
// 在隔离 HOME 下同样完整；绝不读 $HOME（调用方可控输入不作二进制收录的
// 信任边界输入），绝不纳入任意 PATH/TMPDIR 目录。
func agentLookupDirs() []string {
	return agentLookupDirsForHomes(trustedLookupHomes())
}

// agentLookupDirsForHomes 与 agentLookupDirs 同口径，但受信 home 根由调用方
// 显式给定；/root 两项作为既有底底显式补入（lookupHomes 已含 /root 时去重
// 后不重复）。
func agentLookupDirsForHomes(lookupHomes []string) []string {
	var out []string
	seen := make(map[string]struct{}, 8)
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
	for _, home := range lookupHomes {
		add(filepath.Join(home, ".local", "bin"))
		add(filepath.Join(home, ".pig", "bin"))
	}
	// /root 已在受信 home 根内，显式补两项仅作既有底底，去重后不重复。
	add("/root/.local/bin")
	add("/root/.pig/bin")
	return out
}

// entryAgentPathSet 将 EntryAgents 路径规范化为集合，供 Validate / seccomp 使用。
// 只收录清单原文路径，不再二次 EvalSymlinks：软链解析目标由
// ResolveEntryAgentPaths 在并入前过身份校验后直接列入清单；这里再解析会把
// 被校验拦截的毒软链目标（如 claude → dispatch-agent）重新放回有效白名单。
func entryAgentPathSet(paths []string) map[string]struct{} {
	out := make(map[string]struct{}, len(paths))
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
// 追加搜索的宿主目录，覆盖全部 trustedLookupHomes()（不读 $HOME）与硬编码 /root 两种 HOME 形态。
func agyHookLookupDirs() []string {
	seen := make(map[string]struct{}, 4)
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
	for _, home := range trustedLookupHomes() {
		add(filepath.Join(home, ".local", "bin"))
	}
	// /root 已在 trustedLookupHomes() 内，显式补一项仅作既有兜底，去重后不重复。
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
		for _, dir := range agentLookupDirs() {
			add(filepath.Join(dir, name))
		}
		for _, dir := range agyHookLookupDirs() {
			add(filepath.Join(dir, name))
		}
	}

	// herdsman 包装脚本 exec 到 herdsman-go 真实二进制；HERDSMAN_GO_BIN 优先于默认路径。
	// 覆盖全部 trustedLookupHomes()（不读 $HOME；含 /root 与 .agy-pro-home 显式项，
	// 确保 HOME=/root/.agy-pro-home 下同样解析到本体）与 /root 显式兜底。
	addHerdsmanGoBin := func(root string) {
		add(filepath.Join(root, ".local", "lib", "herdsman-go", "herdsman"))
	}
	if envBin := strings.TrimSpace(os.Getenv("HERDSMAN_GO_BIN")); envBin != "" {
		add(envBin)
	}
	for _, home := range trustedLookupHomes() {
		addHerdsmanGoBin(home)
	}
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
// standardBinDirs、agentLookupDirs()、agyHookLookupDirs()（覆盖 trustedLookupHomes() 与 /root 两种 HOME，不读 $HOME）
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
		for _, dir := range agentLookupDirs() {
			resolveExecutableCandidate(filepath.Join(dir, name), seen, &out)
		}
		for _, dir := range agyHookLookupDirs() {
			resolveExecutableCandidate(filepath.Join(dir, name), seen, &out)
		}
	}
	return out
}

// ResolveEntryAgentPaths 解析宿主上的 agy / pig / pi / claude 及其符号链接目标，供 ReviewerAgent 顶层启动。
// 不把 dispatch-agent / grok 纳入；嵌套禁止无法靠 Landlock 一次性放行实现，见 ReviewerAgent 注释。
func ResolveEntryAgentPaths() []string {
	return resolveEntryAgentPaths(trustedHomeDir())
}

// resolveEntryAgentPaths 是 ResolveEntryAgentPaths 的可注入宿主 home 版本：
// hostHome 取代 trustedHomeDir() 的位置参与安装根前缀判定（trustedAgentHomesFor）
// 与二进制收录面枚举（trustedLookupHomesFor）。生产路径传入真实宿主 HOME
// （/etc/passwd 解析，不读 $HOME），行为与内联调用 trustedHomeDir() 完全等价；
// 单测经包内 test seam 传入夹具宿主 home，使版本化 claude 安装根放行与
// agyStateBin 枚举断言摆脱宿主真实安装状态。
func resolveEntryAgentPaths(hostHome string) []string {
	// entryAgentAllowedNames 是软链目标身份白名单：names 成员外加 agy 状态目录下的
	// agentapi / webm_encoder（它们是 agy 本体经软链解析后可能命中的真实文件名）。
	// 已知边界：身份校验第 1 档（allowedTargets basename 命中）无条件放行、不看路径
	// 根——如 claude → /tmp/x/pi 这类 basename 巧合目标会被收编进 EntryAgents。真实
	// 可达需宿主 PATH 前置目录对调用者可写（接近已妥协场景），作为已知弱面记录在案。
	names := []string{"agy", "pig", "pi", "claude"}
	allowedTargets := make(map[string]struct{}, len(names)+2)
	for _, n := range names {
		allowedTargets[n] = struct{}{}
	}
	allowedTargets["agentapi"] = struct{}{}
	allowedTargets["webm_encoder"] = struct{}{}

	// entryAgentInstallRoots 枚举已知安装根前缀（含尾部分隔符）：软链
	// 解析目标只有位于这些根之下，才可经「含 /<查找名>/ 路径段」判定放行。
	// 根覆盖全部 trustedAgentHomesFor(hostHome)（当前 $HOME 与调用方给定宿主
	// home——生产即 trustedHomeDir() 宿主真实 HOME，在隔离测试或 systemd 单元
	// 改 HOME 时仍解析到宿主 /root/.local/share 等真实安装根）与硬编码 /root
	// 两个形态，去重。
	// 安装根含 homeDir() 系既有信任口径，维持不改；下方版本化 pig glob 与
	// agyStateBin 属二进制收录面，改用 trustedLookupHomes()（不读 $HOME）。
	lookupHomes := trustedLookupHomesFor(hostHome)
	trustedHomesWithRoot := dedupeStrings(append(append([]string{}, trustedAgentHomesFor(hostHome)...), "/root"))
	entryAgentInstallRoots := func() []string {
		sep := string(filepath.Separator)
		var roots []string
		seen := make(map[string]struct{}, 8)
		addRoot := func(root string) {
			root = filepath.Clean(strings.TrimSpace(root))
			if root == "" || root == "." {
				return
			}
			if _, ok := seen[root]; ok {
				return
			}
			seen[root] = struct{}{}
			roots = append(roots, root+sep)
		}
		for _, home := range trustedHomesWithRoot {
			addRoot(filepath.Join(home, ".local", "share"))
			addRoot(filepath.Join(home, ".local", "opt"))
			addRoot(filepath.Join(home, ".gemini"))
			addRoot(filepath.Join(home, ".agy-pro-home"))
		}
		for _, d := range standardBinDirs {
			addRoot(d)
		}
		for _, d := range agentLookupDirsForHomes(lookupHomes) {
			addRoot(d)
		}
		return roots
	}()

	// entryAgentTargetRejectReason 校验 EvalSymlinks 解析出的目标 rn 的身份，
	// name 为当前候选的查找名（不是任意白名单名），三层判定：
	//  1) basename(rn) 属于白名单名集合（names + agentapi/webm_encoder）→ 放行；
	//  2) 否则 basename(rn) 命中禁名守卫（isForbiddenBaseName：等于
	//     ForbiddenExecNames 成员，或长禁名的「禁名+"-"/"."」前缀派生，
	//     如 dispatch-agent-go.bak-* 这类备份形态）→ 一律拒；
	//  3) 否则仅当 rn 含 "/"+name+"/" 路径段且 rn 位于已知安装根前缀之下
	//     → 放行。
	// 禁名守卫置于路径段判定之前：basename 属于白名单的合法目标（如
	// pi → …/bin/pi、agy → …/pig 这类名字同被 ForbiddenExecNames 覆盖的
	// 入口）不受影响，而 dispatch-agent / dispatch-agent-go 本体即使落在
	// 含 /<name>/ 段的安装根下也被第 2 档挡死。版本化安装
	// claude → <root>/.local/share/claude/versions/<ver>（basename 是版本号）
	// 过第 3 档；/tmp/claude/evil 这类路径段巧合因不在安装根前缀内被拒。
	// 返回空串表示放行，否则返回分档拒绝原因（调用方按原因打 WARN，便于排障）。
	entryAgentTargetRejectReason := func(name, rn string) string {
		base := filepath.Base(rn)
		if _, ok := allowedTargets[base]; ok {
			return ""
		}
		if isForbiddenBaseName(base) {
			return "forbidden exec name"
		}
		sep := string(filepath.Separator)
		if !strings.Contains(rn, sep+name+sep) {
			return "missing /" + name + "/ path segment"
		}
		for _, root := range entryAgentInstallRoots {
			if strings.HasPrefix(rn, root) {
				return ""
			}
		}
		return "not in allowed roots"
	}

	seen := make(map[string]struct{})
	var out []string
	add := func(name, candidate string) {
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
		// 毒软链封堵必须在收录前闭合：先 EvalSymlinks 解析目标并做身份
		// 校验，只有目标通过才把软链本身（norm）与目标（rn）一并并入；
		// 目标不通过、解析失败或目标 Stat 失败（断链）时整个候选丢弃——
		// 否则 norm 进 EntryAgents→Exec 后 Landlock addPathRule 经 O_PATH
		// 跟随软链按目标 inode 授 EXECUTE，封堵形同虚设。
		// 普通文件 EvalSymlinks 返回自身，rn == norm 走同一路径收录。
		resolved, err := filepath.EvalSymlinks(norm)
		if err != nil || resolved == "" {
			fmt.Fprintf(os.Stderr, "sbx: entry-agent candidate %s symlink resolution failed, skipped\n", norm)
			return
		}
		rn, err := NormalizePath(resolved)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sbx: entry-agent candidate %s symlink resolution failed, skipped\n", norm)
			return
		}
		st, err := os.Stat(rn)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			fmt.Fprintf(os.Stderr, "sbx: entry-agent symlink target %s for %s not executable, skipped\n", rn, norm)
			return
		}
		if reason := entryAgentTargetRejectReason(name, rn); reason != "" {
			fmt.Fprintf(os.Stderr, "sbx: entry-agent symlink target %s for %s %s, skipped\n", rn, norm, reason)
			return
		}
		if _, ok := seen[norm]; !ok {
			seen[norm] = struct{}{}
			out = append(out, norm)
		}
		if rn != norm {
			if _, ok := seen[rn]; !ok {
				seen[rn] = struct{}{}
				out = append(out, rn)
			}
		}
	}

	for _, name := range names {
		if lp, err := exec.LookPath(name); err == nil {
			add(name, lp)
		}
		for _, dir := range standardBinDirs {
			add(name, filepath.Join(dir, name))
		}
		for _, dir := range agentLookupDirsForHomes(lookupHomes) {
			add(name, filepath.Join(dir, name))
		}
	}
	// 版本化 pig 安装根 glob 属二进制收录面：枚举 trustedLookupHomes()
	// （passwd-HOME + 硬编码 /root 与 /root/.agy-pro-home，不读 $HOME——$HOME
	// 是调用方可控输入，不得作为二进制收录的信任边界输入），隔离 HOME 下
	// 仍能命中宿主 opt。
	for _, globHome := range lookupHomes {
		if matches, err := filepath.Glob(filepath.Join(globHome, ".local", "opt", "pig", "*", "pig")); err == nil {
			for _, m := range matches {
				add("pig", m)
			}
		}
	}
	// agy stub (…/.local/bin/agy) 会再 exec 到 antigravity-cli/bin/agy（动态链接真实本体）；
	// agyStateBin 属二进制收录面，按 trustedLookupHomes() 枚举（不读 $HOME；含
	// /root/.agy-pro-home 显式项），隔离 HOME 下仍定位宿主状态目录。
	for _, agyHome := range lookupHomes {
		agyStateBin := filepath.Join(agyHome, ".gemini", "antigravity-cli", "bin")
		for _, name := range []string{"agy", "agentapi", "webm_encoder"} {
			add(name, filepath.Join(agyStateBin, name))
		}
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
// 根级状态文件是 os.ReadDir 快照枚举的瞬态面——宿主 agy 可能在枚举与
// Landlock Apply 的毫秒级窗口内经 WAL checkpoint 删除
// *.db-shm/*.db-wal/*.db-journal 或临时原子写文件，修复前此类瞬态消失
// 会在 Apply 时以 ENOENT 击穿整个沙盒（TOCTOU 竞态）。因此枚举出的根级
// 文件除加入 ReadWrite 外同步登记 OptionalReadWrite，Apply 时缺失仅安全
// 跳过；目录子项（含 agyStateDataSubdirs）保持严格 fail-closed 不登记。
func appendAgyStateReadWrite(p *Policy) {
	for _, agyStateDir := range agyAntigravityStateDirs() {
		// 状态根本体是软链/非真实目录时跳过整个根：跟随软链会把数据子目录与
		// 根级文件写权落到外部目录，叠加 Exec 重合即提权（oracle SF-4 局部）。
		if !isRealDirNoSymlink(agyStateDir) {
			continue
		}
		for _, sub := range agyStateDataSubdirs {
			d := filepath.Join(agyStateDir, sub)
			// 仅在目标不存在时创建；已存在的软链/非常规目录不被 MkdirAll
			// 静默接受，改由 resolvePolicyPathWithin 的 Lstat/EvalSymlinks/
			// Exec 重合校验把关（对齐 appendCtxmodeStateReadWrite 写法）。
			if _, err := os.Lstat(d); os.IsNotExist(err) {
				_ = os.MkdirAll(d, 0o755)
			}
			// Lstat 拒软链本体 + 严格位于 agyStateDir 内 + 不与 Exec 重合或
			// 互为父子，彻底闭合 W^X 写穿；任一不通过即不放行。
			if resolved := resolvePolicyPathWithin(d, agyStateDir, p); resolved != "" {
				p.ReadWrite = append(p.ReadWrite, resolved)
			}
		}
		if entries, err := os.ReadDir(agyStateDir); err == nil {
			for _, ent := range entries {
				name := ent.Name()
				// bin/ 必须保持只读+Exec；agy-pro-home 下 bin 常为指向真实 ELF 目录的符号链接，
				// DirEntry.IsDir 对符号链接为 false，须按名字显式跳过，避免 W^X 写穿。
				// 根级符号链接（含指向目录/文件的软链）一律跳过——状态文件均为普通文件，
				// 放行软链会与 Exec 重合叠出 RWX 提权面。
				if ent.IsDir() || ent.Type()&os.ModeSymlink != 0 || name == "bin" {
					continue
				}
				// 根级状态文件（settings.json / history.jsonl / antigravity-oauth-token /
				// *.db 及其 SQLite 瞬态伴侣 *.db-shm / *.db-wal / *.db-journal 等）
				rw := filepath.Join(agyStateDir, name)
				p.ReadWrite = append(p.ReadWrite, rw)
				// 快照枚举文件属瞬态面：Apply 时若已被宿主进程删除，仅按
				// OptionalReadWrite 容忍 ENOENT 跳过（见 Policy.OptionalReadWrite）。
				p.OptionalReadWrite = append(p.OptionalReadWrite, rw)
			}
		}
	}
}

// ReviewerAgent 构造「只读审查员代理」预设：在 ReadOnlyReviewer 基础上允许顶层启动 agy/pig/claude，
// 并开放代理会话状态目录写权限。
//
// 额外能力：
//   - Exec：agy、pig、pi、claude（及符号链接目标；目标须过身份校验——basename 或路径段
//     命中白名单名，防 PATH 前置毒软链混入）+ mkdir/cp/touch；
//     agy 生命周期钩子二进制（herdsman 及其 herdsman-go ELF 本体、agy-rules-hook、moshi-hook、
//     agy-recover-400、agy-recall，含软链目标），否则 SessionStart/PreInvocation/PreToolUse/
//     PostToolUse/PreCompress/Stop 钩子在沙盒内被 Landlock 拦截，只读子代理无法向 Herdsman
//     注册会话/上报 agent.turn.completed；
//   - Exec：pig / agy MCP 生态服务二进制（ctxmode / codegraph-go / jev-mcp / jev-mcp-go /
//     agy-recall，含软链解析目标），仍不整目录放行 ~/.local/bin；
//   - ReadWrite：/tmp/herdr-role-sessions（pig --session-dir）与
//     /tmp/herdr-role-reports（dispatch-agent 派发报告文档根）；agy 状态为 ~/.gemini/antigravity-cli
//     与 /root/.agy-pro-home/.gemini/antigravity-cli 下数据子目录 + 根目录已有状态文件
//     （不含 bin/，避免对可执行文件 W^X 写穿）；以及 /dev/tty（dispatch-agent-go 对 agy 使用
//     --prompt-interactive / -i，bubbletea 需要打开控制终端）；
//   - ReadWrite（ctxmode/MCP 数据隔离）：CTXMODE_DB 未显式指定时注入
//     <privateTmpDir>/ctxmode/context_mode.db（沙盒私有 0700）；宿主兜底仅精确放行
//     ~/.local/share/ctxmode 与 ~/.gemini/config/plugins/agy-recall/state（含
//     /root/.agy-pro-home 对应目录），绝不放宽到 ~/.local/share 或 gemini-web；
//   - ReadWrite（pig AgentDir 私有隔离）：宿主 AgentDir 保持 100% 只读；
//     进入 Landlock 前把必要配置文件 0600 拷入 <privateTmpDir>/pig-agent，并在私有
//     settings.json 中显式声明宿主扩展目录绝对路径（不再用 extensions 软链，避免
//     cfg.Source 变成随机私有路径而脱靶 packed-go 打包缓存），注入 PIG_CODING_AGENT_DIR；
//     私有副本与预热经 resolveSourcePigAgentDir 对齐同一个实际生效 AgentDir
//     （PIG_CODING_AGENT_DIR / 有效 PIG_USE_PI_DIRS=1 时的 PI_CODING_AGENT_DIR，
//     缺省 <PIG_HOME>/agent）；Policy.Env 同时固定注入解析出的 PIG_HOME，
//     防止 --env PIG_HOME= 空覆盖或子进程 HOME 差异造成缓存根漂移；
//   - Env/ReadWrite（XDG_RUNTIME_DIR 私有隔离）：宿主 /run/user/0（systemd user bus、
//     keyring 等敏感套接字）绝不进 ReadWrite；在 <privateTmpDir> 下预建 0700 的短名
//     私有运行目录 rt（短名 rt 保证 XDG socket 路径远低于 sun_path 108 字节上限），
//     并覆写注入 XDG_RUNTIME_DIR=<privateTmpDir>/rt——sbx mergeEnv 按同名键覆盖，
//     宿主 os.Environ() 中已有的 XDG_RUNTIME_DIR=/run/user/0 被确定性替换，
//     pig 扩展加载期 mkdir /run/user/0/pig/... 的 EACCES 不再发生；
//   - ReadWrite/Exec（Cell 缓存 W^X）：宿主态预刷新 <pigHome>/cache 下缓存单元 usage.json
//     （TouchUsage <1h 只读短路）；仅 cells/<lang>/.locks、ext/.locks 与 toolchains 锁文件
//     可写，仅已构建 <digest>/runner 与 cache/ext 下可执行产物可执行；
//   - Env/ReadWrite（claude 私有配置目录）：CC（claude）会话启动必写配置目录。
//     注入 CLAUDE_CONFIG_DIR=<privateTmpDir>/claude（0700，每会话私有
//     目录，加入 ReadWrite）后 CC 的配置根整体切到私有目录，写面随之
//     收敛；目录随 privateTmpDir 清理、无跨会话状态——CC 会话状态本
//     就不需跨会话持久，固定共享目录会让沙盒内 claude 写的
//     settings/hooks/mcpServers 被下一次会话原样继承（跨会话投毒通道）；
//     与 pig-agent/ctxmode/XDG_RUNTIME_DIR 全落 privateTmpDir 的
//     每会话隔离口径一致。唯一例外：私有配置目录下预置
//     projects → <herdrRoleSessionsRoot>/claude-projects（0700，该共享根
//     已在 ReadWrite 白名单内）符号链接，projects/ 会话转录按既定口径
//     跨会话汇聚共享写根；根级 .claude.json / settings.json 仍每会话私有。
//     宿主 ~/.claude 与 ~/.claude.json 仅随通用
//     只读根顺带可读、CC 并不消费其中配置（其内容也属不可信面，
//     不能放给沙盒内写）；
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
// ReviewerAgent 构造「只读审查员代理」预设：等价于 ReviewerAgentWithEnv(workdir, nil)。
func ReviewerAgent(workdir string) Policy {
	return ReviewerAgentWithEnv(workdir, nil)
}

// ReviewerAgentWithEnv 与 ReviewerAgent 相同，但额外接收调用方 --env 注入
// 列表 cfgEnv：PIG_HOME 按 resolvePigHomeDir 语义解析（最后一项 PIG_HOME 键，
// 显式空覆盖回退默认 <trustedHome>/.pig），与 MaybeWarmGoExtensionCells 共用，
// 保证预热指纹/缓存根与沙盒策略引用同一个 PIG_HOME；解析结果同时固定注入
// Policy.Env，避免 --env 空覆盖或宿主/子进程 HOME 差异导致沙盒内缓存根漂移。
// 显式 AgentDir（PIG_CODING_AGENT_DIR / PI_CODING_AGENT_DIR）经
// resolveSourcePigAgentDir 与预热路径完全对齐，私有副本从其克隆。
func ReviewerAgentWithEnv(workdir string, cfgEnv []string) Policy {
	p := ReadOnlyReviewer(workdir)

	extraTools := BuildExecWhitelist([]string{"mkdir", "cp", "touch"})
	agents := ResolveEntryAgentPaths()
	hooks := ResolveAgyHookPaths()
	p.EntryAgents = agents
	p.Exec = append(p.Exec, extraTools...)
	p.Exec = append(p.Exec, agents...)
	p.Exec = append(p.Exec, hooks...)

	// 共享可写根白名单：会话根（pig --session-dir）与报告根
	//（dispatch-agent DISPATCH_REPORT_PATH 落盘目录）分根并列；两者均先
	// MkdirAll 再 Lstat 确认本体是真实目录而非预置符号链接后才放行写权，
	// 防止软链指向宿主任意路径时把共享写面旁路过去（容忍降级）。
	for _, sharedRoot := range []string{herdrRoleSessionsRoot, herdrRoleReportsRoot} {
		_ = os.MkdirAll(sharedRoot, 0o700)
		if st, lstatErr := os.Lstat(sharedRoot); lstatErr == nil && st.Mode().IsDir() {
			p.ReadWrite = append(p.ReadWrite, sharedRoot)
		}
	}

	// agy 状态：只开放数据子目录，不开放 bin/（真实 ELF 在 bin/agy，须保持只读+Exec，避免 W^X 被写穿）。
	// 同时覆盖 homeDir() 与 agy 硬编码 HOME（/root/.agy-pro-home），否则派活路径写 log/token 会 EACCES。
	appendAgyStateReadWrite(&p)

	// dispatch-agent-go reviewer 对 agy 使用 -i（--prompt-interactive），bubbletea 需要 /dev/tty。
	// 仅本预设放行；不改全局 defaultNullDevices，避免扩大其他预设攻击面。
	p.ReadWrite = append(p.ReadWrite, "/dev/tty")

	// 只读角色回报通道：在 goExecDir/bin 下建指向 sbx 本体的 hc 软链，
	// 经 landlock_linux.go init 分发进入受限客户端（仅接受 hc send <addr> <text>，
	// 提交给沙盒外 report.sock 中继）。仅本预设；ReadOnlyReviewer 与 --preset reviewer
	// 不建此链接。真 /root/.local/bin/hc 不进任何 Exec 白名单。
	if goExecDir := lookupEnvValue(p.Env, envGoExecDirKey); goExecDir != "" {
		if selfExe, err := os.Executable(); err == nil && selfExe != "" {
			if resolvedExe, err := filepath.EvalSymlinks(selfExe); err == nil && resolvedExe != "" {
				selfExe = resolvedExe
			}
			if normExe, err := NormalizePath(selfExe); err == nil && !IsForbiddenExec(normExe) && !IsELFInterpreter(normExe) {
				_ = os.Symlink(normExe, filepath.Join(goExecDir, "bin", "hc"))
			}
		}
	}

	// privateTmpDir 由 ReadOnlyReviewer 创建并记录在 Policy.Env 的 TMPDIR 中。
	privateTmpDir := lookupEnvValue(p.Env, "TMPDIR")

	// claude 私有配置目录：CC 会话必写配置目录；注入 CLAUDE_CONFIG_DIR 后
	// CC 配置根整体切到该私有目录，写面随之收敛。目录落在
	// <privateTmpDir>/claude（0700，每会话私有）：CC 会话状态本就不需
	// 跨会话持久，固定共享目录会让沙盒内 claude 写的
	// settings/hooks/mcpServers 被下一次会话原样继承（跨会话投毒通道），
	// 与 pig-agent/ctxmode/XDG_RUNTIME_DIR 全落 privateTmpDir 的每会话
	// 隔离口径一致；目录随 privateTmpDir 清理、无跨会话状态。唯一例外：
	// 其下预置 projects → claudeSharedProjectsRoot 符号链接（见
	// preparePrivateClaudeConfigDir），projects/ 会话日志跨会话汇聚共享
	// 可写根 /tmp/herdr-role-sessions/claude-projects；根级 .claude.json /
	// settings.json 仍每会话私有。宿主 ~/.claude 与 ~/.claude.json 仅随通用
	// 只读根顺带可读、CC 不消费其配置，且其内容属不可信面，不能放给沙盒内写。
	claudeConfigDir := preparePrivateClaudeConfigDir(privateTmpDir)
	p.ReadWrite = append(p.ReadWrite, claudeConfigDir)
	p.Env = overrideEnvKey(p.Env, "CLAUDE_CONFIG_DIR", claudeConfigDir)

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

	// 安全细则二：pig AgentDir 私有隔离。宿主 AgentDir 保持 100% 只读，
	// 沙盒内 pig 经 PIG_CODING_AGENT_DIR 指向 privateTmpDir 下的私有副本。
	// PIG_HOME 固定回注 Policy.Env：mergeEnv 允许 --env PIG_HOME= 空覆盖把空串
	// 写进最终 env，沙盒内 pig 会按外部 HOME 另算缓存根而打穿预热/策略对齐；
	// 显式 AgentDir 与预热同口径解析，不可信（软链/权限/属主不符）时回退默认
	// <pigHome>/agent——沙盒侧只复制其配置文件，不做预热级硬拒绝。
	pigHome := resolvePigHomeDir(cfgEnv)
	// SF-1（方案 1A）：与预热 MaybeWarmGoExtensionCells 同口径执行
	// validatePigHomeDir——软链/非目录/0777 公共可写/异属主的 PIG_HOME
	// 一律不可信。不可信时绝不读取 <pigHome>/agent、绝不刷新其 usage
	// 元数据、绝不把其 cache 产物枚举进 Policy.Exec、绝不做宿主
	// bin/docs/state 投影，直接退回空私有 pig-home，并向
	// reviewerAgentCellTargets 记录错误状态——顶层命令为真实 pig 入口时
	// buildPolicy 启动前 fail-closed（ReviewerAgentCellTargetError），
	// 非 pig 命令（claude/agy/sh）照常以空私有根放行。
	pigHomeTrusted := validatePigHomeDir(pigHome)
	sel := resolveAgentDirSelection(pigHome, cfgEnv)
	if (!pigHomeTrusted && !sel.trustedCustom) || !sel.sourceTrusted {
		// 不可信 pigHome 下默认层回退目录 <pigHome>/agent 同属不可信域，
		// 且 sourceDir 本体未过受信校验（默认 <pigHome>/agent 为软链/
		// 0777/异属主）时同样回退：复制/扩展发现源一律改为空私有
		// pig-agent，绝不读取不可信 <pigHome>/agent。显式 cfgEnv 受信
		// 自定义 AgentDir（sel.trustedCustom）独立于 pigHome 校验通过，
		// 按既有显式层规则保留。
		sel.sourceDir = filepath.Join(privateTmpDir, "pig-agent")
	}
	// sel.sourceDir 是配置复制与扩展发现的来源（单次选源结果，与预热
	// MaybeWarmGoExtensionCells 完全共用）；只有「cfgEnv 显式声明 + 受信」
	// 的自定义目录（sel.trustedCustom）才能成为最终有效 AgentDir——宿主
	// 环境同名键属残留面，仅能作复制源，不得成为有效目录（FM-4 口径）。
	preparePrivatePigAgentDirFromAgent(filepath.Join(privateTmpDir, "pig-agent"), sel.sourceDir)
	privatePigAgentDir := filepath.Join(privateTmpDir, "pig-agent")
	// 预先在 Policy.Env 写入有效 AgentDir 判定结果，供 buildPolicy 在
	// cfgEnv merge 后统一归一化：
	//   - trustedCustom：有效目录 = sel.sourceDir（受信自定义保留；经
	//     absCleanPath 规范化，相对路径/冗余分隔/点段写法同样生效）；
	//   - 其余：有效目录 = privatePigAgentDir（不可信被拒/缺省回钉）。
	// 不可信路径不得经 cfgEnv merge 原样生效（oracle FM-4/M2）。
	// 写入顺序：inject 先行（清理宿主残留 PI 键、钉私有），有效目录随后
	// 覆盖——预热、配置复制与最终 exec 使用同一个选源结果。
	effectiveAgentDir := privatePigAgentDir
	if sel.trustedCustom {
		effectiveAgentDir = sel.sourceDir
	}
	p.Env = injectPigCodingAgentDir(p.Env, privatePigAgentDir, cfgEnv)
	p.Env = overrideEnvKey(p.Env, "PIG_CODING_AGENT_DIR", effectiveAgentDir)
	p.Env = overrideEnvKey(p.Env, "PI_CODING_AGENT_DIR", effectiveAgentDir)

	// 安全细则三：私有 PIG_HOME（<privateTmpDir>/pig-home，0700，RW，不进 Exec）。
	// 宿主 PIG_HOME 保持只读；宿主态预刷新宿主缓存单元 usage.json（沙盒内
	// TouchUsage 命中 <1h 只读短路），并把宿主已构建缓存单元产物逐文件枚举进
	// Policy.Exec——这两步仍针对宿主 pigHome（缓存单元本体在宿主），与
	// appendCellCachePolicy 内部 resolvePolicyPathWithin 的 W^X 校验口径一致。
	// 之后把 PIG_HOME env 重定向到私有根：私有 cache 下 cells/<lang> 与 ext 是
	// 真实可写目录，承载 .build-* scratch 与 .locks；宿主已构建 <digest>/<entry>
	// 经同名软链投影进私有目录，沙盒内 pig 命中软链后 exec 的是宿主真实 runner
	// 路径（已在 Exec 白名单），而 cells/<lang>/ext 目录本身不进 Exec——私有新建的
	// 自建产物没有可执行授权，私有可写区维持 W^X。
	// 不可信 PIG_HOME（SF-1）：绝不刷新其 usage.json（不写不可信域），也绝不把
	// 其缓存产物枚举进 Policy.Exec（0777/软链根下任意可执行文件不得获执行授权）。
	if pigHomeTrusted {
		refreshCellUsageMetadata(pigHome, time.Now())
		appendCellCachePolicy(&p, pigHome)
	}

	// 目标集枚举与完整性校验（启动前 fail-closed 输入）：扩展集经
	// sourceAgentGoExtensionDirs 枚举（与预热指纹/沙盒内扩展发现同口径）；宿主
	// 有效缓存单元产物经 cellCacheEntryDirs+validCellEntryDir 枚举。逐项把宿主
	// 产物以软链投影进私有 cache，并校验每个 artifact 经软链解析后的宿主真实
	// 路径已入 Policy.Exec。校验结果记入 reviewerAgentCellTargetState，由
	// buildPolicy 在启动前读取：有 Go 扩展但宿主产物为 0，或存在未成功投影/Exec
	// 未命中项时，返回错误拒绝启动并指名缺失项。
	execSet := make(map[string]struct{}, len(p.Exec))
	for _, ep := range p.Exec {
		execSet[filepath.Clean(ep)] = struct{}{}
		if resolved, err := filepath.EvalSymlinks(ep); err == nil && resolved != "" {
			execSet[filepath.Clean(resolved)] = struct{}{}
		}
	}
	settings := readAgentSettings(sel.sourceDir)
	extDirs := sourceAgentGoExtensionDirs(sel.sourceDir, settings)
	hostCache := filepath.Join(pigHome, "cache")
	// total 必须按「language=go 且可执行产物」过滤：宿主 cache/ext 下
	// node-runtime-* 等非 Go 产物（language!="go"）或无执行位的条目
	// 绝不能计入目标集，否则冷缓存 + 非 Go cell 会让 total>0 假通过，
	// fail-closed 被完全旁路（oracle FM-2C）。枚举谓词与投影/Exec 完整性
	// 校验完全一致（cellEntryHiddenOrSymlink）：cells/go 语言根为软链时
	// total 同样不计入，杜绝「投影为空但 total>0 假放行」的软链逃逸。
	// 不可信 PIG_HOME（SF-1）：total 恒 0，不向 preparePrivatePigHome 传
	// 不可信宿主根（绝不建指向不可信域的 bin/docs/state/cache 投影软链），
	// 直接以空私有 pig-home 返回，并向目标集状态记录错误项——真实 pig 入口
	// 启动前 fail-closed，非 pig 命令以空私有根照常放行。
	total := 0
	privatePigHome := filepath.Join(privateTmpDir, "pig-home")
	var missing []string
	pigHomeUntrusted := false
	if pigHomeTrusted {
		total = countValidExecutableGoArtifacts(hostCache)
		privatePigHome, missing = preparePrivatePigHome(privateTmpDir, pigHome, execSet)
	} else {
		pigHomeUntrusted = true
		var prepErr error
		privatePigHome, prepErr = prepareEmptyPrivatePigHome(privateTmpDir)
		missing = append(missing,
			"untrusted PIG_HOME (symlink/not-dir/group-writable/foreign-owner): "+pigHome)
		if prepErr != nil {
			missing = append(missing, "pig-home: "+prepErr.Error())
		}
	}
	// 完整性校验复用同一目标枚举：total 与 missing 都必须只覆盖可执行
	// Go 目标——投影侧已把非执行/非 Go 条目仅作可读投影不计入 missing。
	// 再显式复核「宿主可执行 Go 产物 × 私有投影」一致性：宿主任一有效
	// Go 产物的相对路径在私有 cache 下必须是软链投影（validCellEntryDir
	// 可解析到 artifact），否则意味着枚举口径不一致（如 cells/go 软链
	// 被 total 计入而投影跳过），必须记入 missing fail-closed，而非让
	// 零投影门禁假放行。该检查只走 cellCacheEntryDirs 同一枚举，不重扫。
	if len(extDirs) > 0 && total > 0 {
		privCacheRoot := filepath.Join(privatePigHome, "cache")
		missing = append(missing, verifyProjectedGoCellTargets(hostCache, privCacheRoot)...)
	}
	setReviewerAgentCellTargets(&reviewerAgentCellTargets{
		// 不可信 PIG_HOME 时 hasGoExt 强制为 true：即使空私有 AgentDir
		// 无 Go 扩展（extDirs 为空），missing 中的不可信 PIG_HOME 错误项
		// 也必须让 ReviewerAgentCellTargetError 返回非 nil——真实 pig 入口
		// 启动前 fail-closed，绝不静默放行（SF-1）。
		hasGoExt: len(extDirs) > 0 || pigHomeUntrusted,
		total:    total,
		missing:  missing,
	})
	p.Env = overrideEnvKey(p.Env, "PIG_HOME", privatePigHome)

	// 安全细则四：XDG_RUNTIME_DIR 私有隔离。宿主 /run/user/0 内含 systemd user bus、
	// keyring 等敏感套接字，绝不加入 ReadWrite；改为在 privateTmpDir 下预建 0700 的
	// 短名运行目录 rt（短名 rt 确保 Unix domain socket 路径远低于 sun_path 108 字节
	// 上限），并用 overrideEnvKey 覆写注入 XDG_RUNTIME_DIR。sbx mergeEnv 按同名键
	// 覆盖宿主 os.Environ()，因此即使宿主已有 XDG_RUNTIME_DIR=/run/user/0 也会被
	// 确定性替换，不留双键残留；pig 扩展加载写 /run/user/0/pig/... 的 EACCES 消除。
	privateRtDir := preparePrivateRuntimeDir(privateTmpDir)
	p.Env = overrideEnvKey(p.Env, "XDG_RUNTIME_DIR", privateRtDir)

	return p
}

// resolvePigHomeDir 是 reviewer-agent 预热与策略构造共用的 PIG_HOME 解析：
// 从后往前找 cfgEnv（--env KEY=VALUE 解析结果）中最后一个 PIG_HOME 键：
//   - 非空值：返回其绝对路径（命中即停，不回扫更早被覆盖的条目）；
//   - 显式空串（如 --env PIG_HOME=A --env PIG_HOME=）：空覆盖语义等同未设置，
//     直接返回默认 <trustedHome>/.pig，绝不回读宿主 os.Getenv("PIG_HOME")；
//   - cfgEnv 完全不含 PIG_HOME 键：才读宿主环境变量，缺省 <trustedHome>/.pig。
func resolvePigHomeDir(cfgEnv []string) string {
	for i := len(cfgEnv) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(cfgEnv[i], "=")
		if !ok || k != "PIG_HOME" {
			continue
		}
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return absCleanPath(trimmed)
		}
		return filepath.Join(trustedHomeDir(), ".pig")
	}
	return pigHomeDir()
}

// absCleanPath 返回路径的绝对化+规范化形式；filepath.Abs 失败退化为 Clean。
func absCleanPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// pigHomeDir 返回宿主 PIG_HOME：$PIG_HOME 优先，其次 <trustedHome>/.pig。
// 宿主 HOME 经 os/user.LookupId(euid) 获取（trustedHomeDir），不读
// os.Getenv("HOME")——调用方环境的 HOME 可能被外部篡改。
func pigHomeDir() string {
	if v := strings.TrimSpace(os.Getenv("PIG_HOME")); v != "" {
		return absCleanPath(v)
	}
	return filepath.Join(trustedHomeDir(), ".pig")
}

// cfgEnvLastValue 返回 cfgEnv 中最后一个 k 键的值：从后往前扫描命中即停。
// found 区分「键存在但显式空串（空覆盖屏蔽宿主同名环境变量）」与「键完全缺失」。
func cfgEnvLastValue(cfgEnv []string, key string) (val string, found bool) {
	for i := len(cfgEnv) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(cfgEnv[i], "=")
		if !ok || k != key {
			continue
		}
		return v, true
	}
	return "", false
}

// agentDirOrigin 记录 AgentDir 选中的来源层级（精确来源/键，单次选源消费）。
type agentDirOrigin uint8

const (
	// agentDirOriginDefault 缺省回退层：<pigHome>/agent，无显式/宿主声明。
	agentDirOriginDefault agentDirOrigin = iota
	// agentDirOriginHostEnv 宿主环境变量回退层：仅作配置复制/扩展发现源，
	// 永不成为最终有效目录（宿主残留面）。
	agentDirOriginHostEnv
	// agentDirOriginCfgEnv --env 显式声明层：通过校验后可成为最终有效目录。
	agentDirOriginCfgEnv
)

// resolvedAgentDir 是 AgentDir 单次选源的结构化结果：哪个键、候选路径、
// 复制/扩展发现来源（sourceDir）、是否通过受信校验（srcValid）、最终来源
// 目录本体是否可信（sourceTrusted）、是否为显式受信自定义（trustedCustom）。
// 预热、配置复制与最终 exec 共用同一结果，杜绝「resolver 选 A、
// customFromCfgEnv 判 B」的分裂（oracle MF-1 / reviewer5 F1）。
type resolvedAgentDir struct {
	origin        agentDirOrigin // 来源层级（Default/HostEnv/CfgEnv）
	key           string         // 实际选中的环境变量键（Default 层为空）
	candidateDir  string         // 校验前的规范化候选路径（Default 层为空）
	sourceDir     string         // 配置复制/扩展发现来源（校验失败回退默认）
	srcValid      bool           // 候选是否通过 validateTrustedPigDir
	sourceTrusted bool           // sourceDir 本体是否通过 validateTrustedPigDir
	trustedCustom bool           // 是否为显式（cfgEnv）受信自定义有效目录
}

// resolveAgentDirSelection 是 AgentDir 的唯一选源入口：一次性产出精确
// 来源/键/候选/已校验来源/是否显式受信。三级优先级：
//
//  1. 显式层（origin=CfgEnv）：
//     a. cfgEnv 最后一项 PIG_CODING_AGENT_DIR 非空 → 选中该值；
//     b. 否则（含 --env PIG_CODING_AGENT_DIR= 显式空覆盖！）仅当 cfgEnv
//     同时显式声明有效 PIG_USE_PI_DIRS=="1" 且 PI_CODING_AGENT_DIR 非空
//     → 选中 PI 值。PIG_USE_PI_DIRS=1 仅来自宿主环境时不得激活本层
//     （宿主开关只参与第 2 层判定），否则宿主残留会借显式 PI 值冒认
//     受信自定义。
//     显式 PIG 空覆盖绝不屏蔽显式 PI 入口；显式 PI 入口优先于宿主
//     残留 PIG（宿主 PIG_CODING_AGENT_DIR 不得冒认受信自定义）。
//  2. 宿主回退层（origin=HostEnv）：仅当 cfgEnv 未出现对应键时读宿主
//     环境变量；PIG_CODING_AGENT_DIR 优先，其次有效 PIG_USE_PI_DIRS=1 下
//     的 PI_CODING_AGENT_DIR。通过校验后仅作复制/扩展发现源，永不生效目录。
//  3. 默认层（origin=Default）：<pigHome>/agent。
//
// 对非 Default 候选仅调一次 validateTrustedPigDir：通过则 srcValid=true
// 且仅 CfgEnv 来源才置 trustedCustom；失败则 sourceDir 回退默认目录。
func resolveAgentDirSelection(pigHome string, cfgEnv []string) resolvedAgentDir {
	defaultDir := filepath.Join(pigHome, "agent")
	sel := resolvedAgentDir{origin: agentDirOriginDefault, sourceDir: defaultDir}

	pigV, pigInCfg := cfgEnvLastValue(cfgEnv, "PIG_CODING_AGENT_DIR")
	piDirsV, piDirsInCfg := cfgEnvLastValue(cfgEnv, "PIG_USE_PI_DIRS")
	piV, piInCfg := cfgEnvLastValue(cfgEnv, "PI_CODING_AGENT_DIR")

	// 显式层（CfgEnv）的 PI 入口激活开关必须同样来自 cfgEnv 显式声明：
	// 宿主环境里的 PIG_USE_PI_DIRS=1 只是残留面，绝不能凭它把 cfgEnv 的
	// PI_CODING_AGENT_DIR 标为 agentDirOriginCfgEnv（trustedCustom=true 让
	// 宿主开关越权激活显式受信语义）。宿主开关仅在第 2 层（HostEnv）
	// 参与判定——仅作复制/预热源，永不成为有效目录。
	usePiDirsCfg := piDirsInCfg && strings.TrimSpace(piDirsV) == "1"
	usePiDirs := usePiDirsCfg
	if !piDirsInCfg {
		usePiDirs = strings.TrimSpace(os.Getenv("PIG_USE_PI_DIRS")) == "1"
	}

	// 1. 显式层：PIG 非空优先；显式空覆盖不屏蔽显式 PI 入口。
	if pigInCfg && strings.TrimSpace(pigV) != "" {
		sel.origin, sel.key = agentDirOriginCfgEnv, "PIG_CODING_AGENT_DIR"
		sel.candidateDir = absCleanPath(strings.TrimSpace(pigV))
	} else if usePiDirsCfg && piInCfg && strings.TrimSpace(piV) != "" {
		// cfgEnv 显式 PI 入口（含 PIG 空覆盖并存）：归 CfgEnv 仅当 PI 键
		// 与有效 PIG_USE_PI_DIRS=1 开关均在 cfgEnv 内显式声明。
		sel.origin, sel.key = agentDirOriginCfgEnv, "PI_CODING_AGENT_DIR"
		sel.candidateDir = absCleanPath(strings.TrimSpace(piV))
	}

	// 2. 宿主回退层：仅当显式层未命中时评估（宿主值仅作复制源）。
	if sel.origin == agentDirOriginDefault {
		switch {
		case !pigInCfg && strings.TrimSpace(os.Getenv("PIG_CODING_AGENT_DIR")) != "":
			sel.origin, sel.key = agentDirOriginHostEnv, "PIG_CODING_AGENT_DIR"
			sel.candidateDir = absCleanPath(strings.TrimSpace(os.Getenv("PIG_CODING_AGENT_DIR")))
		case usePiDirs && !piInCfg && strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")) != "":
			sel.origin, sel.key = agentDirOriginHostEnv, "PI_CODING_AGENT_DIR"
			sel.candidateDir = absCleanPath(strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")))
		}
	}

	// 单次受信校验：仅非 Default 候选。通过则 sourceDir=candidateDir，
	// 且仅 CfgEnv 来源才成为显式受信自定义（HostEnv 永不生效目录）；
	// 被拒时 sourceDir 回退默认目录且该回退目录须另行校验（sourceTrusted）
	// ——默认 <pigHome>/agent 为软链/0777/异属主时同样不可作配置复制源。
	if sel.origin == agentDirOriginDefault {
		ok := validateTrustedPigDir(defaultDir, "AgentDir")
		sel.srcValid = ok
		sel.sourceTrusted = ok
	} else if sel.candidateDir != "" {
		if validateTrustedPigDir(sel.candidateDir, "AgentDir") {
			sel.srcValid = true
			sel.sourceTrusted = true
			sel.sourceDir = sel.candidateDir
			sel.trustedCustom = sel.origin == agentDirOriginCfgEnv
		} else {
			sel.srcValid = false
			sel.sourceDir = defaultDir
			sel.sourceTrusted = validateTrustedPigDir(defaultDir, "AgentDir")
		}
	}
	return sel
}

// resolveSourcePigAgentDir 是 resolveAgentDirSelection 的薄包装，供包内
// 既有单测与预热调用：返回 (sourceDir, 是否显式非空候选)。custom=true
// 表示存在显式/宿主自定义候选（无论是否通过校验）——与旧 resolver 语义
// 兼容，零逻辑分叉。
func resolveSourcePigAgentDir(pigHome string, cfgEnv []string) (srcAgentDir string, custom bool) {
	sel := resolveAgentDirSelection(pigHome, cfgEnv)
	return sel.sourceDir, sel.origin != agentDirOriginDefault
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

// preparePrivateClaudeConfigDir 创建 <privateTmpDir>/claude（0700）并返回路径。
// CC 配置根落在每会话私有的 privateTmpDir 下：.claude.json / settings.json
// 等根级配置文件随 privateTmpDir 清理、无跨会话状态，防止沙盒内 claude
// 写入的 settings/hooks/mcpServers 被下一次会话原样继承（跨会话投毒通道）。
// 唯一例外是 projects/ 会话日志目录：项目转录需跨会话汇聚，预先以 0700
// 创建共享根 claudeSharedProjectsRoot（<herdrRoleSessionsRoot>/claude-projects，
// 该根已在 ReadWrite 白名单内），并在私有配置目录下预置
// projects → claudeSharedProjectsRoot 符号链接；沙盒内 claude 经
// <CLAUDE_CONFIG_DIR>/projects/<project-slug>/<uuid>.jsonl 创建的子目录与
// 普通文件实际落盘到共享根，与每会话私有隔离口径互不影响。
// 软链创建失败时容忍降级（该次会话 projects/ 回退为私有目录内自建路径），
// 与同类 prepare* 辅助函数的错误处理口径一致。
func preparePrivateClaudeConfigDir(privateTmpDir string) string {
	dir := filepath.Join(privateTmpDir, "claude")
	if err := os.MkdirAll(dir, 0o700); err == nil {
		_ = os.Chmod(dir, 0o700)
	}
	// 先确认共享根的父目录（herdrRoleSessionsRoot）本体是真实目录：
	// 父目录被预置为符号链接时，直接 MkdirAll(claudeSharedProjectsRoot) 会
	// 跟随父软链在外部目标下创建 claude-projects 子目录。与
	// ReviewerAgentWithEnv 对 sessionRoot 的 Lstat 口径一致：父目录
	// 缺失或为软链/非目录时容忍降级直接返回，既不跟随父软链建目录，
	// 也不预置 projects 软链。
	parentDir := filepath.Dir(filepath.Clean(claudeSharedProjectsRoot))
	_ = os.MkdirAll(parentDir, 0o700)
	pst, lstatErr := os.Lstat(parentDir)
	if lstatErr != nil || !pst.Mode().IsDir() {
		return dir
	}
	if err := os.MkdirAll(claudeSharedProjectsRoot, 0o700); err == nil {
		// Lstat 校验共享根本体是真实目录而非预置符号链接：被预置为软链时
		// 绝不跟随其对外部目标 Chmod，也不预置 projects 软链，容忍降级
		// （该次会话 projects/ 回退为私有目录内自建路径）。
		if st, lstatErr := os.Lstat(claudeSharedProjectsRoot); lstatErr == nil && st.Mode().IsDir() {
			_ = os.Chmod(claudeSharedProjectsRoot, 0o700)
			linkPath := filepath.Join(dir, "projects")
			if _, lstatErr := os.Lstat(linkPath); os.IsNotExist(lstatErr) {
				_ = os.Symlink(claudeSharedProjectsRoot, linkPath)
			}
		}
	}
	return dir
}

// preparePrivateRuntimeDir 创建 <privateTmpDir>/rt（0700）并返回路径。
// 取短名 rt 是有意的：XDG_RUNTIME_DIR 会拼接 socket 文件名，短前缀保证
// Unix domain socket 路径远低于 Linux sun_path 108 字节上限。
// 目录落在 privateTmpDir 内，已由 ReadOnlyReviewer 的 ReadWrite 覆盖，无需额外放行。
func preparePrivateRuntimeDir(privateTmpDir string) string {
	dir := filepath.Join(privateTmpDir, "rt")
	if err := os.MkdirAll(dir, 0o700); err == nil {
		_ = os.Chmod(dir, 0o700)
	}
	return dir
}

// resolvePolicyPathWithin 校验 candidate 是 root 内部（不含 root 本身）的真实目录：
// Lstat 拒绝符号链接本体，EvalSymlinks 解析后的目标必须与 resolvedRoot 下
// 同一相对路径一致（容许 root 祖先含系统软链如 macOS /var -> /private/var，
// 同时拒绝 root 内部任意层级的中间软链），且不得与 p.Exec 中已有路径相等或
// 互为父子（含 Exec 条目解析后的真实路径），彻底闭合 W^X 写穿。
// 命中任何一条拒绝规则返回 ""。
func resolvePolicyPathWithin(candidate, root string, p *Policy) string {
	candidate = filepath.Clean(strings.TrimSpace(candidate))
	root = filepath.Clean(strings.TrimSpace(root))
	if candidate == "" || candidate == "." || root == "" || root == "." {
		return ""
	}
	// 相对路径必须严格落在 root 内部（不含 root 本身），不解析软链先排除
	// .. 逃逸与拼写异常。
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ""
	}
	// root 允许经祖先软链（如系统 /var -> /private/var）传入：先解析出真实根。
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || resolvedRoot == "" {
		return ""
	}
	if info, err := os.Lstat(candidate); err != nil {
		return ""
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		// 显式拒绝符号链接本体（含指向目录的软链）；非目录一律拒绝。
		return ""
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || resolved == "" {
		return ""
	}
	// 解析后的真实路径必须等于 resolvedRoot 下同一相对路径：root 祖先的
	// 系统软链被同时解析抵消，而 root 内部任意层级软链会使 resolved 偏离
	// expectedResolved 被拒。resolved 也必须严格位于 resolvedRoot 之内。
	expectedResolved := filepath.Join(resolvedRoot, rel)
	resolvedClean := filepath.Clean(resolved)
	resolvedRootClean := filepath.Clean(resolvedRoot)
	if resolvedClean != filepath.Clean(expectedResolved) ||
		!strings.HasPrefix(resolvedClean, resolvedRootClean+string(filepath.Separator)) {
		return ""
	}
	// 绝不与 Exec 已有路径（含其软链解析目标）重合或互为父子，避免 W^X 被写穿。
	sep := string(filepath.Separator)
	for _, ep := range p.Exec {
		targets := []string{ep}
		if norm, err := NormalizePath(ep); err == nil && norm != "" {
			targets = append(targets, norm)
		}
		if r, err := filepath.EvalSymlinks(ep); err == nil && r != "" {
			targets = append(targets, r)
		}
		for _, target := range targets {
			epClean := filepath.Clean(target)
			if resolvedClean == epClean ||
				strings.HasPrefix(epClean, resolvedClean+sep) ||
				strings.HasPrefix(resolvedClean, epClean+sep) {
				return ""
			}
		}
	}
	return resolved
}

// appendCtxmodeStateReadWrite 为宿主态 ctxmode / agy-recall 状态目录做精确兜底放行：
//   - <home>/.local/share/ctxmode：CTXMODE_DB 显式指定指回宿主时的最小写域；
//   - <home>/.gemini/config/plugins/agy-recall/state（覆盖 homeDir() 与 /root/.agy-pro-home）。
//
// 绝不加入 ~/.local/share 本身或其兄弟目录（特别是 gemini-web/chrome-profile）。
func appendCtxmodeStateReadWrite(p *Policy) {
	seen := make(map[string]struct{}, 4)
	add := func(d string, mkdir bool, root string) {
		d = filepath.Clean(strings.TrimSpace(d))
		if d == "" || d == "." {
			return
		}
		if mkdir {
			// 仅在目标不存在时创建；已存在的软链或非常规目录不得被 MkdirAll 静默接受。
			if _, err := os.Lstat(d); os.IsNotExist(err) {
				_ = os.MkdirAll(d, 0o700)
			}
		}
		resolved := resolvePolicyPathWithin(d, root, p)
		if resolved == "" {
			return
		}
		if _, ok := seen[resolved]; ok {
			return
		}
		seen[resolved] = struct{}{}
		p.ReadWrite = append(p.ReadWrite, resolved)
	}
	for _, root := range dedupeStrings([]string{homeDir(), "/root", "/root/.agy-pro-home"}) {
		// root 限定到各自 home 根：任何软链/.. 逃逸出该根的目录一律拒绝。
		add(filepath.Join(root, ".local", "share", "ctxmode"), false, root)
		add(filepath.Join(root, ".gemini", "config", "plugins", "agy-recall", "state"), true, root)
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

// preparePrivatePigAgentDirForHome 是默认布局 <pigHome>/agent 的薄包装。
func preparePrivatePigAgentDirForHome(dir, pigHome string) string {
	return preparePrivatePigAgentDirFromAgent(dir, filepath.Join(pigHome, "agent"))
}

// preparePrivatePigAgentDirFromAgent 在 dir（0700）下构建私有 AgentDir：
// 仅当 <srcAgentDir> 下对应普通文件存在时才以 0600 拷入白名单配置文件。返回目录路径。
//
// 不创建 extensions 软链：自动发现走 <agentDir>/extensions 会把 cfg.Source 记成
// 随机私有路径（/tmp/pi-sandbox-XXXXXX/pig-agent/extensions/<ext>），packed-go 的
// cellHash 直接对该路径取哈希，导致每次启动都脱靶宿主已构建的打包 cell。
// 改为在私有 settings.json 中显式声明宿主扩展目录（见 injectHostExtensionsSetting：
// 未声明时注入 [hostExtDir]，已有数组时相对路径相对宿主 srcAgentDir 解析并
// 视需要追加），使 settings 条目经 ResolveConfiguredWithError 解析出的 cfg.Source
// 与宿主自动发现完全一致，packed cell.Key 与 goPackedCellHash 逐字节命中宿主缓存。
func preparePrivatePigAgentDirFromAgent(dir, srcAgentDir string) string {
	dst := dir
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return dst
	}
	_ = os.Chmod(dst, 0o700)

	src := filepath.Clean(srcAgentDir)
	srcExt := filepath.Join(src, "extensions")
	hasHostExt := false
	if info, err := os.Lstat(srcExt); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		hasHostExt = true
	}
	for _, name := range pigAgentConfigFiles {
		srcFile := filepath.Join(src, name)
		info, err := os.Lstat(srcFile)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		data, err := os.ReadFile(srcFile)
		if err != nil {
			continue
		}
		if name == "settings.json" && hasHostExt {
			data = injectHostExtensionsSetting(data, srcExt)
		}
		_ = os.WriteFile(filepath.Join(dst, name), data, 0o600)
	}
	// 宿主缺 settings.json 但存在扩展目录时，补一份最小 settings.json
	// 声明扩展路径，保持与旧软链方案一致的加载行为（且命中打包缓存）。
	if hasHostExt {
		dstSettings := filepath.Join(dst, "settings.json")
		if _, err := os.Lstat(dstSettings); os.IsNotExist(err) {
			minimal := injectHostExtensionsSetting([]byte("{}"), srcExt)
			_ = os.WriteFile(dstSettings, minimal, 0o600)
		}
	}
	return dst
}

// prepareEmptyPrivatePigHome 构造空私有 PIG_HOME（<privateTmpDir>/pig-home，
// 0700）：不做任何指向宿主的 bin/docs/state 投影软链与 cache 条目投影——供
// PIG_HOME 不可信（软链/非目录/0777/异属主，SF-1）时回退使用，沙盒内 pig
// 只能在私有可写根下按需自建，绝无指向不可信宿主域的路径。返回私有根路径
// 与创建错误（错误由调用方记入 missing 触发 fail-closed）。
func prepareEmptyPrivatePigHome(privateTmpDir string) (string, error) {
	privatePigHome := filepath.Join(privateTmpDir, "pig-home")
	if err := os.MkdirAll(privatePigHome, 0o700); err != nil {
		return privatePigHome, err
	}
	_ = os.Chmod(privatePigHome, 0o700)
	if err := os.MkdirAll(filepath.Join(privatePigHome, "cache"), 0o700); err != nil {
		return privatePigHome, err
	}
	return privatePigHome, nil
}

// preparePrivatePigHome 在 <privateTmpDir>/pig-home（0700）构造私有 PIG_HOME：
// 宿主 PIG_HOME 保持 100% 只读，沙盒内 pig 经重定向的 PIG_HOME env 指向该私有根。
//
// 目录布局：
//   - bin/docs/state：直接以软链指向宿主同名目录（pig 读 SDK/文档 bundle/本体，
//     均落在宿主只读域内）；
//   - cache：真实可写目录，承载沙盒内 pig 的写面（顶层 *.lock、logs、toolchains
//     锁文件等）；
//   - cache/cells/<lang> 与 cache/ext：真实可写目录，承载 .build-* 构建 scratch 与
//     .locks 锁目录；目录本身绝不进 Exec——沙盒内 pig 若脱靶预热转冷路径自建产物，
//     产物落在无 Exec 授权的可写目录下、不可执行（W^X 不破）；
//   - cache/cells/<lang>/<entry> 与 cache/ext/<entry>：以同名软链把宿主已构建的
//     有效缓存单元（validCellEntryDir）投影进私有目录。沙盒内 pig 命中投影后经
//     软链 exec 的是宿主真实 runner 路径（已由 appendCellCachePolicy 逐文件枚举进
//     Policy.Exec）。
//   - cache/toolchains：真实可写目录，宿主各版本记录 (*.json/*.json.lock) 以软链
//     投影；pig 新建 *.json.lock 落在可写目录。
//   - cache 其余宿主子目录（logs/jiti/node-compile 等）：建同名可写目录并软链投影
//     宿主既有条目，pig 可写新文件、读旧产物。
//
// 返回私有 PIG_HOME 路径与 missing 清单：missing 是「应命中宿主产物但未能成功
// 投影/未能确认其 artifact 已入 Exec 白名单」的缓存单元目标（相对宿主 cacheRoot
// 的路径），供启动前 fail-closed 判定；无缺失返回 nil。仅在 hostPigHome 校验不
// 通过或私有根无法创建时回退为空私有根。
func preparePrivatePigHome(privateTmpDir, hostPigHome string, execSet map[string]struct{}) (string, []string) {
	privatePigHome := filepath.Join(privateTmpDir, "pig-home")
	if err := os.MkdirAll(privatePigHome, 0o700); err != nil {
		return privatePigHome, []string{"pig-home: " + err.Error()}
	}
	_ = os.Chmod(privatePigHome, 0o700)

	// 只读投影：bin/docs/state 软链到宿主（pig 读 SDK/文档/本体，宿主只读域内）。
	// 宿主对应目录非目录或缺失时跳过——pig 会在私有可写根下按需自建。
	for _, sub := range []string{"bin", "docs", "state"} {
		host := filepath.Join(hostPigHome, sub)
		if st, err := os.Lstat(host); err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0 {
			linkPath := filepath.Join(privatePigHome, sub)
			if _, lerr := os.Lstat(linkPath); os.IsNotExist(lerr) {
				_ = os.Symlink(host, linkPath)
			}
		}
	}

	hostCache := filepath.Join(hostPigHome, "cache")
	privCache := filepath.Join(privatePigHome, "cache")
	if err := os.MkdirAll(privCache, 0o700); err != nil {
		return privatePigHome, []string{"pig-home/cache: " + err.Error()}
	}

	var missing []string
	// projectEntry 投影缓存单元；checkExec 为 true 时同时校验该 artifact 已入
	// Exec 白名单（Exec 完整性 missing 只对预注册的可执行目标集合计算——
	// 与 countValidExecutableGoArtifacts 同口径，非 Go/非执行条目只投影不参与
	// missing，否则无关条目会撑大 missing 分母误拒 pig，oracle M5）。
	projectEntry := func(hostEntryDir, privParent string, checkExec bool) {
		name := filepath.Base(hostEntryDir)
		linkPath := filepath.Join(privParent, name)
		if _, lerr := os.Lstat(linkPath); lerr == nil {
			// 已存在（理论上私有根新建不会发生）；跳过不覆盖。
			return
		}
		if err := os.Symlink(hostEntryDir, linkPath); err != nil {
			if checkExec {
				missing = append(missing, relToCache(hostCache, hostEntryDir)+": symlink: "+err.Error())
			}
			return
		}
		// 校验投影 artifact 经软链解析后的宿主真实路径已入 Exec 白名单。
		if checkExec {
			if _, artifactPath, ok := validCellEntryDir(linkPath); ok {
				if norm, err := NormalizePath(artifactPath); err == nil {
					if resolved, rerr := filepath.EvalSymlinks(norm); rerr == nil && resolved != "" {
						norm = resolved
					}
					if _, hit := execSet[norm]; !hit {
						missing = append(missing, relToCache(hostCache, hostEntryDir)+": artifact not in Exec whitelist: "+norm)
					}
				}
			}
		}
	}

	// cells/<lang>：可写目录承载 .build-* scratch 与 .locks；<entry> 软链投影。
	// 与 total / Exec 完整性校验共用同一谓词 cellEntryHiddenOrSymlink：
	// 语言根本身是软链、隐藏目录（.tombstone-* 除外）、非目录一律跳过，
	// 与 cellCacheEntryDirs / countValidExecutableGoArtifacts 口径严格一致。
	cellsRoot := filepath.Join(privCache, "cells")
	hostCellsRoot := filepath.Join(hostCache, "cells")
	_ = os.MkdirAll(cellsRoot, 0o700)
	// 宿主 cache 与 cells 根本体是软链时整体拒枚举（与 cellCacheEntryDirs /
	// countValidExecutableGoArtifacts 同口径），不跟随软链投影外部目录。
	if isRealDirNoSymlink(hostCache) && isRealDirNoSymlink(hostCellsRoot) {
		if langs, err := os.ReadDir(hostCellsRoot); err == nil {
			for _, lang := range langs {
				if lang.Type()&os.ModeSymlink != 0 || !lang.IsDir() || strings.HasPrefix(lang.Name(), ".") {
					continue
				}
				hostLang := filepath.Join(hostCellsRoot, lang.Name())
				privLang := filepath.Join(cellsRoot, lang.Name())
				_ = os.MkdirAll(privLang, 0o700)
				_ = os.MkdirAll(filepath.Join(privLang, ".locks"), 0o700)
				entries, err := os.ReadDir(hostLang)
				if err != nil {
					continue
				}
				for _, ent := range entries {
					if cellEntryHiddenOrSymlink(ent) {
						continue
					}
					hostEntry := filepath.Join(hostLang, ent.Name())
					meta, artifactPath, ok := validCellEntryDir(hostEntry)
					if !ok {
						continue
					}
					// Exec 完整性 missing 仅对预注册的可执行目标集合计算：
					// cells/go 条目（lang=="go"）且 language 缺省或 "go"，artifact
					// 带执行位；其他语言/非执行条目只投影不参与 missing（M5）。
					checkExec := lang.Name() == "go" &&
						(meta.Language == "" || meta.Language == "go") &&
						isExecutableFile(artifactPath)
					projectEntry(hostEntry, privLang, checkExec)
				}
			}
		}
	}

	// ext：可写目录承载单扩展构建产物与 .locks；<entry> 软链投影。
	extRoot := filepath.Join(privCache, "ext")
	hostExtRoot := filepath.Join(hostCache, "ext")
	_ = os.MkdirAll(extRoot, 0o700)
	_ = os.MkdirAll(filepath.Join(extRoot, ".locks"), 0o700)
	// 宿主 ext 根本体是软链时拒枚举（不跟随软链投影外部目录）。
	if isRealDirNoSymlink(hostCache) && isRealDirNoSymlink(hostExtRoot) {
		if entries, err := os.ReadDir(hostExtRoot); err == nil {
			for _, ent := range entries {
				if cellEntryHiddenOrSymlink(ent) {
					continue
				}
				hostEntry := filepath.Join(hostExtRoot, ent.Name())
				meta, artifactPath, ok := validCellEntryDir(hostEntry)
				if !ok {
					continue
				}
				// Exec 完整性 missing 仅对预注册的可执行目标集合计算：
				// cache/ext 必须 ready.json 显式 language=="go"（排除宿主
				// node-runtime-* 等非 Go 产物）且 artifact 带执行位；其他条目
				// 只投影不参与 missing（与 countValidExecutableGoArtifacts
				// 同口径，M5）。
				checkExec := meta.Language == "go" && isExecutableFile(artifactPath)
				projectEntry(hostEntry, extRoot, checkExec)
			}
		}
	}

	// toolchains：可写目录承载 *.json.lock；宿主各记录软链投影（pig 读记录+写锁）。
	// 其余宿主 cache 子目录（logs/jiti/node-compile 等）：可写目录 + 软链投影条目。
	for _, sub := range []string{"toolchains", "logs", "jiti", "node-compile"} {
		hostSub := filepath.Join(hostCache, sub)
		st, err := os.Lstat(hostSub)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			// 宿主无该目录时仍建空可写目录，供 pig 写 logs/锁。
			_ = os.MkdirAll(filepath.Join(privCache, sub), 0o700)
			continue
		}
		privSub := filepath.Join(privCache, sub)
		_ = os.MkdirAll(privSub, 0o700)
		if entries, rerr := os.ReadDir(hostSub); rerr == nil {
			for _, ent := range entries {
				linkPath := filepath.Join(privSub, ent.Name())
				if _, lerr := os.Lstat(linkPath); os.IsNotExist(lerr) {
					_ = os.Symlink(filepath.Join(hostSub, ent.Name()), linkPath)
				}
			}
		}
	}
	return privatePigHome, missing
}

// isExecutableFile 判定 path 是否为带执行位的普通文件（非目录）。
// 供 preparePrivatePigHome 区分「可执行 Go 目标（参与 missing）」与
// 「非执行缓存条目（只投影不参与 missing）」（oracle M5）。
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// relToCache 返回 path 相对 cacheRoot 的路径（供 missing 清单可读化）。
func relToCache(cacheRoot, path string) string {
	if rel, err := filepath.Rel(cacheRoot, path); err == nil && rel != "" && rel != "." {
		return rel
	}
	return path
}

// verifyProjectedGoCellTargets 二次复核「宿主可执行 Go 产物 × 私有投影」
// 一致性：宿主任一有效 Go 产物的相对路径在私有 cache 下必须是软链投影
// （validCellEntryDir 可解析到 artifact），否则意味着枚举口径不一致（如
// cells/go 软链被 total 计入而投影跳过），记入 missing fail-closed 而非让
// 零投影门禁假放行。只走 cellCacheEntryDirs 同一枚举，不重扫；返回需记入
// missing 的目标清单（无缺失返回 nil）。供包内单测直接注入未投影夹具证伪。
func verifyProjectedGoCellTargets(hostCache, privCacheRoot string) []string {
	var missing []string
	for _, hostEntry := range cellCacheEntryDirs(hostCache) {
		meta, _, ok := validCellEntryDir(hostEntry)
		if !ok {
			continue
		}
		// 与 total/checkExec 同口径：仅可执行 Go 产物参与复核。
		rel := relToCache(hostCache, hostEntry)
		inCellsGo := strings.HasPrefix(rel, "cells"+string(filepath.Separator)+"go"+string(filepath.Separator))
		if inCellsGo {
			if meta.Language != "" && meta.Language != "go" {
				continue
			}
		} else {
			if meta.Language != "go" {
				continue
			}
		}
		artifact := filepath.Join(hostEntry, meta.Artifact)
		if !isExecutableFile(artifact) {
			continue
		}
		projPath := filepath.Join(privCacheRoot, rel)
		if _, _, ok := validCellEntryDir(projPath); !ok {
			missing = append(missing, rel+": host Go artifact not projected into private cache (enumeration/symlink mismatch)")
		}
	}
	return missing
}

// reviewerAgentCellTargets 描述沙盒内 pig 本次运行的目标集完整性校验结果。
type reviewerAgentCellTargets struct {
	hasGoExt bool     // 扩展集是否含 Go 扩展（无扩展时目标集为空，不强制产物）
	total    int      // 宿主有效缓存单元产物数（投影目标数）
	missing  []string // 未能成功投影/Exec 未命中的目标（相对宿主 cacheRoot 路径）
}

// reviewerAgentCellTargetErr 记录最近一次 ReviewerAgentWithEnv 私有 PIG_HOME 目标集
// 完整性校验结果；main 经 buildPolicy 在启动前读取并做 fail-closed 判定。
// 用 mutex 保护，避免与预热/测试并发写竞态。
var reviewerAgentCellTargetState = struct {
	sync.Mutex
	result *reviewerAgentCellTargets
}{}

// setReviewerAgentCellTargets 记录本次目标集校验结果。
func setReviewerAgentCellTargets(t *reviewerAgentCellTargets) {
	reviewerAgentCellTargetState.Lock()
	defer reviewerAgentCellTargetState.Unlock()
	reviewerAgentCellTargetState.result = t
}

// ReviewerAgentCellTargetError 返回本次 reviewer-agent 预设私有 PIG_HOME 目标集
// 完整性校验的失败原因（含指名缺失项），无缺失返回 nil。供 buildPolicy 在启动前
// fail-closed：有 Go 扩展但宿主无有效产物，或投影/Exec 校验存在缺失项时返回 error。
func ReviewerAgentCellTargetError() error {
	reviewerAgentCellTargetState.Lock()
	defer reviewerAgentCellTargetState.Unlock()
	t := reviewerAgentCellTargetState.result
	if t == nil {
		return nil
	}
	if !t.hasGoExt {
		return nil
	}
	if t.total == 0 {
		return fmt.Errorf("no valid Go extension cell artifact under host PIG_HOME cache (warm-up produced none)")
	}
	if len(t.missing) > 0 {
		return fmt.Errorf("pig cell target(s) not covered by host-build projection: %s", strings.Join(t.missing, "; "))
	}
	return nil
}

// ReviewerAgentCellTargets 返回本次目标集枚举与逐项命中清单（供报告/诊断）。
// hasGoExt 为 false 表示扩展集无 Go 扩展（目标集为空）。逐项以相对宿主
// cacheRoot 的路径表示；missing 为未命中/未成功投影项。
func ReviewerAgentCellTargets() (hasGoExt bool, total int, missing []string) {
	reviewerAgentCellTargetState.Lock()
	defer reviewerAgentCellTargetState.Unlock()
	if reviewerAgentCellTargetState.result == nil {
		return false, 0, nil
	}
	t := reviewerAgentCellTargetState.result
	return t.hasGoExt, t.total, append([]string(nil), t.missing...)
}

// injectHostExtensionsSetting 向私有 settings.json 注入宿主扩展目录 hostExtDir，
// 保留宿主原有全部键值：
//   - 未声明 "extensions"：注入 [hostExtDir]；
//   - "extensions" 为 null 或非数组（含解析失败）：原样返回，尊重宿主配置；
//   - "extensions" 为字符串数组：相对本地路径条目（非空，且不以 / ~ - !
//     npm: git: http:// https:// 开头；以 + 开头则保留前缀后对剩余路径按同规则
//     解析）相对宿主 <pigHome>/agent（即 filepath.Dir(hostExtDir)）解析为绝对路径，
//     排除项（- / ! 前缀）中的相对路径同样改写为相对宿主 agentDir 的绝对排除路径；
//     若数组经解析后未包含等价 hostExtDir（含 "./extensions"/"extensions"/
//     "+./extensions"/末尾带 / 等写法）且未显式排除（"-"/"!" 前缀 + hostExtDir /
//     "extensions" / "./extensions" / 解析后等价路径），追加 hostExtDir——
//     PIG_CODING_AGENT_DIR 改指私有目录后自动发现的 <agentDir>/extensions 已不存在，
//     不追加会丢失宿主默认扩展。
func injectHostExtensionsSetting(data []byte, hostExtDir string) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return data
	}
	raw, exists := obj["extensions"]
	if !exists {
		obj["extensions"] = json.RawMessage(mustMarshalJSON([]string{hostExtDir}))
		out, err := json.Marshal(obj)
		if err != nil {
			return data
		}
		return out
	}
	if strings.TrimSpace(string(raw)) == "null" {
		// 显式 null：原样保留，尊重宿主配置。
		return data
	}

	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil {
		// null / 非数组 / 非字符串元素：原样保留。
		return data
	}

	agentDir := filepath.Dir(hostExtDir)
	foundHostExt := false
	excluded := false
	out := make([]string, 0, len(entries)+1)
	for _, e := range entries {
		// 解析后比对："./extensions"、"extensions"、"+./extensions"、末尾带 /
		// 等写法都会解析为同一 hostExtDir，不得二次追加造成重复。
		resolvedEntry := resolveHostExtensionEntry(e, agentDir)
		if rest, ok := strings.CutPrefix(resolvedEntry, "+"); ok {
			if filepath.Clean(rest) == filepath.Clean(hostExtDir) {
				foundHostExt = true
			}
		} else if filepath.Clean(resolvedEntry) == filepath.Clean(hostExtDir) {
			foundHostExt = true
		}
		// 显式排除项：- / ! 前缀。除 "extensions" 组名外，相对路径条目在私有
		// AgentDir 下会失配（相对 <privateTmpDir>/pig-agent 解析），统一改写为
		// 相对宿主 agentDir 的绝对排除路径。
		if e != "" && (e[0] == '-' || e[0] == '!') {
			rest := e[1:]
			if settingsExtensionExcludes(e, hostExtDir) {
				excluded = true
			}
			// 仅 ./ ../ 开头的相对路径排除项改写为相对宿主 agentDir 的绝对路径；
			// 裸名（如 "extensions"、"excluded"）是扩展名排除而非路径，原样保留。
			if strings.HasPrefix(rest, "./") || strings.HasPrefix(rest, "../") {
				resolvedEntry = e[:1] + filepath.Join(agentDir, rest)
			}
		}
		out = append(out, resolvedEntry)
	}
	if !foundHostExt && !excluded {
		out = append(out, hostExtDir)
	}
	obj["extensions"] = json.RawMessage(mustMarshalJSON(out))
	data2, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return data2
}

// settingsExtensionExcludes 判断 - / ! 前缀的排除条目是否排除 targetDir
// （供 injectHostExtensionsSetting 与预热指纹的扩展枚举共用同一口径）：
// 裸组名 "extensions"/"./extensions"、与 targetDir 等价的绝对/相对路径
// （相对 targetDir 的父目录 agentDir 解析，含 ./ ../ 写法）均算排除。
func settingsExtensionExcludes(entry, targetDir string) bool {
	if entry == "" || (entry[0] != '-' && entry[0] != '!') {
		return false
	}
	rest := entry[1:]
	target := filepath.Clean(targetDir)
	agentDir := filepath.Dir(targetDir)
	if rest == "extensions" || rest == "./extensions" || filepath.Clean(rest) == target {
		return true
	}
	if rel, ok := strings.CutPrefix(rest, "./"); ok {
		if filepath.Join(agentDir, rel) == target {
			return true
		}
	}
	if rel, ok := strings.CutPrefix(rest, "../"); ok {
		if filepath.Clean(filepath.Join(agentDir, "../"+rel)) == target {
			return true
		}
	}
	return false
}

// resolveHostExtensionEntry 把 settings.json extensions 数组中的相对本地路径条目
// 解析为相对 agentDir 的绝对路径。非路径条目（npm:/git:/http(s)://）、绝对路径、
// ~ 开头、空串以及 - / ! 显式排除前缀一律原样返回；+ 前缀（启用标记）保留前缀
// 后对剩余路径按同一规则解析。
func resolveHostExtensionEntry(entry, agentDir string) string {
	if entry == "" {
		return entry
	}
	prefix := ""
	rest := entry
	switch rest[0] {
	case '+':
		prefix = "+"
		rest = rest[1:]
	case '-', '!':
		// 显式排除项：按原样保留，不做路径解析（含 "-extensions" 整组排除）。
		return entry
	}
	if isAbsoluteExtensionPath(rest) {
		return prefix + rest
	}
	return prefix + filepath.Join(agentDir, rest)
}

// isAbsoluteExtensionPath 判断 extensions 条目是否为绝对路径 / 非本地相对路径：
// 空串、/ 或 ~ 开头、URI 类前缀（npm: git: http:// https://）返回 true。
// - / ! 前缀由调用方先行剥离/短路，不在此处判断。
func isAbsoluteExtensionPath(rest string) bool {
	if rest == "" {
		return true
	}
	if rest[0] == '/' || rest[0] == '~' {
		return true
	}
	for _, scheme := range []string{"npm:", "git:", "http://", "https://"} {
		if strings.HasPrefix(rest, scheme) {
			return true
		}
	}
	return false
}

// mustMarshalJSON 序列化不会失败的值；兜底返回 "null"（调用方已校验类型）。
func mustMarshalJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return data
}

// injectPigCodingAgentDir 在 env 中注入 PIG_CODING_AGENT_DIR 与
// PI_CODING_AGENT_DIR 指向 <privatePigAgentDir>。
//
// 判定口径与 resolveSourcePigAgentDir 完全对齐（cfgEnv 最后一项含空覆盖语义，
// 显式空串屏蔽宿主同名变量）：
//   - PIG_CODING_AGENT_DIR：cfgEnv 或宿主环境有非空值（即 customAgentDir=true
//     的来源）时保持其优先级，不覆写、不注入；否则无条件 overrideEnvKey
//     回钉为私有目录（override 语义保证 --env PIG_CODING_AGENT_DIR= 空覆盖
//     不会把空串写进最终 env，oracle FM-4）。
//   - PI_CODING_AGENT_DIR：只要 PIG_CODING_AGENT_DIR 无显式非空值，一律
//     无条件 overrideEnvKey 回钉为私有目录——宿主残留的恶意/旧
//     PI_CODING_AGENT_DIR 不得在 pig 包装器后来 export PIG_USE_PI_DIRS=1
//     时被激活生效（任务书口径）；仅当调用方显式声明可信自定义
//     PI_CODING_AGENT_DIR（有效 PIG_USE_PI_DIRS=1 下非空）时保持其优先级。
func injectPigCodingAgentDir(env []string, privatePigAgentDir string, cfgEnv []string) []string {
	// 「显式自定义」仅认 cfgEnv（--env KEY=VALUE 显式声明）：宿主环境
	// PIG_CODING_AGENT_DIR / PI_CODING_AGENT_DIR / PIG_USE_PI_DIRS 属残留面，
	// 宿主残留 PI_CODING_AGENT_DIR 会在 pig 包装器后置 export
	// PIG_USE_PI_DIRS=1 时被激活生效，必须无条件回钉为私有目录
	//（oracle FM-4）。空覆盖（--env KEY=）屏蔽宿主同名变量后仍注入
	// 私有目录，不产生空路径。
	pigCustom := false
	if v, ok := cfgEnvLastValue(cfgEnv, "PIG_CODING_AGENT_DIR"); ok {
		pigCustom = strings.TrimSpace(v) != ""
	}
	if pigCustom {
		// 显式自定义目录保持最高优先级：既不覆写该键，也不注入
		// PI_CODING_AGENT_DIR 抢占（与 resolveSourcePigAgentDir 一致，
		// PIG_CODING_AGENT_DIR 优先于 PI_* 变量）。
		return env
	}
	env = overrideEnvKey(env, "PIG_CODING_AGENT_DIR", privatePigAgentDir)

	// PI_CODING_AGENT_DIR：仅当调用方经 cfgEnv 同时声明 PIG_USE_PI_DIRS=1
	// 与非空 PI_CODING_AGENT_DIR（显式可信自定义）时保持其优先级；
	// 其余情形（含宿主残留）一律回钉私有目录。
	piCustom := false
	if v, ok := cfgEnvLastValue(cfgEnv, "PIG_USE_PI_DIRS"); ok && strings.TrimSpace(v) == "1" {
		if pv, ok := cfgEnvLastValue(cfgEnv, "PI_CODING_AGENT_DIR"); ok && strings.TrimSpace(pv) != "" {
			piCustom = true
		}
	}
	if !piCustom {
		env = overrideEnvKey(env, "PI_CODING_AGENT_DIR", privatePigAgentDir)
	}
	return env
}

// cellReadyMeta 是缓存单元 ready.json 中校验所需的字段。
// Language 供 cache/ext 语言门鉴别：宿主 node-runtime-* 等非 Go 产物不计入
// 有效 Go 预热产物（validCellEntryDir 本身不对 Language 做约束，保持向后兼容）。
type cellReadyMeta struct {
	InputDigest string `json:"inputDigest"`
	Artifact    string `json:"artifact"`
	Size        int64  `json:"size"`
	Created     int64  `json:"created"`
	Language    string `json:"language"`
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

// usageTouchMinInterval 是 touchUsageJSON 的跳过重写窗口：lastUsed 距今小于该值则只读短路。
// 取 5m，比沙盒内 pig 的 PiG runtimecell.TouchUsage <1h 短路更紧，确保宿主态预刷新后
// usage.json 足够新：<55m 的沙盒会话内 pig 运行期 <1h 短路稳定命中、不向缓存目录
// 写临时文件（>55m 超长会话属 PiG 侧只读容错范围外，见 README Known Risks）。
const usageTouchMinInterval = 5 * time.Minute

// touchUsageJSON 与 PiG runtimecell.TouchUsage 同形：<usageTouchMinInterval（5m）
// 或未来时间戳只读短路，否则原子写 <dir>/usage.json（tmp + rename，0644）。
// 宿主态调用，沙盒内不会执行；窗口比 pig 运行期 <1h 短路更紧是有意的。
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
			if last.After(now) || now.Sub(last) < usageTouchMinInterval {
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

// isRealDirNoSymlink 判定 path 是已存在的真实目录（非符号链接本体）。
// 供 cell 根/子根/状态目录枚举共用：跟随软链读目标目录会被越界计入
// total/投影/Exec/usage 刷新，必须 Lstat 拒绝本体软链。
func isRealDirNoSymlink(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0
}

// cellCacheEntryDirs 返回缓存根下全部缓存单元目录（ext/<entry> 与 cells/<lang>/<entry>）。
// 与 PiG runtimecell.cacheEntryPaths 同构：跳过点开头目录，tombstone 除外。
// 单一目标枚举实体：total（countValidExecutableGoArtifacts）、投影
// （preparePrivatePigHome）与 Exec 完整性校验（validCellEntryDir 逐项）全部
// 复用本函数与 cellEntryHiddenOrSymlink 谓词，保证三面口径一致——
// cacheRoot、cache/ext、cache/cells 根与各 cells/<lang> 语言根本身是符号链接
// 时对应根的整个枚举被跳过（与投影侧 lang.Type()&ModeSymlink != 0 拒绝一致），
// total 不得再因 ReadDir 跟随根软链而单独计入假通过，projection/Exec/usage 刷新
// 也不得跟随软链写/放外部目录。
func cellCacheEntryDirs(cacheRoot string) []string {
	var paths []string
	addEntries := func(root string) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, ent := range entries {
			if cellEntryHiddenOrSymlink(ent) {
				continue
			}
			paths = append(paths, filepath.Join(root, ent.Name()))
		}
	}
	// 根本体是软链/非真实目录时整体拒枚举（阻断外部目录被计入/投影/Exec）。
	if !isRealDirNoSymlink(cacheRoot) {
		return nil
	}
	if extRoot := filepath.Join(cacheRoot, "ext"); isRealDirNoSymlink(extRoot) {
		addEntries(extRoot)
	}
	cellsRoot := filepath.Join(cacheRoot, "cells")
	if isRealDirNoSymlink(cellsRoot) {
		if langs, err := os.ReadDir(cellsRoot); err == nil {
			for _, lang := range langs {
				if lang.Type()&os.ModeSymlink != 0 || !lang.IsDir() || strings.HasPrefix(lang.Name(), ".") {
					continue
				}
				addEntries(filepath.Join(cellsRoot, lang.Name()))
			}
		}
	}
	return paths
}

// cellEntryHiddenOrSymlink 是 total / 投影 / Exec 完整性校验三方共用的
// 隐藏/软链谓词：条目为符号链接（DirEntry.IsDir 对软链返回 false）、
// 非目录，或名以 . 开头（.tombstone-* 除外——PiG runtimecell 的墓碑条目
// 属合法缓存单元命名）一律排除，保证三面对同一目标集取同一谓词。
func cellEntryHiddenOrSymlink(ent os.DirEntry) bool {
	if ent.Type()&os.ModeSymlink != 0 || !ent.IsDir() {
		return true
	}
	name := ent.Name()
	return strings.HasPrefix(name, ".") && !strings.HasPrefix(name, ".tombstone-")
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
//
// 缓存单元目录本身保持只读；锁目录与可执行文件分离，不破坏 W^X。
func appendCellCachePolicy(p *Policy, pigHome string) {
	cacheRoot := filepath.Join(pigHome, "cache")
	// cacheRoot 自身是软链视为非预期布局直接返回；祖先目录含系统软链（如
	// macOS /var -> /private/var）不受影响——resolvePolicyPathWithin 会把
	// root 与其子项解析到同一真实根下比较。
	if info, err := os.Lstat(cacheRoot); err != nil ||
		info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return
	}
	// SF-1：cacheRoot 解析后的真实根，作为 artifact 写入 Policy.Exec 的
	// 前缀校验基准——入列的必须是 EvalSymlinks 后的规范真实路径（绝写
	// 未解析软链别名），且须严格位于 resolvedCacheRoot 内。
	resolvedCacheRoot, err := filepath.EvalSymlinks(cacheRoot)
	if err != nil || resolvedCacheRoot == "" {
		return
	}
	resolvedCacheRoot = filepath.Clean(resolvedCacheRoot)
	// SF-1：在任何 os.MkdirAll 创建 .locks 之前冻结中间级目录的可信判定。
	// ext/cells 根本体为软链（指向外部目录）时，下方 addRW 的 MkdirAll 会
	// 跟随软链在外部目标凭空创建 .locks——必须先 Lstat 确认各父级是真实
	// 目录而非软链，任一级为软链/非目录即不为其创建 .locks（枚举侧
	// cellCacheEntryDirs 以同口径拒枚举）。
	extReal := isRealDirNoSymlink(filepath.Join(cacheRoot, "ext"))
	cellsRoot := filepath.Join(cacheRoot, "cells")
	cellsReal := isRealDirNoSymlink(cellsRoot)

	// 先收集已构建产物进 Exec，再处理锁目录 ReadWrite：使 addRW 内的
	// resolvePolicyPathWithin 能看到完整 Exec 集，.locks 软链指向 artifact
	// 目录时能被 Exec 重合检查拒绝，严守 W^X。
	seenExec := make(map[string]struct{}, 8)
	for _, dir := range cellCacheEntryDirs(cacheRoot) {
		_, artifactPath, ok := validCellEntryDir(dir)
		if !ok {
			continue
		}
		// SF-1：Lstat 校验 artifact 本体是普通文件（拒绝软链本体——软链
		// 产物会把缓存根外任意可执行文件经别名拼进 Policy.Exec），权限
		// perm&0o022==0（0777 全局可写二进制绝不放行执行）且属主合规
		//（0 或当前 euid——异属主产物可被宿主其他主体改写）。
		lst, lerr := os.Lstat(artifactPath)
		if lerr != nil || !lst.Mode().IsRegular() || lst.Mode()&os.ModeSymlink != 0 {
			continue
		}
		info, err := os.Stat(artifactPath)
		if err != nil || info.IsDir() {
			continue
		}
		// 缓存产物按普通文件处理：仅放行带执行位的真实文件。
		if info.Mode()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
			continue
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			if stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
				continue
			}
		}
		// SF-1：入 Policy.Exec 的必须是 artifactPath 经 EvalSymlinks 解析
		// 后的规范真实路径（绝不写未解析软链别名——Apply 前改指别名父级
		// 可把任意/0777 二进制带入沙盒执行）；解析失败或解析结果未落在
		// resolvedCacheRoot 内一律跳过。
		resolved, err := filepath.EvalSymlinks(artifactPath)
		if err != nil || resolved == "" {
			continue
		}
		resolved = filepath.Clean(resolved)
		if resolved != resolvedCacheRoot &&
			!strings.HasPrefix(resolved, resolvedCacheRoot+string(filepath.Separator)) {
			continue
		}
		if _, ok := seenExec[resolved]; !ok {
			seenExec[resolved] = struct{}{}
			p.Exec = append(p.Exec, resolved)
		}
	}

	seenRW := make(map[string]struct{}, 4)
	addRW := func(d string) {
		d = filepath.Clean(d)
		if _, ok := seenRW[d]; ok {
			return
		}
		seenRW[d] = struct{}{}
		// 仅当目标不存在时才创建；已存在的软链/非常规目录不被 MkdirAll 静默接受。
		if _, err := os.Lstat(d); os.IsNotExist(err) {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return
			}
		}
		// Lstat 拒绝软链本体 + EvalSymlinks 校验严格位于 cacheRoot 内 + 不与 Exec 重合。
		resolved := resolvePolicyPathWithin(d, cacheRoot, p)
		if resolved == "" {
			return
		}
		p.ReadWrite = append(p.ReadWrite, resolved)
	}

	// 锁目录：cells/<lang>/.locks 与 ext/.locks。仅当对应父级已通过
	// isRealDirNoSymlink 冻结判定（extReal/cellsReal/各语言根本体均非
	// 软链）才进入 addRW——软链父级下的 .locks 一律跳过，不创建（SF-1）。
	if extReal {
		addRW(filepath.Join(cacheRoot, "ext", ".locks"))
	}
	if cellsReal {
		if langs, err := os.ReadDir(cellsRoot); err == nil {
			for _, lang := range langs {
				if lang.Type()&os.ModeSymlink != 0 || !lang.IsDir() || strings.HasPrefix(lang.Name(), ".") {
					continue
				}
				langDir := filepath.Join(cellsRoot, lang.Name())
				if !isRealDirNoSymlink(langDir) {
					continue
				}
				addRW(filepath.Join(langDir, ".locks"))
			}
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
