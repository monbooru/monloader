package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// A built-in without a Path is the app's own sheet.
type Theme struct {
	Name    string
	Path    string
	Logo    string
	Favicon string
	Builtin bool
}

type Note struct {
	Warn bool
	Text string
}

// An installed theme replaces a built-in of its name.
func ListThemes(builtins []Theme, dir string) ([]Theme, []Note) {
	byName := map[string]Theme{}
	for _, b := range builtins {
		byName[b.Name] = b
	}
	if dir == "" {
		return orderThemes(builtins, byName), nil
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return orderThemes(builtins, byName), nil
	}
	var notes []Note
	for _, it := range items {
		folder := it.IsDir()
		if !folder && it.Type()&os.ModeSymlink != 0 {
			fi, err := os.Stat(filepath.Join(dir, it.Name()))
			folder = err == nil && fi.IsDir()
		}
		if folder {
			sheet := filepath.Join(dir, it.Name(), "theme.css")
			if _, err := os.Stat(sheet); err != nil {
				continue
			}
			t := Theme{Name: it.Name(), Path: sheet}
			optional := func(file string) string {
				p := filepath.Join(dir, it.Name(), file)
				if _, err := os.Stat(p); err != nil {
					return ""
				}
				return p
			}
			t.Logo, t.Favicon = optional("logo.png"), optional("favicon.png")
			byName[t.Name] = t
			continue
		}
		if !strings.HasSuffix(it.Name(), ".css") {
			continue
		}
		name := strings.TrimSuffix(it.Name(), ".css")
		if prev, taken := byName[name]; taken && !prev.Builtin {
			notes = append(notes, Note{Warn: true, Text: fmt.Sprintf("themes: %q exists as both a folder and a .css file; the folder wins", name)})
			continue
		}
		byName[name] = Theme{Name: name, Path: filepath.Join(dir, it.Name())}
	}
	return orderThemes(builtins, byName), notes
}

func orderThemes(builtins []Theme, byName map[string]Theme) []Theme {
	out := make([]Theme, 0, len(byName))
	for _, b := range builtins {
		if t, ok := byName[b.Name]; ok {
			out = append(out, t)
			delete(byName, b.Name)
		}
	}
	rest := slices.SortedFunc(maps.Values(byName), func(a, b Theme) int {
		return strings.Compare(a.Name, b.Name)
	})
	return append(out, rest...)
}

// FindTheme never uses name as a path.
func FindTheme(themes []Theme, name string) (t Theme, found bool) {
	if len(themes) == 0 {
		return Theme{}, name == ""
	}
	if name == "" {
		return themes[0], true
	}
	for _, t := range themes {
		if t.Name == name {
			return t, true
		}
	}
	return themes[0], false
}

// SeedFile records the bytes each file was seeded with: a copy still holding
// them is untouched, so a release may refresh it.
const SeedFile = ".seed"

// A copy the operator deleted is not rebuilt: dir/name is created only while
// dir itself does not exist yet.
func SeedTheme(src fs.FS, srcDir string, files []string, dir, name string) []Note {
	if dir == "" {
		return nil
	}
	target := filepath.Join(dir, name)
	if _, err := os.Stat(target); err != nil {
		if _, err := os.Stat(dir); err == nil {
			return nil
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return []Note{{Warn: true, Text: fmt.Sprintf("themes: could not create %s: %v", dir, err)}}
		}
	}
	var notes []Note
	seeded := readSeed(target)
	changed := false
	for _, file := range files {
		body, err := fs.ReadFile(src, srcDir+"/"+file)
		if err != nil {
			continue
		}
		want := sha256Hex(body)
		path := filepath.Join(target, file)
		if cur, err := os.ReadFile(path); err == nil {
			switch have := sha256Hex(cur); {
			case have == want:
				if seeded[file] != want {
					seeded[file], changed = want, true
				}
				continue
			case have != seeded[file] && (seeded[file] == "" || seeded[file] == want):
				notes = append(notes, Note{Text: fmt.Sprintf("themes: %s was edited and is kept", path)})
				continue
			case have != seeded[file]:
				notes = append(notes, Note{Text: fmt.Sprintf("themes: %s is not the seeded copy; the bundled one moved and is left out", path)})
				continue
			}
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			notes = append(notes, Note{Warn: true, Text: fmt.Sprintf("themes: could not write %s: %v", path, err)})
			continue
		}
		seeded[file], changed = want, true
	}
	if changed {
		var b strings.Builder
		for _, file := range slices.Sorted(maps.Keys(seeded)) {
			fmt.Fprintf(&b, "%s  %s\n", seeded[file], file)
		}
		if err := os.WriteFile(filepath.Join(target, SeedFile), []byte(b.String()), 0o644); err != nil {
			notes = append(notes, Note{Warn: true, Text: fmt.Sprintf("themes: could not record the seeded theme: %v", err)})
		}
	}
	return notes
}

func readSeed(dir string) map[string]string {
	seeded := map[string]string{}
	data, err := os.ReadFile(filepath.Join(dir, SeedFile))
	if err != nil {
		return seeded
	}
	for line := range strings.Lines(string(data)) {
		if sum, name, ok := strings.Cut(strings.TrimSpace(line), "  "); ok {
			seeded[name] = sum
		}
	}
	return seeded
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ServeThemeFile(w http.ResponseWriter, r *http.Request, t Theme, path string, builtin fs.FS) {
	if path == "" {
		http.NotFound(w, r)
		return
	}
	if t.Builtin {
		http.ServeFileFS(w, r, builtin, path)
		return
	}
	http.ServeFile(w, r, path)
}
