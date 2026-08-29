package desktop

import "golang.org/x/sys/windows"

// ShowError puts msg in front of a user who has no console: a shortcut
// launch of a GUI-subsystem binary has nowhere else to print.
func ShowError(title, msg string) {
	// MessageBox reaches user32 through a lazy loader that panics when the
	// DLL cannot load; a box that cannot show must not crash the exit path.
	if windows.NewLazySystemDLL("user32.dll").Load() != nil {
		return
	}
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	m, err := windows.UTF16PtrFromString(msg)
	if err != nil {
		return
	}
	_, _ = windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}
