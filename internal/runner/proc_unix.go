//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// setProcessGroup 让取消时能杀掉脚本派生的整个进程组，而不只是 sh 本身。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
