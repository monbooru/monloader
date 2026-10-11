package desktop

import "golang.org/x/sys/windows/registry"

// A Run value, not a Startup-folder .lnk, which would need COM and IShellLink.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

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

// CreateProcess parses this, where a backslash is special only before a
// quote, so LaunchCommand's .desktop escaping would be wrong here.
func runValue() string {
	exe, args := launchTarget()
	if exe == "" {
		return ""
	}
	out := `"` + exe + `"`
	for _, a := range args {
		out += " " + a
	}
	return out
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
