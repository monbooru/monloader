package desktop

import "golang.org/x/sys/windows/registry"

// runKey holds the per-user programs the shell starts at login. A registry
// value rather than a Startup-folder shortcut on purpose: a .lnk means COM
// and IShellLink, where this is a string.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// The installer writes the Start Menu and Desktop shortcuts, so the app
// writing its own menu entry would only duplicate them.
func menuSupported() bool { return false }

func autostartSupported() bool { return true }

func menuEnabled(string) bool { return false }

func enableMenu(Hook) error { return nil }

func disableMenu(string) error { return nil }

func autostartEnabled(app string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer func() { _ = k.Close() }()
	v, _, err := k.GetStringValue(app)
	return err == nil && v != ""
}

func enableAutostart(h Hook) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = k.Close() }()
	return k.SetStringValue(h.App, runValue())
}

// runValue is the Run entry's command line. The shell splits it with
// CommandLineToArgvW, which knows plain double quotes and none of the
// .desktop escaping LaunchCommand applies - a `$` or a backtick in the
// install path (both legal here) would otherwise name a file that does not
// exist, and start-at-login would silently stop working.
func runValue() string {
	exe := Program()
	if exe == "" {
		return ""
	}
	return `"` + exe + `" -desktop`
}

func disableAutostart(app string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer func() { _ = k.Close() }()
	if err := k.DeleteValue(app); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}
