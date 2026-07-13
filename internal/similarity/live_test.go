package similarity

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/leqwin/monloader/internal/config"
	"github.com/leqwin/monloader/internal/queue"
)

// The clients are exercised against the real services when credentials ride
// the environment; a machine without them skips. The probe image is a
// generated gradient, so a clean "no match" is the expected live answer -
// what these pin is the wire format and the auth path, not a hit.

func liveClient(cfg *config.Config) *Client {
	return newClient(cfg, nil)
}

func TestLiveIQDB(t *testing.T) {
	user, key := os.Getenv("MONLOADER_TEST_DANBOORU_USER"), os.Getenv("MONLOADER_TEST_DANBOORU_KEY")
	if user == "" || key == "" {
		t.Skip("MONLOADER_TEST_DANBOORU_USER / _KEY not set")
	}
	cfg := config.Default()
	cfg.Sites = []config.Site{{Name: "danbooru", Username: user, APIKey: key}}
	res, err := liveClient(cfg).Search(context.Background(), "iqdb", ProbeImage())
	if err != nil {
		t.Fatalf("live iqdb: %v", err)
	}
	// Shape only: whatever posts come back are danbooru candidates with sane
	// scores; the gradient must not clear the similarity floor.
	for _, c := range res.Candidates {
		if c.Site != "danbooru" || c.URL == "" || c.Similarity < 0 || c.Similarity > 100 {
			t.Errorf("candidate = %+v", c)
		}
		if c.Similarity >= 80 {
			t.Errorf("the probe gradient scored %v - it must never read as a real match", c.Similarity)
		}
	}
}

func TestLiveIQDBAnonymousRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("live call")
	}
	// No credentials: danbooru's CSRF protection must refuse the upload, which
	// is why the iqdb service is gated on the danbooru api key.
	cfg := config.Default()
	cfg.Sites = nil
	_, err := liveClient(cfg).Search(context.Background(), "iqdb", ProbeImage())
	var ce *queue.CodedError
	if !errors.As(err, &ce) || ce.Code != queue.ErrCodeAuthRequired {
		t.Errorf("anonymous iqdb err = %v, want auth_required", err)
	}
}

func TestLiveIQDBCredentialsReachAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("live call")
	}
	// Dummy credentials must reach danbooru's authenticator (401), not die on
	// the CSRF wall (403) - the wall is what credentials hidden in the
	// multipart body used to hit.
	cfg := config.Default()
	cfg.Sites = []config.Site{{Name: "danbooru", Username: "dummyuser", APIKey: "dummykey"}}
	_, err := liveClient(cfg).Search(context.Background(), "iqdb", ProbeImage())
	var ce *queue.CodedError
	if !errors.As(err, &ce) || ce.Code != queue.ErrCodeAuthRequired || !strings.Contains(ce.Msg, "401") {
		t.Errorf("dummy-credential iqdb err = %v, want auth_required from a 401", err)
	}
}

func TestLiveSauceNAO(t *testing.T) {
	apiKey := os.Getenv("MONLOADER_TEST_SAUCENAO_KEY")
	if apiKey == "" {
		t.Skip("MONLOADER_TEST_SAUCENAO_KEY not set")
	}
	cfg := config.Default()
	cfg.Lookup.Saucenao.APIKey = apiKey
	res, err := liveClient(cfg).Search(context.Background(), "saucenao", ProbeImage())
	if err != nil {
		t.Fatalf("live saucenao: %v", err)
	}
	if res.DailyRemaining < 0 {
		t.Errorf("DailyRemaining = %d, want the reported quota", res.DailyRemaining)
	}
	for _, c := range res.Candidates {
		if c.Site == "" || c.URL == "" || c.Similarity < 0 || c.Similarity > 100 {
			t.Errorf("candidate = %+v", c)
		}
	}
}
