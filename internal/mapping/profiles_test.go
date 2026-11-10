package mapping

import (
	"strings"
	"testing"
)

func TestLoadProfiles(t *testing.T) {
	profiles, err := loadProfiles()
	if err != nil {
		t.Fatalf("loadProfiles: %v", err)
	}
	if len(profiles) != 61 {
		t.Errorf("loaded %d profiles, want 61", len(profiles))
	}
	for category, p := range profiles {
		if err := ValidateProfile(p); err != nil {
			t.Errorf("profile %s: %v", category, err)
		}
		if !ValidCategoryName(category) {
			t.Errorf("profile %s: not a valid gallery-dl category name", category)
		}
		if p.PostURL != "" && (!strings.Contains(p.PostURL, "{") || strings.Count(p.PostURL, "{id}") > 1) {
			t.Errorf("profile %s: post url template %q must carry a placeholder, and {id} at most once", category, p.PostURL)
		}
	}
	if p, ok := profiles["danbooru"]; !ok || p.Family != FamilyDanbooru || p.PostURL == "" {
		t.Errorf("danbooru profile = %+v, want the danbooru family with a post URL", p)
	}
	if p, ok := profiles["nhentai"]; !ok || p.Kind != KindManga || p.DefaultRating != "explicit" {
		t.Errorf("nhentai profile = %+v, want a manga kind defaulting explicit", p)
	}
}
