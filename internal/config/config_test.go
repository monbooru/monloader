package config

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/monbooru/monloader/internal/shared/apitoken"
)

func fullConfig() *Config {
	return &Config{
		Server: ServerConfig{
			BindAddress: "0.0.0.0:9000",
			BaseURL:     "http://example:9000",
			Theme:       "light",
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
			Tokens: apitoken.List{
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
	mustSave(t, want, path)
	got := mustLoad(t, path)
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round-trip mismatch:\n want %+v\n got  %+v", want, got)
	}
}

func mustLoad(t *testing.T, path string) *Config {
	t.Helper()
	got, err := Load(path, "", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return got
}

func mustSave(t *testing.T, cfg *Config, path string) {
	t.Helper()
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func loadLegacy(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "monloader.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return mustLoad(t, path)
}

func TestLoadSeedsLookupDefaultsForExistingInstall(t *testing.T) {
	got := loadLegacy(t, "[[sites]]\nname = \"gelbooru\"\napi_key = \"k\"\n")
	for name, order := range map[string]int{"danbooru": 1, "rule34": 5, "e621": 6, "yandere": 7, "konachan": 8} {
		if site := got.FindSite(name); site == nil || site.LookupOrder != order {
			t.Errorf("%s should be seeded at order %d, got %+v", name, order, site)
		}
	}
	if g := got.FindSite("gelbooru"); g.APIKey != "k" || g.LookupOrder != 0 {
		t.Errorf("an existing site block must not be clobbered by the seed, got %+v", g)
	}
	if got.Lookup.Iqdb.Order != 2 || got.Lookup.Saucenao.Order != 3 {
		t.Errorf("services should be seeded at 2/3, got %+v", got.Lookup)
	}

	got = loadLegacy(t, "[[sites]]\nname = \"danbooru\"\nlookup_order = 0\n")
	if got.FindSite("danbooru").LookupOrder != 0 {
		t.Errorf("a config that cleared danbooru's order should be respected, got %+v", got.Sites)
	}
}

func TestSeedRenumbersUncustomizedWalk(t *testing.T) {
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
	got := loadLegacy(t, "[[sites]]\nname = \"danbooru\"\nlookup_order = 9\n")
	if got.FindSite("danbooru").LookupOrder != 9 {
		t.Errorf("a custom order must not be renumbered, got %+v", got.FindSite("danbooru"))
	}
	if got.Lookup.Iqdb.Order != 10 || got.Lookup.Saucenao.Order != 11 {
		t.Errorf("services should append after the highest order, got %+v", got.Lookup)
	}
}

func TestSeedLeavesPresentLookupSectionAlone(t *testing.T) {
	got := loadLegacy(t, "[lookup]\nmin_similarity = 70\n[lookup.iqdb]\norder = 0\n[lookup.saucenao]\norder = 0\n")
	if got.Lookup.Iqdb.Order != 0 || got.Lookup.Saucenao.Order != 0 {
		t.Errorf("cleared services must stay cleared, got %+v", got.Lookup)
	}
	if got.Lookup.MinSimilarity != 70 {
		t.Errorf("min_similarity = %d, want the file's 70", got.Lookup.MinSimilarity)
	}
}

func TestSeedFillsScheduledLookupBudgetOnUpgrade(t *testing.T) {
	got := loadLegacy(t, "[lookup]\nmin_similarity = 70\n")
	if got.Lookup.ScheduledDailyBudget != defaultScheduledDailyBudget {
		t.Errorf("lookup = %+v, want the budget seeded", got.Lookup)
	}
	got = loadLegacy(t, "[lookup]\nscheduled_daily_budget = 0\n")
	if got.Lookup.ScheduledDailyBudget != 0 {
		t.Errorf("lookup = %+v, want the file's own value kept", got.Lookup)
	}
}

func TestUpgradeInheritsSuccessRetention(t *testing.T) {
	got := loadLegacy(t, "[downloader]\nhistory_retention_days = 7\n")
	if got.Downloader.SuccessRetentionDays != 3 {
		t.Errorf("success_retention_days = %d, want the fresh-install 3 on a config that predates it", got.Downloader.SuccessRetentionDays)
	}
	got = loadLegacy(t, "[downloader]\nsuccess_retention_days = 0\n")
	if got.Downloader.SuccessRetentionDays != 0 {
		t.Errorf("success_retention_days = %d, want the file's own 0 kept", got.Downloader.SuccessRetentionDays)
	}
}

func TestFirstRunWritesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	if _, err := Load(path, "", nil); err != nil {
		t.Fatalf("Load (first run): %v", err)
	}
	var onDisk Config
	if _, err := toml.DecodeFile(path, &onDisk); err != nil {
		t.Fatalf("decode written defaults: %v", err)
	}
	def := Default()
	if !reflect.DeepEqual(*def, onDisk) {
		t.Errorf("written defaults differ from Default():\n want %+v\n got  %+v", *def, onDisk)
	}
	if onDisk.Server.BindAddress != "0.0.0.0:8456" {
		t.Errorf("bind_address = %q, want 0.0.0.0:8456", onDisk.Server.BindAddress)
	}
	if onDisk.Downloader.Concurrency != 1 || onDisk.Downloader.MaxItemsPerJob != 200 ||
		onDisk.Downloader.SuccessRetentionDays != 3 || onDisk.Downloader.HistoryRetentionDays != 14 {
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
	mustSave(t, cfg, path)

	t.Setenv("MONLOADER_MONBOORU_API_TOKEN", "from-env")
	t.Setenv("MONLOADER_DOWNLOADER_CONCURRENCY", "4")
	t.Setenv("MONLOADER_LOG_LEVEL", "debug")

	got := mustLoad(t, path)
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
		{"[]", true},
		{"123", true},
	}
	for _, tc := range cases {
		err := ValidateRawConfig(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateRawConfig(%q) err = %v, wantErr = %v", tc.in, err, tc.wantErr)
		}
	}
}

func TestRedactSecretsCoversOptionsRawConfigAndLookupKeys(t *testing.T) {
	cfg := Default()
	cfg.Sites = []Site{{Name: "pixiv", Options: `{"refresh-token":"optionsecret","nested":{"list":["nestedsecret","abc"]},"retries":3}`}}
	cfg.GalleryDL.RawConfig = `{"extractor":{"cookies":{"sid":"rawsecret1"}}}`
	cfg.Lookup.Saucenao.APIKey = "saucesecret"
	got, _ := RedactSecrets("optionsecret nestedsecret rawsecret1 saucesecret abc", cfg.Secrets())
	for _, leak := range []string{"optionsecret", "nestedsecret", "rawsecret1", "saucesecret"} {
		if strings.Contains(got, leak) {
			t.Errorf("redacted = %q, still carries %s", got, leak)
		}
	}
	if !strings.Contains(got, "abc") {
		t.Errorf("redacted = %q, want a value under the length floor left alone", got)
	}
	cfg.Sites[0].Options = "{not json"
	cfg.Secrets()
}

func TestRedactSecrets(t *testing.T) {
	cfg := Default()
	cfg.Monbooru.APIToken = "monbooru-token-value"
	cfg.PTR.AccessKey = "ptr-access-key-value"
	cfg.Sites = []Site{{Name: "gelbooru", APIKey: "SECRETKEY123", UserID: "42"}}

	line := `[urllib3][debug] "GET /index.php?api_key=SECRETKEY123&user_id=42" 401`
	got, n := RedactSecrets(line, cfg.Secrets())
	if strings.Contains(got, "SECRETKEY123") {
		t.Errorf("redacted = %q, still carries the api key", got)
	}
	if !strings.Contains(got, "<redacted:gelbooru.api_key>") {
		t.Errorf("redacted = %q, want the marker naming the key", got)
	}
	if !strings.Contains(got, "user_id=42") {
		t.Errorf("redacted = %q, want the short user id untouched", got)
	}
	if n != 1 {
		t.Errorf("removed = %d, want 1", n)
	}

	if _, n := RedactSecrets("nothing sensitive here", cfg.Secrets()); n != 0 {
		t.Errorf("removed = %d on clean text, want 0", n)
	}
	if got, _ := RedactSecrets("token monbooru-token-value", cfg.Secrets()); strings.Contains(got, "monbooru-token-value") {
		t.Errorf("redacted = %q, still carries the monbooru token", got)
	}
}

func TestValidateSnapsAndRejects(t *testing.T) {
	cfg := Default()
	cfg.Downloader.Concurrency = 0
	cfg.Downloader.MaxItemsPerJob = -5
	cfg.Downloader.SuccessRetentionDays = -1
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
	if cfg.Downloader.SuccessRetentionDays != 0 {
		t.Errorf("SuccessRetentionDays snapped to %d, want 0", cfg.Downloader.SuccessRetentionDays)
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
	for _, raw := range []string{"http://iqdb.test/q", "/iqdb", "https://"} {
		bad = Default()
		bad.Lookup.Iqdb.URL = raw
		if err := validate(bad); err == nil || !strings.Contains(err.Error(), "lookup.iqdb.url") {
			t.Errorf("lookup.iqdb.url %q: err = %v, want a refusal naming the key", raw, err)
		}
	}
	bad = Default()
	bad.Sites = []Site{{Name: "rule34", Agent: "browser"}}
	if err := validate(bad); err == nil || !strings.Contains(err.Error(), "sites.rule34.user_agent") {
		t.Errorf("a browser agent: err = %v, want a refusal naming the key", err)
	}
}

func TestValidateSiteAgent(t *testing.T) {
	for _, ok := range []string{"", "my agent/1.0", "monloader; user #123", "+firefox", "+chrome", "+gallery-dl"} {
		if err := ValidateSiteAgent(ok); err != nil {
			t.Errorf("%q: %v, want it accepted", ok, err)
		}
	}
	for _, bad := range []string{"browser", "auto", "@firefox", "+ff", "+google-bot", "+Firefox", "h\u00e9llo", "\u30e6\u30fc\u30b6\u30fc", "a\nb"} {
		if err := ValidateSiteAgent(bad); err == nil {
			t.Errorf("%q was accepted, want it refused", bad)
		}
	}
}

func TestSiteAgents(t *testing.T) {
	for _, tc := range []struct {
		name          string
		site          Site
		agent, header string
	}{
		{"nothing set", Site{}, "", ""},
		{"account", Site{UserID: "123"}, "monloader; user #123", "monloader; user #123"},
		{"literal beats the account", Site{UserID: "123", Agent: "mine"}, "mine", "mine"},
		{"preset, account", Site{UserID: "123", Agent: "+firefox"}, "+firefox", "monloader; user #123"},
		{"preset alone", Site{Agent: "+firefox"}, "+firefox", ""},
	} {
		if got := tc.site.UserAgent(); got != tc.agent {
			t.Errorf("%s: UserAgent = %q, want %q", tc.name, got, tc.agent)
		}
		if got := tc.site.HeaderAgent(); got != tc.header {
			t.Errorf("%s: HeaderAgent = %q, want %q", tc.name, got, tc.header)
		}
	}
}

func TestAllEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	mustSave(t, Default(), path)
	env := map[string]string{
		"MONLOADER_SERVER_BIND_ADDRESS":               "1.2.3.4:7000",
		"MONLOADER_SERVER_BASE_URL":                   "http://h:7000",
		"MONLOADER_MONBOORU_API_URL":                  "http://mb:1234",
		"MONLOADER_MONBOORU_API_TOKEN":                "t",
		"MONLOADER_MONBOORU_DEFAULT_GALLERY":          "g",
		"MONLOADER_MONBOORU_WEB_URL":                  "http://mb.local",
		"MONLOADER_DOWNLOADER_CONCURRENCY":            "3",
		"MONLOADER_DOWNLOADER_MAX_ITEMS_PER_JOB":      "77",
		"MONLOADER_DOWNLOADER_DEFAULT_FOLDER":         "f",
		"MONLOADER_DOWNLOADER_HISTORY_RETENTION_DAYS": "9",
		"MONLOADER_DOWNLOADER_SUCCESS_RETENTION_DAYS": "4",
		"MONLOADER_GALLERYDL_BINARY_PATH":             "/b/gdl",
		"MONLOADER_GALLERYDL_CONFIG_PATH":             "/c/gdl.json",
		"MONLOADER_GALLERYDL_ARCHIVE_PATH":            "/c/a.sqlite",
		"MONLOADER_GALLERYDL_COOKIES_DIR":             "/c/cookies",
		"MONLOADER_GALLERYDL_SUPPORTEDSITES_PATH":     "/c/supportedsites.md",
		"MONLOADER_GALLERYDL_SLEEP_REQUEST":           "3.5",
		"MONLOADER_AUTH_ENABLE_PASSWORD":              "true",
		"MONLOADER_AUTH_PASSWORD_HASH":                "$2b$10$xxxxxxxxxxxxxxxxxxxxxx",
		"MONLOADER_LOG_LEVEL":                         "info",
		"MONLOADER_PTR_ENABLED":                       "true",
		"MONLOADER_PTR_DATA_PATH":                     "/mnt/ptr",
		"MONLOADER_PTR_ADDRESS":                       "https://ptr.test:45871",
		"MONLOADER_PTR_ACCESS_KEY":                    "abc123",
		"MONLOADER_PTR_FETCH_SLEEP":                   "2.5",
		"MONLOADER_PTR_COMMIT_SLEEP":                  "4.5",
		"MONLOADER_PTR_MIN_FREE_GB":                   "80",
		"MONLOADER_LOOKUP_SAUCENAO_API_KEY":           "sn",
		"MONLOADER_LOOKUP_IQDB_URL":                   "https://iqdb.test/q.json",
		"MONLOADER_LOOKUP_SAUCENAO_URL":               "https://sn.test/search.php",
		"MONLOADER_LOOKUP_MIN_SIMILARITY":             "65",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	got := mustLoad(t, path)
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
		{"weburl", got.Monbooru.WebURL, "http://mb.local"},
		{"conc", got.Downloader.Concurrency, 3},
		{"maxitems", got.Downloader.MaxItemsPerJob, 77},
		{"folder", got.Downloader.DefaultFolder, "f"},
		{"history_retention", got.Downloader.HistoryRetentionDays, 9},
		{"success_retention", got.Downloader.SuccessRetentionDays, 4},
		{"binpath", got.GalleryDL.BinaryPath, "/b/gdl"},
		{"cfgpath", got.GalleryDL.ConfigPath, "/c/gdl.json"},
		{"archive", got.GalleryDL.ArchivePath, "/c/a.sqlite"},
		{"cookies", got.GalleryDL.CookiesDir, "/c/cookies"},
		{"supportedsites", got.GalleryDL.SupportedSitesPath, "/c/supportedsites.md"},
		{"sleep", got.GalleryDL.SleepRequest, 3.5},
		{"loglevel", got.Log.Level, "info"},

		{"auth_enabled", got.Auth.EnablePassword, true},
		{"auth_hash", got.Auth.PasswordHash, "$2b$10$xxxxxxxxxxxxxxxxxxxxxx"},
		{"ptr_enabled", got.PTR.Enabled, true},
		{"ptr_data", got.PTR.DataPath, "/mnt/ptr"},
		{"ptr_addr", got.PTR.Address, "https://ptr.test:45871"},
		{"ptr_key", got.PTR.AccessKey, "abc123"},
		{"ptr_sleep", got.PTR.FetchSleep, 2.5},
		{"ptr_commit_sleep", got.PTR.CommitSleep, 4.5},
		{"ptr_minfree", got.PTR.MinFreeGB, 80},
		{"saucenao_key", got.Lookup.Saucenao.APIKey, "sn"},
		{"iqdb_url", got.Lookup.Iqdb.URL, "https://iqdb.test/q.json"},
		{"saucenao_url", got.Lookup.Saucenao.URL, "https://sn.test/search.php"},
		{"min_similarity", got.Lookup.MinSimilarity, 65},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`MONLOADER_[A-Z_]+`).FindAllString(configSource(t), -1) {
		declared[m] = true
	}
	for name := range declared {
		if _, ok := env[name]; !ok {
			t.Errorf("%s is declared in config.go but not exercised here", name)
		}
	}
}

func configSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEnvOverrideIgnoresUnparseable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	mustSave(t, Default(), path)
	t.Setenv("MONLOADER_DOWNLOADER_CONCURRENCY", "notanint")
	t.Setenv("MONLOADER_GALLERYDL_SLEEP_REQUEST", "notafloat")
	t.Setenv("MONLOADER_AUTH_ENABLE_PASSWORD", "notabool")
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)
	got := mustLoad(t, path)
	for _, env := range []string{"MONLOADER_DOWNLOADER_CONCURRENCY", "MONLOADER_GALLERYDL_SLEEP_REQUEST", "MONLOADER_AUTH_ENABLE_PASSWORD"} {
		if !strings.Contains(logBuf.String(), "ignoring "+env) {
			t.Errorf("no warning names %s:\n%s", env, logBuf.String())
		}
	}
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
	if _, err := Load(path, "", nil); err == nil {
		t.Error("expected a parse error for malformed TOML")
	}
}

func TestProviderAndClone(t *testing.T) {
	cfg := Default()
	cfg.Sites = []Site{{Name: "gelbooru", APIKey: "k"}}
	cfg.TagOverrides = []TagOverride{{Site: "x", From: "a", To: "b"}}
	cfg.Auth.Tokens = apitoken.List{{ID: "1", Name: "a", Scopes: []string{ScopeRead}}}

	p := NewProvider(cfg)
	if p.Current() != cfg {
		t.Fatal("Current should return the stored config")
	}

	clone := cfg.Clone()
	clone.Sites[0].APIKey = "changed"
	clone.TagOverrides = append(clone.TagOverrides, TagOverride{Site: "y"})
	clone.Monbooru.APIToken = "tok"
	clone.Auth.Tokens[0].Name = "changed"
	clone.Auth.Tokens[0].Scopes[0] = ScopeWrite
	if cfg.Sites[0].APIKey != "k" || len(cfg.TagOverrides) != 1 || cfg.Monbooru.APIToken != "" || cfg.Auth.Tokens[0].Name != "a" {
		t.Error("Clone aliased the original")
	}
	if cfg.Auth.Tokens[0].Scopes[0] != ScopeRead {
		t.Error("Clone shared the token's scopes slice with the original")
	}

	p.Store(clone)
	if p.Current() != clone || p.Current().Monbooru.APIToken != "tok" {
		t.Error("Store did not publish the new snapshot")
	}
}

func TestLoadFromFileIgnoresEnvAndSavePreservesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	base := Default()
	base.Monbooru.APIURL = "http://file:8080"
	mustSave(t, base, path)

	t.Setenv("MONLOADER_MONBOORU_API_TOKEN", "env-secret")
	cfg := mustLoad(t, path)
	if cfg.Monbooru.APIToken != "env-secret" {
		t.Fatalf("effective config should carry the env token, got %q", cfg.Monbooru.APIToken)
	}
	if fileCfg, err := LoadFromFileIn(path, ""); err != nil {
		t.Fatalf("LoadFromFileIn: %v", err)
	} else if fileCfg.Monbooru.APIToken != "" {
		t.Errorf("LoadFromFileIn should not apply the env token, got %q", fileCfg.Monbooru.APIToken)
	}

	persist, err := LoadFromFileIn(path, "")
	if err != nil {
		t.Fatalf("LoadFromFileIn: %v", err)
	}
	persist.Monbooru.DefaultGallery = "newgallery"
	mustSave(t, persist, path)
	onDisk, err := LoadFromFileIn(path, "")
	if err != nil {
		t.Fatalf("LoadFromFileIn: %v", err)
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

func TestRenameGalleryMovesTheDefaultAndSiteTargets(t *testing.T) {
	cfg := Default()
	cfg.Monbooru.DefaultGallery = "anime"
	cfg.Sites = []Site{{Name: "danbooru", Gallery: "anime"}, {Name: "gelbooru", Gallery: "stock"}, {Name: "e621"}}
	if moved := cfg.RenameGallery("anime", "art"); moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}
	got := []string{cfg.Monbooru.DefaultGallery, cfg.Sites[0].Gallery, cfg.Sites[1].Gallery, cfg.Sites[2].Gallery}
	if want := []string{"art", "art", "stock", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("galleries = %q, want %q", got, want)
	}
}

func TestSiteHasUserData(t *testing.T) {
	if (&Site{Name: "danbooru", LookupOrder: 1}).HasUserData() {
		t.Error("a bare seeded lookup block is not a customization")
	}
	for _, s := range []Site{
		{Name: "x", Username: "u"}, {Name: "x", Password: "p"}, {Name: "x", APIKey: "k"},
		{Name: "x", UserID: "1"}, {Name: "x", Gallery: "art"}, {Name: "x", Cookies: "/c"},
		{Name: "x", Options: "{}"}, {Name: "x", Agent: "a"},
	} {
		if !s.HasUserData() {
			t.Errorf("%+v should count as user data", s)
		}
	}
}

func TestValidateSiteOptions(t *testing.T) {
	if err := ValidateSiteOptions(""); err != nil {
		t.Errorf("empty options should be valid: %v", err)
	}
	if err := ValidateSiteOptions(`{"retweets": false}`); err != nil {
		t.Errorf("a JSON object should be valid: %v", err)
	}
	if err := ValidateSiteOptions(`[1,2]`); err == nil {
		t.Error("a JSON array is not an options object")
	}
	if err := ValidateSiteOptions(`{broken`); err == nil {
		t.Error("broken JSON must be refused")
	}
}

func TestPureHelpers(t *testing.T) {
	t.Run("WebBase", func(t *testing.T) {
		for _, tc := range []struct{ web, api, want string }{
			{"http://booru.example/", "http://internal:8080", "http://booru.example"},
			{"", "http://internal:8080/", ""},
			{"", "", ""},
		} {
			m := MonbooruConfig{WebURL: tc.web, APIURL: tc.api}
			if got := m.WebBase(); got != tc.want {
				t.Errorf("WebBase(web=%q api=%q) = %q, want %q", tc.web, tc.api, got, tc.want)
			}
		}
	})

	t.Run("HistoryRetention", func(t *testing.T) {
		if got := (DownloaderConfig{HistoryRetentionDays: 7}).HistoryRetention(); got != 7*24*time.Hour {
			t.Errorf("HistoryRetention(7) = %v, want 168h", got)
		}
		if got := (DownloaderConfig{}).HistoryRetention(); got != 0 {
			t.Errorf("HistoryRetention(0) = %v, want 0", got)
		}
	})

	t.Run("SuccessRetention", func(t *testing.T) {
		if got := (DownloaderConfig{SuccessRetentionDays: 3}).SuccessRetention(); got != 3*24*time.Hour {
			t.Errorf("SuccessRetention(3) = %v, want 72h", got)
		}
		if got := (DownloaderConfig{}).SuccessRetention(); got != 0 {
			t.Errorf("SuccessRetention(0) = %v, want 0", got)
		}
	})

	t.Run("IsHexHash", func(t *testing.T) {
		md5 := strings.Repeat("a", 32)
		for _, tc := range []struct {
			s    string
			n    int
			want bool
		}{
			{md5, 32, true},
			{strings.ToUpper(md5), 32, true},
			{strings.Repeat("a", 31), 32, false},
			{strings.Repeat("a", 33), 32, false},
			{strings.Repeat("g", 32), 32, false},
			{"", 32, false},
			{strings.Repeat("f", 64), 64, true},
		} {
			if got := IsHexHash(tc.s, tc.n); got != tc.want {
				t.Errorf("IsHexHash(%q, %d) = %v, want %v", tc.s, tc.n, got, tc.want)
			}
		}
	})

	t.Run("IsHTTPURL", func(t *testing.T) {
		for _, tc := range []struct {
			s    string
			want bool
		}{
			{"http://danbooru.donmai.us/posts/1", true},
			{"https://danbooru.donmai.us", true},
			{"ftp://x.example", false},
			{"javascript:alert(1)", false},
			{"notaurl", false},
			{"not a url at all", false},
			{"http://", false},
			{"", false},
		} {
			if got := IsHTTPURL(tc.s); got != tc.want {
				t.Errorf("IsHTTPURL(%q) = %v, want %v", tc.s, got, tc.want)
			}
		}
	})

	t.Run("EffectiveAccessKey", func(t *testing.T) {
		if got := (PTRConfig{AccessKey: "mine"}).EffectiveAccessKey(); got != "mine" {
			t.Errorf("configured key = %q, want mine", got)
		}
		if got := (PTRConfig{}).EffectiveAccessKey(); got != PublicAccessKey {
			t.Errorf("unset key = %q, want the published read-only key", got)
		}
	})
}

func TestTrayDefaultsOnForAnExistingConfig(t *testing.T) {
	cfg := loadLegacy(t, "[server]\nbind_address = \"0.0.0.0:8081\"\n")
	if !cfg.Desktop.Tray {
		t.Error("desktop.tray should read as true for a file that predates the key")
	}
}

func TestTrayHonoursAnExplicitFalse(t *testing.T) {
	cfg := loadLegacy(t, "[desktop]\ntray = false\n")
	if cfg.Desktop.Tray {
		t.Error("desktop.tray = false was ignored")
	}
}

func TestSetupDoneDefaultsTrueForAnExistingConfig(t *testing.T) {
	cfg := loadLegacy(t, "[server]\nbind_address = \"0.0.0.0:8081\"\n")
	if !cfg.SetupDone {
		t.Error("setup_done should read as true for a file that predates the key")
	}
}

func TestFreshConfigStartsUnsetUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	cfg := mustLoad(t, path)
	if cfg.SetupDone {
		t.Error("a config monloader just created should start at setup_done = false")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "setup_done = false") {
		t.Errorf("setup_done not written to the fresh config:\n%s", data)
	}
}

func TestLoadSeedsOnlyAFreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	cfg, err := Load(path, "", func(c *Config) { c.Server.BindAddress = "127.0.0.1:8456" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.BindAddress != "127.0.0.1:8456" {
		t.Fatalf("seed not applied: %q", cfg.Server.BindAddress)
	}
	again, err := Load(path, "", func(c *Config) { c.Server.BindAddress = "0.0.0.0:9999" })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if again.Server.BindAddress != "127.0.0.1:8456" {
		t.Errorf("bind_address = %q, want the stored one", again.Server.BindAddress)
	}
}

func TestLoadTakesWhatTheFileNeedsFromTheEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	cfg := Default()
	cfg.Auth.EnablePassword = true
	cfg.PTR.Enabled = true
	cfg.PTR.DataPath = ""
	mustSave(t, cfg, path)
	hash := "$2a$10$abcdefghijklmnopqrstuuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0"
	t.Setenv("MONLOADER_AUTH_PASSWORD_HASH", hash)
	t.Setenv("MONLOADER_PTR_DATA_PATH", "/ptr")
	got := mustLoad(t, path)
	if got.Auth.PasswordHash != hash || got.PTR.DataPath != "/ptr" {
		t.Errorf("hash %q, ptr data path %q, want the environment's", got.Auth.PasswordHash, got.PTR.DataPath)
	}
	if _, err := LoadFromFileIn(path, ""); err != nil {
		t.Errorf("the file layer refused to load: %v", err)
	}
}

func TestLoadStillRefusesAnIncompleteConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	cfg := Default()
	cfg.Auth.EnablePassword = true
	mustSave(t, cfg, path)
	if _, err := Load(path, "", nil); err == nil {
		t.Error("a password switch with no hash anywhere loaded")
	}
}

func TestLoadNamesTheRetiredBrandingKeys(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	path := filepath.Join(t.TempDir(), "monloader.toml")
	if err := os.WriteFile(path, []byte("[server]\ncustom_css = \"/config/custom.css\"\nlogo = \"/config/logo.png\"\ntheme = \"light\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, path)
	if out := buf.String(); !strings.Contains(out, "server.custom_css") || !strings.Contains(out, "server.logo") {
		t.Errorf("logged %q, want both retired keys named", out)
	}
	if cfg.Server.Theme != "light" {
		t.Errorf("theme = %q", cfg.Server.Theme)
	}
	mustSave(t, cfg, path)
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "custom_css") || strings.Contains(string(b), "logo =") {
		t.Errorf("a save kept a retired key:\n%s", b)
	}
}

func TestLoadWarnsOnAnUnknownKey(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	saved := filepath.Join(t.TempDir(), "monloader.toml")
	mustSave(t, Default(), saved)
	mustLoad(t, saved)
	if strings.Contains(buf.String(), "unknown key") {
		t.Errorf("a saved config warned: %s", buf.String())
	}

	buf.Reset()
	typo := filepath.Join(t.TempDir(), "monloader.toml")
	if err := os.WriteFile(typo, []byte("[server]\nbind_adress = \"0.0.0.0:1\"\n\n[sever]\nbase_url = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, typo)
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "server.bind_adress") || !strings.Contains(out, "sever.base_url") {
		t.Errorf("logged %q, want a warning naming server.bind_adress and sever.base_url", out)
	}
	if cfg.Server.BindAddress != Default().Server.BindAddress {
		t.Errorf("BindAddress = %q, want the default", cfg.Server.BindAddress)
	}

	buf.Reset()
	if _, err := LoadFromFileIn(typo, ""); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("the file layer a save reads logged %q", buf.String())
	}
}

func TestLoadAnchorsRelativePathsToTheStartingFolder(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "monloader.toml")
	if err := os.WriteFile(path, []byte("[gallerydl]\nbinary_path = \"gallery-dl\"\nconfig_path = \"gdl.json\"\narchive_path = \"state/archive.sqlite\"\ncookies_dir = \"cookies\"\n\n[ptr]\ndata_path = \"ptr\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, load := range map[string]func() (*Config, error){
		"startup":    func() (*Config, error) { return Load(path, "", nil) },
		"file layer": func() (*Config, error) { return LoadFromFileIn(path, "") },
	} {
		cfg, err := load()
		if err != nil {
			t.Fatal(err)
		}
		g := cfg.GalleryDL
		if g.ConfigPath != filepath.Join(dir, "gdl.json") || g.ArchivePath != filepath.Join(dir, "state", "archive.sqlite") ||
			g.CookiesDir != filepath.Join(dir, "cookies") || cfg.PTR.DataPath != filepath.Join(dir, "ptr") {
			t.Errorf("%s: paths %+v, ptr %q, want them under %s", name, g, cfg.PTR.DataPath, dir)
		}
		if g.BinaryPath != "gallery-dl" {
			t.Errorf("%s: binary_path = %q, a command name must stay as written", name, g.BinaryPath)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"Https://Danbooru.donmai.us/posts/1": "https://danbooru.donmai.us/posts/1",
		"HTTP://x/y":                         "http://x/y",
		"https://x/Y":                        "https://x/Y",
		"https://x/Y#Z":                      "https://x/Y#Z",
		"https://U:P@HOST/Y?Q=A#Z":           "https://U:P@host/Y?Q=A#Z",
		"https://HOST":                       "https://host",
		"https://HOST#Z":                     "https://host#Z",
		"md5:ABC":                            "md5:ABC",
		"ftp://X":                            "ftp://X",
	} {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProviderUpdateWritesTheFileLayer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monloader.toml")
	cfg := Default()
	mustSave(t, cfg, path)
	t.Setenv("MONLOADER_MONBOORU_API_TOKEN", "from-the-env")
	cfg.Monbooru.APIToken = "from-the-env"
	pr := NewProvider(cfg)
	pr.Persist(path, "")

	if err := pr.Update(func(c *Config) error { c.Downloader.Concurrency = 7; return nil }); err != nil {
		t.Fatal(err)
	}
	if pr.Current().Downloader.Concurrency != 7 {
		t.Error("the running config did not take the change")
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "concurrency = 7") {
		t.Errorf("the file did not take the change:\n%s", written)
	}
	if strings.Contains(string(written), "from-the-env") {
		t.Error("an env override was baked into the file")
	}
}

func TestProviderUpdateRefusalChangesNothing(t *testing.T) {
	pr := NewProvider(Default())
	before := pr.Current()
	refused := errors.New("no")
	if err := pr.Update(func(c *Config) error { c.Downloader.Concurrency = 7; return refused }); !errors.Is(err, refused) {
		t.Errorf("err = %v", err)
	}
	if pr.Current() != before {
		t.Error("a refused update replaced the config")
	}
}
