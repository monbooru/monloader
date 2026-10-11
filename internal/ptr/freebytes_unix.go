//go:build !windows

package ptr

import "syscall"

// freeBytes reports Bavail, the space an unprivileged user can fill.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
