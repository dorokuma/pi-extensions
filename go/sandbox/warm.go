package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// warmGoCellTimeout 是宿主态 Go 扩展 cell 预热子进程的默认超时；测试可通过
// warmGoCellTestHook 覆盖为更短的值以验证进程组清理与优雅降级。
const warmGoCellTimeout = 30 * time.Second

// warmGoCellLockName / warmGoCellStampName 是 <pigHome>/cache/.locks
// 下预热互斥锁文件与指纹墓碑文件名。
const (
	warmGoCellLockName  = "sbx-warm.lock"
	warmGoCellStampName = "sbx-warm.sha256"
)

// warmGoCellTestHook 暴露给单测的最小钩子集合；生产路径零开销。
// PigBin 非空时显式注入受控测试 pig 路径（绕过可信目录解析），
// 生产路径绝不设置该字段，零攻击面。
type warmGoCellTestHook struct {
	Timeout time.Duration // 0 表示使用 warmGoCellTimeout
	Stderr  io.Writer     // nil 表示 os.Stderr
	// PigBin 非空时替代 resolvePigEntryBinary 的解析结果（仅单测注入）。
	PigBin string
	// ExtraTrackedPaths 附加纳入指纹的追踪目标路径（单测模拟 pi 软链切换等场景）。
	ExtraTrackedPaths []string
}

// warmGoCellHook 是包级测试钩子；nil 时走真实执行路径。
var warmGoCellHook *warmGoCellTestHook

// MaybeWarmGoExtensionCells 在 buildPolicy(cfg) 之前调用：
// 仅当 cfg.Preset == "reviewer-agent" 且目标命令解析为 pig 入口二进制，
// 且实际生效 AgentDir（resolveSourcePigAgentDir：PIG_CODING_AGENT_DIR /
// 有效 PIG_USE_PI_DIRS=1 的 PI_CODING_AGENT_DIR / 默认 <pigHome>/agent）的
// 扩展集内至少存在 1 个原生 Go 扩展时进入预热检查。
// 常态（指纹未变 + 至少 1 个有效 Go 产物）零子进程短路返回 0；
// 冷路径经双检文件锁串行化预热，预热子进程使用最小干净环境与私有 cwd，
// 失败/超时仅告警降级，不阻断后续 buildPolicy。
// 返回退出码：0 表示继续进入沙盒；非 0（128+信号）表示预热期被信号中断，
// 调用方（runMain）须直接退出，不再进入沙盒。
func MaybeWarmGoExtensionCells(cfgPreset string, command []string, cfgEnv []string) int {
	if cfgPreset != "reviewer-agent" || len(command) == 0 {
		return 0
	}
	pigBin := ""
	if warmGoCellHook != nil && warmGoCellHook.PigBin != "" {
		// 单测显式注入受控 pig 路径；仅在命令名确实指向 pig 入口时生效。
		if filepath.Base(command[0]) == "pig" {
			pigBin = warmGoCellHook.PigBin
		}
	}
	if pigBin == "" {
		pigBin = resolvePigEntryBinary(command[0])
	}
	if pigBin == "" {
		return 0
	}
	pigHome := resolvePigHomeDir(cfgEnv)
	if !validatePigHomeDir(pigHome) {
		return 0
	}
	// AgentDir 单次选源（与 ReviewerAgentWithEnv 完全共用的
	// resolveAgentDirSelection）：显式/宿主自定义目录已通过
	// validateTrustedPigDir 才作扩展发现源；显式候选被拒时回退默认
	// <pigHome>/agent 与沙盒侧同口径，不再独立判定（oracle MF-1 消除
	// resolver 与 custom 判定分裂）。
	sel := resolveAgentDirSelection(pigHome, cfgEnv)
	if (sel.origin == agentDirOriginCfgEnv && !sel.srcValid) || !sel.sourceTrusted {
		// 显式自定义候选被拒（软链/权限/属主不符）或最终来源目录本身
		// 未过受信校验（默认 <pigHome>/agent 为软链/0777/异属主）：预热跳过，
		// 不阻断启动（沙盒侧将回退私有目录，预热是性能优化）。
		return 0
	}
	srcAgentDir := sel.sourceDir
	cacheRoot := filepath.Join(pigHome, "cache")
	tracked, extDirs, sdkDirs, settings := warmFingerprintInputs(pigBin, pigHome, srcAgentDir)
	if len(extDirs) == 0 {
		// 无 Go 扩展时 pig 无需构建 Go cell，不必预热。
		// state/pigsdk 允许为空：真实 pig --help 会自动落盘内嵌 sdk-*。
		return 0
	}
	if isCellCacheWarm(cacheRoot, pigHome, tracked, extDirs, sdkDirs, settings) {
		return 0
	}
	return warmGoExtensionCells(pigBin, pigHome, cacheRoot, srcAgentDir)
}

// goCellFingerprintInputsForAgent 枚举实际生效的 AgentDir 下的 Go 扩展目录与
// <pigHome>/state/pigsdk 下的 SDK 目录：扩展集由 sourceAgentGoExtensionDirs
// 按 settings.json extensions 声明解析（声明 null → 空集；显式本地路径经
// EvalSymlinks 纳入；未排除默认目录时并集 <srcAgentDir>/extensions 自动发现），
// 返回已解析软链的真实目录条目（供指纹与预热前后重检共用）。
func goCellFingerprintInputsForAgent(pigHome, srcAgentDir string, settings []byte) (extDirs, sdkDirs []goDirEntry) {
	extDirs = sourceAgentGoExtensionDirs(srcAgentDir, settings)
	sdkDirs = nativeGoSDKDirs(filepath.Join(pigHome, "state", "pigsdk"))
	return extDirs, sdkDirs
}

// readAgentSettings 读取 <srcAgentDir>/settings.json：仅接受普通文件
// （Lstat 拒绝软链与目录），不存在或非普通文件返回 nil。
// nil 与内容空 []byte 区分：nil 表示无显式声明（默认扫描 extensions 目录）。
func readAgentSettings(srcAgentDir string) []byte {
	settingsPath := filepath.Join(srcAgentDir, "settings.json")
	st, err := os.Lstat(settingsPath)
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil
	}
	return data
}

