package pipeline

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/queue"
)

func TestPipelineItemCarriesSourceURL(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("100001")}, writeIdx: []int{0}}
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7001}`))
	})

	id := q.Enqueue("https://danbooru.donmai.us/posts/100001", queue.Options{})
	job := waitJob(t, q, id)
	if len(job.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(job.Items))
	}
	if got := job.Items[0].URL; got != "https://danbooru.donmai.us/posts/100001" {
		t.Errorf("item URL = %q, want the danbooru post page", got)
	}
}

func TestPipelinePermalinkBeatsSubmittedURL(t *testing.T) {
	item := multiAssetPost("22018269", 1)
	item.Meta["permalink"] = "https://www.artstation.com/artwork/WXeQmG"
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	var gotURL string
	q := testEnv(t, fake, capturePush(8003, map[string]*string{"url": &gotURL}))

	job := waitJob(t, q, q.Enqueue("https://www.artstation.com/wlop", queue.Options{}))

	const want = "https://www.artstation.com/artwork/WXeQmG"
	if gotURL != want {
		t.Errorf("pushed url = %q, want the permalink", gotURL)
	}
	if len(job.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(job.Items))
	}
	if got := job.Items[0].URL; got != want {
		t.Errorf("queue item URL = %q, want the permalink", got)
	}
}

func TestPipelinePixivListingItemsLinkTheirArtworks(t *testing.T) {
	work := func(id string) gdl.Item {
		return gdl.Item{
			Category: "pixiv", Subcategory: "bookmark", ID: id,
			Meta: map[string]any{
				"category": "pixiv", "subcategory": "bookmark", "id": id,
				"num": float64(0), "rating": "General", "tags": []any{"original"},
			},
		}
	}
	fake := &fakeRunner{resolved: []gdl.Item{work("93122368"), work("93122369")}, writeIdx: []int{0, 1}}
	var pushed []string
	q := testEnv(t, fake, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		pushed = append(pushed, r.FormValue("url"))
		reply(w, http.StatusCreated, map[string]any{"id": len(pushed)})
	})

	job := waitJob(t, q, q.Enqueue("https://www.pixiv.net/users/25324443/bookmarks/artworks", queue.Options{}))
	want := []string{"https://www.pixiv.net/artworks/93122368", "https://www.pixiv.net/artworks/93122369"}
	if !reflect.DeepEqual(pushed, want) {
		t.Errorf("pushed urls = %v, want each work's own page %v", pushed, want)
	}
	if len(job.Items) != 2 || job.Items[0].URL != want[0] || job.Items[1].URL != want[1] {
		t.Errorf("queue items = %+v, want each work's own page", job.Items)
	}
}

func TestPipelinePushCarriesParentURL(t *testing.T) {
	item := danbooruPost("100002")
	item.Meta["parent_id"] = float64(100001)
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}}
	var gotParent string
	q := testEnv(t, fake, capturePush(7002, map[string]*string{"parent_url": &gotParent}))

	waitJob(t, q, q.Enqueue("https://danbooru.donmai.us/posts/100002", queue.Options{}))
	if gotParent != "https://danbooru.donmai.us/posts/100001" {
		t.Errorf("pushed parent_url = %q, want the parent's post page", gotParent)
	}
}

func TestPipelineDirectlinkProvidesSourceAndURL(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{directlinkItem()}, writeIdx: []int{0}}
	var gotSource, gotURL string
	q := testEnv(t, fake, capturePush(8001, map[string]*string{"source": &gotSource, "url": &gotURL}))

	job := waitJob(t, q, q.Enqueue("https://img.example.com/art/2024/picture.jpg", queue.Options{}))

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

func TestPipelineDirectlinkUsesPageURL(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{directlinkItem()}, writeIdx: []int{0}}
	var gotSource, gotURL string
	q := testEnv(t, fake, capturePush(8004, map[string]*string{"source": &gotSource, "url": &gotURL}))

	const page = "https://wiki.example/wiki/File:picture.jpg"
	job := waitJob(t, q, q.Enqueue("https://img.example.com/art/2024/picture.jpg", queue.Options{PageURL: page}))

	if gotURL != page {
		t.Errorf("pushed url = %q, want the page URL", gotURL)
	}
	if gotSource != "wiki.example" {
		t.Errorf("pushed source = %q, want the page host", gotSource)
	}
	if got := job.Items[0].URL; got != page {
		t.Errorf("queue item URL = %q, want the page URL", got)
	}
}

func TestPipelineDirectlinkURLKeepsResolvedExtension(t *testing.T) {
	item := directlinkItem()
	item.Meta["extension"] = "webp"
	fake := &fakeRunner{resolved: []gdl.Item{item}, writeIdx: []int{0}, dlExt: "jpg"}
	var gotURL string
	q := testEnv(t, fake, capturePush(8002, map[string]*string{"url": &gotURL}))

	job := waitJob(t, q, q.Enqueue("https://img.example.com/art/2024/picture.webp", queue.Options{}))

	const wantURL = "https://img.example.com/art/2024/picture.webp"
	if gotURL != wantURL {
		t.Errorf("pushed url = %q, want the submitted URL %q (not the adjusted-extension variant)", gotURL, wantURL)
	}
	if got := job.Items[0].URL; got != wantURL {
		t.Errorf("queue item URL = %q, want %q", got, wantURL)
	}
}

func TestPipelineDirectlinkHostLabel(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{directlinkItem()}, writeIdx: []int{0}}
	var gotSource string
	cfg := config.Default()
	cfg.HostLabels = []config.HostLabel{{Host: "wiki.example", Label: "example wiki"}}
	q := lookupEnvCfg(t, fake, capturePush(8005, map[string]*string{"source": &gotSource}), cfg, nil, nil)

	const page = "https://files.wiki.example/wiki/File:picture.jpg"
	waitJob(t, q, q.Enqueue("https://img.example.com/art/2024/picture.jpg", queue.Options{PageURL: page}))

	if gotSource != "example wiki" {
		t.Errorf("pushed source = %q, want the host label", gotSource)
	}
}
