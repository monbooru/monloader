//go:build !windows

package gdl

import (
	"os/exec"
	"syscall"
)

// A one-file gallery-dl is a bootloader and an interpreter child, so a
// cancel kills the whole group: the child alone would go on downloading.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
