//go:build ptrsmoke

// This live smoke test contacts the real Hydrus PTR. It is behind the ptrsmoke
// build tag so the default suite never touches the donated repository server;
// run it with `make ptr-smoke`. It fetches the manifest and the first update,
// verifies the blob against its hash, and parses both.
package ptr

import (
	"context"
	"testing"
	"time"

	"github.com/leqwin/monloader/internal/config"
)

func TestPTRSmoke(t *testing.T) {
	c := NewClient(config.Default().PTR) // public key, real address

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	slice, err := c.Metadata(ctx, 0)
	if err != nil {
		t.Fatalf("live Metadata: %v", err)
	}
	t.Logf("manifest: %d update indices, next due %d", len(slice.Updates), slice.NextUpdateDue)
	// Early indices cover empty time windows with no update files; find the
	// first that actually carries one.
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