// sourceAgentGoExtensionDirs 按 settings.json 的 "extensions" 字段解析实际生效的
// Go 扩展目录集合（与 injectHostExtensionsSetting / resolveHostExtensionEntry
// 同口径）：
//   - settings 为 nil / 非法 JSON / 字段缺失 / 非字符串数组：回退默认扫描
//     <srcAgentDir>/extensions（settings.json 是常规文件时允许为空字节）；
//   - "extensions": null：扩展集为空（显式禁用全部扩展）；
//   - 字符串数组：条目经 resolveHostExtensionEntry 相对 srcAgentDir 解析
//     （剥可选 + 前缀；~ 开头按 trustedHomeDir 展开；URI 类条目跳过）；
//     未显式排除默认目录时才扫描 <srcAgentDir>/extensions；
//     非排除条目解析后是目录的——自身含 go.mod 则直接作为 Go 扩展条目，
//     否则扫其下 nativeGoExtensionDirs 并入集。
//
// 全部条目按 (name, realDir) 去重排序，保证指纹确定。
func sourceAgentGoExtensionDirs(srcAgentDir string, settings []byte) []goDirEntry {
	srcAgentDir = filepath.Clean(srcAgentDir)
	defaultExtDir := filepath.Join(srcAgentDir, "extensions")

	// settings 为 nil（文件缺失/非普通）→ 无显式声明，默认扫描。
	if settings == nil {
		return nativeGoExtensionDirs(defaultExtDir)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(settings, &obj); err != nil || obj == nil {
		return nativeGoExtensionDirs(defaultExtDir)
	}
	raw, exists := obj["extensions"]
	if !exists {
		return nativeGoExtensionDirs(defaultExtDir)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		// 空 settings 键值：无显式声明，默认扫描。
		return nativeGoExtensionDirs(defaultExtDir)
	}
	if trimmed == "null" {
		// 显式 null：宿主完全禁用扩展，扫描集为空。
		return nil
	}
	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil {
		// 非字符串数组：宿主配置本就不合法，回退默认扫描（宁可多入指纹）。
		return nativeGoExtensionDirs(defaultExtDir)
	}

	defaultExcluded := false
	var candidates []string
	for _, e := range entries {
		if e == "" {
			continue
		}
		if e[0] == '-' || e[0] == '!' {
			if settingsExtensionExcludes(e, defaultExtDir) {
				defaultExcluded = true
			}
			continue
		}
		candidates = append(candidates, resolveHostExtensionEntry(e, srcAgentDir))
	}

	seen := make(map[string]struct{})
	var out []goDirEntry
	add := func(de goDirEntry) {
		key := de.name + "\x00" + de.realDir
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, de)
	}
	if !defaultExcluded {
		for _, de := range nativeGoExtensionDirs(defaultExtDir) {
			add(de)
		}
	}
	for _, e := range candidates {
		rest := strings.TrimPrefix(e, "+")
		if isAbsoluteExtensionPath(rest) {
			// 绝对路径直接使用；~ 开头按 trustedHomeDir 展开；URI 跳过。
			if rest == "" || strings.Contains(rest, "://") ||
				strings.HasPrefix(rest, "npm:") || strings.HasPrefix(rest, "git:") {
				continue
			}
			if rest[0] == '~' {
				rest = filepath.Join(trustedHomeDir(), strings.TrimPrefix(rest, "~"))
			}
		}
		if de, ok := resolveGoDirEntry(filepath.Dir(rest), filepath.Base(rest)); ok {
			add(de)
			continue
		}
		// 目录但不含 go.mod：扫其下含 go.mod 的一级子目录（扩展组目录）。
		resolved, err := filepath.EvalSymlinks(rest)
		if err != nil || resolved == "" {
			continue
		}
		if st, err := os.Stat(resolved); err != nil || !st.IsDir() {
			continue
		}
		if filepath.Clean(resolved) == filepath.Clean(defaultExtDir) {
			continue // 已由默认扫描覆盖
		}
		for _, de := range nativeGoExtensionDirs(resolved) {
			add(de)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].realDir < out[j].realDir
	})
	return out
}

// trustedHomeDir 返回当前 euid 的可信宿主 HOME：经 os/user.LookupId 查
// /etc/passwd 获取，euid==0 兜底 /root。绝不读 os.Getenv("HOME")——
// 调用方环境的 HOME 可能被外部篡改，不能作为信任边界输入。
func trustedHomeDir() string {
	if u, err := user.LookupId(strconv.Itoa(os.Geteuid())); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	if os.Geteuid() == 0 {
		return "/root"
	}
	return "/"
}

// trustedPigSearchDirs 返回 resolvePigEntryBinary 允许查找 pig 的固定可信目录。
// 绝不使用调用方 os.Getenv("PATH")：外部可控 PATH 中的前置目录会被用于定位
// 伪造 pig，从而击穿信任边界。
func trustedPigSearchDirs() []string {
	trustedHome := trustedHomeDir()
	dirs := []string{
		filepath.Join(trustedHome, ".local", "bin"),
		filepath.Join(trustedHome, ".pig", "bin"),
		"/root/.local/bin",
		"/root/.pig/bin",
	}
	return append(dirs, standardBinDirs...)
}

// trustedPigExecPrefixes 返回 pig 真实文件（EvalSymlinks 后）允许落入的可信前缀。
// 除全部查找目录外，纳入版本化安装根 <home>/.local/opt/pig 与
// /root/.local/opt/pig（pig -> opt 软链的解析目标）。
func trustedPigExecPrefixes() []string {
	trustedHome := trustedHomeDir()
	prefixes := trustedPigSearchDirs()
	return append(prefixes,
		filepath.Join(trustedHome, ".local", "opt", "pig"),
		"/root/.local/opt/pig",
	)
}

// pathWithinPrefixes 判断真实路径 path 是否严格位于 prefixes 任一根之下。
// 比较前对两侧各自 EvalSymlinks（容忍祖先目录含系统软链，如 /var -> /private/var）。
func pathWithinPrefixes(path string, prefixes []string) bool {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil || realPath == "" {
		return false
	}
	realPath = filepath.Clean(realPath)
	sep := string(filepath.Separator)
	for _, prefix := range prefixes {
		p := filepath.Clean(prefix)
		if rp, err := filepath.EvalSymlinks(p); err == nil && rp != "" {
			p = filepath.Clean(rp)
		}
		if realPath == p || strings.HasPrefix(realPath, p+sep) {
			return true
		}
	}
	return false
}

