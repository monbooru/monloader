package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

var testBuiltins = []Theme{{Name: "dark", Builtin: true}, {Name: "light", Builtin: true, Path: "light/theme.css"}}

func names(ts []Theme) string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return strings.Join(out, ",")
}

func TestListThemesTakesBothShapesAfterTheBuiltins(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"aurora/theme.css":  ":root{--bg:#fff}",
		"aurora/logo.png":   "png",
		"midnight.css":      ":root{--bg:#000}",
		"notatheme/read.me": "no stylesheet here",
	})
	got, notes := ListThemes(testBuiltins, dir)
	if names(got) != "dark,light,aurora,midnight" || len(notes) != 0 {
		t.Fatalf("themes = %s, notes %v", names(got), notes)
	}
	if got[2].Logo == "" || got[2].Favicon != "" {
		t.Errorf("folder form = %+v, want aurora carrying its logo only", got[2])
	}
	if got[3].Logo != "" || got[3].Builtin {
		t.Errorf("bare css form = %+v", got[3])
	}
}

func TestListThemesFollowsASymlinkedFolder(t *testing.T) {
	dir, target := t.TempDir(), t.TempDir()
	writeFiles(t, target, map[string]string{"theme.css": ":root{--bg:#123}"})
	if err := os.Symlink(target, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	got, _ := ListThemes(nil, dir)
	if len(got) != 1 || got[0].Path != filepath.Join(dir, "linked", "theme.css") {
		t.Errorf("themes = %+v, want the linked folder", got)
	}
}

func TestListThemesLetsAFolderWinAndReplaceABuiltin(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"light/theme.css": ":root{--bg:#fff}",
		"light.css":       ":root{--bg:#eee}",
		"blue/theme.css":  ":root{--bg:#00f}",
		"blue.css":        ":root{--bg:#00e}",
	})
	got, notes := ListThemes(testBuiltins, dir)
	if names(got) != "dark,light,blue" || got[1].Builtin || !strings.HasSuffix(got[2].Path, filepath.Join("blue", "theme.css")) {
		t.Errorf("themes = %+v", got)
	}
	if len(notes) != 2 || !notes[0].Warn || !strings.Contains(notes[0].Text, `"blue"`) || !strings.Contains(notes[1].Text, `"light"`) {
		t.Errorf("notes = %+v, want a warning for each collision", notes)
	}
}

func TestListThemesReadsAFoldersDrawings(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"aurora/theme.css":          ":root{--bg:#fff}",
		"aurora/mascot-lost.png":    "png",
		"aurora/mascot-strange.png": "png",
		"plain/theme.css":           ":root{--bg:#000}",
	})
	got, _ := ListThemes(testBuiltins, dir)
	if len(got[2].Mascots) != 1 || !strings.HasSuffix(got[2].Mascots["lost"], filepath.Join("aurora", "mascot-lost.png")) {
		t.Errorf("aurora's drawings = %v, want its one known face", got[2].Mascots)
	}
	if got[3].Mascots != nil {
		t.Errorf("a folder without drawings lists %v", got[3].Mascots)
	}
}

func TestFindThemeOnlyMatchesAListedName(t *testing.T) {
	themes := []Theme{{Name: "dark", Builtin: true}, {Name: "light"}}
	if got, ok := FindTheme(themes, ""); got.Name != "dark" || !ok {
		t.Errorf("empty name = %+v %v", got, ok)
	}
	if got, ok := FindTheme(themes, "light"); got.Name != "light" || !ok {
		t.Errorf("light = %+v %v", got, ok)
	}
	for _, name := range []string{"../../etc/passwd", "light/theme.css", "gone"} {
		if got, ok := FindTheme(themes, name); got.Name != "dark" || ok {
			t.Errorf("%q = %+v %v, want the first theme and not found", name, got, ok)
		}
	}
}

var bundle = fstest.MapFS{
	"src/theme.css": {Data: []byte(":root{--bg:#fff}")},
	"src/logo.png":  {Data: []byte("png")},
}

func TestSeedThemeWritesOnceAndLeavesADeletedCopyAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "themes")
	if notes := SeedTheme(bundle, "src", []string{"theme.css", "logo.png", "favicon.png"}, dir, "light"); len(notes) != 0 {
		t.Errorf("notes = %+v", notes)
	}
	sheet := filepath.Join(dir, "light", "theme.css")
	if body, err := os.ReadFile(sheet); err != nil || string(body) != ":root{--bg:#fff}" {
		t.Fatalf("seeded sheet = %q, %v", body, err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "light")); err != nil {
		t.Fatal(err)
	}
	SeedTheme(bundle, "src", []string{"theme.css"}, dir, "light")
	if _, err := os.Stat(sheet); err == nil {
		t.Error("seeding rebuilt a copy the operator deleted")
	}
}

