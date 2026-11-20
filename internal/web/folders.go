package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/shared/contract"
)

// A gallery's folders as one render reads them. Pages are asked of monbooru
// fresh for every render, which reads its own cached tree, and kept only
// while the render runs: locating the held branch asks some twice.
type folderReader struct {
	s       *Server
	r       *http.Request
	gallery string
	pages   map[string]folderAnswer
}

type folderAnswer struct {
	page monbooru.FolderPage
	err  error
}

// An empty gallery is monbooru's active one.
func (s *Server) folderReader(r *http.Request, gallery string) *folderReader {
	return &folderReader{s: s, r: r, gallery: gallery, pages: map[string]folderAnswer{}}
}

// An empty path is the top level. The same short budget as the gallery
// list: the page must render when monbooru is slow or down.
func (f *folderReader) page(path string, page int) (monbooru.FolderPage, error) {
	key := path + "\x00" + strconv.Itoa(page)
	if a, ok := f.pages[key]; ok {
		return a.page, a.err
	}
	ctx, cancel := context.WithTimeout(f.r.Context(), 4*time.Second)
	defer cancel()
	fp, err := f.s.client.ListFolders(ctx, f.gallery, path, page, contract.FolderPageSize)
	f.pages[key] = folderAnswer{page: fp, err: err}
	return fp, err
}

// A folder field, on the site dialog and for the default folder. Listed is
// false when monbooru could not say, and the field is a plain text box.
type folderPicker struct {
	Kind, ID, Name, Label, Blank, Hint, GalleryID string
	// The gallery the folders are listed from, for the panel's requests.
	Gallery string
	Listed  bool
	// Why not, when monbooru answered with a refusal rather than nothing:
	// an unknown gallery, or a monbooru without the folder routes.
	Unavailable string
	Value       string
	Missing     bool
	held        func(path string) bool
	// The form field holding the gallery picked beside it.
	galleryField string
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
		Hint:         "Subfolder in the gallery. A folder picked when sending wins over this one.",
		galleryField: "gallery",
	},
	"default": {
		Kind: "default", ID: "dl-folder", Name: "default_folder", Label: "Default folder", Blank: "(monbooru's default)", GalleryID: "mb-default-gallery",
		Hint:         "Subfolder in monbooru. Blank = monbooru's \"Where received files go\"",
		galleryField: "default_gallery",
	},
}

// A site with no gallery of its own sends to the default one, so its
// folders are listed from there.
func (s *Server) folderPicker(r *http.Request, kind, gallery string) folderPicker {
	p := folderPickerKinds[kind]
	if kind == "site" {
		gallery = cmp.Or(gallery, s.cfg.Current().Monbooru.DefaultGallery)
	}
	p.Gallery = gallery
	rd := s.folderReader(r, gallery)
	_, err := rd.page("", 1)
	p.Listed = err == nil
	var ce *outcome.CodedError
	if errors.As(err, &ce) && ce.Code == outcome.ErrCodeMonbooruRejected {
		p.Unavailable = ce.Msg
	}
	// Only a clear "not there" marks a folder missing; a failed ask does not.
	p.held = func(path string) bool {
		_, err := rd.page(path, 1)
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
func (f *folderReader) level(p folderPicker, path string, page int, value string) (*folderLevel, error) {
	fp, err := f.page(path, page)
	if err != nil {
		return nil, err
	}
	lv := &folderLevel{Kind: p.Kind, ID: p.ID, Gallery: f.gallery, Path: path}
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
	for _, row := range fp.Results {
		if row.Path == next {
			seen = true
			if page > 1 {
				continue
			}
		}
		lv.Rows = append(lv.Rows, folderRowOf(row, value))
	}
	left := fp.Total - (page-1)*contract.FolderPageSize - len(fp.Results)
	if next != "" && !seen {
		// Shown on the first page already, so not among those left when its
		// own page is still ahead.
		if row, at, ok := f.locate(path, next, fp.Total); ok && (page == 1 || at > page) {
			if page == 1 {
				lv.Rows = append(lv.Rows, folderRowOf(row, value))
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
			row.Sub, _ = f.level(p, row.Path, 1, value)
		}
	}
	return lv, nil
}

// The page of parent's level holding child, and child's row. monbooru
// lists a level in byte order of path, so halving the pages finds it in a
// few requests however wide the level.
func (f *folderReader) locate(parent, child string, total int) (monbooru.Folder, int, bool) {
	lo, hi := 1, (total+contract.FolderPageSize-1)/contract.FolderPageSize
	for lo <= hi {
		mid := (lo + hi) / 2
		fp, err := f.page(parent, mid)
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
			return monbooru.Folder{}, 0, false
		}
	}
	return monbooru.Folder{}, 0, false
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
	gallery := strings.TrimSpace(r.FormValue(p.galleryField))
	s.render(w, "folder_picker", s.folderPicker(r, p.Kind, gallery).WithValue(strings.TrimSpace(r.FormValue(p.Name))))
}

// A page of the panel; a path monbooru no longer holds answers no rows.
func (s *Server) folderPickerLevel(w http.ResponseWriter, r *http.Request) {
	p, ok := folderPickerKind(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	lv, err := s.folderReader(r, q.Get("gallery")).level(p, strings.Trim(q.Get("path"), "/"), max(page, 1), strings.TrimSpace(r.FormValue(p.Name)))
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
	found, err := s.client.SearchFolders(ctx, r.URL.Query().Get("gallery"), q, contract.FolderSearchLimit)
	if err != nil {
		return
	}
	rows := make([]folderRow, len(found))
	for i, f := range found {
		rows[i] = folderRowOf(f, "")
	}
	s.render(w, "folder_picker_suggest", rows)
}
