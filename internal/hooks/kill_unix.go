//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

// killTree 让超时取消作用于整棵进程树。
//
// sh -c 会 fork 出真正干活的子进程，而 exec.CommandContext 的默认行为
// 只 kill sh 本身——子进程被过继给 init 继续跑。把子进程放进独立进程组，
// 取消时对整个组发信号，才真的停得下来。
func killTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// 负 pid = 整个进程组。
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
