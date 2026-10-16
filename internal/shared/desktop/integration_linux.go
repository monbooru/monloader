package desktop

import (
	"bytes"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
)

func menuSupported() bool { return !Sandboxed() && !underSystemPrefix() }

func autostartSupported() bool { return true }

func shareRoot() (string, error) { return dataHome("") }

func menuEntryPath(app string) (string, error) {
	dir, err := shareRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "applications", app+".desktop"), nil
}

// The sandbox redirects XDG_CONFIG_HOME to where no session manager
// looks, so Flatpak writes the real ~/.config.
func autostartPath(app string) (string, error) {
	if Sandboxed() {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "autostart", app+".desktop"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart", app+".desktop"), nil
}

func menuEnabled(app string) bool { return fileAt(menuEntryPath(app)) }

func autostartEnabled(app string) bool { return fileAt(autostartPath(app)) }

func enableMenu(h Hook) error {
	path, err := menuEntryPath(h.App)
	if err != nil {
		return err
	}
	icon := h.App
	if err := installIcon(h); err != nil {
		// A missing icon costs a generic launcher tile, not the entry.
		icon = ""
	}
	return writeFile(path, desktopEntry(h, icon, false))
}

func disableMenu(app string) error { return removeAt(menuEntryPath(app)) }

func enableAutostart(h Hook) error {
	path, err := autostartPath(h.App)
	if err != nil {
		return err
	}
	return writeFile(path, desktopEntry(h, h.App, true))
}

func disableAutostart(app string) error { return removeAt(autostartPath(app)) }

// GNOME reads X-GNOME-Autostart-enabled to tell whether its own settings
// disabled the entry.
func desktopEntry(h Hook, icon string, autostart bool) string {
	body := "[Desktop Entry]\nType=Application\n" +
		"Name=" + h.Name + "\n"
	if h.Comment != "" {
		body += "Comment=" + h.Comment + "\n"
	}
	body += "Exec=" + LaunchCommand() + "\n"
	if icon != "" {
		body += "Icon=" + icon + "\n"
	}
	body += "Terminal=false\nCategories=" + h.Categories + "\n"
	if autostart {
		body += "X-GNOME-Autostart-enabled=true\n"
	}
	return body
}

// Under its real size: the icon lookup keys on the hicolor size directory.
func installIcon(h Hook) error {
	if len(h.Icon) == 0 {
		return fmt.Errorf("no icon")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(h.Icon))
	if err != nil {
		return err
	}
	dir, err := shareRoot()
	if err != nil {
		return err
	}
	size := fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
	path := filepath.Join(dir, "icons", "hicolor", size, "apps", h.App+".png")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, h.Icon, 0o644)
}

func underSystemPrefix() bool {
	dir := ExeDir()
	if dir == "" {
		return false
	}
	for _, prefix := range []string{"/usr/", "/opt/"} {
		if strings.HasPrefix(dir+"/", prefix) {
			return true
		}
	}
	return false
}

func writeFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func fileAt(path string, err error) bool { return err == nil && IsFile(path) }

func removeAt(path string, err error) error {
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
