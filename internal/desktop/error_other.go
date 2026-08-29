//go:build !windows

package desktop

// ShowError is a no-op off Windows: a menu-launched failure stays silent
// by design there, and the log file is the answer.
func ShowError(_, _ string) {}
