//go:build !windows

package android

import (
	"os/exec"
	"syscall"
)

// detach 让进程脱离插件进程的会话与进程组（模拟器在运行停止、插件退出后继续开着）。
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// hideWindow 在 Unix 上无事可做。
func hideWindow(cmd *exec.Cmd) {}
