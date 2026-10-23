//go:build !windows

package desktop

import "syscall"

// Free is Bavail, the space an unprivileged user can fill, as df reports it.
func DiskUsage(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), nil
}