// trustedPigCandidateFile 校验候选 pig 文件：EvalSymlinks 后的真实文件必须是
// 普通文件、权限位不含组/其他写位（perm & 0o022 == 0）且属主为 root 或当前 euid。
func trustedPigCandidateFile(candidate string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || resolved == "" {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", false
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if st.Uid != 0 && st.Uid != uint32(os.Geteuid()) {
			return "", false
		}
	}
	return resolved, true
}

// IsPigEntryCommand 判定 command 是否指向真实 pig 入口（与
// MaybeWarmGoExtensionCells 同口径）：空命令或非 pig 返回 false。
// 供 buildPolicy 决定 reviewer-agent 的 cell 目标集 fail-closed 是否生效
// ——claude/agy/sh 等非 pig 命令不会被预热，冷缓存下无条件校验会误杀
// 启动（oracle FM-2B）。
func IsPigEntryCommand(command []string) bool {
	if len(command) == 0 {
		return false
	}
	if warmGoCellHook != nil && warmGoCellHook.PigBin != "" {
		if filepath.Base(command[0]) == "pig" {
			return true
		}
	}
	return resolvePigEntryBinary(command[0]) != ""
}

// resolvePigEntryBinary 仅在固定可信目录（trustedPigSearchDirs）中解析
// pig 入口二进制，绝不调用 exec.LookPath 或读取调用方 PATH：
//   - 命令名含路径分隔符：basename 必须为 pig，真实文件落在可信前缀内且
//     通过 trustedPigCandidateFile 校验；
//   - 裸命令名 pig：按 trustedPigSearchDirs 顺序取第一个通过校验的候选。
//
// 命中返回解析后的绝对路径（供指纹 stat 与预热执行）；未命中返回 ""。
func resolvePigEntryBinary(cmdName string) string {
	trimmed := strings.TrimSpace(cmdName)
	if trimmed == "" {
		return ""
	}
	if strings.ContainsRune(trimmed, filepath.Separator) {
		if filepath.Base(trimmed) != "pig" {
			return ""
		}
		return checkPigCandidate(trimmed)
	}
	if trimmed != "pig" {
		return ""
	}
	for _, dir := range trustedPigSearchDirs() {
		if resolved := checkPigCandidate(filepath.Join(dir, "pig")); resolved != "" {
			return resolved
		}
	}
	return ""
}

