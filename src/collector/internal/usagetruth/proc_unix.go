//go:build unix

package usagetruth

import (
	"os/exec"
	"syscall"
)

// setNewProcessGroup starts cmd in its own process group (pgid == its own
// pid), so killProcessGroup below can signal the whole tree — including a
// grandchild like `docker run` that reparents to PID 1 (and keeps running)
// the moment exec.CommandContext's default, child-only kill fires on
// timeout.
func setNewProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to cmd's entire process group via the
// negative-PID convention (kill(2)) — reaching grandchildren that a plain
// cmd.Process.Kill() (PID only) leaves behind.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
