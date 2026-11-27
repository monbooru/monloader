package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monloader/internal/shared/contract"
	"github.com/monbooru/monloader/internal/shared/logx"
	"github.com/monbooru/monloader/internal/shared/webui"
)

const tagSuggestRoute = "/internal/tags/suggest"

// Every keystroke asks, so a refusal is logged when it changes, not each
// time: an old monbooru says once that it has no suggestions to give.
type suggestWarning struct {
	mu   sync.Mutex
	last string
}

func (s *suggestWarning) warn(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == msg {
		return
	}
	s.last = msg
	logx.Warnf("tag suggestions: %s", msg)
}

func (s *suggestWarning) clear() {
	s.mu.Lock()
	s.last = ""
	s.mu.Unlock()
}

// A tags field's route, suggesting from the gallery its tags land in.
func tagSuggestURL(gallery string) string {
	return tagSuggestRoute + "?gallery=" + url.QueryEscape(gallery)
}

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
	switch {
	case err != nil:
		// A request dropped by the next keystroke says nothing of monbooru.
		if r.Context().Err() == nil {
			s.tagSuggestWarn.warn(err.Error())
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case len(found) == 0:
		s.tagSuggestWarn.clear()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.tagSuggestWarn.clear()
	items := make([]webui.SuggestItem, len(found))
	for i, t := range found {
		items[i] = webui.SuggestItem{Name: t.Name, Alias: t.Alias, Color: t.Color, UsageCount: t.UsageCount, ShowCount: true}
	}
	s.render(w, "webui/suggest_list", webui.TagSuggestList(items))
}
