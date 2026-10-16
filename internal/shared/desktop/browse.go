package desktop

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Dir struct {
	Name string
	Path string
}

// ListDirs answers path's subdirectories, dot folders left out, sorted
// case-insensitively and cut to the first limit; truncated says one was cut.
func ListDirs(path string, limit int) (dirs []Dir, truncated bool, err error) {
	entries, err := os.ReadDir(path)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(path, e.Name())
		// Only a symlink needs a stat; the dirent answers for a real directory.
		if !e.IsDir() {
			if e.Type()&fs.ModeSymlink == 0 {
				continue
			}
			if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
				continue
			}
		}
		dirs = append(dirs, Dir{Name: e.Name(), Path: full})
	}
	// Sorted before the cap, so the listing is the folder's alphabetical head.
	slices.SortFunc(dirs, func(a, b Dir) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	if len(dirs) > limit {
		dirs, truncated = dirs[:limit], true
	}
	return dirs, truncated, err
}
