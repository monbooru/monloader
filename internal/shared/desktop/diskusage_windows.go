package desktop

import "golang.org/x/sys/windows"

func DiskUsage(path string) (total, free uint64, err error) {
	dir, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &free, &total, &totalFree); err != nil {
		return 0, 0, err
	}
	return total, free, nil
}
