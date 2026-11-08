//go:build !linux

package main

import "syscall"

// childSysProcAttr 非 Linux 平台无挂载命名空间隔离，返回 nil；
// 对应 childExecSpec.MountNS 恒为 false，isolateRuntimeSockets 不会执行。
func childSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// isolateRuntimeSockets 非 Linux 平台为空操作。
func isolateRuntimeSockets(bestEffort bool) error {
	return nil
}

// sealMountNamespace 非 Linux 平台无挂载命名空间 seccomp 封锁，为空操作。
func sealMountNamespace() error {
	return nil
}
