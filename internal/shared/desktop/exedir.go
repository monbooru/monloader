package desktop

import (
	"os"
	"path/filepath"
	"runtime"
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

// BundledTool is the named tool beside the executable, or "". A copy without
// exec bits, unpacked from a tarball that dropped the modes, must not shadow
// a working one on PATH.
func BundledTool(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dir := ExeDir()
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, name)
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() || (runtime.GOOS != "windows" && fi.Mode()&0o111 == 0) {
		return ""
	}
	return p
}
