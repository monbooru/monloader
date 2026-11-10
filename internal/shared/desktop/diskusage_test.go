package desktop

import (
	"path/filepath"
	"testing"
)

func TestDiskUsageReadsTheVolume(t *testing.T) {
	total, free, err := DiskUsage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || free > total {
		t.Errorf("total %d, free %d", total, free)
	}
	if _, _, err := DiskUsage(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing path reported a volume")
	}
}
