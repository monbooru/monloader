//go:build !windows

package ptr

import "syscall"

// freeBytes returns the bytes available to an unprivileged user on the volume
// holding path. Unix builds use Statfs; Windows has its own implementation in
// freebytes_windows.go because syscall.Statfs does not exist there.
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
