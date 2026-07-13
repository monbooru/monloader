package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// fullConfig returns a config whose every field holds a valid, non-default
// value so validate() is a no-op and a Save/Load round-trip must reproduce
// it exactly.
func fullConfig() *Config {
	return &Config{
		Server: ServerConfig{
			BindAddress: "0.0.0.0:9000",
			BaseURL:     "http://example:9000",
			CustomCSS:   "/config/custom.css",
		},
		Monbooru: MonbooruConfig{
			APIURL:         "http://mb:8080",
			APIToken:       "tok",
			DefaultGallery: "main",
		},
		Downloader: DownloaderConfig{
			Concurrency:    2,
			MaxItemsPerJob: 50,
			DefaultFolder:  "incoming",
		},
		GalleryDL: GalleryDLConfig{
			BinaryPath:   "/usr/bin/gallery-dl",
			ConfigPath:   "/config/gdl.json",
			ArchivePath:  "/config/archive.sqlite",
			CookiesDir:   "/config/cookies",
			SleepRequest: 2.5,
			RawConfig:    `{"extractor":{"timeout":30}}`,
		},
		Auth: AuthConfig{
			EnablePassword:      false,
			SessionLifetimeDays: 14,
			Tokens: []Token{
				{ID: "tok1", Name: "default", TokenHash: "abc123", Scopes: []string{ScopeRead, ScopeWrite}, CreatedAt: "2026-06-29T00:00:00Z"},
			},
		},
		Log: LogConfig{Level: "debug"},
		PTR: PTRConfig{
			Enabled: true, DataPath: "/data/ptr", Address: "https://ptr.example:45871",
			AccessKey: "deadbeef", FetchSleep: 2.0, MinFreeGB: 40,
		},
		Lookup: LookupConfig{
			MinSimilarity: 90,
			Iqdb:          LookupService{Order: 2},
			Saucenao:      LookupService{Order: 3, APIKey: "sn-key"},
		},
		Sites: []Site{
			{Name: "danbooru", LookupOrder: 1},
			{Name: "gelbooru", APIKey: "k", UserID: "u", Gallery: "art", LookupOrder: 2},
			{Name: "rule34", LookupOrder: 3},
			{Name: "e621", Username: "user", APIKey: "k2", Gallery: "furry"},
			{Name: "yandere", LookupOrder: 5},
			{Name: "konachan", LookupOrder: 6},
			{Name: "sankaku", Cookies: "/config/cookies/sankaku.txt"},
		},
		TagOverrides: []TagOverride{
			{Site: "e621", From: "species", To: "general"},
		},
		RatingOverrides: []RatingOverride{
			{Site: "danbooru", From: "s", To: "sensitive"},
		},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	want := fullConfig()
	if err := Save(want, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round-trip mismatch:\n want %+v\n got  %+v", want, got)
	}
}

// loadLegacy writes a raw pre-similarity config (no [lookup] section - Save
// would add one) and loads it, so the seeding tests exercise a real upgrade.
func loadLegacy(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monloader.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return got
}

func TestLoadSeedsLookupDefaultsForExistingInstall(t *testing.T) {
	// An existing config that predates hash lookup: a gelbooru block with a key
	// (no order), and no other lookup sites.
	got := loadLegacy(t, "[[sites]]\nname = \"gelbooru\"\napi_key = \"k\"\n")
	// The absent defaults are seeded with their chain positions...
	for name, order := range map[string]int{"danbooru": 1, "rule34": 5, "e621": 6, "yandere": 7, "konachan": 8} {
		if site := got.FindSite(name); site == nil || site.LookupOrder != order {
			t.Errorf("%s should be seeded at order %d, got %+v", name, order, site)
		}
	}
	// ...but the existing gelbooru block keeps its key and its (unset) order.
	if g := got.FindSite("gelbooru"); g.APIKey != "k" || g.LookupOrder != 0 {
		t.Errorf("an existing site block must not be clobbered by the seed, got %+v", g)
	}
	// The similarity services slot in behind danbooru.
	if got.Lookup.Iqdb.Order != 2 || got.Lookup.Saucenao.Order != 3 {
		t.Errorf("services should be seeded at 2/3, got %+v", got.Lookup)
	}

	// A config that already names danbooru (here with the order cleared) keeps
	// its choice - the seed does not re-add or renumber it.
	got = loadLegacy(t, "[[sites]]\nname = \"danbooru\"\nlookup_order = 0\n")
	if got.FindSite("danbooru").LookupOrder != 0 {
		t.Errorf("a config that cleared danbooru's order should be respected, got %+v", got.Sites)
	}
}

func TestSeedRenumbersUncustomizedWalk(t *testing.T) {
	// A pre-similarity file holding the full old walk (danbooru..konachan at
	// 1-6) was never customized, so the upgrade renumbers it into the current
	// chain instead of appending the services after it.
	var body strings.Builder
	for name, order := range legacyLookupOrders {
		fmt.Fprintf(&body, "[[sites]]\nname = %q\nlookup_order = %d\n", name, order)
	}
	got := loadLegacy(t, body.String())
	for _, d := range defaultLookupSites {
		if s := got.FindSite(d.Name); s == nil || s.LookupOrder != d.LookupOrder {
			t.Errorf("%s should be renumbered to %d, got %+v", d.Name, d.LookupOrder, s)
		}
	}
	if got.Lookup.Iqdb.Order != 2 || got.Lookup.Saucenao.Order != 3 {
		t.Errorf("services should be seeded at 2/3, got %+v", got.Lookup)
	}
}

func TestSeedAppendsServicesAfterCustomWalk(t *testing.T) {
	// An operator-arranged walk keeps its orders; the services land at the end
	// for the operator to reorder.
	got := loadLegacy(t, "[[sites]]\nname = \"danbooru\"\nlookup_order = 9\n")
	if got.FindSite("danbooru").LookupOrder != 9 {
		t.Errorf("a custom order must not be renumbered, got %+v", got.FindSite("danbooru"))
	}
	if got.Lookup.Iqdb.Order != 10 || got.Lookup.Saucenao.Order != 11 {
		t.Errorf("services should append after the highest order, got %+v", got.Lookup)
	}
}

func TestSeedLeavesPresentLookupSectionAlone(t *testing.T) {
	// A [lookup] section with the services switched off is a deliberate choice;
	// re-seeding would resurrect them on every load.
	got := loadLegacy(t, "[lookup]\nmin_similarity = 70\n[lookup.iqdb]\norder = 0\n[lookup.saucenao]\norder = 0\n")
	if got.Lookup.Iqdb.Order != 0 || got.Lookup.Saucenao.Order != 0 {
		t.Errorf("cleared services must stay cleared, got %+v", got.Lookup)
	}
	if got.Lookup.MinSimilarity != 70 {
		t.Errorf("min_similarity = %d, want the file's 70", got.Lookup.MinSimilarity)
	}
}

func TestFirstRunWritesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	if _, err := Load(path); err != nil {
		t.Fatalf("Load (first run): %v", err)
	}
	// Read the file straight back (bypassing env overrides) and confirm it
	// holds the defaults.
	var onDisk Config
	if _, err := toml.DecodeFile(path, &onDisk); err != nil {
		t.Fatalf("decode written defaults: %v", err)
	}
	def := Default()
	if !reflect.DeepEqual(*def, onDisk) {
		t.Errorf("written defaults differ from Default():\n want %+v\n got  %+v", *def, onDisk)
	}
	// Spot-check a few key default values explicitly.
	if onDisk.Server.BindAddress != "0.0.0.0:8081" {
		t.Errorf("bind_address = %q, want 0.0.0.0:8081", onDisk.Server.BindAddress)
	}
	if onDisk.Downloader.Concurrency != 1 || onDisk.Downloader.MaxItemsPerJob != 200 {
		t.Errorf("downloader defaults wrong: %+v", onDisk.Downloader)
	}
	if onDisk.GalleryDL.SleepRequest != 1.0 {
		t.Errorf("sleep_request = %v, want 1.0", onDisk.GalleryDL.SleepRequest)
	}
	if onDisk.Log.Level != "warn" {
		t.Errorf("log.level = %q, want warn", onDisk.Log.Level)
	}
	if len(onDisk.Sites) != len(defaultLookupSites) || onDisk.Sites[0].Name != "danbooru" || onDisk.Sites[0].LookupOrder != 1 {
		t.Errorf("default sites = %+v, want the seeded lookup defaults led by danbooru", onDisk.Sites)
	}
	if onDisk.Lookup.MinSimilarity != 80 || onDisk.Lookup.Iqdb.Order != 2 || onDisk.Lookup.Saucenao.Order != 3 {
		t.Errorf("default lookup = %+v, want min 80 with iqdb/saucenao at 2/3", onDisk.Lookup)
	}
}

func TestEnvOverrideBeatsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	cfg := Default()
	cfg.Monbooru.APIToken = "from-file"
	cfg.Downloader.Concurrency = 1
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("MONLOADER_MONBOORU_API_TOKEN", "from-env")
	t.Setenv("MONLOADER_DOWNLOADER_CONCURRENCY", "4")
	t.Setenv("MONLOADER_LOG_LEVEL", "debug")

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Monbooru.APIToken != "from-env" {
		t.Errorf("APIToken = %q, want from-env", got.Monbooru.APIToken)
	}
	if got.Downloader.Concurrency != 4 {
		t.Errorf("Concurrency = %d, want 4", got.Downloader.Concurrency)
	}
	if got.Log.Level != "debug" {
		t.Errorf("Log.Level = %q, want debug", got.Log.Level)
	}
}

func TestValidateRawConfig(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{"   ", false},
		{"{}", false},
		{`{"extractor":{"timeout":30}}`, false},
		{"{bad json", true},
		{"[]", true},  // an array is not a mergeable config object
		{"123", true}, // a bare scalar is not an object
	}
	for _, tc := range cases {
		err := ValidateRawConfig(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateRawConfig(%q) err = %v, wantErr = %v", tc.in, err, tc.wantErr)
		}
	}
}

func TestValidateSnapsAndRejects(t *testing.T) {
	// Snaps: non-positive worker count, negative sleep, and a zero session
	// lifetime are coerced to sane values rather than failing.
	cfg := Default()
	cfg.Downloader.Concurrency = 0
	cfg.Downloader.MaxItemsPerJob = -5
	cfg.GalleryDL.SleepRequest = -1
	cfg.Auth.SessionLifetimeDays = 0
	if err := validate(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Downloader.Concurrency != 1 {
		t.Errorf("Concurrency snapped to %d, want 1", cfg.Downloader.Concurrency)
	}
	if cfg.Downloader.MaxItemsPerJob != 200 {
		t.Errorf("MaxItemsPerJob snapped to %d, want 200", cfg.Downloader.MaxItemsPerJob)
	}
	if cfg.GalleryDL.SleepRequest != 0 {
		t.Errorf("SleepRequest snapped to %v, want 0", cfg.GalleryDL.SleepRequest)
	}
	if cfg.Auth.SessionLifetimeDays != 7 {
		t.Errorf("SessionLifetimeDays snapped to %d, want 7", cfg.Auth.SessionLifetimeDays)
	}
	cfg.Lookup.MinSimilarity = 150
	cfg.Lookup.Iqdb.Order = -1
	if err := validate(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Lookup.MinSimilarity != 80 {
		t.Errorf("MinSimilarity snapped to %d, want 80", cfg.Lookup.MinSimilarity)
	}
	if cfg.Lookup.Iqdb.Order != 0 {
		t.Errorf("a negative service order snapped to %d, want 0", cfg.Lookup.Iqdb.Order)
	}

	// Rejects: a missing host:port, and enable_password without a hash.
	bad := Default()
	bad.Server.BindAddress = "noport"
	if err := validate(bad); err == nil {
		t.Error("expected error for bind_address without a colon")
	}
	bad = Default()
	bad.Auth.EnablePassword = true
	if err := validate(bad); err == nil {
		t.Error("expected error for enable_password with empty hash")
	}
	bad.Auth.PasswordHash = "$2a$10$abcdefghijklmnopqrstuv"
	if err := validate(bad); err != nil {
		t.Errorf("a bcrypt-shaped hash should pass: %v", err)
	}
}

func TestAllEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	if err := Save(Default(), path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	env := map[string]string{
		"MONLOADER_SERVER_BIND_ADDRESS":          "1.2.3.4:7000",
		"MONLOADER_SERVER_BASE_URL":              "http://h:7000",
		"MONLOADER_MONBOORU_API_URL":             "http://mb:1234",
		"MONLOADER_MONBOORU_API_TOKEN":           "t",
		"MONLOADER_MONBOORU_DEFAULT_GALLERY":     "g",
		"MONLOADER_DOWNLOADER_CONCURRENCY":       "3",
		"MONLOADER_DOWNLOADER_MAX_ITEMS_PER_JOB": "77",
		"MONLOADER_DOWNLOADER_DEFAULT_FOLDER":    "f",
		"MONLOADER_GALLERYDL_BINARY_PATH":        "/b/gdl",
		"MONLOADER_GALLERYDL_CONFIG_PATH":        "/c/gdl.json",
		"MONLOADER_GALLERYDL_ARCHIVE_PATH":       "/c/a.sqlite",
		"MONLOADER_GALLERYDL_COOKIES_DIR":        "/c/cookies",
		"MONLOADER_GALLERYDL_SLEEP_REQUEST":      "3.5",
		"MONLOADER_AUTH_ENABLE_PASSWORD":         "false",
		"MONLOADER_AUTH_PASSWORD_HASH":           "$2b$10$xxxxxxxxxxxxxxxxxxxxxx",
		"MONLOADER_LOG_LEVEL":                    "info",
		"MONLOADER_PTR_ENABLED":                  "true",
		"MONLOADER_PTR_DATA_PATH":                "/mnt/ptr",
		"MONLOADER_PTR_ADDRESS":                  "https://ptr.test:45871",
		"MONLOADER_PTR_ACCESS_KEY":               "abc123",
		"MONLOADER_PTR_FETCH_SLEEP":              "2.5",
		"MONLOADER_PTR_MIN_FREE_GB":              "80",
		"MONLOADER_LOOKUP_SAUCENAO_API_KEY":      "sn",
		"MONLOADER_LOOKUP_MIN_SIMILARITY":        "65",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"bind", got.Server.BindAddress, "1.2.3.4:7000"},
		{"baseurl", got.Server.BaseURL, "http://h:7000"},
		{"apiurl", got.Monbooru.APIURL, "http://mb:1234"},
		{"apitoken", got.Monbooru.APIToken, "t"},
		{"gallery", got.Monbooru.DefaultGallery, "g"},
		{"conc", got.Downloader.Concurrency, 3},
		{"maxitems", got.Downloader.MaxItemsPerJob, 77},
		{"folder", got.Downloader.DefaultFolder, "f"},
		{"binpath", got.GalleryDL.BinaryPath, "/b/gdl"},
		{"cfgpath", got.GalleryDL.ConfigPath, "/c/gdl.json"},
		{"archive", got.GalleryDL.ArchivePath, "/c/a.sqlite"},
		{"cookies", got.GalleryDL.CookiesDir, "/c/cookies"},
		{"sleep", got.GalleryDL.SleepRequest, 3.5},
		{"loglevel", got.Log.Level, "info"},
		{"ptr_enabled", got.PTR.Enabled, true},
		{"ptr_data", got.PTR.DataPath, "/mnt/ptr"},
		{"ptr_addr", got.PTR.Address, "https://ptr.test:45871"},
		{"ptr_key", got.PTR.AccessKey, "abc123"},
		{"ptr_sleep", got.PTR.FetchSleep, 2.5},
		{"ptr_minfree", got.PTR.MinFreeGB, 80},
		{"saucenao_key", got.Lookup.Saucenao.APIKey, "sn"},
		{"min_similarity", got.Lookup.MinSimilarity, 65},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestEnvOverrideIgnoresUnparseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	if err := Save(Default(), path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("MONLOADER_DOWNLOADER_CONCURRENCY", "notanint")
	t.Setenv("MONLOADER_GALLERYDL_SLEEP_REQUEST", "notafloat")
	t.Setenv("MONLOADER_AUTH_ENABLE_PASSWORD", "notabool")
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Unparseable values are ignored, leaving the file/default values intact.
	if got.Downloader.Concurrency != 1 {
		t.Errorf("Concurrency = %d, want default 1", got.Downloader.Concurrency)
	}
	if got.GalleryDL.SleepRequest != 1.0 {
		t.Errorf("SleepRequest = %v, want default 1.0", got.GalleryDL.SleepRequest)
	}
}

func TestLoadRejectsMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	if err := os.WriteFile(path, []byte("this is = not valid = toml ]["), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("expected a parse error for malformed TOML")
	}
}

func TestProviderAndClone(t *testing.T) {
	cfg := Default()
	cfg.Sites = []Site{{Name: "gelbooru", APIKey: "k"}}
	cfg.TagOverrides = []TagOverride{{Site: "x", From: "a", To: "b"}}
	cfg.Auth.Tokens = []Token{{ID: "1", Name: "a", Scopes: []string{ScopeRead}}}

	p := NewProvider(cfg)
	if p.Current() != cfg {
		t.Fatal("Current should return the stored config")
	}

	// Clone is an independent deep copy: mutating it must not touch the original.
	clone := cfg.Clone()
	clone.Sites[0].APIKey = "changed"
	clone.TagOverrides = append(clone.TagOverrides, TagOverride{Site: "y"})
	clone.Monbooru.APIToken = "tok"
	clone.Auth.Tokens[0].Name = "changed"
	if cfg.Sites[0].APIKey != "k" || len(cfg.TagOverrides) != 1 || cfg.Monbooru.APIToken != "" || cfg.Auth.Tokens[0].Name != "a" {
		t.Error("Clone aliased the original")
	}

	// Store publishes the new snapshot.
	p.Store(clone)
	if p.Current() != clone || p.Current().Monbooru.APIToken != "tok" {
		t.Error("Store did not publish the new snapshot")
	}
}

func TestTokenModelCRUD(t *testing.T) {
	cfg := Default()
	tok, secret := GenerateToken("ci", []string{ScopeRead})
	if len(secret) != 32 || tok.TokenHash != HashToken(secret) || tok.TokenHash == secret {
		t.Fatal("GenerateToken must hash the secret and return 32 hex chars")
	}
	cfg.Auth.Tokens = append(cfg.Auth.Tokens, tok)
	if !cfg.TokenNameExists("CI") {
		t.Error("TokenNameExists should be case-insensitive")
	}
	if got := cfg.FindTokenByHash(HashToken(secret)); got == nil || !got.HasScope(ScopeRead) || got.HasScope(ScopeWrite) {
		t.Errorf("lookup/scope mismatch: %+v", got)
	}
	if !cfg.SetTokenScopes(tok.ID, AllScopes) || !cfg.FindTokenByHash(HashToken(secret)).HasScope(ScopeWrite) {
		t.Error("SetTokenScopes did not apply")
	}
	if !cfg.RemoveToken(tok.ID) || len(cfg.Auth.Tokens) != 0 {
		t.Error("RemoveToken did not drop the token")
	}
}

func TestValidateTokenNameReserved(t *testing.T) {
	if err := ValidateTokenName("monbooru"); err != nil {
		t.Errorf("plain name rejected: %v", err)
	}
	for _, bad := range []string{"", "  ", "monbooru (paired)", "X (PAIRED)"} {
		if err := ValidateTokenName(bad); err == nil {
			t.Errorf("ValidateTokenName(%q) = nil, want error", bad)
		}
	}
}

func TestLoadFromFileIgnoresEnvAndSavePreservesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	base := Default()
	base.Monbooru.APIURL = "http://file:8080"
	if err := Save(base, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// An ephemeral env secret is part of the effective config but not the file.
	t.Setenv("MONLOADER_MONBOORU_API_TOKEN", "env-secret")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Monbooru.APIToken != "env-secret" {
		t.Fatalf("effective config should carry the env token, got %q", cfg.Monbooru.APIToken)
	}
	if fileCfg, err := LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	} else if fileCfg.Monbooru.APIToken != "" {
		t.Errorf("LoadFromFile should not apply the env token, got %q", fileCfg.Monbooru.APIToken)
	}

	// A settings save persists the file layer plus the change, never the env.
	persist, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	persist.Monbooru.DefaultGallery = "newgallery"
	if err := Save(persist, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	onDisk, err := LoadFromFile(path)
	if err != nil {
		t.Fatalf("LoadFromFile: %v", err)
	}
	if onDisk.Monbooru.APIToken != "" {
		t.Errorf("env token leaked to disk: %q", onDisk.Monbooru.APIToken)
	}
	if onDisk.Monbooru.DefaultGallery != "newgallery" {
		t.Errorf("the saved field was lost: %q", onDisk.Monbooru.DefaultGallery)
	}
	if onDisk.Monbooru.APIURL != "http://file:8080" {
		t.Errorf("the file's api_url should be preserved: %q", onDisk.Monbooru.APIURL)
	}
}

func TestSaveRejectsUnwritableDir(t *testing.T) {
	// A path whose parent component is a regular file makes MkdirAll fail,
	// exercising Save's directory-creation error branch.
	base := t.TempDir()
	notDir := filepath.Join(base, "afile")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Save(Default(), filepath.Join(notDir, "sub", "monloader.toml")); err == nil {
		t.Error("expected Save to fail when a path component is a file")
	}
}

func TestValidateRejectsNonBcryptHash(t *testing.T) {
	cfg := Default()
	cfg.Auth.EnablePassword = true
	cfg.Auth.PasswordHash = "plaintext-not-bcrypt"
	if err := validate(cfg); err == nil {
		t.Error("expected error for a non-bcrypt password_hash")
	}
}

func TestFindSite(t *testing.T) {
	cfg := fullConfig()
	if s := cfg.FindSite("e621"); s == nil || s.Gallery != "furry" {
		t.Errorf("FindSite(e621) = %+v, want the furry block", s)
	}
	if cfg.FindSite("nope") != nil {
		t.Error("FindSite of an unknown site should be nil")
	}
}
