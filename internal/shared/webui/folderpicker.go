package webui

import "strings"

// FolderPicker is a folder field: a text box that a tree of a gallery's
// folders, the suggestions and [+ new folder] all write into. Each app
// mounts its own routes under Route: Route redraws the field,
// Route+"/level" answers a page of a level and Route+"/suggest" the
// suggestions, each with ?kind=Kind.
type FolderPicker struct {
	Route, Kind, ID, Name, Label, Blank, Hint string
	// The select picking the gallery beside the field, whose change redraws
	// it; empty when the field has none.
	GalleryID string
	// The gallery the folders are listed from, for the panel's requests.
	Gallery string
	// False when the folders could not be listed: the field is a plain text
	// box, and Unavailable says why when there is a reason worth showing.
	Listed      bool
	Unavailable string
	// Shown under a value the gallery does not hold yet.
	NewHint string
	Value   string
	Missing bool
	// Whether the gallery holds a folder; nil when it cannot be told.
	Held func(path string) bool
}

// WithValue lets the templates set the value they already resolve, a draft
// or the stored setting.
func (p FolderPicker) WithValue(v string) FolderPicker {
	p.Value = v
	p.Missing = p.Listed && v != "" && p.Held != nil && !p.Held(v)
	return p
}

// FolderEntry is one folder of a level, with the fields of the API's
// folder listing, so an app's own type converts to it.
type FolderEntry struct {
	Path    string
	Name    string
	Images  int
	Folders int
}

// FolderPage is one page of a level as the app reads it, Limit rows a page.
type FolderPage struct {
	Total, Limit int
	Results      []FolderEntry
}

type FolderRow struct {
	Path, Name     string
	Images         int
	HasSub, Active bool
	// Rendered open: the branch holding the field's folder.
	Sub *FolderLevel
}

type FolderLevel struct {
	Route, Kind, ID, Gallery, Path string
	Rows                           []FolderRow
	More, Left                     int
}

func FolderRowOf(f FolderEntry, value string) FolderRow {
	return FolderRow{Path: f.Path, Name: f.Name, Images: f.Images, HasSub: f.Folders > 0, Active: f.Path == value}
}

// FolderLevelOf builds one page of the level under path, reading pages
// through read. The branch leading to value opens with it, and its row
// joins the first page when it sits on a later one, so later pages leave
// it out.
func (p FolderPicker) FolderLevelOf(read func(path string, page int) (FolderPage, error), path string, page int, value string) (*FolderLevel, error) {
	fp, err := read(path, page)
	if err != nil {
		return nil, err
	}
	lv := &FolderLevel{Route: p.Route, Kind: p.Kind, ID: p.ID, Gallery: p.Gallery, Path: path}
	next := nextFolder(path, value)
	seen := false
	for _, row := range fp.Results {
		if row.Path == next {
			seen = true
			if page > 1 {
				continue
			}
		}
		lv.Rows = append(lv.Rows, FolderRowOf(row, value))
	}
	left := fp.Total - (page-1)*fp.Limit - len(fp.Results)
	if next != "" && !seen {
		// Shown on the first page already, so not among those left when its
		// own page is still ahead.
		if row, at, ok := locateFolder(read, path, next, fp); ok && (page == 1 || at > page) {
			if page == 1 {
				lv.Rows = append(lv.Rows, FolderRowOf(row, value))
			}
			left--
		}
	}
	if left > 0 {
		lv.More, lv.Left = page+1, left
	}
	for i := range lv.Rows {
		row := &lv.Rows[i]
		if row.HasSub && row.Path == next && value != next {
			row.Sub, _ = p.FolderLevelOf(read, row.Path, 1, value)
		}
	}
	return lv, nil
}

// The child of path on the way to value, or "" when value is not under
// path.
func nextFolder(path, value string) string {
	rest, ok := strings.CutPrefix(value, path+"/")
	if path == "" {
		rest, ok = value, true
	}
	if !ok {
		return ""
	}
	seg, _, _ := strings.Cut(rest, "/")
	if seg == "" {
		return ""
	}
	return strings.TrimPrefix(path+"/"+seg, "/")
}

// The page of parent's level holding child, and child's row. A level is
// listed in byte order of path, so halving the pages finds it in a few
// reads however wide the level.
func locateFolder(read func(path string, page int) (FolderPage, error), parent, child string, at FolderPage) (FolderEntry, int, bool) {
	lo, hi := 1, (at.Total+at.Limit-1)/max(at.Limit, 1)
	for lo <= hi {
		mid := (lo + hi) / 2
		fp, err := read(parent, mid)
		if err != nil || len(fp.Results) == 0 {
			break
		}
		switch first, last := fp.Results[0].Path, fp.Results[len(fp.Results)-1].Path; {
		case child < first:
			hi = mid - 1
		case child > last:
			lo = mid + 1
		default:
			for _, row := range fp.Results {
				if row.Path == child {
					return row, mid, true
				}
			}
			return FolderEntry{}, 0, false
		}
	}
	return FolderEntry{}, 0, false
}
