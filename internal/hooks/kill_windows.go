//go:build windows

package hooks

import (
	"os/exec"
	"strconv"
)

// killTree 让超时取消作用于整棵进程树。
//
// Windows 没有进程组信号，cmd /C 下面的子进程要显式按进程树杀。
// 只 TerminateProcess 直接子进程的话，钩子里起的程序会留成孤儿进程。
func killTree(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		pid := strconv.Itoa(cmd.Process.Pid)
		// 先试 taskkill 杀树；失败再退回单进程结束，至少不留直接子进程。
		if err := exec.Command("taskkill", "/F", "/T", "/PID", pid).Run(); err == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
