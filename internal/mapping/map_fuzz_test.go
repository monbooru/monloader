package mapping

import (
	"encoding/json"
	"testing"

	"github.com/monbooru/monloader/internal/config"
)

func FuzzMap(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"category":"danbooru","id":1,"rating":"s","tag_string_artist":"a","tags_general":["x","y"]}`))
	f.Add([]byte(`{"category":"safebooru","tags":"hello,_happy_world! ,_, solo","tags_copyright":"a,_b"}`))
	f.Add([]byte(`{"category":"exhentai","tags":["female:slime girl","other:guro"],"tags_female":["slime girl"]}`))
	f.Add([]byte(`{"category":"mangadex","group":["TCB Scans"],"artist":"Oda","tags":null}`))
	f.Add([]byte(`{"category":"e621","file":{"width":"x","size":[]},"tags":{"artist":[1,null]},"rating":7}`))
	f.Add([]byte(`{"category":"civitai","file":{"nsfwLevel":8},"post_url":"javascript:alert(1)"}`))
	f.Add([]byte(`{"category":"pixiv","tags":[{"name":"a"}],"tags_bookmark":["***"]}`))
	f.Add([]byte(`{"category":"directlink","domain":"","path":null,"filename":3,"source":["a"]}`))
	cfg := config.Default()
	cfg.Tags.Blacklist = []string{"guro -solo", "artist:x", "rating:explicit"}
	m, err := New(config.NewProvider(cfg), "")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var meta map[string]any
		if json.Unmarshal(raw, &meta) != nil {
			t.Skip()
		}
		m.Map(meta)
		m.Blacklisted(meta)
		m.Trace(meta)
	})
}