// checkPigCandidate 校验单个候选路径：普通文件/权限/属主 + 真实路径落在可信前缀内。
func checkPigCandidate(candidate string) string {
	resolved, ok := trustedPigCandidateFile(candidate)
	if !ok {
		return ""
	}
	if !pathWithinPrefixes(resolved, trustedPigExecPrefixes()) {
		return ""
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	return resolved
}

// validatePigHomeDir 在预热前校验 pigHome 可信度：Lstat 拒绝符号链接；
// 路径已存在时必须是目录、属主为当前 euid（或 root）且 perm & 0o022 == 0，
// 拒绝 0777/公共可写目录（mktemp -d 的 0700 天然通过）。不存在返回 true，
// 由后续 MkdirAll 按 0755 创建并报真实错误。
func validatePigHomeDir(pigHome string) bool {
	return validateTrustedPigDir(pigHome, "PIG_HOME")
}

// validateTrustedPigDir 是 PIG_HOME 与显式自定义 AgentDir 共用的可信度校验：
// 语义与 validatePigHomeDir 一致（末级本体拒软链/非目录/0o022 可写/异属主），
// 另加 SF-1 祖先链逐级校验——末级 Lstat 自动跟随中间软链，仅查末级会让
// <tmp>/pig-link/host 或 <tmp>/world-0777/host 穿透；且末级不存在（ENOENT）
// 时也必须校验首个已存在祖先及其上整条链，不能让 MkdirAll 跟随祖先软链在
// 外部目标凭空建目录。label 仅用于告警文案区分。
//
// 放行边界（不因系统布局误拒合法临时根）：
//   - 链上软链：仅豁免直接被 / 持有且属主为 0 的系统顶层软链（如 macOS
//     /var -> /private/var、/tmp -> /private/tmp）；其余任何层级的软链一律拒；
//   - 属主：每级目录 Uid 必须为 0 或当前 euid（异属主目录可被他方替换内容）；
//   - 权限：Perm()&0o022 != 0 的目录一律拒，但豁免 Uid==0 且带 os.ModeSticky
//     的临时根（/tmp、/var/tmp 的 01777——非粘滞 0777 父目录可被非特权方
//     重命名替换 host，不能豁免）；该豁免仅适用于 filepath.Dir(dir) 及之上
//     的真正祖先目录，末级本体 0o022 可写（含 01777 sticky）一律拒——
//     /tmp、/var/tmp 自身不得被当作可信 PIG_HOME/AgentDir。
func validateTrustedPigDir(dir, label string) bool {
	return validateTrustedPigDirWithRoot(dir, label, "/")
}

// trustedPigTopLinkMaxHops 是祖先链上允许的顶层软链（/ 直属且 uid==0）
// 单跳跟随总上限：超限视为软链环，拒绝。
const trustedPigTopLinkMaxHops = 8

// validateTrustedPigDirWithRoot 与 validateTrustedPigDir 同口径，但文件系统
// 根由 sysRoot 显式给定：祖先链向上遍历到 sysRoot 即终止，「/ 直属顶层软链」
// 判定与 os.Readlink 单跳目标解析均以 sysRoot 为根（单测可注入 t.TempDir()
// 假根确定性覆盖顶层软链目标链分支，Linux 上无需依赖 macOS 布局）。
// 生产一律经 validateTrustedPigDir 固定传 sysRoot="/"，零外部开启入口。
func validateTrustedPigDirWithRoot(dir, label, sysRoot string) bool {
	sysRoot = filepath.Clean(sysRoot)
	if sysRoot == "" || sysRoot == "." {
		sysRoot = "/"
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		warmWarnf("sbx-warm: refusing empty %s", label)
		return false
	}
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	dir = filepath.Clean(dir)
	// dir 必须位于 sysRoot 之下（不含 sysRoot 本身）：sysRoot="/" 时所有
	// 绝对路径均在根内；注入根之外的 dir 既无意义也会让顶层软链豁免
	// 判定失真。
	if dir == sysRoot || (sysRoot != "/" && !strings.HasPrefix(dir, sysRoot+string(filepath.Separator))) {
		warmWarnf("sbx-warm: refusing %s %s outside sysRoot %s", label, dir, sysRoot)
		return false
	}

	// 末级本体校验（已存在时）：拒软链/非目录/异属主；末级 0o022 可写
	// 一律拒（含 01777 sticky——sticky 豁免只留给真正的祖先级临时根）。
	if isSymlinkPath(dir) {
		warmWarnf("sbx-warm: refusing symlink %s %s", label, dir)
		return false
	}
	info, err := os.Lstat(dir)
	lastMissing := false
	if err != nil {
		if !os.IsNotExist(err) {
			// 非 ENOENT 暂态错误交由后续路径操作报真实错误。
			return false
		}
		lastMissing = true
	} else {
		if !info.IsDir() {
			warmWarnf("sbx-warm: %s %s is not a directory", label, dir)
			return false
		}
		if info.Mode().Perm()&0o022 != 0 {
			warmWarnf("sbx-warm: refusing group/world-writable %s %s", label, dir)
			return false
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok &&
			st.Uid != 0 && st.Uid != uint32(os.Geteuid()) {
			warmWarnf("sbx-warm: refusing foreign-owner %s %s", label, dir)
			return false
		}
	}

	// 祖先链逐级校验：从末级（已存在时）或首个已存在祖先（末级缺失时）
	// 一路向上到 sysRoot，任一级命中拒收规则即 false。
	ancestor := dir
	if lastMissing {
		ancestor = filepath.Dir(dir)
	}
	topLinkHops := 0
	for {
		if ancestor == "" || ancestor == "." {
			return true
		}
		if ancestor == sysRoot {
			return true
		}
		ai, lerr := os.Lstat(ancestor)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				// 继续向上找已存在祖先——末级不存在的场景须校验整条已存在链。
				ancestor = filepath.Dir(ancestor)
				continue
			}
			warmWarnf("sbx-warm: refusing %s %s (ancestor lstat error: %v)", label, dir, lerr)
			return false
		}
		if ai.Mode()&os.ModeSymlink != 0 {
			// 豁免：仅 sysRoot 直属且 uid==0 的系统顶层软链（macOS /var、/tmp）
			// ——且不直接跳回 sysRoot 结束，而是 os.Readlink 单跳读取目标，
			// 把 ancestor 切换到真实目标路径继续沿同一循环逐级向上校验
			// 目标本体及其全部祖先：顶层软链指向非粘滞 0777、异属主目录或
			// 目标链内含非顶层中间软链一律拒；悬空软链（Readlink 目标不可
			// 解析为绝对路径或越出 sysRoot）同样拒。
			st, statOK := ai.Sys().(*syscall.Stat_t)
			if statOK && st.Uid == 0 && filepath.Dir(ancestor) == sysRoot {
				topLinkHops++
				if topLinkHops > trustedPigTopLinkMaxHops {
					warmWarnf("sbx-warm: refusing %s %s (top-level symlink hop limit exceeded)", label, dir)
					return false
				}
				target, rerr := os.Readlink(ancestor)
				if rerr != nil {
					warmWarnf("sbx-warm: refusing %s %s (top-level symlink %s readlink: %v)", label, dir, ancestor, rerr)
					return false
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(sysRoot, target)
				}
				target = filepath.Clean(target)
				if target == sysRoot || (sysRoot != "/" && !strings.HasPrefix(target, sysRoot+string(filepath.Separator))) {
					warmWarnf("sbx-warm: refusing %s %s (top-level symlink %s escapes sysRoot)", label, dir, ancestor)
					return false
				}
				if _, terr := os.Lstat(target); terr != nil {
					// 悬空顶层软链（目标不存在或暂态错误）一律拒：不得让
					// ENOENT 跳级把未校验目标当作缺失祖先吞掉。
					warmWarnf("sbx-warm: refusing %s %s (top-level symlink %s target lstat: %v)", label, dir, ancestor, terr)
					return false
				}
				ancestor = target
				continue
			}
			warmWarnf("sbx-warm: refusing %s %s (ancestor %s is a symlink)", label, dir, ancestor)
			return false
		}
		if !ai.IsDir() {
			warmWarnf("sbx-warm: refusing %s %s (ancestor %s is not a directory)", label, dir, ancestor)
			return false
		}
		if !trustedPigDirModeOwnerOK(ai) {
			warmWarnf("sbx-warm: refusing %s %s (ancestor %s group/world-writable or foreign-owner)", label, dir, ancestor)
			return false
		}
		ancestor = filepath.Dir(ancestor)
	}
}

// trustedPigDirModeOwnerOK 判定单级目录的权限与属主是否可信：
//   - 属主必须是 0 或当前 euid；
//   - Perm()&0o022 == 0 直接通过；Perm()&0o022 != 0 时仅豁免 Uid==0 且
//     os.ModeSticky 置位的临时根（/tmp、/var/tmp 的 01777）；非粘滞 0777
//     目录（任何用户都可 rmdir/mkdir 替换 host）一律不豁免。
func trustedPigDirModeOwnerOK(info os.FileInfo) bool {
	permWritable := info.Mode().Perm()&0o022 != 0
	st, hasStat := info.Sys().(*syscall.Stat_t)
	uidOK := true
	uid := uint32(0)
	if hasStat {
		uid = st.Uid
		uidOK = st.Uid == 0 || st.Uid == uint32(os.Geteuid())
	}
	if !permWritable {
		return uidOK
	}
	// 可写目录：仅豁免 root 拥有且带 sticky 位的系统临时根；其余一律拒。
	if hasStat && uid == 0 && info.Mode()&os.ModeSticky != 0 {
		return true
	}
	return false
}

// warmTrackedBinaryPaths 返回除 pigBin 外参与指纹的真实 pig ELF 追踪目标：
// <pigHome>/bin/pi、<trustedHome>/.pig/bin/pi、/root/.pig/bin/pi、
// <trustedHome>/.local/opt/pig/official/pig、/root/.local/opt/pig/official/pig。
// 覆盖包装脚本不动但经 pi / official pig 软链切换真实本体的升级路径；
// 单测经 warmGoCellHook.ExtraTrackedPaths 追加受控目标。
func warmTrackedBinaryPaths(pigHome string) []string {
	trustedHome := trustedHomeDir()
	return []string{
		filepath.Join(pigHome, "bin", "pi"),
		filepath.Join(trustedHome, ".pig", "bin", "pi"),
		"/root/.pig/bin/pi",
		filepath.Join(trustedHome, ".local", "opt", "pig", "official", "pig"),
		"/root/.local/opt/pig/official/pig",
	}
}

