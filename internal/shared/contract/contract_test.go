package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIMismatchNamesBothSides(t *testing.T) {
	for _, tc := range []struct {
		peer Health
		want bool
	}{
		{Health{}, false},
		{Health{API: 3, MinPeerAPI: 1}, false},
		{Health{API: 4, MinPeerAPI: 3}, false},
		{Health{API: 4, MinPeerAPI: 4}, true},
		{Health{API: 1, MinPeerAPI: 1}, true},
	} {
		got := tc.peer.APIMismatch("self", 3, 2)
		if (got != "") != tc.want {
			t.Errorf("%+v: %q", tc.peer, got)
		}
		if got != "" && !strings.Contains(got, "this self speaks 3 and drives 2 or newer") {
			t.Errorf("%+v: %q does not name this side", tc.peer, got)
		}
	}
}

func TestValidateViaRefusesWhatBreaksTheSourceSelector(t *testing.T) {
	for _, ok := range []string{"", "monloader", "my_app", strings.Repeat("字", MaxVia)} {
		if err := ValidateVia(ok); err != nil {
			t.Errorf("ValidateVia(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"a b", "a\tb", `a"b`, "a'b", "a<b", "a]b", `a\b`, strings.Repeat("字", MaxVia+1)} {
		if err := ValidateVia(bad); err == nil {
			t.Errorf("ValidateVia(%q) = nil, want an error", bad)
		}
	}
}

func TestHealthLeavesOutWhatASideDoesNotHave(t *testing.T) {
	b, err := json.Marshal(Health{App: "x", Status: "ok", Version: "1", API: 2, MinPeerAPI: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); strings.Contains(got, "gallerydl_version") || !strings.Contains(got, `"api":2`) {
		t.Errorf("health = %s", got)
	}
}

func TestTheTagNameVectorsLoad(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "tagnames.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Names, Forms []struct{ In, Out string } }
	if err := json.Unmarshal(b, &v); err != nil || len(v.Names) == 0 || len(v.Forms) == 0 {
		t.Fatalf("vectors = %+v, %v", v, err)
	}
}
