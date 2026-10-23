package gdl

import (
	"os/exec"
	"strconv"

	"github.com/monbooru/monloader/internal/shared/desktop"
)

// Windows has no process group to signal, so taskkill walks the tree; a
// one-file gallery-dl's interpreter child would otherwise go on downloading.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		desktop.HideConsole(kill)
		if kill.Run() == nil {
			return nil
		}
		return cmd.Process.Kill()
	}
}
