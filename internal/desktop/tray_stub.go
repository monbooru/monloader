//go:build !windows && !(linux && tray)

package desktop

import "context"

// RunTray has nothing to serve on this build. Linux gets a tray from the
// `tray` build tag, which needs CGo and libayatana-appindicator.
func RunTray(context.Context, TrayMenu) error { return ErrTrayUnavailable }

// TrayAvailable reports whether this build has a tray at all, which is what
// decides whether Settings offers a switch for it.
func TrayAvailable() bool { return false }
