package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config holds all monloader configuration. The override tables back the
// per-site tag and rating settings.
type Config struct {
	Server          ServerConfig     `toml:"server"`
	Monbooru        MonbooruConfig   `toml:"monbooru"`
	Downloader      DownloaderConfig `toml:"downloader"`
	GalleryDL       GalleryDLConfig  `toml:"gallerydl"`
	Auth            AuthConfig       `toml:"auth"`
	Log             LogConfig        `toml:"log"`
	PTR             PTRConfig        `toml:"ptr"`
	Lookup          LookupConfig     `toml:"lookup"`
	Sites           []Site           `toml:"sites"`
	TagOverrides    []TagOverride    `toml:"tag_overrides"`
	RatingOverrides []RatingOverride `toml:"rating_overrides"`
	HostLabels      []HostLabel      `toml:"host_labels"`
}

type ServerConfig struct {
	BindAddress string `toml:"bind_address"`
	BaseURL     string `toml:"base_url"`
	// CustomCSS is an optional path to a stylesheet served at /custom.css
	// and linked after the bundled main.css, so the palette can be retuned
	// without a rebuild. Same shape as monbooru's knob.
	CustomCSS string `toml:"custom_css"`
	// BooruName overrides the brand shown in every page <title>, the topbar
	// wordmark, and the login heading. Empty resolves to "monloader" at
	// render time so an existing install upgrades without a config edit.
	BooruName string `toml:"name"`
	// BooruLogo is an optional path to a logo / favicon image served at
	// /custom.logo. When set it replaces both the favicon link and the
	// topbar logo on every page. Same shape as the custom_css knob.
	BooruLogo string `toml:"logo"`
}

// MonbooruConfig points at the monbooru instance the downloader pushes to.
type MonbooruConfig struct {
	APIURL   string `toml:"api_url"`
	APIToken string `toml:"api_token"`
	// WebURL is monbooru's browser-facing base, used to build the links to
	// pushed images in the queue. Empty falls back to APIURL - set it when
	// monbooru is reached at an internal address but browsed at a different
	// public one.
	WebURL         string `toml:"web_url"`
	DefaultGallery string `toml:"default_gallery"`
	// Paused holds the link from the footer light's kill switch: the
	// connectivity probe stops and no new download is accepted, while the
	// pairing stays on disk so resuming needs no re-pair.
	Paused bool `toml:"paused,omitempty"`
}

// WebBase is the browser-facing monbooru base for a link: WebURL when set,
// else the API URL, right-trimmed of "/".
func (m MonbooruConfig) WebBase() string {
	base := m.WebURL
	if base == "" {
		base = m.APIURL
	}
	return strings.TrimRight(base, "/")
}

type DownloaderConfig struct {
	Concurrency    int    `toml:"concurrency"`
	MaxItemsPerJob int    `toml:"max_items_per_job"`
	DefaultFolder  string `toml:"default_folder"`
	// HistoryRetentionDays drops a finished job from the queue's recent-history
	// ring once it is this old; 0 keeps it until the ring's bound evicts it.
	HistoryRetentionDays int `toml:"history_retention_days"`
}

// HistoryRetention is the retention window as a duration, for the queue.
func (d DownloaderConfig) HistoryRetention() time.Duration {
	return time.Duration(d.HistoryRetentionDays) * 24 * time.Hour
}

// GalleryDLConfig controls the gallery-dl subprocess. ConfigPath is the
// managed file the app writes (never hand-edited); RawConfig is an
// optional JSON object merged into it.
type GalleryDLConfig struct {
	BinaryPath   string  `toml:"binary_path"`
	ConfigPath   string  `toml:"config_path"`
	ArchivePath  string  `toml:"archive_path"`
	CookiesDir   string  `toml:"cookies_dir"`
	SleepRequest float64 `toml:"sleep_request"`
	RawConfig    string  `toml:"raw_config"`
	// SupportedSitesPath is gallery-dl's docs/supportedsites.md, bundled in
	// the image at the pinned version; it seeds display names and auth kinds
	// for sites without a shipped profile. Missing is fine.
	SupportedSitesPath string `toml:"supportedsites_path"`
}

