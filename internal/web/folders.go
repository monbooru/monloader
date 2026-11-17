package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/monbooru"
)

const (
	// As monbooru's sidebar: a level shows this many rows, then [more].
	folderLevelRows   = 500
	folderSuggestRows = 10
	// Opening a branch asks for the same tree again; a short reuse spares
	// monbooru the walk without hiding a folder made a minute ago.
	folderTreeTTL = 30 * time.Second
)

type folderTreeCache struct {
	mu    sync.Mutex
	trees map[string]cachedFolderTree
}

type cachedFolderTree struct {
	root monbooru.Folder
	at   time.Time
}

// An empty gallery is monbooru's active one. The same short budget as the
// gallery list: the page must render when monbooru is slow or down.
func (s *Server) folderTree(r *http.Request, gallery string) (monbooru.Folder, bool) {
	c := &s.folderTrees
	c.mu.Lock()
	hit, ok := c.trees[gallery]
	c.mu.Unlock()
	if ok && time.Since(hit.at) < folderTreeTTL {
		return hit.root, true
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	root, err := s.client.ListFolders(ctx, gallery)
	if err != nil {
		return monbooru.Folder{}, false
	}
	c.mu.Lock()
	if c.trees == nil {
		c.trees = map[string]cachedFolderTree{}
	}
	c.trees[gallery] = cachedFolderTree{root: root, at: time.Now()}
	c.mu.Unlock()
	return root, true
}

func findFolder(root *monbooru.Folder, path string) *monbooru.Folder {
	if path == "" {
		return root
	}
	for i := range root.Children {
		c := &root.Children[i]
		if c.Path == path {
			return c
		}
		if strings.HasPrefix(path, c.Path+"/") {
			return findFolder(c, path)
		}
	}
	return nil
}

// A folder field, on the site dialog and for the default folder. Listed is
// false when monbooru could not say, and the field is a plain text box.
type folderPicker struct {
	Kind, ID, Name, Label, Blank, Hint, GalleryID string
	// The gallery the folders are listed from, for the panel's requests.
	Gallery string
	Listed  bool
	Value   string
	Missing bool
	root    monbooru.Folder
}

// WithValue lets the templates set the value they already resolve, a draft
// or the stored setting.
func (p folderPicker) WithValue(v string) folderPicker {
	p.Value = v
	p.Missing = p.Listed && v != "" && findFolder(&p.root, v) == nil
	return p
}

var folderPickerKinds = map[string]folderPicker{
	"site": {
		Kind: "site", ID: "sd-folder", Name: "folder", Label: "Folder", Blank: "(default folder)", GalleryID: "sd-gallery",
		Hint: "Subfolder in the gallery. A folder picked when sending wins over this one.",
	},
	"default": {
		Kind: "default", ID: "dl-folder", Name: "default_folder", Label: "Default folder", Blank: "(monbooru's default)", GalleryID: "mb-default-gallery",
		Hint: "Subfolder in monbooru. Blank = monbooru's \"Where received files go\"",
	},
}

func (s *Server) folderPicker(r *http.Request, kind, gallery string) folderPicker {
	p := folderPickerKinds[kind]
	p.Gallery = gallery
	p.root, p.Listed = s.folderTree(r, gallery)
	return p
}

type folderRow struct {
	Path, Name, Count string
	Images            int
	HasSub, Active    bool
	// Rendered open: the branch holding the field's folder.
	Sub *folderLevel
}

type folderLevel struct {
	Kind, Gallery, Path string
	Rows                []folderRow
	More, Left          int
}

// One level of the panel. The branch leading to value opens with it, and
// its row stays on the first page even past the cap.
func folderLevelOf(kind, gallery string, node *monbooru.Folder, offset int, value string) *folderLevel {
	lv := &folderLevel{Kind: kind, Gallery: gallery, Path: node.Path}
	onPath := func(path string) bool { return value == path || strings.HasPrefix(value, path+"/") }
	kids := node.Children
	pin := -1
	if value != "" {
		pin = slices.IndexFunc(kids, func(c monbooru.Folder) bool { return onPath(c.Path) })
	}
	end := min(offset+folderLevelRows, len(kids))
	var shown []int
	for i := offset; i < end; i++ {
		if offset == 0 || i != pin {
			shown = append(shown, i)
		}
	}
	left := len(kids) - max(end, offset)
	if pin >= end {
		left--
		if offset == 0 {
			shown = append(shown, pin)
		}
	}
	if left > 0 {
		lv.More, lv.Left = end, left
	}
	for _, i := range shown {
		c := &kids[i]
		row := folderRow{Path: c.Path, Name: c.Name, Images: c.Images, Count: abbrevCount(c.Images), HasSub: len(c.Children) > 0, Active: c.Path == value}
		if row.HasSub && value != "" && strings.HasPrefix(value, c.Path+"/") {
			row.Sub = folderLevelOf(kind, gallery, c, 0, value)
		}
		lv.Rows = append(lv.Rows, row)
	}
	return lv
}

// The folders whose path holds q, those starting with it first.
func folderSuggestions(root *monbooru.Folder, q string) []folderRow {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	var starts, holds []folderRow
	var walk func(f *monbooru.Folder)
	walk = func(f *monbooru.Folder) {
		for i := range f.Children {
			c := &f.Children[i]
			path := strings.ToLower(c.Path)
			row := folderRow{Path: c.Path, Images: c.Images, Count: abbrevCount(c.Images)}
			switch {
			case strings.HasPrefix(path, q):
				starts = append(starts, row)
			case strings.Contains(path, q):
				holds = append(holds, row)
			}
			if len(starts) >= folderSuggestRows {
				return
			}
			walk(c)
		}
	}
	walk(root)
	out := append(starts, holds...)
	return out[:min(len(out), folderSuggestRows)]
}

// As monbooru's sidebar counts: 4.2k, 12k, 1.3M.
func abbrevCount(n int) string {
	unit := func(div int, suffix string) string {
		if tenths := (n*10 + div/2) / div; tenths < 100 {
			return fmt.Sprintf("%d.%d%s", tenths/10, tenths%10, suffix)
		}
		return fmt.Sprintf("%d%s", (n+div/2)/div, suffix)
	}
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 999500:
		return unit(1000, "k")
	default:
		return unit(1000000, "M")
	}
}

