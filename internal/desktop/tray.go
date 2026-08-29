package desktop

import "errors"

// ErrTrayUnavailable is returned when this build has no tray for the
// platform it is running on. The tray is never the only route to anything,
// so a caller logs it and carries on.
var ErrTrayUnavailable = errors.New("no system tray in this build")

// TrayMenu is the whole tray: open the app, toggle start-at-login, quit.
// Nothing else - the tray is not a second settings page.
//
// Open and Quit are called from the tray's own thread and must not block.
// Autostart is read for the checkbox and written when it is clicked; a zero
// Hook hides the entry.
type TrayMenu struct {
	Title     string
	IconPath  string
	Open      func()
	Quit      func()
	Autostart Hook
}

// autostartItem reports whether the toggle is worth showing, and its state.
func (m TrayMenu) autostartItem() (show, on bool) {
	if m.Autostart.App == "" || !m.Autostart.AutostartSupported() {
		return false, false
	}
	return true, m.Autostart.AutostartEnabled()
}

// toggleAutostart flips the entry, returning the state it ended in.
func (m TrayMenu) toggleAutostart() bool {
	on := m.Autostart.AutostartEnabled()
	if err := m.Autostart.SetAutostart(!on); err != nil {
		return on
	}
	return !on
}
