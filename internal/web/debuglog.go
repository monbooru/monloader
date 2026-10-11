package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
)

// Longer than the site probe's: this run fetches the file, not just the post.
const debugLogTimeout = 60 * time.Second

func (s *Server) itemDebug(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"CSRFToken": s.csrfToken(sessionFromContext(r.Context()))}
	url := strings.TrimSpace(r.FormValue("url"))
	if !config.IsHTTPURL(url) {
		data["Error"] = "this item has no source link to re-run"
		s.render(w, "item_debug", data)
		return
	}
	data["URL"] = url
	if r.Method == http.MethodGet {
		s.render(w, "item_debug", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), debugLogTimeout)
	defer cancel()
	log, noExtractor, err := s.runner.DebugLog(ctx, url, s.workRoot)
	if err != nil {
		data["Error"] = err.Error()
		s.render(w, "item_debug", data)
		return
	}
	data["NoExtractor"] = noExtractor
	// gallery-dl logs request URLs verbatim, and some boorus take the API
	// key as a query parameter.
	text, redacted := config.RedactSecrets(log, s.cfg.Current().Secrets())
	data["Log"] = debugHeader(url, s.catalog.Version()) + text
	data["Redacted"] = redacted
	s.render(w, "item_debug", data)
}

// gallery-dl writes its banner before the log file exists, so the log lacks it.
func debugHeader(url, gdlVersion string) string {
	return "monloader " + Version + " - gallery-dl " + gdlVersion + "\n" +
		"url: " + url + "\n" +
		"reproduce: gallery-dl -v --range 1-1 " + url + "\n\n"
}
