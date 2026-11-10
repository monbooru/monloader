package mapping

import "testing"

func TestPostURL(t *testing.T) {
	m := newMapper(t, nil)
	bluesky := map[string]any{
		"category": "bluesky", "subcategory": "likes", "post_id": "3mz4cvlf7z22v",
		"uri":    "at://did:plc:z72i7/app.bsky.feed.post/3mz4cvlf7z22v",
		"author": map[string]any{"handle": "bsky.app"},
	}
	for _, tc := range []struct {
		name string
		meta map[string]any
		want string
	}{
		{"the danbooru post page", map[string]any{"category": "danbooru", "id": float64(11474309)}, "https://danbooru.donmai.us/posts/11474309"},
		{"the sankaku post page", map[string]any{"category": "sankaku", "id": "vkr3EelGKMZ"}, "https://sankaku.app/posts/vkr3EelGKMZ"},
		{"the idolcomplex post page", map[string]any{"category": "idolcomplex", "id": "6ea43jg5M3v"}, "https://www.idolcomplex.com/posts/6ea43jg5M3v"},
		{"the pixiv artwork page", map[string]any{"category": "pixiv", "id": float64(93122368)}, "https://www.pixiv.net/artworks/93122368"},
		{"the furaffinity submission page", map[string]any{"category": "furaffinity", "id": float64(21835115)}, "https://www.furaffinity.net/view/21835115/"},
		{"the booruvar post page", map[string]any{"category": "booruvar", "id": float64(1487)}, "https://booru.borvar.art/posts/1487"},
		{"the leftybooru post page", map[string]any{"category": "leftybooru", "id": float64(100)}, "https://lefty.pictures/post/view/100"},
		{"empty for an unmapped source", map[string]any{"category": "wackybooru", "id": "7"}, ""},
		{"the artstation permalink", map[string]any{
			"category": "artstation", "id": float64(22018269), "hash_id": "WXeQmG",
			"permalink": "https://www.artstation.com/artwork/WXeQmG",
		}, "https://www.artstation.com/artwork/WXeQmG"},
		{"the fantia post_url", map[string]any{
			"category": "fantia", "post_id": float64(1234), "post_url": "https://fantia.jp/posts/1234",
		}, "https://fantia.jp/posts/1234"},
		{"the newgrounds post_url", map[string]any{
			"category": "newgrounds", "index": float64(7), "post_url": "https://www.newgrounds.com/art/view/tomfulp/ryu-is-hawt",
		}, "https://www.newgrounds.com/art/view/tomfulp/ryu-is-hawt"},
		{"empty for a relative post_url", map[string]any{
			"category": "wackybooru", "id": "7", "post_url": "/posts/7",
		}, ""},
		{"the danbooru template over a post_url", map[string]any{
			"category": "danbooru", "id": float64(11474309), "post_url": "https://elsewhere.example/post",
		}, "https://danbooru.donmai.us/posts/11474309"},
		{"empty for a relative permalink", map[string]any{
			"category": "wackybooru", "id": "7", "permalink": "/r/pics/comments/abc",
		}, ""},
		{"the danbooru template over the permalink", map[string]any{
			"category": "danbooru", "id": float64(11474309),
			"permalink": "https://elsewhere.example/post",
		}, "https://danbooru.donmai.us/posts/11474309"},
		{"empty for a post with no id, not the listing", map[string]any{"category": "danbooru"}, ""},
		{"the permalink for a post with no id", map[string]any{
			"category": "danbooru", "permalink": "https://danbooru.donmai.us/posts/5",
		}, "https://danbooru.donmai.us/posts/5"},
		{"the post page built from its keywords", bluesky, "https://bsky.app/profile/bsky.app/post/3mz4cvlf7z22v"},
		{"empty rather than a half-filled template", map[string]any{"category": "bluesky", "post_id": "3mz4cvlf7z22v"}, ""},
		{"a keyword that would break the path escaped", map[string]any{
			"category": "bluesky", "post_id": "3mz4", "author": map[string]any{"handle": "a/b"},
		}, "https://bsky.app/profile/a%2Fb/post/3mz4"},
		{"{id} to keep the id's fallbacks", map[string]any{"category": "nhentai", "gallery_id": float64(7)}, "https://nhentai.net/g/7/"},
		{"nothing for a direct link, which pushes what was submitted", map[string]any{
			"category": "directlink", "domain": "img.example.com",
			"path": "art/2024", "filename": "picture", "extension": "jpg",
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.PostURL(tc.meta); got != tc.want {
				t.Errorf("PostURL = %q, want %s", got, tc.name)
			}
		})
	}
}
