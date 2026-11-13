//go:build linux

package desktop

import (
	"os"
	"testing"
)

func TestProcRSSSplitsAnonAndFile(t *testing.T) {
	r, ok := ProcRSS(os.Getpid())
	if !ok {
		t.Fatal("ProcRSS(self) = not ok")
	}
	if r.Total == 0 || r.Anon == 0 || r.File == 0 {
		t.Fatalf("ProcRSS(self) = %+v, want a nonzero total, anon and file", r)
	}
	if r.Anon+r.File > r.Total {
		t.Errorf("anon %d + file %d exceeds total %d", r.Anon, r.File, r.Total)
	}
}

func TestProcRSSGonePid(t *testing.T) {
	if _, ok := ProcRSS(1 << 30); ok {
		t.Error("ProcRSS on a pid that cannot exist = ok")
	}
}

func TestProcStatusReadsBytes(t *testing.T) {
	if rss := ProcStatus("self", "VmRSS"); rss == 0 || rss%1024 != 0 {
		t.Errorf("ProcStatus(self, VmRSS) = %d, want a nonzero count of bytes", rss)
	}
	if v := ProcStatus("self", "NoSuchField"); v != 0 {
		t.Errorf("ProcStatus on a missing field = %d", v)
	}
}

func TestDBPagesCountEitherAppsDatabases(t *testing.T) {
	for path, want := range map[string]bool{
		"/data/galleries/default/gallery.db":     true,
		"/data/galleries/default/gallery.db-wal": true,
		"/config/queue.sqlite":                   true,
		"/config/queue.sqlite-shm":               true,
		"/config/ptr/index.sqlite3":              true,
		"/usr/lib/libc.so.6":                     false,
		"/app/server":                            false,
	} {
		if got := isDBPath(path); got != want {
			t.Errorf("isDBPath(%q) = %v, want %v", path, got, want)
		}
	}
}
