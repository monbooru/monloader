package pipeline

import (
	"net/http"
	"testing"

	"github.com/leqwin/monloader/internal/gdl"
	"github.com/leqwin/monloader/internal/queue"
)

// A resolved post carries its canonical source URL on the queue item so the UI
// can link it back to the booru. It is set at resolve time, independent of the
// per-item outcome.
func TestPipelineItemCarriesSourceURL(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001")}, writeIdx: []int{0}}
	q, cleanup := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7001}`))
	})
	defer cleanup()

	id := q.Enqueue("https://danbooru.donmai.us/posts/100001", queue.Options{})
	job := waitJob(t, q, id)
	if len(job.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(job.Items))
	}
	if got := job.Items[0].URL; got != "https://danbooru.donmai.us/posts/100001" {
		t.Errorf("item URL = %q, want the danbooru post page", got)
	}
}

// A post that declares a parent carries the parent's canonical post URL on
// the push, so monbooru can link the pair as a derivative relation.
func TestPipelinePushCarriesParentURL(t *testing.T) {
	item := danbooruPost("100002")
	item.Meta["parent_id"] = float64(100001)
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	var gotParent string
	q, cleanup := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		gotParent = r.FormValue("parent_url")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7002}`))
	})
	defer cleanup()

	waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts/100002", queue.Options{}))
	if gotParent != "https://danbooru.donmai.us/posts/100001" {
		t.Errorf("pushed parent_url = %q, want the parent's post page", gotParent)
	}
}

// A directlink (a bare media URL) has no booru post, but monloader still
// provides the file's host as the source and the rebuilt file URL, both as the
// queue item URL and the fields pushed to monbooru.
func TestPipelineDirectlinkProvidesSourceAndURL(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{directlinkItem()}, writeIdx: []int{0}}
	var gotSource, gotURL string
	q, cleanup := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		gotSource = r.FormValue("source")
		gotURL = r.FormValue("url")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":8001}`))
	})
	defer cleanup()

	id := q.Enqueue("https://img.example.com/art/2024/picture.jpg", queue.Options{})
	job := waitJob(t, q, id)

	const wantURL = "https://img.example.com/art/2024/picture.jpg"
	if gotSource != "img.example.com" {
		t.Errorf("pushed source = %q, want the file host", gotSource)
	}
	if gotURL != wantURL {
		t.Errorf("pushed url = %q, want %q", gotURL, wantURL)
	}
	if len(job.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(job.Items))
	}
	if got := job.Items[0].URL; got != wantURL {
		t.Errorf("queue item URL = %q, want %q", got, wantURL)
	}
}

// A directlink whose served bytes differ in type from its URL extension (a
// .webp link that returns jpeg) has its file extension rewritten by gallery-dl
// to the real type. The canonical url must stay the submitted one, rebuilt from
// the resolved metadata - the adjusted .jpg variant does not exist upstream.
func TestPipelineDirectlinkURLKeepsResolvedExtension(t *testing.T) {
	item := directlinkItem()
	item.Meta["extension"] = "webp" // the submitted URL ended in .webp
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}, dlExt: "jpg"}
	var gotURL string
	q, cleanup := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		gotURL = r.FormValue("url")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":8002}`))
	})
	defer cleanup()

	id := q.Enqueue("https://img.example.com/art/2024/picture.webp", queue.Options{})
	job := waitJob(t, q, id)

	const wantURL = "https://img.example.com/art/2024/picture.webp"
	if gotURL != wantURL {
		t.Errorf("pushed url = %q, want the submitted URL %q (not the adjusted-extension variant)", gotURL, wantURL)
	}
	if got := job.Items[0].URL; got != wantURL {
		t.Errorf("queue item URL = %q, want %q", got, wantURL)
	}
}
