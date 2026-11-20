package webui

import (
	"errors"
	"fmt"
	"html/template"
	"slices"
	"strings"
	"testing"
)

// A reader over a flat folder list, in path order, pageSize rows a page.
// A path the list does not hold, other than the top, is an error.
func folderReaderOf(all []FolderEntry, pageSize int) func(string, int) (FolderPage, error) {
	return func(path string, page int) (FolderPage, error) {
		if path != "" && !slices.ContainsFunc(all, func(f FolderEntry) bool { return f.Path == path }) {
			return FolderPage{}, errors.New("folder not found: " + path)
		}
		var kids []FolderEntry
		for _, f := range all {
			if parent := f.Path[:max(strings.LastIndex(f.Path, "/"), 0)]; parent == path {
				kids = append(kids, f)
			}
		}
		from := min((page-1)*pageSize, len(kids))
		return FolderPage{Total: len(kids), Limit: pageSize, Results: kids[from:min(from+pageSize, len(kids))]}, nil
	}
}

func folderEntry(path string, folders int) FolderEntry {
	name := path[strings.LastIndex(path, "/")+1:]
	return FolderEntry{Path: path, Name: name, Images: 1, Folders: folders}
}

// n top-level folders f0000..., the last with a subfolder "sub".
func longFolders(n int) []FolderEntry {
	var out []FolderEntry
	for i := range n {
		out = append(out, folderEntry(fmt.Sprintf("f%04d", i), 0))
	}
	out[n-1].Folders = 1
	return append(out, folderEntry(out[n-1].Path+"/sub", 0))
}

func TestFolderLevelOf(t *testing.T) {
	second := []FolderEntry{folderEntry("second", 1), folderEntry("second/sub", 0)}
	// f000 to f501 run past a 500 row page; f501 has a subfolder.
	var wide []FolderEntry
	for i := range 502 {
		wide = append(wide, folderEntry(fmt.Sprintf("f%03d", i), 0))
	}
	wide[501].Folders = 1
	wide = append(wide, folderEntry("f501/deep", 0))
	// "art" sits on the top level's second page, behind "ART", its twelve
	// subfolders and 500 more, which a search for "art" would list first.
	crowd := []FolderEntry{folderEntry("ART", 12)}
	for i := range 12 {
		crowd = append(crowd, folderEntry(fmt.Sprintf("ART/x%02d", i), 0))
	}
	for i := range 500 {
		crowd = append(crowd, folderEntry(fmt.Sprintf("a%03d", i), 0))
	}
	crowd = append(crowd, folderEntry("art", 1), folderEntry("art/deep", 0))

	cases := []struct {
		name        string
		folders     []FolderEntry
		path        string
		page        int
		value       string
		has, hasNot []string
		open        string // the row rendered with its branch open
		active      string // the row marked as the field's folder
		more, left  int
	}{
		{"one level, closed", second, "", 1, "", []string{"second"}, []string{"second/sub"}, "", "", 0, 0},
		{"a branch on demand", second, "second", 1, "", []string{"second/sub"}, nil, "", "", 0, 0},
		{"the branch holding the folder starts open", second, "", 1, "second/sub", []string{"second"}, nil, "second", "second/sub", 0, 0},
		{"a page holds 500, then more", wide, "", 1, "", []string{"f000", "f499"}, []string{"f500"}, "", "", 2, 2},
		{"the next page", wide, "", 2, "", []string{"f500", "f501"}, []string{"f499"}, "", "", 0, 0},
		{"the folder held joins the first page", wide, "", 1, "f501", []string{"f499", "f501"}, nil, "", "f501", 2, 1},
		{"and its branch opens there", wide, "", 1, "f501/deep", []string{"f501"}, nil, "f501", "f501/deep", 2, 1},
		{"a later page leaves it out", wide, "", 2, "f501/deep", []string{"f500"}, []string{"f501"}, "", "", 0, 0},
		{"found past look-alikes", crowd, "", 1, "art/deep", []string{"art"}, nil, "art", "art/deep", 2, 1},
		{"the first page counts it as shown", longFolders(1200), "", 1, "f1199/sub", []string{"f1199"}, nil, "f1199", "f1199/sub", 2, 699},
		{"a page before its own leaves it out of those left", longFolders(1200), "", 2, "f1199/sub", nil, []string{"f1199"}, "", "", 3, 199},
		{"its own page skips it, nothing left", longFolders(1200), "", 3, "f1199/sub", []string{"f1198"}, []string{"f1199"}, "", "", 0, 0},
		{"no more for a page holding only it", longFolders(1001), "", 2, "f1000/sub", []string{"f0999"}, nil, "", "", 0, 0},
	}
	p := FolderPicker{Route: "/picker", Kind: "k", ID: "f", Gallery: "g"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lv, err := p.FolderLevelOf(folderReaderOf(tc.folders, 500), tc.path, tc.page, tc.value)
			if err != nil {
				t.Fatal(err)
			}
			var paths, open, active []string
			var walk func(rows []FolderRow)
			walk = func(rows []FolderRow) {
				for _, r := range rows {
					if r.Active {
						active = append(active, r.Path)
					}
					if r.Sub != nil {
						open = append(open, r.Path)
						walk(r.Sub.Rows)
					}
				}
			}
			for _, r := range lv.Rows {
				paths = append(paths, r.Path)
			}
			walk(lv.Rows)
			for _, h := range tc.has {
				if !slices.Contains(paths, h) {
					t.Errorf("%s missing from the level", h)
				}
			}
			for _, h := range tc.hasNot {
				if slices.Contains(paths, h) {
					t.Errorf("%s should not be on this page", h)
				}
			}
			if got := strings.Join(open, ","); got != tc.open {
				t.Errorf("open branches %q, want %q", got, tc.open)
			}
			if got := strings.Join(active, ","); got != tc.active {
				t.Errorf("active rows %q, want %q", got, tc.active)
			}
			if lv.More != tc.more || lv.Left != tc.left {
				t.Errorf("more %d left %d, want %d and %d", lv.More, lv.Left, tc.more, tc.left)
			}
			if lv.Route != "/picker" || lv.Kind != "k" || lv.ID != "f" || lv.Gallery != "g" || lv.Path != tc.path {
				t.Errorf("level carries %+v, not the picker's route, kind, id, gallery and path", lv)
			}
		})
	}

	if _, err := p.FolderLevelOf(folderReaderOf(second, 500), "gone", 1, ""); err == nil {
		t.Error("a path the reader does not hold should fail")
	}
}

