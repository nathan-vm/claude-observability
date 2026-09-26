//go:build windows

package usagetruth

import "os/exec"

// setNewProcessGroup is a no-op on Windows: grouping a process tree for a
// single kill signal needs a job object, a materially bigger mechanism than
// POSIX Setpgid, and this fix's motivating case (a `docker run` grandchild
// reparenting to PID 1 on kill) is a Unix process-model-specific failure
// mode.
func setNewProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup kills only the direct child, same as
// exec.CommandContext's own default Cancel would have done anyway — this
// platform is strictly no worse than before this fix, not improved.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
