//go:build live

package similarity

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/outcome"
)

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
	skipUnlessReachable(t, "https://danbooru.donmai.us")
	cfg := config.Default()
	cfg.Sites = nil
	_, err := liveClient(cfg).Search(context.Background(), "iqdb", ProbeImage())
	var ce *outcome.CodedError
	if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeAuthRequired {
		t.Errorf("anonymous iqdb err = %v, want auth_required", err)
	}
}

func TestLiveIQDBCredentialsReachAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("live call")
	}
	skipUnlessReachable(t, "https://danbooru.donmai.us")
	cfg := config.Default()
	cfg.Sites = []config.Site{{Name: "danbooru", Username: "dummyuser", APIKey: "dummykey"}}
	_, err := liveClient(cfg).Search(context.Background(), "iqdb", ProbeImage())
	var ce *outcome.CodedError
	if !errors.As(err, &ce) || ce.Code != outcome.ErrCodeAuthRequired || !strings.Contains(ce.Msg, "401") {
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

func skipUnlessReachable(t *testing.T, base string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("%s unreachable: %v", base, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		t.Skipf("%s answered %d", base, resp.StatusCode)
	}
}
