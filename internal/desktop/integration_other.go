//go:build !linux && !windows

package desktop

func menuSupported() bool { return false }

func autostartSupported() bool { return false }

func menuEnabled(string) bool { return false }

func enableMenu(Hook) error { return nil }

func disableMenu(string) error { return nil }

func autostartEnabled(string) bool { return false }

func enableAutostart(Hook) error { return nil }

func disableAutostart(string) error { return nil }
