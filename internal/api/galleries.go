package api

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/monbooru"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/shared/contract"
)

func (h *Handler) galleryRename(w http.ResponseWriter, r *http.Request) {
	var body contract.GalleryRename
	if !contract.DecodeJSON(w, r, &body, maxRequestBody) {
		return
	}
	from, to := strings.TrimSpace(body.From), strings.TrimSpace(body.To)
	if from == "" || to == "" {
		contract.WriteError(w, http.StatusBadRequest, errInvalidRequest, "from and to are required")
		return
	}
	changed := 0
	if err := h.cfg.Update(func(c *config.Config) error {
		changed = c.RenameGallery(from, to)
		return nil
	}); err != nil {
		contract.WriteError(w, http.StatusInternalServerError, errSaveFailed, err.Error())
		return
	}
	h.queue.RenameGallery(from, to)
	contract.WriteJSON(w, http.StatusOK, map[string]any{"changed": changed})
}

func (h *Handler) listGalleries(w http.ResponseWriter, r *http.Request) {
	if !h.monbooruReady(w) {
		return
	}
	galleries, err := h.host.Monbooru.ListGalleries(r.Context())
	if err != nil {
		writeMonbooruError(w, r, err)
		return
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{"galleries": galleries})
}

func (h *Handler) listFolders(w http.ResponseWriter, r *http.Request) {
	if !h.monbooruReady(w) {
		return
	}
	q := r.URL.Query()
	page, limit := contract.PageParams(q.Get("page"), cmp.Or(q.Get("limit"), q.Get("page_size")), folderPageSize, folderPageSize)
	level, err := h.host.Monbooru.ListFolders(r.Context(), strings.TrimSpace(q.Get("gallery")), strings.TrimSpace(q.Get("path")), page, limit)
	if errors.Is(err, monbooru.ErrFolderNotFound) {
		contract.WriteError(w, http.StatusNotFound, errNotFound, "folder not found: "+q.Get("path"))
		return
	}
	if err != nil {
		writeMonbooruError(w, r, err)
		return
	}
	contract.WriteJSON(w, http.StatusOK, level)
}

func (h *Handler) searchFolders(w http.ResponseWriter, r *http.Request) {
	if !h.monbooruReady(w) {
		return
	}
	q := r.URL.Query()
	_, limit := contract.PageParams("", q.Get("limit"), folderSearchLimit, folderSearchMax)
	found, err := h.host.Monbooru.SearchFolders(r.Context(), strings.TrimSpace(q.Get("gallery")), q.Get("q"), limit)
	if err != nil {
		writeMonbooruError(w, r, err)
		return
	}
	contract.WriteJSON(w, http.StatusOK, map[string]any{"results": found})
}

// monbooru's own bounds, so a client sees the same pages either way.
const (
	folderPageSize    = 500
	folderSearchLimit = 10
	folderSearchMax   = 100
)

type siteDefault struct {
	Name    string `json:"name"`
	Gallery string `json:"gallery"`
	Folder  string `json:"folder"`
}

// Read off the settings alone, so it answers with monbooru down too. Only
// sites setting a gallery or folder are listed; the rest take the defaults.
func (h *Handler) defaults(w http.ResponseWriter, r *http.Request) {
	c := h.cfg.Current()
	sites := []siteDefault{}
	for _, s := range c.Sites {
		if s.Gallery != "" || s.Folder != "" {
			sites = append(sites, siteDefault{Name: s.Name, Gallery: s.Gallery, Folder: s.Folder})
		}
	}
	slices.SortFunc(sites, func(a, b siteDefault) int { return strings.Compare(a.Name, b.Name) })
	contract.WriteJSON(w, http.StatusOK, map[string]any{
		"gallery": c.Monbooru.DefaultGallery,
		"folder":  c.Downloader.DefaultFolder,
		"sites":   sites,
	})
}

// A 502 either way: monloader is up, the monbooru behind it is not, or it
// refused (an unknown gallery among the reasons).
func writeMonbooruError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	code, msg := outcome.ErrCodeMonbooruRejected, err.Error()
	var ce *outcome.CodedError
	if errors.As(err, &ce) {
		code, msg = ce.Code, ce.Msg
	}
	contract.WriteError(w, http.StatusBadGateway, code, msg)
}
