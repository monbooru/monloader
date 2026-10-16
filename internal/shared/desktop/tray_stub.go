//go:build !windows && !(linux && tray)

package desktop

import "context"

func RunTray(context.Context, TrayMenu) error { return ErrTrayUnavailable }

func TrayAvailable() bool { return false }
