package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/webui"
)

const tagSuggestRoute = "/internal/tags/suggest"

// The tags field's word at the caret, answered from the gallery the tags
// land in. A 204 clears the list: no word, no match, or no monbooru.
func (s *Server) tagSuggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	word := strings.TrimSpace(q.Get("tag"))
	if word == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	found, err := s.client.SuggestTags(ctx, q.Get("gallery"), word, contract.TagSuggestLimit)
	if err != nil || len(found) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	items := make([]webui.SuggestItem, len(found))
	for i, t := range found {
		items[i] = webui.SuggestItem{Name: t.Name, Alias: t.Alias, Color: t.Color, UsageCount: t.UsageCount, ShowCount: true}
	}
	s.render(w, "webui/suggest_list", webui.TagSuggestList(items))
}
