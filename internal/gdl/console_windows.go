package gdl

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// A GUI-subsystem parent has no console to share, so each console child
// would open its own window.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
