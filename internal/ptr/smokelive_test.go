//go:build live

package ptr

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/monbooru/monloader/internal/config"
)

func TestLiveHydrusSmoke(t *testing.T) {
	addr := os.Getenv("MONLOADER_HYDRUS_SMOKE")
	if addr == "" {
		t.Skip("set MONLOADER_HYDRUS_SMOKE to the local hydrus tag repo address")
	}
	e := NewEngine(config.PTRConfig{Address: addr, DataPath: t.TempDir()})

	key, err := e.CreateContribAccount(context.Background())
	if err != nil {
		t.Fatalf("CreateContribAccount: %v", err)
	}
	t.Logf("created account key %s...", key[:16])
	acc := e.CachedAccount()
	if acc == nil {
		t.Fatal("no account state after creation")
	}
	t.Logf("account type %q, can create mappings=%v petition=%v",
		acc.Type.Title, acc.Type.CanCreateMappings(), acc.Type.CanSend(ContribSibling))
	if !acc.Type.CanCreateMappings() || !acc.Type.CanSend(ContribSibling) {
		t.Errorf("account permissions = %v, want mappings create + petition", acc.Type.Permissions)
	}

	filter, err := e.RefreshTagFilter(context.Background())
	if err != nil {
		t.Fatalf("RefreshTagFilter: %v", err)
	}
	if blocked, _ := filter.Blocks("blonde hair"); blocked {
		t.Error("fresh repo should block nothing")
	}

	h1 := bytes.Repeat([]byte{0x11}, 32)
	h2 := bytes.Repeat([]byte{0x22}, 32)
	u := &C2SUpdate{}
	u.AddMappings(ActionPetition, "spec test tag", [][]byte{h2}, "not in the file")
	u.AddMappings(ActionPend, "spec test tag", [][]byte{h1, h2}, "")
	u.AddMappings(ActionPend, "creator:spec test artist", [][]byte{h1}, "")
	u.AddSibling(ActionPend, "speck test tag", "spec test tag", "misspelling")
	u.AddParent(ActionPend, "spec test tag", "series:spec test", "canonical")
	if err := e.PostUpdate(context.Background(), u); err != nil {
		t.Fatalf("PostUpdate: %v", err)
	}
	t.Log("POST /update accepted by the live server")

	if err := e.PostUpdate(context.Background(), u); err != nil {
		t.Fatalf("idempotent re-send: %v", err)
	}
	t.Log("identical re-send accepted (idempotent)")
}