// AuthConfig gates the optional UI password and the downloader's own API
// bearer token. Both are off by default for LAN trust.
type AuthConfig struct {
	EnablePassword      bool    `toml:"enable_password"`
	PasswordHash        string  `toml:"password_hash"`
	SessionLifetimeDays int     `toml:"session_lifetime_days"`
	Tokens              []Token `toml:"tokens,omitempty"`
}

// API privilege scopes. A token grants any combination; new tokens default
// to all of them.
const (
	ScopeRead  = "read"
	ScopeWrite = "write"
)

// AllScopes is every scope a monloader token can hold.
var AllScopes = []string{ScopeRead, ScopeWrite}

// Token is a named API credential. Only the secret's hash is stored; the
// plaintext is shown once at creation. Paired is set by the pairing flow and
// names the peer; it is empty for operator-created tokens.
type Token struct {
	ID        string   `toml:"id"`
	Name      string   `toml:"name"`
	TokenHash string   `toml:"token_hash"`
	Scopes    []string `toml:"scopes"`
	CreatedAt string   `toml:"created_at"`
	Paired    string   `toml:"paired,omitempty"`
	PeerURL   string   `toml:"peer_url,omitempty"`
}

// HasScope reports whether the token carries the given scope.
func (t Token) HasScope(scope string) bool { return slices.Contains(t.Scopes, scope) }

// HashToken returns the hex SHA-256 of a bearer secret.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// GenerateSecret returns a fresh 32-character hex bearer secret.
func GenerateSecret() string {
	return hex.EncodeToString(mustRandom(16))
}

// mustRandom reads n random bytes or dies: a failed CSPRNG must never
// degrade into a predictable all-zero secret.
func mustRandom(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return buf
}

// IsHexHash reports whether s is exactly n hex characters (either case). The
// web add bar and the API share it to recognize md5 and sha256 inputs.
func IsHexHash(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// IsHTTPURL reports whether s is an absolute http(s) URL with a host - the
// boundary check the web add bar and the API apply so a typo or non-URL is
// rejected at the door instead of failing at resolve.
func IsHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func newTokenID() string {
	return hex.EncodeToString(mustRandom(8))
}

// reservedTokenName matches names the pairing flow owns, so an operator cannot
// create one that collides with or impersonates a paired token.
func reservedTokenName(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), "(paired)")
}

// ValidateTokenName rejects empty and pairing-reserved names.
func ValidateTokenName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return fmt.Errorf("token name must not be empty")
	}
	if reservedTokenName(n) {
		return fmt.Errorf("token names ending in \"(paired)\" are reserved")
	}
	return nil
}

// GenerateToken builds a token from a name and scopes, returning the plaintext
// secret (available only here). Call it before a replayed updateConfig closure
// so the id, secret, and timestamp are stable across both applications.
func GenerateToken(name string, scopes []string) (Token, string) {
	secret := GenerateSecret()
	return TokenFromSecret(name, secret, scopes), secret
}

