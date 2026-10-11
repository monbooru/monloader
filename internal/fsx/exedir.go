package fsx

import (
	"os"
	"path/filepath"
)

// ExeDir resolves symlinks, so a link in ~/.local/bin leads to the
// unpacked folder.
func ExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

func IsFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
