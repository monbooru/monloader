//go:build windows

package ptr

import "golang.org/x/sys/windows"

// freeBytes reports FreeBytesAvailableToCaller, which honors per-user
// quotas.
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
