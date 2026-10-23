//go:build !windows

package desktop

import "os/exec"

func AttachConsole() {}

func HideConsole(*exec.Cmd) {}
