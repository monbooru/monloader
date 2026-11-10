//go:build ptrsmoke

package ptr

import (
	"context"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
)

func TestPTRSmoke(t *testing.T) {
	c := newClient(config.Default().PTR)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	slice, err := c.Metadata(ctx, 0)
	if err != nil {
		t.Fatalf("live Metadata: %v", err)
	}
	t.Logf("manifest: %d update indices, next due %d", len(slice.Updates), slice.NextUpdateDue)

	var firstHash string
	for _, e := range slice.Updates {
		if len(e.Hashes) > 0 {
			firstHash = e.Hashes[0]
			break
		}
	}
	if firstHash == "" {
		t.Fatal("live manifest carried no update files")
	}

	up, err := c.Update(ctx, firstHash)
	if err != nil {
		t.Fatalf("live Update: %v", err)
	}
	switch {
	case up.Definitions != nil:
		t.Logf("first update: definitions (%d hashes, %d tags)", len(up.Definitions.Hashes), len(up.Definitions.Tags))
	case up.Content != nil:
		t.Logf("first update: content (%d mapping-add rows)", len(up.Content.MappingsAdd))
	}
}
