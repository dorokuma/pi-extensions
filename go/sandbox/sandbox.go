// Package sandbox 提供基于 Linux Landlock LSM 的无特权进程沙盒封装。
package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrUnsupported 表示当前内核不支持或未启用 Landlock LSM。
	ErrUnsupported = errors.New("sandbox: landlock is not supported by the running kernel")
	// ErrNetUnsupported 表示当前内核 Landlock ABI 版本低于 4，不支持 TCP 网络隔离。
	ErrNetUnsupported = errors.New("sandbox: Landlock network restriction requires ABI >= 4")
	// ErrInvalidPolicy 表示策略配置存在非法路径或参数。
	ErrInvalidPolicy = errors.New("sandbox: invalid policy")
	// ErrEmptyPolicy 表示策略既未指定任何文件路径，也未启用网络隔离。
	ErrEmptyPolicy = errors.New("sandbox: policy specifies no filesystem rules and DenyNet is false")
	// ErrForbiddenExec 表示 Exec 白名单中包含被禁止的嵌套执行器或动态链接器。
	ErrForbiddenExec = errors.New("sandbox: executable is in forbidden list")
	// ErrSeccomp 表示安装或初始化 seccomp PROT_EXEC 内存映射守卫失败。
	ErrSeccomp = errors.New("sandbox: failed to install seccomp exec guard")
)

// Policy 定义 Landlock 沙盒的访问控制策略。
//
// 当 ReadOnly、ReadWrite 或 Exec 中任意一项非空时，文件系统进入白名单模式：
// 除显式声明的路径外，其余所有受 Landlock 管控的文件系统访问均被拒绝。
// ReadWrite 默认仅授予读写、创建、删除、截断权限，不授予执行权限（严格遵循 W^X 原则）。
// 在 ReadOnlyReviewer 预设下，所有可写目录（含 <workdir>/reports、TMPDIR、GOTMPDIR、GOCACHE）
// 均不具备执行权限；go test 编译产物由受控的 Go 工具链包装器在独立的只读可执行目录中构建并降权运行。
type Policy struct {
	// ReadOnly 允许只读访问的目录或文件路径列表。
	ReadOnly []string
	// ReadWrite 允许读写访问的目录或文件路径列表（默认不含执行权限）。
	ReadWrite []string
	// Exec 允许执行（execve）的二进制文件或目录路径白名单。
	Exec []string
	// DenyNet 为 true 时禁止所有 TCP 端口的 bind 与 connect 操作（需 Landlock ABI >= 4）。
	DenyNet bool

	// Env 需要在沙盒生效时注入当前进程及子进程的环境变量列表（格式为 "KEY=VALUE"）。
	// 例如 ReadOnlyReviewer 会在此设置指向私有可写目录的 TMPDIR、GOTMPDIR、GOCACHE 以及受控 PATH。
	Env []string

	// BestEffort 为 true 时，若内核不支持 Landlock，或 ABI < 4 不支持 DenyNet，
	// 或 seccomp 守卫无法安装，或部分路径不存在，将输出告警并降级执行而非返回错误。
	BestEffort bool

	// EntryAgents 列出允许出现在 Exec 白名单中的代理二进制绝对路径（agy/pig/pi/claude 及其
	// 符号链接解析目标）。这些路径的 basename 通常命中 ForbiddenExecNames，但
	// ReviewerAgent 预设需要顶层启动它们。Landlock 按路径放行，无法做到「只允许
	// 第一次 exec」；嵌套再起同名代理仍会被 Landlock 允许，需靠角色工具白名单兜底。
	EntryAgents []string

	// WarnWriter 指定 BestEffort 模式下的告警输出目标；为 nil 时默认输出到 os.Stderr。
	WarnWriter io.Writer
}

// WithBestEffort 返回开启或关闭 BestEffort 降级模式的 Policy 副本。
func (p Policy) WithBestEffort(enabled bool) Policy {
	p.BestEffort = enabled
	return p
}

// NormalizePath 校验并将路径规范化为干净的绝对路径。
func NormalizePath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalidPolicy)
	}
	if strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("%w: path contains NUL byte", ErrInvalidPolicy)
	}
	cleaned := filepath.Clean(raw)
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", fmt.Errorf("%w: resolve abs path %q: %v", ErrInvalidPolicy, raw, err)
	}
	return abs, nil
}

