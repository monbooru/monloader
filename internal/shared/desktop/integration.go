package desktop

import (
	"fmt"
	"os"
	"strings"
)

// Hook's Icon must be a PNG; nil installs none. Categories is the menu
// entry's freedesktop list, ";"-terminated.
type Hook struct {
	App        string
	Name       string
	Comment    string
	Categories string
	Icon       []byte
}

func (h Hook) Apply(menu, autostart bool) []string {
	var failures []string
	if h.MenuSupported() {
		if err := h.SetMenu(menu); err != nil {
			failures = append(failures, "menu entry: "+err.Error())
		}
	}
	if h.AutostartSupported() {
		if err := h.SetAutostart(autostart); err != nil {
			failures = append(failures, "start at login: "+err.Error())
		}
	}
	return failures
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

// Menu and autostart are read from disk so a hand-removed file shows as off.
type Integration struct {
	MenuSupported      bool
	MenuEnabled        bool
	AutostartSupported bool
	AutostartEnabled   bool
	TraySupported      bool
	TrayEnabled        bool
}

func (d Integration) Supported() bool { return d.MenuSupported || d.AutostartSupported }

func (h Hook) Integration(trayEnabled bool) Integration {
	return Integration{
		MenuSupported:      h.MenuSupported(),
		MenuEnabled:        h.MenuEnabled(),
		AutostartSupported: h.AutostartSupported(),
		AutostartEnabled:   h.AutostartEnabled(),
		TraySupported:      TrayAvailable(),
		TrayEnabled:        trayEnabled,
	}
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
	if strings.ContainsAny(s, " \t\n\"'\\$`<>~|&;*?#()") {
		// The key file's string escapes are undone first: each quoting backslash is doubled.
		r := strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "`", "\\\\`", `$`, `\\$`)
		s = `"` + r.Replace(s) + `"`
	}
	return strings.ReplaceAll(s, "%", "%%")
}
