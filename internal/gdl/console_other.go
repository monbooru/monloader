//go:build !windows

package gdl

import "os/exec"

func hideConsole(_ *exec.Cmd) {}
