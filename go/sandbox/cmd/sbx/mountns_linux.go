//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// 沙盒 socket 白名单视图（仅 Linux，需 CLONE_NEWNS）：
// sbx 外层父进程以 Unshareflags=CLONE_NEWNS 派生受限子进程，子进程在进入
// Landlock 之前（runChildFromEnv → isolateRuntimeSockets → Apply）把宿主
// 暴露的 IPC socket 面收成白名单视图：
//   - /run/user/0 被空 tmpfs 覆盖，仅回绑 moshi-hook.sock（agy 钩子遥测）；
//   - /tmp/tmux-0 存在时被空 tmpfs 覆盖（tmux send-keys 注入面）；
//   - 覆盖路径不存在时跳过；任一步骤失败默认拒绝启动（fail-closed），
//     --best-effort 下只告警降级。
//
// 已知边界（按设计保持可达）：/root/.herdsman/herdsman.sock（herdsman 扩展
// 必需）、/root/.config/herdr/herdr.sock（老 herdr agent 状态上报）、
// goExecDir 内 broker.sock 与 report.sock。

const (
	userRuntimeDir    = "/run/user/0"
	tmuxSockDir       = "/tmp/tmux-0"
	moshiHookSockName = "moshi-hook.sock"
	sandboxStagePref  = "pi-sbx-stage-"
)

// childSysProcAttr 返回派生受限子进程使用的 SysProcAttr：CLONE_NEWNS 创建
// 独立挂载命名空间。Go 运行时在 unshare 后自动把 / 置为 rprivate
// （src/syscall/exec_linux.go: unshare 不处理 --shared 挂载的传播标记，
// Go 会补 mount(NULL, "/", NULL, MS_REC|MS_PRIVATE, NULL)），子进程内的
// 覆盖挂载不会传播回宿主挂载命名空间。
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
}

// isolateRuntimeSockets 在受限子进程内（进入 Landlock 之前）施加白名单视图。
// 仅当父进程确实以 CLONE_NEWNS 派生（childExecSpec.MountNS=true）时才被调用；
// 任一覆盖/回绑步骤失败默认返回错误拒绝启动，bestEffort 时仅告警并继续。
func isolateRuntimeSockets(bestEffort bool) error {
	step := func(name string, fn func() error) error {
		if err := fn(); err != nil {
			if bestEffort {
				fmt.Fprintf(os.Stderr, "sbx: WARN mount step %s failed, socket hiding degraded: %v\n", name, err)
				return nil
			}
			return fmt.Errorf("mount step %s failed: %w", name, err)
		}
		return nil
	}

	// Go 运行时在 CLONE_NEWNS 后已自动执行一次；此处显式再置一次，语义独立于
	// runtime 实现细节，确保后续覆盖挂载不传播回宿主挂载命名空间。
	if err := step("private-propagation", func() error {
		return unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
	}); err != nil {
		return err
	}
	if err := step("cover-tmux-sockets", coverTmuxSockets); err != nil {
		return err
	}
	return step("cover-user-runtime-dir", hideUserRuntimeDir)
}

// coverTmuxSockets 以空只读 tmpfs 覆盖 /tmp/tmux-0（存在时），切断沙盒内
// 进程向任意宿主终端注入按键的 tmux socket 通道。
func coverTmuxSockets() error {
	st, err := os.Stat(tmuxSockDir)
	if err != nil || !st.IsDir() {
		return nil // 覆盖路径不存在时跳过
	}
	return unix.Mount("tmpfs", tmuxSockDir, "tmpfs",
		unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0555")
}

// hideUserRuntimeDir 以 tmpfs 覆盖 /run/user/0，隐藏 systemd private bus、
// user bus、herdr-core、gnupg、openssh_agent 等全部 pathname socket；随后只把
// agy 钩子遥测所需的 moshi-hook.sock 回绑回原路径，并把覆盖层收紧为只读。
// 覆盖路径不存在时跳过。
func hideUserRuntimeDir() error {
	st, err := os.Stat(userRuntimeDir)
	if err != nil || !st.IsDir() {
		return nil
	}

	origSock := filepath.Join(userRuntimeDir, moshiHookSockName)
	haveMoshi := false
	if s, err := os.Stat(origSock); err == nil && s.Mode()&os.ModeSocket != 0 {
		haveMoshi = true
	}
	if !haveMoshi {
		// 无必需回绑项：直接以只读空 tmpfs 覆盖。
		return unix.Mount("tmpfs", userRuntimeDir, "tmpfs",
			unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755")
	}

	// moshi-hook.sock 需要保持可达：覆盖前先把它 bind 到 /tmp 下私有暂存点
	//（bind 挂载在覆盖后仍引用原 socket），再以可写 tmpfs 覆盖、重建挂载点并
	// 回绑，最后把覆盖层 remount 为只读（MS_REMOUNT 只作用于覆盖层自身，
	// 不影响其上的回绑挂载）。
	stage, err := os.MkdirTemp("/tmp", sandboxStagePref)
	if err != nil {
		return fmt.Errorf("create stage dir: %w", err)
	}
	defer func() {
		_ = unix.Unmount(filepath.Join(stage, moshiHookSockName), unix.MNT_DETACH)
		_ = os.RemoveAll(stage)
	}()

	stageSock := filepath.Join(stage, moshiHookSockName)
	if f, err := os.OpenFile(stageSock, os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		return fmt.Errorf("create stage sock mountpoint: %w", err)
	} else {
		_ = f.Close()
	}
	if err := unix.Mount(origSock, stageSock, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("stage %s: %w", moshiHookSockName, err)
	}

	if err := unix.Mount("tmpfs", userRuntimeDir, "tmpfs",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755"); err != nil {
		return fmt.Errorf("cover %s: %w", userRuntimeDir, err)
	}
	newSock := filepath.Join(userRuntimeDir, moshiHookSockName)
	if f, err := os.OpenFile(newSock, os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		return fmt.Errorf("create %s mountpoint: %w", moshiHookSockName, err)
	} else {
		_ = f.Close()
	}
	if err := unix.Mount(stageSock, newSock, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("rebind %s: %w", moshiHookSockName, err)
	}
	if err := unix.Mount("tmpfs", userRuntimeDir, "tmpfs",
		unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755"); err != nil {
		return fmt.Errorf("remount %s read-only: %w", userRuntimeDir, err)
	}
	return nil
}
