// Package desktop is the per-OS side of the -desktop profile, copied from
// monbooru with internal/fsx/exedir.go and kept in step by hand: a fix
// here belongs there too.
package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/monbooru/monloader/internal/fsx"
)

type Layout struct {
	ConfigPath string
	ConfigDir  string
	DataDir    string
	LogDir     string
	Portable   bool
}

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
		// A file test, never a write attempt, so a read-only install
		// folder is not seeded.
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

// macOS does not separate data from config, so its data lives under the
// config dir.
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

// Program is the AppImage itself when run from one: the executable's
// mount is gone once it exits.
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

// InstallDir is not for bundled tools: fsx.ExeDir has to keep pointing
// inside the AppImage mount.
func InstallDir() string {
	p := Program()
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

func Sandboxed() bool {
	if os.Getenv("FLATPAK_ID") != "" {
		return true
	}
	return fsx.IsFile("/.flatpak-info")
}
