package desktop

import "golang.org/x/sys/windows"

func ShowError(title, msg string) {
	// The lazy loader panics if user32 cannot load, and the exit path
	// must not crash.
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
