package web

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/webui"
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
	page webui.FolderPage
	err  error
}

// An empty gallery is monbooru's active one.
func (s *Server) folderReader(r *http.Request, gallery string) *folderReader {
	return &folderReader{s: s, r: r, gallery: gallery, pages: map[string]folderAnswer{}}
}

// An empty path is the top level. The same short budget as the gallery
// list: the page must render when monbooru is slow or down.
func (f *folderReader) page(path string, page int) (webui.FolderPage, error) {
	key := path + "\x00" + strconv.Itoa(page)
	if a, ok := f.pages[key]; ok {
		return a.page, a.err
	}
	ctx, cancel := context.WithTimeout(f.r.Context(), 4*time.Second)
	defer cancel()
	fp, err := f.s.client.ListFolders(ctx, f.gallery, path, page, contract.FolderPageSize)
	out := webui.FolderPage{Total: fp.Total, Limit: contract.FolderPageSize, Results: make([]webui.FolderEntry, len(fp.Results))}
	for i, row := range fp.Results {
		out.Results[i] = webui.FolderEntry(row)
	}
	f.pages[key] = folderAnswer{page: out, err: err}
	return out, err
}

const folderPickerRoute = "/settings/folder-picker"

// The folder fields, on the site dialog and for the default folder. One
// not listed yet is a text box because monbooru could not be reached.
var folderPickerKinds = map[string]webui.FolderPicker{
	"site": {
		Route: folderPickerRoute, Kind: "site", ID: "sd-folder", Name: "folder", Label: "Folder", Blank: "(default folder)", GalleryID: "sd-gallery",
		Hint:        "Subfolder in the gallery. A folder picked when sending wins over this one.",
		NewHint:     "Not in monbooru yet; the first send there creates it.",
		Unavailable: "monbooru unreachable",
	},
	"default": {
		Route: folderPickerRoute, Kind: "default", ID: "dl-folder", Name: "default_folder", Label: "Default folder", Blank: "(monbooru's default)", GalleryID: "mb-default-gallery",
		Hint:        "Subfolder in monbooru. Blank = monbooru's \"Where received files go\"",
		NewHint:     "Not in monbooru yet; the first send there creates it.",
		Unavailable: "monbooru unreachable",
	},
}

// The form field holding the gallery picked beside each kind of field.
var folderPickerGalleryFields = map[string]string{"site": "gallery", "default": "default_gallery"}

// A site with no gallery of its own sends to the default one, so its
// folders are listed from there. The field is a plain text box when
// monbooru cannot list them, saying why when monbooru refused.
func (s *Server) folderPicker(r *http.Request, kind, gallery string) webui.FolderPicker {
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
	p.Held = func(path string) bool {
		_, err := rd.page(path, 1)
		return !errors.Is(err, monbooru.ErrFolderNotFound)
	}
	return p
}

func folderPickerKind(w http.ResponseWriter, r *http.Request) (webui.FolderPicker, bool) {
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
	gallery := strings.TrimSpace(r.FormValue(folderPickerGalleryFields[p.Kind]))
	s.render(w, "webui/folder_picker", s.folderPicker(r, p.Kind, gallery).WithValue(strings.TrimSpace(r.FormValue(p.Name))))
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
	lv, err := p.FolderLevelOf(s.folderReader(r, p.Gallery).page, strings.Trim(q.Get("path"), "/"), max(page, 1), strings.TrimSpace(r.FormValue(p.Name)))
	if err != nil {
		return
	}
	s.render(w, "webui/folder_picker_rows", lv)
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
	rows := make([]webui.FolderRow, len(found))
	for i, f := range found {
		rows[i] = webui.FolderRowOf(webui.FolderEntry(f), "")
	}
	s.render(w, "webui/folder_picker_suggest", rows)
}