func folderPickerKind(w http.ResponseWriter, r *http.Request) (folderPicker, bool) {
	p, ok := folderPickerKinds[r.URL.Query().Get("kind")]
	if !ok {
		http.NotFound(w, r)
	}
	return p, ok
}

// Redraws a folder field for the gallery picked beside it, keeping the
// folder it had.
func (s *Server) folderPickerFragment(w http.ResponseWriter, r *http.Request) {
	p, ok := folderPickerKind(w, r)
	if !ok {
		return
	}
	var gallery string
	switch p.Kind {
	case "site":
		// A site with no gallery of its own sends to the default one.
		gallery = strings.TrimSpace(r.FormValue("gallery"))
		if gallery == "" {
			gallery = s.cfg.Current().Monbooru.DefaultGallery
		}
	case "default":
		gallery = strings.TrimSpace(r.FormValue("default_gallery"))
	}
	s.render(w, "folder_picker", s.folderPicker(r, p.Kind, gallery).WithValue(strings.TrimSpace(r.FormValue(p.Name))))
}

// A level of the panel; a path monbooru no longer lists answers no rows.
func (s *Server) folderPickerLevel(w http.ResponseWriter, r *http.Request) {
	p, ok := folderPickerKind(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	gallery := q.Get("gallery")
	root, ok := s.folderTree(r, gallery)
	node := findFolder(&root, q.Get("path"))
	if !ok || node == nil {
		return
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	s.render(w, "folder_picker_rows", folderLevelOf(p.Kind, gallery, node, max(offset, 0), strings.TrimSpace(r.FormValue(p.Name))))
}

func (s *Server) folderPickerSuggest(w http.ResponseWriter, r *http.Request) {
	p, ok := folderPickerKind(w, r)
	if !ok {
		return
	}
	root, ok := s.folderTree(r, r.URL.Query().Get("gallery"))
	if !ok {
		return
	}
	s.render(w, "folder_picker_suggest", folderSuggestions(&root, r.FormValue(p.Name)))
}
