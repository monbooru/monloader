package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/queue"
)

func TestTagSuggestAsksTheGalleryForTheWordAtTheCaret(t *testing.T) {
	cases := []struct {
		name, query   string
		code          int
		want, wantNot []string
	}{
		{"a match comes with its colour and count", "tag=user", http.StatusOK,
			[]string{`data-tag-name="artist:user_a"`, `onclick="applyTagSuggest(this)"`, `style="color:var(--cat-aa0000,#aa0000)"`, `<span class="suggest-count">3</span>`},
			[]string{`data-tag-name="wip"`}},
		{"a tag found through its alias names the alias", "tag=work", http.StatusOK,
			[]string{`<span class="suggest-alias">work_in_progress →</span> wip`}, nil},
		{"the gallery asked is the one listed", "tag=in_&gallery=second", http.StatusOK,
			[]string{`data-tag-name="in_second"`}, nil},
		{"no word clears the list", "tag=+", http.StatusNoContent, nil, nil},
		{"no match clears the list", "tag=nothing_like_it", http.StatusNoContent, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := serveWeb(t, "")
			pairMB(t, srv)
			code, body := get(t, ts, tagSuggestRoute+"?"+tc.query)
			if code != tc.code {
				t.Fatalf("status = %d, want %d: %s", code, tc.code, body)
			}
			wantInBody(t, body, tc.want, tc.wantNot)
		})
	}
}