// warmFingerprintInputs 计算一次指纹枚举：pigBin + 全部追踪目标 + 扩展/SDK 目录
// + <srcAgentDir>/settings.json 内容字节。
// pre/postStamp 各调用一次，保证枚举时点独立（预热中 pig 可能落盘新 SDK 或
// 源码被切换，postStamp 重新枚举才能感知漂移）。
func warmFingerprintInputs(pigBin, pigHome, srcAgentDir string) (tracked []string, extDirs, sdkDirs []goDirEntry, settings []byte) {
	seen := map[string]struct{}{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil || resolved == "" {
			return
		}
		if _, err := os.Stat(resolved); err != nil {
			return
		}
		if _, dup := seen[resolved]; !dup {
			seen[resolved] = struct{}{}
			tracked = append(tracked, resolved)
		}
	}
	add(pigBin)
	for _, p := range warmTrackedBinaryPaths(pigHome) {
		add(p)
	}
	if warmGoCellHook != nil {
		for _, p := range warmGoCellHook.ExtraTrackedPaths {
			add(p)
		}
	}
	settings = readAgentSettings(srcAgentDir)
	extDirs, sdkDirs = goCellFingerprintInputsForAgent(pigHome, srcAgentDir, settings)
	return tracked, extDirs, sdkDirs, settings
}

// goDirEntry 是参与指纹的 Go 目录条目：name 为 extRoot/sdkRoot 下的
// 逻辑条目名（软链名，参与排序与哈希命名），realDir 为解析软链后的
// 真实目录路径（WalkDir/os.Stat 的对象）。生产环境下
// <pigHome>/agent/extensions/<ext> 全是指向源码仓的目录符号链接，
// 必须先 EvalSymlinks 再递归——filepath.WalkDir 对 root 用 os.Lstat，
// 直接传软链路径不会进入目录遍历任何文件。
type goDirEntry struct {
	name    string
	realDir string
}

// resolveGoDirEntry 将 root 下的候选条目 name 解析为 Go 目录条目：
// EvalSymlinks 解析软链（悬链/自引用返回错误即丢弃），再确认真实路径
// 是目录且根下含 go.mod。
func resolveGoDirEntry(root, name string) (goDirEntry, bool) {
	realDir, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		return goDirEntry{}, false
	}
	st, err := os.Stat(realDir)
	if err != nil || !st.IsDir() {
		return goDirEntry{}, false
	}
	if _, err := os.Stat(filepath.Join(realDir, "go.mod")); err != nil {
		return goDirEntry{}, false
	}
	return goDirEntry{name: name, realDir: realDir}, true
}

