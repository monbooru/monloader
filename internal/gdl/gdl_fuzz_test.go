package gdl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzParseResolve(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte("not json"))
	f.Add([]byte(`[]`))
	f.Add([]byte(`[[3,"http://x/1.jpg",{"category":"danbooru","id":1,"extension":"jpg"}]]`))
	f.Add([]byte(`[[3,"http://x/1.jpg",{"tags":"a b c"}],[3,"http://x/2.jpg",{"tags":["a","b"]}]]`))
	f.Add([]byte(`[[3,null,{"id":true}]]`))
	f.Add([]byte(`[[6,"http://x/chapter/1",{"category":"mangadex"}]]`))
	f.Add([]byte(`[[-1,{"error":"NoExtractorError","message":"Unsupported URL"}]]`))
	f.Add([]byte(`[[3,"http://x/1.jpg",[]]]`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = parseResolve(raw)
	})
}

func FuzzReportDownloads(f *testing.F) {
	f.Add("")
	f.Add("# /work/job-1/skipped.jpg\n")
	f.Add("/work/job-1/p1.jpg\n/work/job-1/p2.jpg\n")
	f.Add("../../etc/passwd\n")
	f.Add("/work/job-1/p1.jpg\r\n\n# \n")
	f.Fuzz(func(t *testing.T, out string) {
		dir := t.TempDir()

		path := filepath.Join(dir, "p1.jpg")
		if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
			t.Skip()
		}
		if err := os.WriteFile(path+".json", []byte(`{"category":"danbooru"}`), 0o644); err != nil {
			t.Skip()
		}
		_, _ = reportDownloads(strings.NewReader(out), dir, nil, nil)
	})
}