func TestWatchTagsDialog(t *testing.T) {
	srv, ts := serveWatches(t, noopProc{})
	mustUpdate(t, srv, func(c *config.Config) {
		c.Monbooru.DefaultGallery = "main"
		c.FindSite("danbooru").Gallery = "art"
	})
	id, err := srv.queue.AddWatch("https://danbooru.donmai.us/posts?tags=a", 24, 0, []string{"artist:user_a", "wip"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := srv.queue.AddWatch("https://unknown.example/posts?tags=a", 24, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, query   string
		want, wantNot []string
	}{
		{"a row's opens on its tags and saves to it, suggesting from its site's gallery before its first check", "id=" + strconv.FormatInt(id, 10),
			[]string{`hx-post="/watches/` + strconv.FormatInt(id, 10) + `/tags"`, `value="artist:user_a wip"`, `https://danbooru.donmai.us/posts?tags=a`,
				`hx-get="/internal/tags/suggest?gallery=art"`, `id="watch-tags-suggest"`},
			[]string{`hx-post="/internal/watch-tags"`}},
		{"a row for a site with no gallery of its own asks the default one", "id=" + strconv.FormatInt(other, 10),
			[]string{`hx-get="/internal/tags/suggest?gallery=main"`}, nil},
		{"the add bar's holds what the bar does", "tags=" + url.QueryEscape("user_a  wip"),
			[]string{`hx-post="/internal/watch-tags" hx-target="#watch-add-tags-box"`, `value="user_a wip"`, `The watch you are adding`, `hx-get="/internal/tags/suggest?gallery=main"`}, nil},
		{"the add bar's asks the gallery of the site typed", "url=" + url.QueryEscape("https://danbooru.donmai.us/posts?tags=b"),
			[]string{`https://danbooru.donmai.us/posts?tags=b`, `hx-get="/internal/tags/suggest?gallery=art"`}, []string{`The watch you are adding`}},
		{"a gone watch says so", "id=999", []string{"That watch is gone"}, []string{`watch-tags-form`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, body := get(t, ts, "/internal/watch-tags?"+tc.query)
			wantInBody(t, body, tc.want, tc.wantNot)
		})
	}
}

func manyTags(n int) string {
	tags := make([]string, n)
	for i := range tags {
		tags[i] = "t" + strconv.Itoa(i)
	}
	return strings.Join(tags, " ")
}

func TestSetWatchTags(t *testing.T) {
	cases := []struct {
		name, tags string
		want       []string
		refusal    string
	}{
		{"spelled as monbooru gets them, repeats dropped", " Artist:user_a WIP  wip ", []string{"artist:user_a", "wip"}, ""},
		{"emptied", "", nil, ""},
		{"at the cap", manyTags(queue.MaxOptionTags), strings.Fields(manyTags(queue.MaxOptionTags)), ""},
		{"past the cap refused", manyTags(queue.MaxOptionTags + 1), []string{"kept"}, "At most 64 tags."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := serveWatches(t, noopProc{})
			id, _ := srv.queue.AddWatch("https://danbooru.donmai.us/posts?tags=a", 24, 0, []string{"kept"}, nil)
			resp := postForm(t, ts, srv, "/watches/"+strconv.FormatInt(id, 10)+"/tags", url.Values{"tags": {tc.tags}})
			retarget, body := resp.Header.Get("HX-Retarget"), readBody(t, resp)
			if got := srv.queue.Watches()[0].Tags; !slices.Equal(got, tc.want) {
				t.Errorf("tags = %q, want %q", got, tc.want)
			}
			switch {
			case tc.refusal != "" && (retarget != "#watch-tags-flash" || !strings.Contains(body, tc.refusal)):
				t.Errorf("refusal = %s %q, want %q in the dialog", retarget, truncate(body), tc.refusal)
			case tc.refusal == "" && retarget != "":
				t.Errorf("a save retargeted to %s: %q", retarget, truncate(body))
			}
		})
	}
	srv, ts := serveWatches(t, noopProc{})
	resp := postForm(t, ts, srv, "/watches/999/tags", url.Values{"tags": {"a"}})
	if retarget, body := resp.Header.Get("HX-Retarget"), readBody(t, resp); retarget != "#watch-tags-flash" || !strings.Contains(body, "That watch is gone") {
		t.Errorf("tags on a gone watch = %s %q, want the gone notice in the dialog", retarget, truncate(body))
	}
}

func TestTheAddBarHoldsItsTagsAsTheWatchWill(t *testing.T) {
	cases := []struct {
		name, tags    string
		retarget      string
		want, wantNot []string
	}{
		{"folded, repeats dropped, counted as saved", "WIP wip Artist:user_a", "",
			[]string{`id="watch-add-tags-box"`, `value="wip artist:user_a"`, `title="Adds wip artist:user_a"`, `Tags <span class="watch-tags-count">2</span>`},
			[]string{`WIP`}},
		{"emptied", "  ", "", []string{`value=""`, `title="Tags this watch adds to every post"`, `>Tags</button>`}, []string{`watch-tags-count`}},
		{"past the cap refused in the dialog", manyTags(queue.MaxOptionTags + 1), "#watch-tags-flash", []string{"At most 64 tags."}, []string{`watch-add-tags-box`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := serveWatches(t, noopProc{})
			resp := postForm(t, ts, srv, "/internal/watch-tags", url.Values{"tags": {tc.tags}})
			if got := resp.Header.Get("HX-Retarget"); got != tc.retarget {
				t.Errorf("HX-Retarget = %q, want %q", got, tc.retarget)
			}
			wantInBody(t, readBody(t, resp), tc.want, tc.wantNot)
		})
	}
}

func TestAnAddedWatchEmptiesTheBarsTags(t *testing.T) {
	srv, ts := serveWatches(t, noopProc{})
	_, page := get(t, ts, "/watches")
	wantInBody(t, page, []string{`id="watch-add-tags-box" hx-get="/internal/watch-add-tags" hx-trigger="watch-added from:body"`}, nil)
	resp := postForm(t, ts, srv, "/watches", url.Values{"url": {"https://danbooru.donmai.us/posts?tags=a"}, "interval_hours": {"24"}, "tags": {"wip"}})
	readBody(t, resp)
	if got := resp.Header.Get("HX-Trigger"); got != "watch-added" {
		t.Errorf("HX-Trigger = %q, want watch-added", got)
	}
	_, box := get(t, ts, "/internal/watch-add-tags")
	wantInBody(t, box, []string{`value=""`, `>Tags</button>`}, []string{`watch-tags-count`})
	waitWatchChecked(t, srv)
}

func TestWatchRowsCountTheirTags(t *testing.T) {
	srv, ts := serveWatches(t, noopProc{})
	_, _ = srv.queue.AddWatch("https://danbooru.donmai.us/posts?tags=a", 24, 0, []string{"artist:user_a", "wip"}, nil)
	_, _ = srv.queue.AddWatch("https://danbooru.donmai.us/posts?tags=b", 24, 0, nil, nil)
	_, body := get(t, ts, "/internal/watch-rows")
	wantInBody(t, body, []string{`title="Adds artist:user_a wip">Tags <span class="watch-tags-count">2</span>`,
		`title="Tags this watch adds to every post">Tags</button>`}, nil)
}

func TestAddWatchTakesTheBarsTags(t *testing.T) {
	srv, ts := serveWatches(t, noopProc{})
	vals := url.Values{"url": {"https://danbooru.donmai.us/posts?tags=a"}, "interval_hours": {"24"}, "tags": {manyTags(queue.MaxOptionTags + 1)}}
	if body := readBody(t, postForm(t, ts, srv, "/watches", vals)); !strings.Contains(body, "At most 64 tags.") || len(srv.queue.Watches()) != 0 {
		t.Fatalf("too many tags = %q with %d watches, want the refusal and none", truncate(body), len(srv.queue.Watches()))
	}
	vals.Set("tags", "user_a WIP user_a")
	readBody(t, postForm(t, ts, srv, "/watches", vals))
	if ws := srv.queue.Watches(); len(ws) != 1 || !slices.Equal(ws[0].Tags, []string{"user_a", "wip"}) {
		t.Errorf("watches = %+v, want one with the bar's tags", ws)
	}
	waitWatchChecked(t, srv)
}
