package gdl

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// hideConsole keeps a subprocess from opening a console window. The desktop
// build has no console of its own, so Windows gives every console program it
// starts a fresh one: a black box over the desktop for the length of a pass.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}
