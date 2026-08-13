//go:build windows

package ptr

import "golang.org/x/sys/windows"

// freeBytes returns the bytes available to an unprivileged user on the volume
// holding path. syscall.Statfs is unix-only, so Windows uses
// GetDiskFreeSpaceEx's FreeBytesAvailableToCaller, which already accounts for
// per-user quotas. The path may be relative or "." (the current volume).
func freeBytes(path string) (uint64, error) {
	dir, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}
