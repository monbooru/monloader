package web

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/queue"
)

type stubDebugRunner struct {
	fakeRunner
	log         string
	err         error
	noExtractor bool
	urls        []string
}

func (s *stubDebugRunner) DebugLog(_ context.Context, u, _ string) (string, bool, error) {
	s.urls = append(s.urls, u)
	return s.log, s.noExtractor, s.err
}

func TestItemDebugOffersTheRun(t *testing.T) {
	_, ts := serveWeb(t, "")
	_, body := get(t, ts, "/internal/item-debug?url="+url.QueryEscape("https://danbooru.donmai.us/posts/1"))

	if !strings.Contains(body, "Run with debug log") {
		t.Errorf("body = %q, want the run button", truncate(body))
	}
	if !strings.Contains(body, "Nothing goes to monbooru") {
		t.Errorf("body = %q, want the note that nothing is pushed", truncate(body))
	}

	if !strings.Contains(body, `hx-disabled-elt="find button"`) {
		t.Errorf("body = %q, want the run form to disable its button in flight", truncate(body))
	}
}

func TestItemDebugNamesAURLGalleryDLNeverFetched(t *testing.T) {
	srv, ts := serveWeb(t, "")
	srv.runner = &stubDebugRunner{log: "[gallery-dl][error] Unsupported URL", noExtractor: true}

	body := readBody(t, postForm(t, ts, srv, "/internal/item-debug",
		url.Values{"url": {"http://cdn.example/avatar"}}))
	if !strings.Contains(body, "monloader fetched the file itself") {
		t.Errorf("body = %q, want the panel to explain the refusal", truncate(body))
	}
}

func TestItemDebugRunsTheURLThatWasFetched(t *testing.T) {
	const page = "https://forum.example/thread/77"
	const file = "http://cdn.example/avatar"
	got := itemView(queue.Item{Num: 1, URL: page}, map[string]any{"JobURL": file, "PageURL": page})
	if got.SubjectURL != page {
		t.Errorf("SubjectURL = %q, want the page the item links to", got.SubjectURL)
	}
	if got.DebugURL != file {
		t.Errorf("DebugURL = %q, want the file that was fetched", got.DebugURL)
	}
	post := queue.Item{PostID: "1", URL: "https://danbooru.donmai.us/posts/1"}
	if d := itemView(post, map[string]any{}); d.DebugURL != post.URL {
		t.Errorf("DebugURL = %q, want the post link", d.DebugURL)
	}
}

func TestItemDebugWithoutAURL(t *testing.T) {
	_, ts := serveWeb(t, "")
	if _, body := get(t, ts, "/internal/item-debug"); !strings.Contains(body, "no source link") {
		t.Errorf("body = %q, want the missing-url refusal", truncate(body))
	}
}

func TestItemDebugRunsAndRedacts(t *testing.T) {
	srv, ts := serveWeb(t, "")
	mustUpdate(t, srv, func(c *config.Config) {
		c.Sites = []config.Site{{Name: "gelbooru", APIKey: "SECRETKEY123"}}
	})
	runner := &stubDebugRunner{log: `[urllib3][debug] "GET /index.php?api_key=SECRETKEY123" 401`}
	srv.runner = runner

	post := "https://gelbooru.com/index.php?page=post&s=view&id=1"
	body := readBody(t, postForm(t, ts, srv, "/internal/item-debug", url.Values{"url": {post}}))

	if len(runner.urls) != 1 || runner.urls[0] != post {
		t.Errorf("ran %v, want the item's post url", runner.urls)
	}
	if strings.Contains(body, "SECRETKEY123") {
		t.Errorf("body = %q, still carries the api key", truncate(body))
	}
	if !strings.Contains(body, "redacted:gelbooru.api_key") {
		t.Errorf("body = %q, want the marker naming the removed key", truncate(body))
	}
	if !strings.Contains(body, "1 secret removed") {
		t.Errorf("body = %q, want the removal count", truncate(body))
	}
	if !strings.Contains(body, "reproduce: gallery-dl -v --range 1-1") {
		t.Errorf("body = %q, want the reproduce line", truncate(body))
	}
}

func TestItemDebugReportsAFailedRun(t *testing.T) {
	srv, ts := serveWeb(t, "")
	srv.runner = &stubDebugRunner{err: errors.New("gallery-dl is not installed")}

	body := readBody(t, postForm(t, ts, srv, "/internal/item-debug",
		url.Values{"url": {"https://danbooru.donmai.us/posts/1"}}))
	if !strings.Contains(body, "gallery-dl is not installed") {
		t.Errorf("body = %q, want the failure reported in the pop-in", truncate(body))
	}
}
