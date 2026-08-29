//go:build !windows

package gdl

import "os/exec"

// hideConsole is a no-op off Windows, where a subprocess opens no window.
func hideConsole(_ *exec.Cmd) {}
