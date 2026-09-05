package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/config"
)

// debugLogTimeout bounds one debug run: a wall or a hung host must not hold the
// request open. Longer than the per-site probe's, since this one fetches the
// file rather than just reading the post.
const debugLogTimeout = 60 * time.Second

// itemDebug backs the queue item's [?]: a GET offers the re-run, a POST makes
// it and swaps the log in. gallery-dl's error text asks the reporter to re-run
// with --verbose, which is otherwise only possible from inside the container.
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
	// gallery-dl logs request URLs verbatim and the query-parameter booru
	// families put their key there, so nothing is shown before it is scrubbed.
	text, redacted := config.RedactSecrets(log, s.cfg.Current().Secrets())
	data["Log"] = debugHeader(url, s.catalog.Version()) + text
	data["Redacted"] = redacted
	s.render(w, "item_debug", data)
}

// debugHeader names the versions and the post a log belongs to. gallery-dl
// writes its own banner before the log file exists, so this stands in for it.
func debugHeader(url, gdlVersion string) string {
	return "monloader " + Version + " - gallery-dl " + gdlVersion + "\n" +
		"url: " + url + "\n" +
		"reproduce: gallery-dl -v --range 1-1 " + url + "\n\n"
}