// TokenFromSecret builds a token whose hash matches a caller-provided secret.
// Pairing uses this: the initiator generates the secret to hand to the peer and
// stores the matching token so the peer's calls authenticate.
func TokenFromSecret(name, secret string, scopes []string) Token {
	return Token{
		ID:        newTokenID(),
		Name:      name,
		TokenHash: HashToken(secret),
		Scopes:    scopes,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// TokenNameExists reports whether a token already uses name (case-insensitive).
func (cfg *Config) TokenNameExists(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, t := range cfg.Auth.Tokens {
		if strings.ToLower(t.Name) == n {
			return true
		}
	}
	return false
}

// FindTokenByHash returns the token whose stored hash matches, or nil.
func (cfg *Config) FindTokenByHash(hash string) *Token {
	for i := range cfg.Auth.Tokens {
		if subtle.ConstantTimeCompare([]byte(cfg.Auth.Tokens[i].TokenHash), []byte(hash)) == 1 {
			return &cfg.Auth.Tokens[i]
		}
	}
	return nil
}

// RemoveToken drops the token with the given id, reporting whether it existed.
func (cfg *Config) RemoveToken(id string) bool {
	i := slices.IndexFunc(cfg.Auth.Tokens, func(t Token) bool { return t.ID == id })
	if i < 0 {
		return false
	}
	cfg.Auth.Tokens = slices.Delete(cfg.Auth.Tokens, i, i+1)
	return true
}

// FindToken returns the token with the given id, or nil.
func (cfg *Config) FindToken(id string) *Token {
	for i := range cfg.Auth.Tokens {
		if cfg.Auth.Tokens[i].ID == id {
			return &cfg.Auth.Tokens[i]
		}
	}
	return nil
}

// SetTokenScopes replaces a token's scopes, reporting whether it existed.
func (cfg *Config) SetTokenScopes(id string, scopes []string) bool {
	tok := cfg.FindToken(id)
	if tok == nil {
		return false
	}
	tok.Scopes = scopes
	return true
}

// LogConfig controls log verbosity: "warn" (default), "info", "debug".
type LogConfig struct {
	Level string `toml:"level"`
}

// PTRConfig controls the optional Hydrus Public Tag Repository thin client. It
// is disabled by default and downloads nothing until enabled; once enabled it
// streams the repository's update history into a local SQLite index on its own
// volume (never /config) and answers hash -> tags lookups by sha256.
type PTRConfig struct {
	Enabled bool `toml:"enabled"`
	// DataPath is the dedicated volume holding the index; kept off /config so the
	// tens-of-GB database never bloats the small config volume.
	DataPath string `toml:"data_path"`
	Address  string `toml:"address"`
	// AccessKey authenticates every request; empty uses the PTR's published
	// public read-only key.
	AccessKey  string  `toml:"access_key"`
	FetchSleep float64 `toml:"fetch_sleep"`
	// MinFreeGB refuses to start the initial sync when the data volume has less
	// free space, so a multi-tens-of-GB stream cannot fill the disk.
	MinFreeGB int `toml:"min_free_gb"`
	// CommitSleep paces contribution uploads: seconds between POSTs,
	// matching the hydrus client's politeness.
	CommitSleep float64 `toml:"commit_sleep"`
}

// PublicAccessKey is the PTR's published read-only access key (hydrus docs,
// access_keys.html). It is not a secret; an operator may override it with a
// personal or private-repository key via [ptr].access_key.
const PublicAccessKey = "4a285629721ca442541ef2c15ea17d1f7f7578b0c3f4f5f2a05f8f0ab297786f"

// EffectiveAccessKey is the configured access key, or the public key when none
// is set.
func (p PTRConfig) EffectiveAccessKey() string {
	if p.AccessKey != "" {
		return p.AccessKey
	}
	return PublicAccessKey
}

// LookupConfig parameterizes the hash-lookup chain's similarity stage: the
// two image-similarity services and the score floor a candidate must clear.
// A service's chain position shares one number space with the sites'
// lookup_order, so exact-md5 searches and similarity queries interleave.
type LookupConfig struct {
	// MinSimilarity is the percent score below which a similarity candidate
	// is ignored (1-100).
	MinSimilarity int           `toml:"min_similarity"`
	Iqdb          LookupService `toml:"iqdb"`
	Saucenao      LookupService `toml:"saucenao"`
	// ScheduledDailyBudget bounds how many images a day the budgeted lookups
	// monbooru's nightly run sends may cover, 0 refusing them all. It counts
	// images, not requests: one image walks a chain whose length only
	// monloader knows.
	ScheduledDailyBudget int `toml:"scheduled_daily_budget"`
}

// LookupService is one similarity service's settings. Order is its chain
// position (0 or absent = never queried). APIKey is saucenao's key; iqdb
// authenticates with the danbooru [[sites]] credentials instead.
type LookupService struct {
	Order  int    `toml:"order"`
	APIKey string `toml:"api_key,omitempty"`
}

// Site is one repeatable [[sites]] block: credentials and a per-source
// target gallery, written into the managed gallery-dl config. Name is the
// gallery-dl category (e.g. "gelbooru", "e621").
type Site struct {
	Name     string `toml:"name"`
	Username string `toml:"username"`
	// Password is the account password for sites gallery-dl signs into with
	// username+password (twitter-style logins). The danbooru/e621 families
	// keep authenticating with APIKey, which doubles as their password.
	Password string `toml:"password,omitempty"`
	APIKey   string `toml:"api_key"`
	UserID   string `toml:"user_id"`
	Gallery  string `toml:"gallery"`
	// Label is stamped as the source on every push from this site; empty
	// keeps the gallery-dl category name.
	Label   string `toml:"label,omitempty"`
	Cookies string `toml:"cookies"`
	// Options is a JSON object of extra gallery-dl extractor options merged
	// under extractor.<name>. The instance-side escape hatch: unlike a
	// profile's options it may hold secrets and is never shared.
	Options string `toml:"options,omitempty"`
	// LookupOrder is the site's position in the booru hash-lookup walk
	// (1 = first); 0 or absent keeps the site out of the walk entirely.
	LookupOrder int `toml:"lookup_order,omitempty"`
}

// HasUserData reports whether the block carries anything the operator set -
// credentials, a cookies file, a target gallery, or options. The settings
// sites tables show only such sites; a bare or lookup-order-only block (the
// seeded chain) is not a customization.
func (s *Site) HasUserData() bool {
	return s.Username != "" || s.Password != "" || s.APIKey != "" || s.UserID != "" ||
		s.Gallery != "" || s.Label != "" || s.Cookies != "" || s.Options != ""
}

// TagOverride routes a gallery-dl tag-category suffix to a monbooru
// category for one site, winning over the curated profile.
type TagOverride struct {
	Site string `toml:"site"`
	From string `toml:"from"`
	To   string `toml:"to"`
}

// RatingOverride routes a booru rating value to a monbooru rating level
// for one site, winning over the curated profile.
type RatingOverride struct {
	Site string `toml:"site"`
	From string `toml:"from"`
	To   string `toml:"to"`
}

// HostLabel names the source label for pushes whose only identity is a host
// (a direct file link, a page send): exact host first, parent domains after,
// winning over a profile's host aliases.
type HostLabel struct {
	Host  string `toml:"host"`
	Label string `toml:"label"`
}

// Default returns a fully populated config with the built-in defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			BindAddress: "0.0.0.0:8081",
			BaseURL:     "http://localhost:8081",
		},
		Monbooru: MonbooruConfig{
			APIURL: "http://monbooru:8080",
		},
		Downloader: DownloaderConfig{
			Concurrency:          1,
			MaxItemsPerJob:       200,
			DefaultFolder:        "downloads",
			HistoryRetentionDays: 7,
		},
		GalleryDL: GalleryDLConfig{
			BinaryPath:         "gallery-dl",
			ConfigPath:         "/config/gallery-dl.json",
			ArchivePath:        "/config/gallery-dl-archive.sqlite",
			CookiesDir:         "/config/cookies",
			SleepRequest:       1.0,
			SupportedSitesPath: "/usr/local/share/monloader/supportedsites.md",
		},
		Auth: AuthConfig{
			SessionLifetimeDays: 7,
		},
		Log: LogConfig{
			Level: "warn",
		},
		PTR: PTRConfig{
			DataPath:    "/ptr",
			Address:     "https://ptr.hydrus.network:45871",
			FetchSleep:  1.0,
			MinFreeGB:   70,
			CommitSleep: 1.0,
		},
		Lookup: LookupConfig{
			MinSimilarity:        defaultMinSimilarity,
			Iqdb:                 LookupService{Order: 2},
			Saucenao:             LookupService{Order: 3},
			ScheduledDailyBudget: defaultScheduledDailyBudget,
		},
		Sites: append([]Site(nil), defaultLookupSites...),
	}
}

