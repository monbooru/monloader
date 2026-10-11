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
	name, args := opener(target)
	if name == "" {
		return fmt.Errorf("no opener for %s", runtime.GOOS)
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func opener(target string) (string, []string) {
	switch runtime.GOOS {
	case "windows":
		return "explorer", []string{target}
	case "darwin":
		return "open", []string{target}
	default:
		return "xdg-open", []string{target}
	}
}