// nativeGoExtensionDirs 返回 <extRoot> 下所有含 go.mod 的原生 Go 扩展目录
// （已解析软链）。内部遍历时跳过 .git / testdata / 隐藏目录。
func nativeGoExtensionDirs(extRoot string) []goDirEntry {
	entries, err := os.ReadDir(extRoot)
	if err != nil {
		return nil
	}
	var out []goDirEntry
	for _, ent := range entries {
		name := ent.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		// DirEntry.IsDir 对软链为 false；生产布局全部是目录软链，
		// 这里不预判类型，统一交给 EvalSymlinks + Stat 判定。
		if de, ok := resolveGoDirEntry(extRoot, name); ok {
			out = append(out, de)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// nativeGoSDKDirs 返回 <sdkRoot> 下所有含 go.mod 的 SDK 目录（已解析软链，不递归）。
func nativeGoSDKDirs(sdkRoot string) []goDirEntry {
	entries, err := os.ReadDir(sdkRoot)
	if err != nil {
		return nil
	}
	var out []goDirEntry
	for _, ent := range entries {
		name := ent.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if de, ok := resolveGoDirEntry(sdkRoot, name); ok {
			out = append(out, de)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// warmCacheChainSymlink 检查 pigHome、cacheRoot、cacheRoot/.locks 逐级是否
// 为符号链接：任一层是软链即拒绝在其下创建或写入锁文件与墓碑。
func warmCacheChainSymlink(pigHome, cacheRoot string) bool {
	for _, p := range []string{
		pigHome,
		cacheRoot,
		filepath.Join(cacheRoot, ".locks"),
	} {
		if isSymlinkPath(p) {
			warmWarnf("sbx-warm: refusing symlink path %s", p)
			return true
		}
	}
	return false
}

// isCellCacheWarm 判断当前指纹是否与墓碑一致且至少存在 1 个带执行位的有效 Go 产物
// （cache/cells/go/<digest>/runner 合包形态或 cache/ext/<name>-<digest>/ 单扩展形态）。
// 墓碑必须先 Lstat 确认是普通文件（拒绝软链穿透），再读内容。
func isCellCacheWarm(cacheRoot, pigHome string, tracked []string, extDirs, sdkDirs []goDirEntry, settings []byte) bool {
	if warmCacheChainSymlink(pigHome, cacheRoot) {
		return false
	}
	stampPath := filepath.Join(cacheRoot, ".locks", warmGoCellStampName)
	st, err := os.Lstat(stampPath)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(stampPath)
	if err != nil {
		return false
	}
	want := computeGoCellFingerprint(tracked, extDirs, sdkDirs, settings)
	if strings.TrimSpace(string(data)) != want {
		return false
	}
	return countValidExecutableGoArtifacts(cacheRoot) > 0
}

// computeGoCellFingerprint 计算纳入 pig 与追踪二进制 stat、<srcAgentDir>/settings.json
// 内容哈希、SDK 目录与全部 Go 扩展源文件清单的确定性 sha256 指纹。
// 扩展与 SDK 目录一律以 realDir（已解析软链）递归，逻辑名（软链名）参与排序与哈希命名。
// 文件清单覆盖全部普通文件（含 //go:embed 静态资产如 tools.json、locales/*.json），
// 仅跳过 .git / testdata / 点开头隐藏目录与软链/非常规条目——非 .go 资产变更同样失效重热。
func computeGoCellFingerprint(tracked []string, extDirs, sdkDirs []goDirEntry, settings []byte) string {
	h := sha256.New()
	writeStat := func(tag, path string, info os.FileInfo) {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00", tag, path, info.Size(), info.ModTime().UnixNano())
	}
	for _, p := range tracked {
		if info, err := os.Stat(p); err == nil {
			writeStat("bin", p, info)
		}
	}
	// AgentDir 设置内容：启用/禁用扩展或配置变更立即使墓碑失效。
	if settings != nil {
		sum := sha256.Sum256(settings)
		fmt.Fprintf(h, "settings\x00%s\x00", hex.EncodeToString(sum[:]))
	}
	type ent struct {
		ext, rel string
		size     int64
		mtime    int64
	}
	var sdkFiles, extFiles []ent
	collect := func(dir goDirEntry, files *[]ent) {
		_ = filepath.WalkDir(dir.realDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".git" || name == "testdata" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			// 软链条目一律跳过（WalkDir 不跟随软链，这里显式拒绝，防逃逸）；
			// fifo/socket/device 等非常规文件同样不纳入（仅普通文件参与指纹）。
			if d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(dir.realDir, path)
			if err != nil {
				return nil
			}
			*files = append(*files, ent{ext: dir.name, rel: rel, size: info.Size(), mtime: info.ModTime().UnixNano()})
			return nil
		})
	}
	for _, dir := range sdkDirs {
		collect(dir, &sdkFiles)
	}
	for _, dir := range extDirs {
		collect(dir, &extFiles)
	}
	sortByNameRel := func(files []ent) {
		sort.Slice(files, func(i, j int) bool {
			if files[i].ext != files[j].ext {
				return files[i].ext < files[j].ext
			}
			return files[i].rel < files[j].rel
		})
	}
	sortByNameRel(sdkFiles)
	for _, f := range sdkFiles {
		fmt.Fprintf(h, "sdk\x00%s\x00%s\x00%d\x00%d\x00", f.ext, f.rel, f.size, f.mtime)
	}
	sortByNameRel(extFiles)
	for _, f := range extFiles {
		fmt.Fprintf(h, "ext\x00%s\x00%s\x00%d\x00%d\x00", f.ext, f.rel, f.size, f.mtime)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// countValidExecutableGoArtifacts 统计 <cacheRoot> 下两种 Go 产物形态中带
// 执行位的有效产物总数：cache/cells/go/*（多扩展合包 runner）与
// cache/ext/*（单扩展编译产物）。语言门禁：cache/ext 必须 ready.json 显式
// 声明 language=="go"（排除宿主 node-runtime-* 等非 Go 产物）；cells/go
// 目录本身已限定 go，允许 language 缺省或 =="go"。不扫描无关语言 cells/python 等。
//
// 枚举口径与 cellCacheEntryDirs / preparePrivatePigHome / appendCellCachePolicy
// 完全共用同一枚举实体：cacheRoot、cache/ext、cache/cells 根与各语言根本身
// 是符号链接时对应根的整个枚举被跳过，total 不得再因独立 ReadDir 跟随根软链
// 而计入投影为空的 target，造成完整性门禁零投影假放行。
func countValidExecutableGoArtifacts(cacheRoot string) int {
	count := 0
	for _, dir := range cellCacheEntryDirs(cacheRoot) {
		rel := relToCache(cacheRoot, dir)
		inCellsGo := strings.HasPrefix(rel, "cells"+string(filepath.Separator)+"go"+string(filepath.Separator))
		meta, artifact, ok := validCellEntryDir(dir)
		if !ok {
			continue
		}
		if inCellsGo {
			// cells/go 已限定 go：language 缺省或 =="go"。
			if meta.Language != "" && meta.Language != "go" {
				continue
			}
		} else {
			// cache/ext：ready.json 必须显式 language=="go"。
			if meta.Language != "go" {
				continue
			}
		}
		info, err := os.Stat(artifact)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		count++
	}
	return count
}

// isSymlinkPath 对路径做 Lstat：已存在且是符号链接时返回 true。
// 不存在或 Lstat 失败返回 false，由后续 MkdirAll/OpenFile 报真实错误。
func isSymlinkPath(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// warmGoCellMaxAttempts 是预热的有界重试上限：第 0 轮预热后指纹若漂移
// （冷启动落盘 state/pigsdk 或源码 A->B 切换），自动进第 1 轮重验；
// 第 1 轮仍漂移则告警跳过写戳，绝不把过期指纹标为热态。
const warmGoCellMaxAttempts = 2

// warmGoExtensionCells 执行冷路径预热：先取 cache/.locks/sbx-warm.lock 排他锁
// （acquireWarmFlock 非阻塞轮询，等待期间捕获信号立即退出），获锁后重检快路径，
// 再以干净环境与超时执行 pig --help（最多 2 轮）。
// 对齐 preset.go 的软链纪律：pigHome/cache/cache/.locks 任一层是软链即告警
// 返回，绝不在软链目标上创建或写入文件；锁文件以 O_NOFOLLOW 打开。
// 预热期间注册 SIGINT/SIGTERM/SIGHUP/SIGQUIT：捕获信号时取消子进程 ctx
// 触发进程组 SIGKILL，正常退栈跑完所有 defer（清理 /tmp/sbx-warm-*、释放
// flock 与锁句柄），返回 128+信号 由 runMain 直接退出、不再进入沙盒。
func warmGoExtensionCells(pigBin, pigHome, cacheRoot, srcAgentDir string) (exitCode int) {
	if warmCacheChainSymlink(pigHome, cacheRoot) {
		return 0
	}
	lockDir := filepath.Join(cacheRoot, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		warmWarnf("sbx-warm: mkdir lock dir: %v", err)
		return 0
	}
	lockPath := filepath.Join(lockDir, warmGoCellLockName)
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		warmWarnf("sbx-warm: open lock file: %v", err)
		return 0
	}
	defer lockFile.Close()

	// 信号捕获：在 flock 之前注册，保证等待锁期间收到信号也能在中断后
	// 正常退栈清理。sigCtx 取消会触发 runPigWarmSubprocess 内 cmd.Cancel
	// 向整个进程组 SIGKILL。
	var sigSeen atomic.Int32
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	sigCtx, sigCancel := context.WithCancel(context.Background())
	defer sigCancel()
	go func() {
		for {
			select {
			case s := <-sigCh:
				if s != nil {
					if sig, ok := s.(syscall.Signal); ok {
						sigSeen.Store(int32(sig))
					}
				}
				sigCancel()
			case <-sigCtx.Done():
				return
			}
		}
	}()
	defer func() {
		signal.Stop(sigCh)
		if sig := int(sigSeen.Load()); sig != 0 {
			exitCode = 128 + sig
		}
	}()

	if err := acquireWarmFlock(sigCtx, int(lockFile.Fd())); err != nil {
		warmWarnf("sbx-warm: flock: %v", err)
		return 0
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	// 获锁后重检：并发的另一 sbx 可能已完成预热。
	tracked, extDirs, sdkDirs, settings := warmFingerprintInputs(pigBin, pigHome, srcAgentDir)
	if len(extDirs) == 0 {
		return 0
	}
	if isCellCacheWarm(cacheRoot, pigHome, tracked, extDirs, sdkDirs, settings) {
		return 0
	}

	for attempt := 0; attempt < warmGoCellMaxAttempts; attempt++ {
		preTracked, preExt, preSDK, preSettings := warmFingerprintInputs(pigBin, pigHome, srcAgentDir)
		preStamp := computeGoCellFingerprint(preTracked, preExt, preSDK, preSettings)

		if err := runPigWarmSubprocess(sigCtx, pigBin, pigHome, srcAgentDir); err != nil {
			warmWarnf("sbx-warm: pig warm-up failed: %v", err)
			return 0
		}

		// 预热成功后确认至少存在一个有效 Go 产物（cells/go 合包或 cache/ext 单扩展）。
		if countValidExecutableGoArtifacts(cacheRoot) == 0 {
			warmWarnf("sbx-warm: pig warm-up completed but no executable Go artifact found")
			return 0
		}

		// 预热前后指纹一致性：pre/post 各自独立枚举（含 state/pigsdk 与
		// 源码目录），仅一致时才把 postStamp 原子写入墓碑标记热态。
		postTracked, postExt, postSDK, postSettings := warmFingerprintInputs(pigBin, pigHome, srcAgentDir)
		postStamp := computeGoCellFingerprint(postTracked, postExt, postSDK, postSettings)
		if preStamp == postStamp {
			if err := atomicWriteFile(filepath.Join(lockDir, warmGoCellStampName), []byte(postStamp+"\n"), 0o644); err != nil {
				warmWarnf("sbx-warm: write stamp: %v", err)
			}
			return 0
		}
		if attempt == warmGoCellMaxAttempts-1 {
			warmWarnf("sbx-warm: fingerprint drifted during warm-up (attempts exhausted); skipping stamp")
		}
	}
	return 0
}

// warmFlockRetryInterval 是 acquireWarmFlock 等待锁释放的轮询间隔。
const warmFlockRetryInterval = 20 * time.Millisecond

// acquireWarmFlock 以 LOCK_EX|LOCK_NB 轮询获取排他文件锁：遇 EWOULDBLOCK/
// EAGAIN/EINTR 时以 20ms ticker 与 sigCtx.Done() select，等待期间收到信号
// （sigCtx 取消）立即退出返回错误，由调用方退栈跑 defer 并以 128+sig 退出。
// 获锁成功后立即二次检查 sigCtx，避免信号与获锁同拍抵达时误持锁继续预热。
func acquireWarmFlock(sigCtx context.Context, fd int) error {
	ticker := time.NewTicker(warmFlockRetryInterval)
	defer ticker.Stop()
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if cerr := sigCtx.Err(); cerr != nil {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				return cerr
			}
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-sigCtx.Done():
			return sigCtx.Err()
		case <-ticker.C:
		}
	}
}

// runPigWarmSubprocess 用最小干净环境在 0700 私有临时目录中执行
// <pigBin> --no-context-files --no-skills --help。
// sigCtx 是预热级信号取消上下文：与超时 ctx 任一取消都会触发进程组 SIGKILL。
func runPigWarmSubprocess(sigCtx context.Context, pigBin, pigHome, srcAgentDir string) error {
	timeout := warmGoCellTimeout
	if warmGoCellHook != nil && warmGoCellHook.Timeout > 0 {
		timeout = warmGoCellHook.Timeout
	}

	warmTmp, err := os.MkdirTemp("", "sbx-warm-")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(warmTmp)
	warmCwd := filepath.Join(warmTmp, "cwd")
	warmRt := filepath.Join(warmTmp, "rt")
	if err := os.MkdirAll(warmCwd, 0o700); err != nil {
		return fmt.Errorf("mkdir cwd: %w", err)
	}
	if err := os.MkdirAll(warmRt, 0o700); err != nil {
		return fmt.Errorf("mkdir rt: %w", err)
	}

	// 私有 AgentDir：与 ReviewerAgent 沙盒内扩展发现视图 100% 同构——
	// 绝不直接指向 srcAgentDir，而是经共用辅助函数生成 <warmTmp>/pig-agent
	// （白名单配置 0600 副本 + settings.json 显式声明宿主扩展目录）。
	warmAgentDir := preparePrivatePigAgentDirFromAgent(filepath.Join(warmTmp, "pig-agent"), srcAgentDir)

	env := []string{
		"HOME=" + trustedHomeDir(),
		"PIG_HOME=" + pigHome,
		"PIG_CODING_AGENT_DIR=" + warmAgentDir,
		"PI_CODING_AGENT_DIR=" + warmAgentDir,
		"GOWORK=off",
		"GOPROXY=off",
		"TMPDIR=" + warmTmp,
		"XDG_RUNTIME_DIR=" + warmRt,
		"TERM=dumb",
		"NO_COLOR=1",
		"PATH=" + controlledWarmPath(),
	}
	// Go 工具链对齐：goPackedCellHash 把 toolchain 的 commandVersion/goroot/
	// compiler 折进 cell 摘要，预热 cell 与沙盒内 pig 实际构建的 cell 必须用
	// 同一个 go 才能命中同一 <digest>/runner。沙盒内 go 经 goExecDir 包装器
	// 派发 resolveRealGoBinary（宿主 PATH 顺序）选出的真实 go；预热子进程若
	// 用 controlledWarmPath() 的固定目录顺序解析出不同 go（如 /usr/local/go
	// 优先而宿主 PATH 先命中 /usr/bin），cell 摘要必然错配、沙盒内被迫在
	// cells/go 下重建 .build-* 而撞 W^X 只读。因此把沙盒将要选中的真实 go
	// 所在目录提前到受控 PATH 之首，保证 exec.LookPath("go") 两侧解析一致。
	//
	// 所选真实 go 必须经 resolveSandboxRealGoBinary 受信解析（真实路径位于
	// 固定可信工具链前缀内）——绝不把 resolveRealGoBinary 直接按宿主 PATH /
	// PI_SANDBOX_REAL_GO 选中的任意父目录前置进预热 PATH：调用方可控目录内
	// 的「假 go」会在 Landlock 生效前以宿主权限被预热子进程执行
	//（controlledWarmPath 刻意不读调用方 PATH 的既有防线由此被打穿）。
	if realGo := resolveSandboxRealGoBinary(""); realGo != "" {
		if goDir := filepath.Dir(realGo); goDir != "" {
			for i, kv := range env {
				if strings.HasPrefix(kv, "PATH=") {
					env[i] = "PATH=" + goDir + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
					break
				}
			}
		}
	}

	ctx, cancel := context.WithTimeout(sigCtx, timeout)
	defer cancel()

	return execWarmCommand(ctx, pigBin, warmCwd, env)
}

// controlledWarmPath 返回供预热子进程使用的受控 PATH：仅由固定可信
// 系统/Go 工具链目录常量组成，绝不读取调用方 os.Getenv("PATH")——
// 否则宿主 PATH 中的恶意前置目录会被拼进子进程 PATH 击穿隔离。
// 不存在的目录仍保留拼接无害（exec 找不到就跳过）。
func controlledWarmPath() string {
	dirs := []string{
		"/usr/local/go/bin",
		"/usr/lib/go/bin",
		"/root/.local/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
	}
	seen := make(map[string]struct{}, len(dirs)+len(standardBinDirs))
	for _, d := range dirs {
		seen[d] = struct{}{}
	}
	for _, d := range standardBinDirs {
		if _, ok := seen[d]; !ok {
			seen[d] = struct{}{}
			dirs = append(dirs, d)
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// warmStderrCaptureLimit 是预热子进程 stderr 的最大保留字节数（尾部环形缓冲）。
const warmStderrCaptureLimit = 4096

// warmStderrScanOverlap 是 stderr 错误标记跨块扫描的重叠窗口：大于最长标记
// 长度，保证标记即使被 4KB 环形缓冲切分也能命中。
const warmStderrScanOverlap = 128

// warmFatalStderrMarkers 是 pig 扩展加载/编译失败但进程仍退 0 的 stderr
// 标记子串：真实 pig --help 在 Go 扩展编译失败时退出码为 0，仅在 stderr
// 输出这些行。命中即视为预热失败，绝不写入指纹墓碑。
var warmFatalStderrMarkers = []string{
	"Failed to load extension",
	"extension build failed",
}

// stderrScanWriter 是流式 stderr 扫描器：边写尾部环形缓冲边对跨块边界
// 做子串匹配（维护 overlap 窗口，不受缓冲截断影响）。mu 保护全部字段：
// cmd.WaitDelay 到期后后台管道拷贝协程可能仍在 Write，主协程经 Snapshot
// 读取时必须加锁，消除 data race。
type stderrScanWriter struct {
	mu      sync.Mutex
	tail    *tailBuffer
	pending []byte
	hit     string
}

func newStderrScanWriter() *stderrScanWriter {
	return &stderrScanWriter{tail: newTailBuffer(warmStderrCaptureLimit)}
}

func (s *stderrScanWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hit == "" {
		window := append(append([]byte(nil), s.pending...), p...)
		for _, m := range warmFatalStderrMarkers {
			if strings.Contains(string(window), m) {
				s.hit = m
				break
			}
		}
	}
	if len(p) > warmStderrScanOverlap {
		s.pending = append(s.pending[:0], p[len(p)-warmStderrScanOverlap:]...)
	} else {
		s.pending = append(s.pending, p...)
		if len(s.pending) > warmStderrScanOverlap {
			s.pending = append(s.pending[:0], s.pending[len(s.pending)-warmStderrScanOverlap:]...)
		}
	}
	return s.tail.Write(p)
}

// Snapshot 返回当前命中的失败标记与 stderr 尾部快照（加锁读取，并发安全）。
func (s *stderrScanWriter) Snapshot() (hit, tail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hit, s.tail.String()
}

// tailBuffer 是保留最后 limit 字节的环形 writer，用于限长捕获子进程 stderr。
type tailBuffer struct {
	buf   []byte
	start int
	full  bool
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{buf: make([]byte, limit)}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= len(t.buf) {
		copy(t.buf, p[n-len(t.buf):])
		t.start = 0
		t.full = true
		return n, nil
	}
	for _, b := range p {
		t.buf[t.start] = b
		t.start = (t.start + 1) % len(t.buf)
		if t.start == 0 {
			t.full = true
		}
	}
	return n, nil
}

func (t *tailBuffer) String() string {
	if !t.full {
		return string(t.buf[:t.start])
	}
	out := make([]byte, len(t.buf))
	copy(out, t.buf[t.start:])
	copy(out[len(t.buf)-t.start:], t.buf[:t.start])
	return string(out)
}

// execWarmCommand 启动 pig 子进程并等待结束；超时或取消时向整个进程组发
// SIGKILL。stderr 经限长环形缓冲捕获并流式扫描扩展加载/编译失败标记：
// 进程退 0 但命中标记同样返回错误，绝不写指纹墓碑。
func execWarmCommand(ctx context.Context, pigBin, warmCwd string, env []string) error {
	cmd := exec.CommandContext(ctx, pigBin,
		"--no-context-files", "--no-skills", "--help")
	cmd.Dir = warmCwd
	cmd.Env = env
	cmd.Stdout = io.Discard
	stderrScanner := newStderrScanWriter()
	cmd.Stderr = stderrScanner
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// WaitDelay 限制 cmd.Run 等待 stdout/stderr 管道 EOF 的时间：
	// 若孙子进程脱离进程组仍持有 stderr fd，Kill(-pid) 后仍保证有界返回。
	cmd.WaitDelay = 2 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	err := cmd.Run()
	hit, tail := stderrScanner.Snapshot()
	if hit != "" {
		return fmt.Errorf("pig reported extension load/build failure on stderr (marker %q)", hit)
	}
	if err != nil {
		if t := strings.TrimSpace(tail); t != "" {
			return fmt.Errorf("%w (stderr tail: %s)", err, t)
		}
		return err
	}
	return nil
}

// atomicWriteFile 通过同目录临时文件 + Rename 原子写入目标文件。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// warmWarnf 向 stderr 输出单行精简告警。
func warmWarnf(format string, args ...any) {
	w := io.Writer(os.Stderr)
	if warmGoCellHook != nil && warmGoCellHook.Stderr != nil {
		w = warmGoCellHook.Stderr
	}
	fmt.Fprintf(w, "sbx: "+format+"\n", args...)
}
