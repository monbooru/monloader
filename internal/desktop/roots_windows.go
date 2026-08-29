package desktop

import "golang.org/x/sys/windows"

// Roots are the top-level places a directory picker starts from. Windows
// has no single filesystem root, so the drive letters are it.
func Roots() []string {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) != 0 {
			out = append(out, string(rune('A'+i))+`:\`)
		}
	}
	return out
}