func TestFolderPickerWithValue(t *testing.T) {
	held := func(path string) bool { return path == "kept" }
	cases := []struct {
		name    string
		listed  bool
		held    func(string) bool
		value   string
		missing bool
	}{
		{"a folder the gallery holds", true, held, "kept", false},
		{"one it does not hold yet", true, held, "new", true},
		{"blank is never missing", true, held, "", false},
		{"not when the folders could not be listed", false, held, "new", false},
		{"not when it cannot be told", true, nil, "new", false},
	}
	for _, tc := range cases {
		p := FolderPicker{Listed: tc.listed, Held: tc.held}.WithValue(tc.value)
		if p.Value != tc.value || p.Missing != tc.missing {
			t.Errorf("%s: value %q missing %v, want %q and %v", tc.name, p.Value, p.Missing, tc.value, tc.missing)
		}
	}
}

func TestFolderPickerTemplate(t *testing.T) {
	tmpl := template.Must(template.New("").Funcs(Funcs("")).ParseFS(Templates, "folder_picker.html"))
	render := func(name string, data any) string {
		t.Helper()
		var b strings.Builder
		if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	base := FolderPicker{Route: "/r", Kind: "k", ID: "f", Name: "folder", Label: "Folder", Blank: "(blank)", Gallery: "my art", Listed: true}

	withGallery := base
	withGallery.GalleryID = "g-select"
	unlisted := base
	unlisted.Listed, unlisted.Unavailable = false, "unknown gallery"
	missing := base
	missing.NewHint, missing.Held = "Not there yet.", func(string) bool { return false }
	cases := []struct {
		name          string
		tmpl          string
		data          any
		want, wantNot []string
	}{
		{"the app's routes", "webui/folder_picker", base,
			[]string{`hx-get="/r/suggest?kind=k&amp;gallery=my&#43;art"`, `hx-get="/r/level?kind=k&amp;gallery=my&#43;art&amp;path="`, `[browse]`},
			[]string{`hx-trigger="change`}},
		// A form around the field may set a target its requests would inherit.
		{"the tree swaps into itself", "webui/folder_picker", base,
			[]string{`id="f-tree" hx-trigger="folder-open" hx-target="this"`}, nil},
		{"redrawn when its gallery changes", "webui/folder_picker", withGallery,
			[]string{`hx-get="/r?kind=k"`, `hx-trigger="change[target.id=='g-select'] from:body"`, `hx-include="#g-select, #f-input"`}, nil},
		{"a text box with the reason when not listed", "webui/folder_picker", unlisted,
			[]string{`(folders unavailable; unknown gallery)`}, []string{`[browse]`, `/r/suggest`}},
		{"a value it does not hold yet", "webui/folder_picker", missing.WithValue("new"),
			[]string{`value="new"`, `Not there yet.`}, nil},
		{"rows on the app's route, counts abbreviated", "webui/folder_picker_rows",
			&FolderLevel{Route: "/r", Kind: "k", ID: "f", Gallery: "g", Rows: []FolderRow{{Path: "art", Name: "art", Images: 4250, HasSub: true}}, More: 2, Left: 3},
			[]string{`data-folder-pick="art"`, `>4.3k<`, `hx-get="/r/level?kind=k&amp;gallery=g&amp;path=art"`, `hx-target="this"`, `page=2`, `title="3 more folders"`}, nil},
		{"suggestions", "webui/folder_picker_suggest", []FolderRow{{Path: "art/sky", Images: 12}},
			[]string{`data-folder-suggest="art/sky"`, `>art/sky/<`, `>12<`}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := render(tc.tmpl, tc.data)
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %s in\n%s", w, out)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(out, w) {
					t.Errorf("unexpected %s in\n%s", w, out)
				}
			}
		})
	}
}
