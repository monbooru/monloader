package desktop

import (
	"fmt"
	"os"
	"strings"
)

// Hook's Icon must be a PNG; nil installs none.
type Hook struct {
	App     string
	Name    string
	Comment string
	Icon    []byte
}

// MenuSupported is false for a packaged or sandboxed install, which ships
// its own menu entry; a second would only duplicate it.
func (h Hook) MenuSupported() bool { return menuSupported() }

func (h Hook) MenuEnabled() bool { return menuEnabled(h.App) }

func (h Hook) SetMenu(on bool) error {
	if !h.MenuSupported() {
		return fmt.Errorf("the applications menu entry is provided by the install here")
	}
	if on {
		return enableMenu(h)
	}
	return disableMenu(h.App)
}

func (h Hook) AutostartSupported() bool { return autostartSupported() }

func (h Hook) AutostartEnabled() bool { return autostartEnabled(h.App) }

func (h Hook) SetAutostart(on bool) error {
	if !h.AutostartSupported() {
		return fmt.Errorf("start at login is not supported on this platform")
	}
	if on {
		return enableAutostart(h)
	}
	return disableAutostart(h.App)
}

// Unquoted: each sink quotes by its own rules. In a sandbox only the
// host's flatpak run form can launch the app.
func launchTarget() (string, []string) {
	if id := os.Getenv("FLATPAK_ID"); id != "" {
		return "flatpak", []string{"run", id, "-desktop"}
	}
	exe := Program()
	if exe == "" {
		return "", nil
	}
	return exe, []string{"-desktop"}
}

// LaunchCommand is the Exec line a .desktop entry runs.
func LaunchCommand() string {
	exe, args := launchTarget()
	if exe == "" {
		return ""
	}
	return strings.Join(append([]string{execQuote(exe)}, args...), " ")
}

func execQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\$`") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`)
	return `"` + r.Replace(s) + `"`
}