// NormalizePaths 批量校验、规范化路径并按首次出现顺序去重。
func NormalizePaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		norm, err := NormalizePath(raw)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[norm]; !ok {
			seen[norm] = struct{}{}
			out = append(out, norm)
		}
	}
	return out, nil
}

// Validate 校验 Policy 的合法性，包括非空检查、路径规范化、禁止执行的二进制与动态链接器拦截以及环境变量格式校验。
func (p Policy) Validate() error {
	if len(p.ReadOnly) == 0 && len(p.ReadWrite) == 0 && len(p.Exec) == 0 && !p.DenyNet {
		return ErrEmptyPolicy
	}
	if _, err := NormalizePaths(p.ReadOnly); err != nil {
		return fmt.Errorf("ReadOnly: %w", err)
	}
	if _, err := NormalizePaths(p.ReadWrite); err != nil {
		return fmt.Errorf("ReadWrite: %w", err)
	}
	execPaths, err := NormalizePaths(p.Exec)
	if err != nil {
		return fmt.Errorf("Exec: %w", err)
	}
	entrySet := entryAgentPathSet(p.EntryAgents)
	for _, ep := range execPaths {
		if IsELFInterpreter(ep) {
			return fmt.Errorf("%w: %s", ErrForbiddenExec, ep)
		}
		if IsForbiddenExec(ep) {
			if _, ok := entrySet[ep]; !ok {
				return fmt.Errorf("%w: %s", ErrForbiddenExec, ep)
			}
		}
	}
	for _, kv := range p.Env {
		if err := validateEnvEntry(kv); err != nil {
			return fmt.Errorf("Env: %w", err)
		}
	}
	return nil
}

func validateEnvEntry(kv string) error {
	if strings.ContainsRune(kv, 0) {
		return fmt.Errorf("%w: env entry contains NUL byte", ErrInvalidPolicy)
	}
	k, _, ok := strings.Cut(kv, "=")
	if !ok || strings.TrimSpace(k) == "" || strings.ContainsRune(k, '=') {
		return fmt.Errorf("%w: invalid env entry %q (expected KEY=VALUE)", ErrInvalidPolicy, kv)
	}
	return nil
}

func applyEnv(env []string) error {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("%w: invalid env entry %q", ErrInvalidPolicy, kv)
		}
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("sandbox: setenv %q: %w", k, err)
		}
	}
	return nil
}

// lookupEnvValue 返回 env 中首个 key= 前缀条目的值；未命中返回空串。
// 供跨平台预设构造（preset.go）与 Linux 实现（landlock_linux.go）共用，
// 必须留在无构建标签的文件中，否则 GOOS!=linux 编译缺符号。
func lookupEnvValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

// overrideEnvKey 将 env 中 key= 前缀条目替换为 key=val；不存在则追加。
// 同名键出现多次时仅在首个命中位置替换一次，后续重复条目跳过（去重，
// 消除调用方传入切片本身含重复同名键时的双键残留）。
// 供跨平台预设构造（preset.go）与 Linux 实现（landlock_linux.go）共用，
// 必须留在无构建标签的文件中，否则 GOOS!=linux 编译缺符号。
func overrideEnvKey(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			if replaced {
				continue
			}
			out = append(out, prefix+val)
			replaced = true
		} else {
			out = append(out, kv)
		}
	}
	if !replaced {
		out = append(out, prefix+val)
	}
	return out
}

func (p Policy) warnf(format string, args ...any) {
	w := p.WarnWriter
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// Supported 检测当前内核是否支持并启用了 Landlock。
// 若支持，返回最高支持的 ABI 版本号 (>= 1) 和 true；否则返回 (0, false)。
func Supported() (abi int, ok bool) {
	return supported()
}

// Apply 将给定沙盒策略应用到当前进程（及其之后 fork/exec 的所有子进程），
// 并将 Policy.Env 中声明的环境变量写入当前进程环境。
// 该操作一旦成功不可撤销，后续只能继续叠加更严格的规则。
func Apply(p Policy) error {
	return applyPolicy(p)
}