func TestSeedThemeNamesTheFolderItCouldNotCreate(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	writeFiles(t, filepath.Dir(blocker), map[string]string{"file": "x"})
	dir := filepath.Join(blocker, "themes")
	notes := SeedTheme(bundle, "src", []string{"theme.css"}, dir, "light")
	if len(notes) != 1 || !notes[0].Warn || !strings.Contains(notes[0].Text, filepath.Join(dir, "light")) {
		t.Errorf("notes = %+v, want a warning naming %s", notes, filepath.Join(dir, "light"))
	}
}

func TestSeedThemeRefreshesOnlyAnUntouchedCopy(t *testing.T) {
	dir := t.TempDir()
	light := filepath.Join(dir, "light")
	sheet := filepath.Join(light, "theme.css")
	old := ":root{--bg:#fafafa}"
	writeFiles(t, light, map[string]string{"theme.css": old, SeedFile: sha256Hex([]byte(old)) + "  theme.css\n"})
	SeedTheme(bundle, "src", []string{"theme.css"}, dir, "light")
	if body, _ := os.ReadFile(sheet); string(body) != ":root{--bg:#fff}" {
		t.Errorf("an untouched copy was not refreshed: %q", body)
	}

	writeFiles(t, light, map[string]string{"theme.css": ":root{--bg:#eee}"})
	notes := SeedTheme(bundle, "src", []string{"theme.css"}, dir, "light")
	if body, _ := os.ReadFile(sheet); string(body) != ":root{--bg:#eee}" {
		t.Errorf("an edited copy was rewritten: %q", body)
	}
	if len(notes) != 1 || notes[0].Warn || !strings.Contains(notes[0].Text, "theme.css was edited and is kept") || strings.Contains(notes[0].Text, "moved") {
		t.Errorf("notes = %+v, want the edit named and no move", notes)
	}

	if err := os.Remove(filepath.Join(light, SeedFile)); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, light, map[string]string{"theme.css": old})
	SeedTheme(bundle, "src", []string{"theme.css"}, dir, "light")
	if body, _ := os.ReadFile(sheet); string(body) != old {
		t.Errorf("a copy seeded before the record existed was rewritten: %q", body)
	}
}

func TestSeedThemeDropsOnlyAnUntouchedFileTheBundleStoppedShipping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "themes")
	light := filepath.Join(dir, "light")
	SeedTheme(bundle, "src", []string{"theme.css", "logo.png"}, dir, "light")
	SeedTheme(bundle, "src", []string{"theme.css"}, dir, "light")
	if _, err := os.Stat(filepath.Join(light, "logo.png")); err == nil {
		t.Error("an untouched logo the bundle no longer ships was kept")
	}
	if seed, _ := os.ReadFile(filepath.Join(light, SeedFile)); strings.Contains(string(seed), "logo.png") {
		t.Errorf("the record still names the dropped file: %q", seed)
	}

	SeedTheme(bundle, "src", []string{"theme.css", "logo.png"}, dir, "light")
	writeFiles(t, light, map[string]string{"logo.png": "my own png"})
	notes := SeedTheme(bundle, "src", []string{"theme.css"}, dir, "light")
	if body, _ := os.ReadFile(filepath.Join(light, "logo.png")); string(body) != "my own png" {
		t.Errorf("an edited logo was removed: %q", body)
	}
	if len(notes) != 1 || !strings.Contains(notes[0].Text, "logo.png was edited and is kept") {
		t.Errorf("notes = %+v, want the kept edit named", notes)
	}
}

func TestServeThemeFileReadsTheRightSource(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"mine.css": ":root{--bg:#111}"})
	builtin := fstest.MapFS{"light/theme.css": {Data: []byte(":root{--bg:#fff}")}}
	for _, tc := range []struct {
		t    Theme
		path string
		code int
		body string
	}{
		{Theme{Name: "light", Builtin: true}, "light/theme.css", 200, "#fff"},
		{Theme{Name: "mine"}, filepath.Join(dir, "mine.css"), 200, "#111"},
		{Theme{Name: "dark", Builtin: true}, "", 404, ""},
	} {
		w := httptest.NewRecorder()
		ServeThemeFile(w, httptest.NewRequest(http.MethodGet, "/theme.css", nil), tc.t, tc.path, builtin)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.body) {
			t.Errorf("%s: %d %q", tc.t.Name, w.Code, w.Body.String())
		}
	}
}

func TestSeedThemeKeepsAFileItsRecordNamesOutsideTheFolder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "themes")
	light := filepath.Join(dir, "light")
	SeedTheme(bundle, "src", []string{"theme.css"}, dir, "light")

	outside := filepath.Join(root, "outside.txt")
	const body = "not a theme file"
	writeFiles(t, root, map[string]string{"outside.txt": body})
	record := sha256Hex([]byte(body)) + "  ../../outside.txt\n"
	writeFiles(t, light, map[string]string{SeedFile: record})

	SeedTheme(bundle, "src", []string{"theme.css"}, dir, "light")
	if got, err := os.ReadFile(outside); err != nil || string(got) != body {
		t.Errorf("a file outside the folder was removed: %q, %v", got, err)
	}
}
