//go:build !windows

package desktop

// Roots is empty off Windows: one filesystem root, and a picker that starts
// at the home directory has nothing else to offer.
func Roots() []string { return nil }
