package desktop

import (
	"fmt"
	"os/exec"
	"runtime"
)

// OpenBrowser points the desktop's default browser at url.
func OpenBrowser(url string) error { return launch(url) }

// OpenFolder shows path in the desktop's file manager.
func OpenFolder(path string) error { return launch(path) }

// launch hands a URL or a path to the platform's opener. The process is
// started and not waited on: explorer.exe exits non-zero on perfectly
// successful opens, and none of the three tells us anything useful once
// the handler has been handed the target.
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
