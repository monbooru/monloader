package desktop

import (
	"cmp"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

type Listing struct {
	Into      string
	Path      string
	Parent    string
	Entries   []Dir
	Roots     []Dir
	Sandboxed bool
	Err       string
	Truncated bool
	Limit     int
}

// The id lands in an attribute the picker's script reads, so it must stay
// an identifier.
var browseIntoRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func Browse(into, path string, limit int) (l Listing, ok bool) {
	if !browseIntoRe.MatchString(into) {
		return Listing{}, false
	}
	l = Listing{Into: into, Sandboxed: Sandboxed(), Limit: limit}
	for _, root := range Roots() {
		l.Roots = append(l.Roots, Dir{Name: root, Path: root})
	}
	home, _ := os.UserHomeDir()
	l.Path = filepath.Clean(cmp.Or(strings.TrimSpace(path), home, string(filepath.Separator)))
	if parent := filepath.Dir(l.Path); parent != l.Path {
		l.Parent = parent
	}
	var err error
	l.Entries, l.Truncated, err = ListDirs(l.Path, limit)
	if err != nil {
		l.Err = "Cannot read this folder."
	}
	return l, true
}
