package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/monbooru"
)

const (
	// monbooru's own page: a level shows this many rows, then [more].
	folderLevelRows   = 500
	folderSuggestRows = 10
	// Opening a branch twice, or the field and its panel, asks for the same
	// page; a short reuse spares monbooru without hiding a new folder long.
	folderPageTTL = 30 * time.Second
)

type folderPageCache struct {
	mu    sync.Mutex
	pages map[string]cachedFolderPage
}

type cachedFolderPage struct {
	page monbooru.FolderPage
	err  error
	at   time.Time
}

// An empty gallery is monbooru's active one, an empty path the top level.
// The same short budget as the gallery list: the page must render when
// monbooru is slow or down.
func (s *Server) folderPage(r *http.Request, gallery, path string, page int) (monbooru.FolderPage, error) {
	key := gallery + "\x00" + path + "\x00" + strconv.Itoa(page)
	c := &s.folderPages
	c.mu.Lock()
	hit, ok := c.pages[key]
	c.mu.Unlock()
	if ok && time.Since(hit.at) < folderPageTTL {
		return hit.page, hit.err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	fp, err := s.client.ListFolders(ctx, gallery, path, page, folderLevelRows)
	// A folder monbooru does not hold is an answer too; a failure is not.
	if err == nil || errors.Is(err, monbooru.ErrFolderNotFound) {
		c.mu.Lock()
		if c.pages == nil {
			c.pages = map[string]cachedFolderPage{}
		}
		c.pages[key] = cachedFolderPage{page: fp, err: err, at: time.Now()}
		c.mu.Unlock()
	}
	return fp, err
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
	held    func(path string) bool
}

// WithValue lets the templates set the value they already resolve, a draft
// or the stored setting.
func (p folderPicker) WithValue(v string) folderPicker {
	p.Value = v
	p.Missing = p.Listed && v != "" && !p.held(v)
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
	_, err := s.folderPage(r, gallery, "", 1)
	p.Listed = err == nil
	// Only a clear "not there" marks a folder missing; a failed ask does not.
	p.held = func(path string) bool {
		_, err := s.folderPage(r, gallery, path, 1)
		return !errors.Is(err, monbooru.ErrFolderNotFound)
	}
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
	Kind, ID, Gallery, Path string
	Rows                    []folderRow
	More, Left              int
}

func folderRowOf(f monbooru.Folder, value string) folderRow {
	return folderRow{Path: f.Path, Name: f.Name, Images: f.Images, Count: abbrevCount(f.Images), HasSub: f.Folders > 0, Active: f.Path == value}
}

// One page of a level. The branch leading to value opens with it, and its
// row joins the first page when monbooru pages it later, so later pages
// leave it out.
func (s *Server) folderLevelOf(r *http.Request, p folderPicker, path string, page int, value string) (*folderLevel, error) {
	fp, err := s.folderPage(r, p.Gallery, path, page)
	if err != nil {
		return nil, err
	}
	lv := &folderLevel{Kind: p.Kind, ID: p.ID, Gallery: p.Gallery, Path: path}
	// The child of path on the way to value, if value is under path.
	next := ""
	if rest, ok := strings.CutPrefix(value, path+"/"); path == "" || ok {
		if path == "" {
			rest = value
		}
		if seg, _, _ := strings.Cut(rest, "/"); seg != "" {
			next = strings.TrimPrefix(path+"/"+seg, "/")
		}
	}
	seen := false
	for _, f := range fp.Results {
		if f.Path == next {
			if page > 1 {
				continue
			}
			seen = true
		}
		lv.Rows = append(lv.Rows, folderRowOf(f, value))
	}
	left := fp.Total - (page-1)*folderLevelRows - len(fp.Results)
	if next != "" && page == 1 && !seen {
		if f, ok := s.findFolder(r, p.Gallery, next); ok {
			lv.Rows = append(lv.Rows, folderRowOf(f, value))
			left--
		}
	}
	if left > 0 {
		lv.More, lv.Left = page+1, left
	}
	for i := range lv.Rows {
		row := &lv.Rows[i]
		if row.HasSub && row.Path == next && value != next {
			row.Sub, _ = s.folderLevelOf(r, p, row.Path, 1, value)
		}
	}
	return lv, nil
}

// A folder's own row when its page is not at hand: the search lists it
// among those starting with its path.
func (s *Server) findFolder(r *http.Request, gallery, path string) (monbooru.Folder, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	found, err := s.client.SearchFolders(ctx, gallery, path, folderSuggestRows)
	if err != nil {
		return monbooru.Folder{}, false
	}
	for _, f := range found {
		if f.Path == path {
			return f, true
		}
	}
	return monbooru.Folder{}, false
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

// A page of the panel; a path monbooru no longer holds answers no rows.
func (s *Server) folderPickerLevel(w http.ResponseWriter, r *http.Request) {
	p, ok := folderPickerKind(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	p.Gallery = q.Get("gallery")
	page, _ := strconv.Atoi(q.Get("page"))
	lv, err := s.folderLevelOf(r, p, strings.Trim(q.Get("path"), "/"), max(page, 1), strings.TrimSpace(r.FormValue(p.Name)))
	if err != nil {
		return
	}
	s.render(w, "folder_picker_rows", lv)
}

func (s *Server) folderPickerSuggest(w http.ResponseWriter, r *http.Request) {
	p, ok := folderPickerKind(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.FormValue(p.Name))
	if q == "" {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	found, err := s.client.SearchFolders(ctx, r.URL.Query().Get("gallery"), q, folderSuggestRows)
	if err != nil {
		return
	}
	rows := make([]folderRow, len(found))
	for i, f := range found {
		rows[i] = folderRowOf(f, "")
	}
	s.render(w, "folder_picker_suggest", rows)
}
