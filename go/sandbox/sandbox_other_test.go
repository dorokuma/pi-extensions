//go:build !linux

package sandbox_test

import "errors"

// occupySeccompListenerSlot 的非 Linux 存根：seccomp 监听槽位仅 Linux 存在。
// 非 Linux 下 subprocSeccompFailure 不会被触发（requireLandlock 先跳过），
// 此处仅保证 GOOS!=linux 编译通过。
func occupySeccompListenerSlot() (int, error) {
	return -1, errors.New("occupySeccompListenerSlot: seccomp requires linux")
}
