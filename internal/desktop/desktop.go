// Package desktop resolves the per-OS locations and the one-time startup
// behaviour the -desktop profile needs. It knows nothing about the app it
// serves beyond the name it is handed, so both halves of the pair can use
// the same shape without sharing a config. Copied from monbooru, which is
// where a fix to the shared files belongs too.
package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/monbooru/monloader/internal/fsx"
)

// Layout is where one app keeps its files under the desktop profile.
// Portable records that the config came from beside the executable, which
// also moves the data directory there.
type Layout struct {
	ConfigPath string
	ConfigDir  string
	DataDir    string
	LogDir     string
	Portable   bool
}

// Resolve builds the layout for app ("monloader"). explicitConfig is the
// -config value when the operator passed one and always wins, so the
// container entrypoint and the healthcheck are unaffected by the profile.
// Otherwise a config already sitting beside the executable is used in
// place (portable mode: a folder on a stick that carries its own data),
// and failing that the OS config directory, whose file Load then seeds.
func Resolve(app, explicitConfig string) (Layout, error) {
	data, err := dataHome(app)
	if err != nil {
		return Layout{}, err
	}
	l := Layout{DataDir: data}
	switch {
	case explicitConfig != "":
		l.ConfigPath = explicitConfig
	default:
		// A file test, never a write attempt, so an executable in a
		// read-only location does not try to seed one there. Skipped in a
		// sandbox, whose directories are already private to the install.
		if !Sandboxed() {
			if dir := InstallDir(); dir != "" {
				if p := filepath.Join(dir, app+".toml"); fsx.IsFile(p) {
					l.ConfigPath, l.Portable = p, true
					l.DataDir = filepath.Join(dir, "data")
				}
			}
		}
		if l.ConfigPath == "" {
			cfgDir, err := os.UserConfigDir()
			if err != nil {
				return Layout{}, fmt.Errorf("locating the config directory: %w", err)
			}
			l.ConfigPath = filepath.Join(cfgDir, app, app+".toml")
		}
	}
	l.ConfigDir = filepath.Dir(l.ConfigPath)
	l.LogDir = filepath.Join(l.DataDir, "logs")
	return l, nil
}

// dataHome is the OS data directory for app. There is no stdlib
// equivalent of os.UserConfigDir for it, so each platform is spelled out;
// macOS does not separate the two, so its data lives under the config dir.
func dataHome(app string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		if dir := os.Getenv("LocalAppData"); dir != "" {
			return filepath.Join(dir, app), nil
		}
	case "darwin":
	default:
		if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
			return filepath.Join(dir, app), nil
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share", app), nil
		}
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the data directory: %w", err)
	}
	return filepath.Join(dir, app, "data"), nil
}

// Program is the path that starts this install again. Inside an AppImage
// the executable lives in a mount that is gone once the process exits, so
// the file the user actually has is the AppImage itself.
func Program() string {
	if p := os.Getenv("APPIMAGE"); p != "" {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}

// InstallDir is the folder portable mode reads its config from: the one
// holding the AppImage, or the executable's own. Bundled tools stay behind
// fsx.ExeDir, which has to keep pointing inside the mount.
func InstallDir() string {
	p := Program()
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

// Sandboxed reports whether the process runs inside a Flatpak sandbox,
// which redirects the XDG directories and makes portable mode meaningless.
func Sandboxed() bool {
	if os.Getenv("FLATPAK_ID") != "" {
		return true
	}
	return fsx.IsFile("/.flatpak-info")
}