// defaultMinSimilarity is the similarity floor a candidate must clear: an
// exact copy scores near 100 and the first wrong candidate far below (the
// live probe saw 96 vs 20), so 80 separates cleanly.
const defaultMinSimilarity = 80

// defaultScheduledDailyBudget is how many images a night's unattended lookup
// may cover. Small on purpose: the sites are free and polite about a trickle,
// and a library worth sweeping is swept over weeks either way.
const defaultScheduledDailyBudget = 25

// defaultLookupSites is the exact-md5 half of the lookup chain a fresh
// install gets. danbooru leads (best tags, free anonymous md5 metatag) with
// the similarity services at 2-3 right behind it; gelbooru and rule34 are the
// largest coverage but need an api key, so the chain skips them until one is
// configured; e621, yandere, and konachan add anonymous reach.
var defaultLookupSites = []Site{
	{Name: "danbooru", LookupOrder: 1},
	{Name: "gelbooru", LookupOrder: 4},
	{Name: "rule34", LookupOrder: 5},
	{Name: "e621", LookupOrder: 6},
	{Name: "yandere", LookupOrder: 7},
	{Name: "konachan", LookupOrder: 8},
}

// DefaultChainSite reports whether a site is one the first-run config seeds
// into the lookup chain. A load re-seeds such a site when it has no block, so
// dropping the block is not a way to configure anything: it puts the site back
// at its seeded position on the next read.
func DefaultChainSite(name string) bool {
	return slices.ContainsFunc(defaultLookupSites, func(s Site) bool { return s.Name == name })
}

