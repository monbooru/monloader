package mapping

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/monbooru/monloader/internal/config"
)

func newMapperDir(t *testing.T, cfg *config.Config, dir string) *Mapper {
	t.Helper()
	if cfg == nil {
		cfg = config.Default()
	}
	m, err := New(config.NewProvider(cfg), dir)
	if err != nil {
		t.Fatalf("New mapper: %v", err)
	}
	return m
}

func writeProfile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func TestUserProfileOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "danbooru.json", `{"family":"generic","kind":"other","post_url_template":"https://mirror.example/posts/{id}","auth":"none"}`)
	writeProfile(t, dir, "szurubooru.json", `{"family":"generic","auth":"none","md5_search_template":"https://booru.example/posts?tags=md5:{md5}","post_url_template":"https://booru.example/post/{id}"}`)
	m := newMapperDir(t, nil, dir)

	if got := m.PostURLFor("danbooru", "7"); got != "https://mirror.example/posts/7" {
		t.Errorf("PostURLFor = %q, want the user template", got)
	}
	if m.LookupURL("danbooru", "aa") != "" {
		t.Error("the user profile carries no md5 template, so danbooru should not be lookupable")
	}
	if !m.CustomProfile("danbooru") || m.CustomProfile("gelbooru") {
		t.Error("custom flag should mark exactly the overridden category")
	}
	if got := m.LookupURL("szurubooru", "beef"); got != "https://booru.example/posts?tags=md5:beef" {
		t.Errorf("LookupURL = %q, want the user site's search", got)
	}
	if p, ok := m.Lookup("szurubooru"); !ok || p.Family != FamilyGeneric {
		t.Errorf("Lookup(szurubooru) = %+v %v, want the user profile", p, ok)
	}
}

func TestUserProfileBadFilesSkipped(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "gelbooru.json", `{not json`)
	writeProfile(t, dir, "rule34.json", `{"family":"martian"}`)
	writeProfile(t, dir, "Bad Name.json", `{"family":"generic"}`)
	m := newMapperDir(t, nil, dir)
	if m.CustomProfile("gelbooru") || m.CustomProfile("rule34") {
		t.Error("unparsable or invalid files must be skipped, not applied")
	}
	if p, _ := m.Lookup("gelbooru"); p.Family != FamilyGelbooruV02 {
		t.Errorf("gelbooru family = %q, want the shipped profile intact", p.Family)
	}
}

func TestUserProfileEqualToBuiltinIgnored(t *testing.T) {
	dir := t.TempDir()
	b, _ := loadProfiles()
	m := newMapperDir(t, nil, dir)
	if err := m.SaveUserProfile("danbooru", b["danbooru"]); err != nil {
		t.Fatalf("SaveUserProfile: %v", err)
	}
	if m.CustomProfile("danbooru") {
		t.Error("a profile equal to the shipped one is not a customization")
	}
	if _, err := os.Stat(filepath.Join(dir, "danbooru.json")); !os.IsNotExist(err) {
		t.Error("an equal profile should delete the file, not write it")
	}
}

func TestSaveUserProfileValidatesAndReloads(t *testing.T) {
	dir := t.TempDir()
	m := newMapperDir(t, nil, dir)
	if err := m.SaveUserProfile("../evil", Profile{Family: FamilyGeneric}); err == nil {
		t.Error("a path-shaped site name must be refused")
	}
	if err := m.SaveUserProfile("newsite", Profile{Family: "nope"}); err == nil {
		t.Error("an unknown family must be refused")
	}
	if err := m.SaveUserProfile("newsite", Profile{Family: FamilyGeneric, PostURL: "https://x.example/posts"}); err == nil {
		t.Error("a post template without a placeholder must be refused")
	}
	if err := ValidateProfile(Profile{Family: FamilyGeneric, CategoryOverrides: map[string]string{"voice": "va", "x": ""}}); err != nil {
		t.Errorf("a custom category monbooru may not have listed yet must pass: %v", err)
	}
	for _, bad := range []string{"not a name", "VA", "rating"} {
		if err := ValidateProfile(Profile{Family: FamilyGeneric, CategoryOverrides: map[string]string{"voice": bad}}); err == nil {
			t.Errorf("category target %q must be refused", bad)
		}
	}
	if err := ValidateProfile(Profile{Family: FamilyGeneric, PostURL: "https://x.example/{author[handle]}/{post_id}"}); err != nil {
		t.Errorf("a post template naming gallery-dl keywords must pass: %v", err)
	}
	p := Profile{Family: FamilyGeneric, Kind: KindOther, Auth: AuthUsernamePassword,
		RatingOverrides: map[string]string{"e": RatingGeneral}}
	if err := m.SaveUserProfile("newgrounds", p); err != nil {
		t.Fatalf("SaveUserProfile: %v", err)
	}
	if !m.CustomProfile("newgrounds") {
		t.Error("the save should apply without a restart")
	}
	if got := m.ratingFor("newgrounds", m.profileFor("newgrounds"), "e"); got != RatingGeneral {
		t.Errorf("rating e = %q, want the user override applied", got)
	}
	if err := m.DeleteUserProfile("newgrounds"); err != nil {
		t.Fatalf("DeleteUserProfile: %v", err)
	}
	if m.CustomProfile("newgrounds") {
		t.Error("the delete should revert the category")
	}
}

func TestProfileSaveGuardsLookupChain(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	m := newMapperDir(t, cfg, dir)
	noMD5 := Profile{Family: FamilyDanbooru, Auth: AuthAPIOptional,
		PostURL: "https://danbooru.donmai.us/posts/{id}"}
	if err := m.SaveUserProfile("danbooru", noMD5); err == nil {
		t.Error("dropping the md5 template of a chained site must be refused")
	}
	writeProfile(t, dir, "szurubooru.json", `{"family":"generic","auth":"none","md5_search_template":"https://booru.example/posts?tags=md5:{md5}"}`)
	cfg.Sites = append(cfg.Sites, config.Site{Name: "szurubooru", LookupOrder: 9})
	m = newMapperDir(t, cfg, dir)
	if err := m.DeleteUserProfile("szurubooru"); err == nil {
		t.Error("resetting the profile of a chained user site must be refused")
	}
}

func TestValidateProfileOnTagRuleTargets(t *testing.T) {
	for to, ok := range map[string]bool{"general:y": true, "artist:y": true, "y": true, "": true, "general:": false, "Y Z": false} {
		err := ValidateProfile(Profile{Family: FamilyGeneric, TagRules: map[string]string{"artist:x": to}})
		if (err == nil) != ok {
			t.Errorf("target %q: err = %v", to, err)
		}
	}
}
