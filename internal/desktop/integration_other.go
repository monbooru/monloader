//go:build !linux && !windows

package desktop

// Neither hook is written on the platforms this build covers; both report
// unsupported so the UI hides the controls rather than offering a no-op.

func menuSupported() bool { return false }

func autostartSupported() bool { return false }

func menuEnabled(string) bool { return false }

func enableMenu(Hook) error { return nil }

func disableMenu(string) error { return nil }

func autostartEnabled(string) bool { return false }

func enableAutostart(Hook) error { return nil }

func disableAutostart(string) error { return nil }
