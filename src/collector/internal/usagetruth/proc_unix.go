//go:build unix

package usagetruth

import (
	"errors"
	"os"
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
//
// ESRCH (the group is already gone — claude exited on its own right as the
// timeout fired) is mapped to os.ErrProcessDone: os/exec's Cmd.Cancel
// contract only treats that sentinel as "not a real Cancel failure", so a
// raw ESRCH would otherwise surface as a spurious "exec: canceling Cmd: no
// such process" on an already-successful run, same as cmd.Process.Kill()
// (the default Cancel this replaces) already handles via its own state
// tracking.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
