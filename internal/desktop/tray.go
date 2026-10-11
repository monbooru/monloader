package desktop

import "errors"

var ErrTrayUnavailable = errors.New("no system tray in this build")

// TrayMenu stays open, start-at-login and quit: the tray is optional, so
// nothing may be reachable only through it.
type TrayMenu struct {
	Title     string
	IconPath  string
	Open      func()
	Quit      func()
	Autostart Hook
}

func (m TrayMenu) autostartItem() (show, on bool) {
	if m.Autostart.App == "" || !m.Autostart.AutostartSupported() {
		return false, false
	}
	return true, m.Autostart.AutostartEnabled()
}

func (m TrayMenu) toggleAutostart() bool {
	on := m.Autostart.AutostartEnabled()
	if err := m.Autostart.SetAutostart(!on); err != nil {
		return on
	}
	return !on
}
