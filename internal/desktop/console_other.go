//go:build !windows

package desktop

// AttachConsole is a no-op off Windows, where a process keeps the terminal
// it was launched from whatever subsystem it was linked for.
func AttachConsole() {}
