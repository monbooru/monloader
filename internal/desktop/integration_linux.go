package desktop

import (
	"bytes"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monloader/internal/fsx"
)

func menuSupported() bool { return !Sandboxed() && !underSystemPrefix() }

func autostartSupported() bool { return true }

// shareRoot is the XDG data root the applications and icons directories
// hang off; dataHome with no app name resolves exactly that.
func shareRoot() (string, error) { return dataHome("") }

// menuEntryPath is where an unpackaged install puts its own launcher entry.
func menuEntryPath(app string) (string, error) {
	dir, err := shareRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "applications", app+".desktop"), nil
}

// autostartPath is the XDG autostart entry. Under Flatpak it is written to
// the real ~/.config, not to XDG_CONFIG_HOME, which the sandbox redirects
// into its own directory where no session manager looks.
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

func menuEnabled(app string) bool {
	path, err := menuEntryPath(app)
	return err == nil && fsx.IsFile(path)
}

func autostartEnabled(app string) bool {
	path, err := autostartPath(app)
	return err == nil && fsx.IsFile(path)
}

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

func disableMenu(app string) error {
	path, err := menuEntryPath(app)
	if err != nil {
		return err
	}
	return removeFile(path)
}

func enableAutostart(h Hook) error {
	path, err := autostartPath(h.App)
	if err != nil {
		return err
	}
	return writeFile(path, desktopEntry(h, h.App, true))
}

func disableAutostart(app string) error {
	path, err := autostartPath(app)
	if err != nil {
		return err
	}
	return removeFile(path)
}

// desktopEntry renders a .desktop file. autostart adds the key GNOME reads
// to decide whether a user disabled the entry from its own settings.
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
	body += "Terminal=false\nCategories=Graphics;Photography;\n"
	if autostart {
		body += "X-GNOME-Autostart-enabled=true\n"
	}
	return body
}

// installIcon drops the PNG into the hicolor theme under the size it
// actually is, which is what the icon lookup keys on.
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

// underSystemPrefix reports whether the executable was installed by a
// package manager, which then owns the menu entry.
func underSystemPrefix() bool {
	dir := fsx.ExeDir()
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

// writeFile writes body to path, creating the directory.
func writeFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// removeFile drops path, treating an already-absent file as success.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