// legacyLookupOrders is the walk seeded before the similarity services
// existed. A config whose ordered sites all still match it was never
// customized, so the upgrade seeding may renumber it into the current chain.
var legacyLookupOrders = map[string]int{
	"danbooru": 1, "gelbooru": 2, "rule34": 3, "e621": 4, "yandere": 5, "konachan": 6,
}

// Load reads the config (creating it with defaults when absent), applies
// MONLOADER_* env overrides, and validates. The result is the effective
// runtime view. Env overrides are applied before the final validation so a
// MONLOADER_* value is sanity-checked the same way a file value is.
func Load(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if writeErr := Save(Default(), path); writeErr != nil {
			return nil, fmt.Errorf("creating default config: %w", writeErr)
		}
	} else if err != nil {
		return nil, fmt.Errorf("checking config file: %w", err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	applyEnvOverrides(cfg)
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// seedLookupDefaults gives an existing config the same lookup defaults a fresh
// install gets. A default site is added only when it has no block, so an
// install that has configured a site (or cleared its order) keeps its choice.
// A config with no [lookup] section predates the similarity services: when its
// site orders were never customized they are renumbered into the current
// chain, otherwise the services are appended after the operator's own order;
// either way a cleared site stays cleared. A present [lookup] section - even
// one with the services switched off - is never touched, except for a key the
// file predates entirely: it takes the fresh-install value, so an upgrade does
// not silently refuse every scheduled lookup.
func seedLookupDefaults(cfg *Config, md toml.MetaData) {
	if !md.IsDefined("lookup", "scheduled_daily_budget") {
		cfg.Lookup.ScheduledDailyBudget = defaultScheduledDailyBudget
	}
	legacy := true
	for _, s := range cfg.Sites {
		if s.LookupOrder > 0 && legacyLookupOrders[s.Name] != s.LookupOrder {
			legacy = false
			break
		}
	}
	for _, d := range defaultLookupSites {
		if cfg.FindSite(d.Name) == nil {
			cfg.Sites = append(cfg.Sites, d)
		}
	}
	if md.IsDefined("lookup") {
		return
	}
	if legacy {
		for _, d := range defaultLookupSites {
			if s := cfg.FindSite(d.Name); s.LookupOrder > 0 {
				s.LookupOrder = d.LookupOrder
			}
		}
		cfg.Lookup.Iqdb.Order = 2
		cfg.Lookup.Saucenao.Order = 3
		return
	}
	highest := 0
	for _, s := range cfg.Sites {
		if s.LookupOrder > highest {
			highest = s.LookupOrder
		}
	}
	cfg.Lookup.Iqdb.Order = highest + 1
	cfg.Lookup.Saucenao.Order = highest + 2
}

// LoadFromFile decodes the config file (or defaults when absent) and validates
// it, without applying MONLOADER_* env overrides. This is the persistence
// view: a settings save writes this layer so an ephemeral env value (e.g. a
// token passed via the container env) is never baked into monloader.toml.
func LoadFromFile(path string) (*Config, error) {
	cfg := Default()
	if _, err := os.Stat(path); err == nil {
		// Null the slices and the lookup section so the file's entries replace
		// the defaults rather than appending to (or resurrecting) them; the
		// seeding below restores the defaults a pre-similarity file lacks.
		cfg.Sites = nil
		cfg.TagOverrides = nil
		cfg.RatingOverrides = nil
		cfg.HostLabels = nil
		cfg.Lookup = LookupConfig{}
		md, err := toml.DecodeFile(path, cfg)
		if err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", path, err)
		}
		seedLookupDefaults(cfg, md)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("checking config file: %w", err)
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save marshals cfg to TOML and writes atomically to path.
func Save(cfg *Config, path string) error {
	return WriteFileAtomic(path, func(f *os.File) error {
		if err := toml.NewEncoder(f).Encode(cfg); err != nil {
			return fmt.Errorf("encoding config: %w", err)
		}
		return nil
	})
}

// WriteFileAtomic writes a file through a temp-then-rename in its directory, so
// a crash mid-write can never leave a truncated file a later reader would load
// broken: the content is on disk before the rename publishes it, so a reader
// sees either the old file or the whole new one. write streams the content into
// the temp file; both the config and the managed gallery-dl config go through
// here, and monloader.toml holds the monbooru token, every site credential and
// the PTR access key, so a zero-length one costs a full re-pair.
func WriteFileAtomic(path string, write func(f *os.File) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	if err := write(tmp); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

// Clone returns a deep copy safe to mutate without affecting the original, so a
// settings save can build the next snapshot from the current one. Every element
// but a token's Scopes is a value type, so copying the headers and elements is
// a full copy; Scopes is the one slice inside one and is cloned with it.
func (cfg *Config) Clone() *Config {
	cp := *cfg
	cp.Sites = append([]Site(nil), cfg.Sites...)
	cp.TagOverrides = append([]TagOverride(nil), cfg.TagOverrides...)
	cp.RatingOverrides = append([]RatingOverride(nil), cfg.RatingOverrides...)
	cp.HostLabels = append([]HostLabel(nil), cfg.HostLabels...)
	cp.Auth.Tokens = append([]Token(nil), cfg.Auth.Tokens...)
	for i := range cp.Auth.Tokens {
		cp.Auth.Tokens[i].Scopes = slices.Clone(cp.Auth.Tokens[i].Scopes)
	}
	return &cp
}

// FindSite returns the per-site block with the given gallery-dl category
// name, or nil.
func (cfg *Config) FindSite(name string) *Site {
	for i := range cfg.Sites {
		if cfg.Sites[i].Name == name {
			return &cfg.Sites[i]
		}
	}
	return nil
}

// ValidateRawConfig rejects a non-empty raw gallery-dl passthrough that is
// not a JSON object. An empty string is valid (no passthrough). The
// settings page calls this before Save so invalid JSON is never persisted.
func ValidateRawConfig(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return fmt.Errorf("raw gallery-dl config must be a JSON object: %w", err)
	}
	return nil
}

// ValidateSiteOptions rejects a non-empty per-site options value that is not
// a JSON object; empty means no options. Same save-time gate as the raw
// passthrough.
func ValidateSiteOptions(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return fmt.Errorf("site options must be a JSON object: %w", err)
	}
	return nil
}

// applyEnvOverrides lets a MONLOADER_* variable override its config field, so
// a container env can set what the file does not. Adding a variable is one
// table line; a value that fails to parse is ignored, keeping the file's.
func applyEnvOverrides(cfg *Config) {
	for _, o := range []struct {
		env string
		dst *string
	}{
		{"MONLOADER_SERVER_BIND_ADDRESS", &cfg.Server.BindAddress},
		{"MONLOADER_SERVER_BASE_URL", &cfg.Server.BaseURL},
		{"MONLOADER_MONBOORU_API_URL", &cfg.Monbooru.APIURL},
		{"MONLOADER_MONBOORU_API_TOKEN", &cfg.Monbooru.APIToken},
		{"MONLOADER_MONBOORU_WEB_URL", &cfg.Monbooru.WebURL},
		{"MONLOADER_MONBOORU_DEFAULT_GALLERY", &cfg.Monbooru.DefaultGallery},
		{"MONLOADER_DOWNLOADER_DEFAULT_FOLDER", &cfg.Downloader.DefaultFolder},
		{"MONLOADER_GALLERYDL_BINARY_PATH", &cfg.GalleryDL.BinaryPath},
		{"MONLOADER_GALLERYDL_CONFIG_PATH", &cfg.GalleryDL.ConfigPath},
		{"MONLOADER_GALLERYDL_ARCHIVE_PATH", &cfg.GalleryDL.ArchivePath},
		{"MONLOADER_GALLERYDL_COOKIES_DIR", &cfg.GalleryDL.CookiesDir},
		{"MONLOADER_GALLERYDL_SUPPORTEDSITES_PATH", &cfg.GalleryDL.SupportedSitesPath},
		{"MONLOADER_AUTH_PASSWORD_HASH", &cfg.Auth.PasswordHash},
		{"MONLOADER_LOG_LEVEL", &cfg.Log.Level},
		{"MONLOADER_PTR_DATA_PATH", &cfg.PTR.DataPath},
		{"MONLOADER_PTR_ADDRESS", &cfg.PTR.Address},
		{"MONLOADER_PTR_ACCESS_KEY", &cfg.PTR.AccessKey},
		{"MONLOADER_LOOKUP_SAUCENAO_API_KEY", &cfg.Lookup.Saucenao.APIKey},
	} {
		if v := os.Getenv(o.env); v != "" {
			*o.dst = v
		}
	}
	for _, o := range []struct {
		env string
		dst *int
	}{
		{"MONLOADER_DOWNLOADER_CONCURRENCY", &cfg.Downloader.Concurrency},
		{"MONLOADER_DOWNLOADER_MAX_ITEMS_PER_JOB", &cfg.Downloader.MaxItemsPerJob},
		{"MONLOADER_DOWNLOADER_HISTORY_RETENTION_DAYS", &cfg.Downloader.HistoryRetentionDays},
		{"MONLOADER_PTR_MIN_FREE_GB", &cfg.PTR.MinFreeGB},
		{"MONLOADER_LOOKUP_MIN_SIMILARITY", &cfg.Lookup.MinSimilarity},
	} {
		if v := os.Getenv(o.env); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*o.dst = n
			}
		}
	}
	for _, o := range []struct {
		env string
		dst *float64
	}{
		{"MONLOADER_GALLERYDL_SLEEP_REQUEST", &cfg.GalleryDL.SleepRequest},
		{"MONLOADER_PTR_FETCH_SLEEP", &cfg.PTR.FetchSleep},
		{"MONLOADER_PTR_COMMIT_SLEEP", &cfg.PTR.CommitSleep},
	} {
		if v := os.Getenv(o.env); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				*o.dst = f
			}
		}
	}
	for _, o := range []struct {
		env string
		dst *bool
	}{
		{"MONLOADER_AUTH_ENABLE_PASSWORD", &cfg.Auth.EnablePassword},
		{"MONLOADER_PTR_ENABLED", &cfg.PTR.Enabled},
	} {
		if v := os.Getenv(o.env); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*o.dst = b
			}
		}
	}
}

