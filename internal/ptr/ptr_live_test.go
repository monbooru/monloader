//go:build live

package ptr

import (
	"context"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
)

func TestLivePTRWireFormat(t *testing.T) {
	c := newClient(config.Default().PTR)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	meta, err := c.Metadata(ctx, 0)
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if len(meta.Updates) == 0 {
		t.Fatal("the PTR listed no updates")
	}
	last := meta.Updates[len(meta.Updates)-1]
	if len(last.Hashes) == 0 || last.End < last.Begin {
		t.Fatalf("newest update entry = %+v", last)
	}
	u, err := c.Update(ctx, last.Hashes[len(last.Hashes)-1])
	if err != nil {
		t.Fatalf("update %d: %v", last.Index, err)
	}
	if u.Definitions == nil && u.Content == nil {
		t.Fatal("the update parsed as neither definitions nor content")
	}
	t.Logf("%d updates listed; update %d carries %d rows", len(meta.Updates), last.Index, u.Rows())
}
