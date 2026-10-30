package desktop

import (
	"fmt"
	"os/exec"
	"runtime"
)

func OpenBrowser(url string) error { return launch(url) }

func OpenFolder(path string) error { return launch(path) }

// The exit status is ignored: explorer.exe exits non-zero even on a
// successful open.
func launch(target string) error {
	name := opener()
	cmd := exec.Command(name, target)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func opener() string {
	switch runtime.GOOS {
	case "windows":
		return "explorer"
	case "darwin":
		return "open"
	default:
		return "xdg-open"
	}
}