func validate(cfg *Config) error {
	if cfg.Server.BindAddress == "" {
		return fmt.Errorf("server.bind_address must not be empty")
	}
	if !strings.Contains(cfg.Server.BindAddress, ":") {
		return fmt.Errorf("server.bind_address %q is not a valid host:port", cfg.Server.BindAddress)
	}
	// enable_password=true with an empty hash would let the password-update
	// handler bypass the current-password check, so refuse it at load with
	// the same hint monbooru gives.
	if cfg.Auth.EnablePassword && strings.TrimSpace(cfg.Auth.PasswordHash) == "" {
		return fmt.Errorf("auth.enable_password is true but auth.password_hash is empty - " +
			"run `monloader -hash-password 'your-password'` and paste the result into monloader.toml")
	}
	if cfg.Auth.EnablePassword {
		h := strings.TrimSpace(cfg.Auth.PasswordHash)
		if !strings.HasPrefix(h, "$2a$") && !strings.HasPrefix(h, "$2b$") && !strings.HasPrefix(h, "$2y$") {
			return fmt.Errorf("auth.password_hash does not look like a bcrypt hash - " +
				"run `monloader -hash-password 'your-password'` and paste the result into monloader.toml")
		}
	}
	// A non-positive worker count would stall the queue; snap to one worker
	// rather than fail a user-fixable typo.
	cfg.Downloader.Concurrency = max(cfg.Downloader.Concurrency, 1)
	if cfg.Downloader.MaxItemsPerJob <= 0 {
		cfg.Downloader.MaxItemsPerJob = 200
	}
	// A negative window would expire every job the moment it finished; read it
	// as "no age limit" rather than fail a user-fixable typo.
	cfg.Downloader.HistoryRetentionDays = max(cfg.Downloader.HistoryRetentionDays, 0)
	cfg.GalleryDL.SleepRequest = max(cfg.GalleryDL.SleepRequest, 0)
	if cfg.Auth.SessionLifetimeDays <= 0 {
		cfg.Auth.SessionLifetimeDays = 7
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "warn"
	}
	cfg.PTR.CommitSleep = max(cfg.PTR.CommitSleep, 0)
	cfg.PTR.FetchSleep = max(cfg.PTR.FetchSleep, 0)
	if cfg.PTR.Enabled && strings.TrimSpace(cfg.PTR.DataPath) == "" {
		return fmt.Errorf("ptr.enabled is true but ptr.data_path is empty")
	}
	// An out-of-range similarity floor would either drop every candidate or
	// accept garbage; snap a user-fixable typo to the default like the worker
	// count above.
	if cfg.Lookup.MinSimilarity < 1 || cfg.Lookup.MinSimilarity > 100 {
		cfg.Lookup.MinSimilarity = defaultMinSimilarity
	}
	cfg.Lookup.Iqdb.Order = max(cfg.Lookup.Iqdb.Order, 0)
	cfg.Lookup.Saucenao.Order = max(cfg.Lookup.Saucenao.Order, 0)
	cfg.Lookup.ScheduledDailyBudget = max(cfg.Lookup.ScheduledDailyBudget, 0)
	return nil
}
