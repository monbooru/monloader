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

	"github.com/monbooru/monloader/internal/logx"
)

type Config struct {
	// SetupDone gates the desktop first-run wizard; a file without the
	// key loads as true, so an upgrade skips it.
	SetupDone       bool             `toml:"setup_done"`
	Server          ServerConfig     `toml:"server"`
	Monbooru        MonbooruConfig   `toml:"monbooru"`
	Downloader      DownloaderConfig `toml:"downloader"`
	GalleryDL       GalleryDLConfig  `toml:"gallerydl"`
	Auth            AuthConfig       `toml:"auth"`
	Log             LogConfig        `toml:"log"`
	PTR             PTRConfig        `toml:"ptr"`
	Lookup          LookupConfig     `toml:"lookup"`
	Desktop         DesktopConfig    `toml:"desktop"`
	Sites           []Site           `toml:"sites"`
	TagOverrides    []TagOverride    `toml:"tag_overrides"`
	RatingOverrides []RatingOverride `toml:"rating_overrides"`
	HostLabels      []HostLabel      `toml:"host_labels"`
}

type ServerConfig struct {
	BindAddress string `toml:"bind_address"`
	BaseURL     string `toml:"base_url"`
	CustomCSS   string `toml:"custom_css"`
	BooruName   string `toml:"name"`
	BooruLogo   string `toml:"logo"`
}

type MonbooruConfig struct {
	APIURL   string `toml:"api_url"`
	APIToken string `toml:"api_token"`
	// Browser-facing, for links; empty falls back to APIURL.
	WebURL         string `toml:"web_url"`
	DefaultGallery string `toml:"default_gallery"`
	// Pausing keeps the pairing on disk, so resuming needs no re-pair.
	Paused bool `toml:"paused,omitempty"`
}

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
	// 0 keeps a job until the ring's bound evicts it.
	HistoryRetentionDays int `toml:"history_retention_days"`
	// For a job that imported everything with no window left to fetch; 0
	// leaves it to HistoryRetentionDays.
	SuccessRetentionDays int `toml:"success_retention_days"`
}

func (d DownloaderConfig) HistoryRetention() time.Duration {
	return time.Duration(d.HistoryRetentionDays) * 24 * time.Hour
}

func (d DownloaderConfig) SuccessRetention() time.Duration {
	return time.Duration(d.SuccessRetentionDays) * 24 * time.Hour
}

// ConfigPath is rewritten by monloader; hand edits belong in RawConfig.
type GalleryDLConfig struct {
	BinaryPath   string  `toml:"binary_path"`
	ConfigPath   string  `toml:"config_path"`
	ArchivePath  string  `toml:"archive_path"`
	CookiesDir   string  `toml:"cookies_dir"`
	SleepRequest float64 `toml:"sleep_request"`
	RawConfig    string  `toml:"raw_config"`
	// gallery-dl's docs/supportedsites.md; empty uses the copy embedded
	// at the pinned version.
	SupportedSitesPath string `toml:"supportedsites_path"`
}

type AuthConfig struct {
	EnablePassword      bool    `toml:"enable_password"`
	PasswordHash        string  `toml:"password_hash"`
	SessionLifetimeDays int     `toml:"session_lifetime_days"`
	Tokens              []Token `toml:"tokens,omitempty"`
}

const (
	ScopeRead  = "read"
	ScopeWrite = "write"
)

var AllScopes = []string{ScopeRead, ScopeWrite}

type Token struct {
	ID        string   `toml:"id"`
	Name      string   `toml:"name"`
	TokenHash string   `toml:"token_hash"`
	Scopes    []string `toml:"scopes"`
	CreatedAt string   `toml:"created_at"`
	Paired    string   `toml:"paired,omitempty"`
}

func (t Token) HasScope(scope string) bool { return slices.Contains(t.Scopes, scope) }

func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func GenerateSecret() string {
	return hex.EncodeToString(mustRandom(16))
}

// A failed CSPRNG must never degrade into a predictable all-zero secret.
func mustRandom(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return buf
}

func IsHexHash(s string, n int) bool {
	_, err := hex.DecodeString(s)
	return len(s) == n && err == nil
}

func IsHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func IsRelativeFolder(s string) bool {
	return !strings.HasPrefix(s, "/") && !slices.Contains(strings.Split(s, "/"), "..")
}

// monbooru refuses a longer origin label.
const maxViaLen = 200

// monbooru selects the tag-source group with [data-source="<via>"], so
// whitespace or a quote there breaks the selector.
func IsValidVia(s string) bool {
	return len(s) <= maxViaLen && !strings.ContainsAny(s, " \t\n\r\"'<>[]\\")
}

func newTokenID() string {
	return hex.EncodeToString(mustRandom(8))
}

// An operator's token must not pass for one the pairing flow minted.
func reservedTokenName(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), "(paired)")
}

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

// GenerateToken belongs outside a replayed config mutation, so the id,
// secret and timestamp stay stable.
func GenerateToken(name string, scopes []string) (Token, string) {
	secret := GenerateSecret()
	return TokenFromSecret(name, secret, scopes), secret
}

func TokenFromSecret(name, secret string, scopes []string) Token {
	return Token{
		ID:        newTokenID(),
		Name:      name,
		TokenHash: HashToken(secret),
		Scopes:    scopes,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func (cfg *Config) TokenNameExists(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return slices.ContainsFunc(cfg.Auth.Tokens, func(t Token) bool { return strings.ToLower(t.Name) == n })
}

func (cfg *Config) FindTokenByHash(hash string) *Token {
	for i := range cfg.Auth.Tokens {
		if subtle.ConstantTimeCompare([]byte(cfg.Auth.Tokens[i].TokenHash), []byte(hash)) == 1 {
			return &cfg.Auth.Tokens[i]
		}
	}
	return nil
}

func (cfg *Config) RemoveToken(id string) bool {
	i := slices.IndexFunc(cfg.Auth.Tokens, func(t Token) bool { return t.ID == id })
	if i < 0 {
		return false
	}
	cfg.Auth.Tokens = slices.Delete(cfg.Auth.Tokens, i, i+1)
	return true
}

func (cfg *Config) FindToken(id string) *Token {
	for i := range cfg.Auth.Tokens {
		if cfg.Auth.Tokens[i].ID == id {
			return &cfg.Auth.Tokens[i]
		}
	}
	return nil
}

func (cfg *Config) SetTokenScopes(id string, scopes []string) bool {
	tok := cfg.FindToken(id)
	if tok == nil {
		return false
	}
	tok.Scopes = scopes
	return true
}

type LogConfig struct {
	Level string `toml:"level"`
}

// DesktopConfig holds only the tray switch: start-at-login and the menu
// entry live where the platform reads them.
type DesktopConfig struct {
	Tray bool `toml:"tray"`
}

type PTRConfig struct {
	Enabled bool `toml:"enabled"`
	Paused  bool `toml:"paused,omitempty"`
	// Kept off /config: the index runs to tens of GB.
	DataPath   string  `toml:"data_path"`
	Address    string  `toml:"address"`
	AccessKey  string  `toml:"access_key"`
	FetchSleep float64 `toml:"fetch_sleep"`
	// The initial sync refuses to start with less free space than this.
	MinFreeGB int `toml:"min_free_gb"`
	// Seconds between contribution uploads.
	CommitSleep float64 `toml:"commit_sleep"`
}

// The PTR's published read-only key (hydrus docs, access_keys.html); not
// a secret.
const PublicAccessKey = "4a285629721ca442541ef2c15ea17d1f7f7578b0c3f4f5f2a05f8f0ab297786f"

func (p PTRConfig) EffectiveAccessKey() string {
	if p.AccessKey != "" {
		return p.AccessKey
	}
	return PublicAccessKey
}

type LookupConfig struct {
	MinSimilarity int           `toml:"min_similarity"`
	Iqdb          LookupService `toml:"iqdb"`
	Saucenao      LookupService `toml:"saucenao"`
	// Images, not requests: only monloader knows how long a chain one
	// image walks. 0 refuses every scheduled lookup.
	ScheduledDailyBudget int `toml:"scheduled_daily_budget"`
}

// Order shares one number space with the sites' lookup_order; 0 never
// queries the service. iqdb signs in with the danbooru [[sites]]
// credentials, not APIKey.
type LookupService struct {
	Order  int    `toml:"order"`
	APIKey string `toml:"api_key,omitempty"`
}

// A new service also needs its LookupConfig field and a Service case.
var LookupServices = []string{"iqdb", "saucenao"}

func (l *LookupConfig) Service(name string) *LookupService {
	switch name {
	case "iqdb":
		return &l.Iqdb
	case "saucenao":
		return &l.Saucenao
	}
	return nil
}

// Site's Name is the gallery-dl category, such as "gelbooru".
type Site struct {
	Name     string `toml:"name"`
	Username string `toml:"username"`
	// For username+password logins; the danbooru and e621 families sign
	// in with APIKey instead.
	Password string `toml:"password,omitempty"`
	APIKey   string `toml:"api_key"`
	UserID   string `toml:"user_id"`
	Gallery  string `toml:"gallery"`
	// Stamped as the source on this site's pushes; empty keeps the category.
	Label   string `toml:"label,omitempty"`
	Cookies string `toml:"cookies"`
	// Merged under extractor.<name>. Unlike a profile's options it may
	// hold secrets, so it is never shared.
	Options string `toml:"options,omitempty"`
	// 1 is first; 0 keeps the site out of the hash-lookup walk.
	LookupOrder int `toml:"lookup_order,omitempty"`
}

// HasUserData ignores LookupOrder: the seeded chain gives untouched blocks one.
func (s *Site) HasUserData() bool {
	return s.Username != "" || s.Password != "" || s.APIKey != "" || s.UserID != "" ||
		s.Gallery != "" || s.Label != "" || s.Cookies != "" || s.Options != ""
}

type TagOverride struct {
	Site string `toml:"site"`
	From string `toml:"from"`
	To   string `toml:"to"`
}

type RatingOverride struct {
	Site string `toml:"site"`
	From string `toml:"from"`
	To   string `toml:"to"`
}

type HostLabel struct {
	Host  string `toml:"host"`
	Label string `toml:"label"`
}

func Default() *Config {
	return &Config{
		// 8456 beside monbooru's 8455, the same pair in every profile.
		Server: ServerConfig{
			BindAddress: "0.0.0.0:8456",
			BaseURL:     "http://localhost:8456",
		},
		Monbooru: MonbooruConfig{
			APIURL: "http://monbooru:8455",
		},
		Downloader: DownloaderConfig{
			Concurrency:          1,
			MaxItemsPerJob:       200,
			DefaultFolder:        "downloads",
			HistoryRetentionDays: 14,
			SuccessRetentionDays: 3,
		},
		GalleryDL: GalleryDLConfig{
			BinaryPath:   "gallery-dl",
			ConfigPath:   "/config/gallery-dl.json",
			ArchivePath:  "/config/gallery-dl-archive.sqlite",
			CookiesDir:   "/config/cookies",
			SleepRequest: 1.0,
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
		Desktop: DesktopConfig{
			Tray: true,
		},
		Sites: append([]Site(nil), defaultLookupSites...),
	}
}

// An exact copy scores near 100 and a wrong candidate far below.
const defaultMinSimilarity = 80

// Small on purpose: the free sites tolerate a trickle, and a library is
// swept over weeks anyway.
const defaultScheduledDailyBudget = 25

// danbooru leads for its tags and anonymous md5 search; gelbooru and
// rule34 need an API key, so the chain skips them until one is set.
var defaultLookupSites = []Site{
	{Name: "danbooru", LookupOrder: 1},
	{Name: "gelbooru", LookupOrder: 4},
	{Name: "rule34", LookupOrder: 5},
	{Name: "e621", LookupOrder: 6},
	{Name: "yandere", LookupOrder: 7},
	{Name: "konachan", LookupOrder: 8},
}

// A load re-seeds a missing default site, so dropping its block cannot
// remove it.
func DefaultChainSite(name string) bool {
	return slices.ContainsFunc(defaultLookupSites, func(s Site) bool { return s.Name == name })
}

// The walk seeded before the similarity services existed; a config still
// matching it was never customized.
var legacyLookupOrders = map[string]int{
	"danbooru": 1, "gelbooru": 2, "rule34": 3, "e621": 4, "yandere": 5, "konachan": 6,
}

// Relative on purpose: a moved folder or a new drive letter must not
// break the paths. The PTR folder stays unset for the desktop profile to
// create at start.
const portableDataDir = "data"

func portableDefaults(cfg *Config) {
	cfg.GalleryDL.ConfigPath = filepath.Join(portableDataDir, "gallery-dl.json")
	cfg.GalleryDL.ArchivePath = filepath.Join(portableDataDir, "gallery-dl-archive.sqlite")
	cfg.GalleryDL.CookiesDir = filepath.Join(portableDataDir, "cookies")
	cfg.PTR.DataPath = ""
}

// An empty file counts only for a portable install: the archives ship one
// as their marker.
func absent(path string, portable bool) (bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading config file: %w", err)
	}
	return portable && len(strings.TrimSpace(string(b))) == 0, nil
}

// binary_path is left alone: it names a command on PATH as often as a file.
func resolveIn(cfg *Config, base string) {
	cfg.GalleryDL.ConfigPath = absIn(cfg.GalleryDL.ConfigPath, base)
	cfg.GalleryDL.ArchivePath = absIn(cfg.GalleryDL.ArchivePath, base)
	cfg.GalleryDL.CookiesDir = absIn(cfg.GalleryDL.CookiesDir, base)
	cfg.GalleryDL.SupportedSitesPath = absIn(cfg.GalleryDL.SupportedSitesPath, base)
	cfg.PTR.DataPath = absIn(cfg.PTR.DataPath, base)
}

func absIn(path, base string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

func RelativizeIn(cfg *Config, base string) {
	cfg.GalleryDL.ConfigPath = RelativeTo(cfg.GalleryDL.ConfigPath, base)
	cfg.GalleryDL.ArchivePath = RelativeTo(cfg.GalleryDL.ArchivePath, base)
	cfg.GalleryDL.CookiesDir = RelativeTo(cfg.GalleryDL.CookiesDir, base)
	cfg.GalleryDL.SupportedSitesPath = RelativeTo(cfg.GalleryDL.SupportedSitesPath, base)
	cfg.PTR.DataPath = RelativeTo(cfg.PTR.DataPath, base)
}

// A path outside base stays absolute rather than moving with the folder.
func RelativeTo(path, base string) string {
	if path == "" || base == "" {
		return path
	}
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.ToSlash(rel)
}

// LoadWithDefaults applies seed only to a config it creates; one already on
// disk loads as it stands, so a container's volume layout is never rewritten.
func LoadWithDefaults(path string, seed func(*Config)) (*Config, error) {
	return load(path, "", seed)
}

// LoadPortable resolves the file's relative paths against its folder, so
// the install can move.
func LoadPortable(path string, seed func(*Config)) (*Config, error) {
	return load(path, filepath.Dir(path), seed)
}

func load(path, base string, seed func(*Config)) (*Config, error) {
	fresh, err := absent(path, base != "")
	if err != nil {
		return nil, err
	}
	if fresh {
		cfg := Default()
		if base != "" {
			portableDefaults(cfg)
		}
		if seed != nil {
			seed(cfg)
		}
		if writeErr := Save(cfg, path); writeErr != nil {
			return nil, fmt.Errorf("creating default config: %w", writeErr)
		}
	}

	cfg, err := LoadFromFileIn(path, base)
	if err != nil {
		return nil, err
	}
	applyEnvOverrides(cfg)
	// Again, so an env override is checked like a file value.
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// A file predating scheduled_daily_budget gets the default: 0 refuses
// every scheduled lookup. One with no [lookup] predates the similarity
// services, which renumber an untouched walk or follow a custom one.
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
		highest = max(highest, s.LookupOrder)
	}
	cfg.Lookup.Iqdb.Order = highest + 1
	cfg.Lookup.Saucenao.Order = highest + 2
}

// LoadFromFileIn skips the MONLOADER_* overrides: a save writes this layer,
// so an env value never lands in the file. A caller must pass the base it
// loaded with, or a key the file omits takes the container default and a
// save writes it into the file.
func LoadFromFileIn(path, base string) (*Config, error) {
	cfg := Default()
	if base != "" {
		portableDefaults(cfg)
	}
	if _, err := os.Stat(path); err == nil {
		// Cleared so the file's entries replace the defaults rather than
		// merge into them.
		cfg.Sites = nil
		cfg.TagOverrides = nil
		cfg.RatingOverrides = nil
		cfg.HostLabels = nil
		cfg.Lookup = LookupConfig{}
		cfg.SetupDone = true
		md, err := toml.DecodeFile(path, cfg)
		if err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", path, err)
		}
		seedLookupDefaults(cfg, md)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("checking config file: %w", err)
	}
	if base != "" {
		resolveIn(cfg, base)
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func Save(cfg *Config, path string) error {
	return WriteFileAtomic(path, func(f *os.File) error {
		if err := toml.NewEncoder(f).Encode(cfg); err != nil {
			return fmt.Errorf("encoding config: %w", err)
		}
		return nil
	})
}

// Synced before the rename: a zero-length monloader.toml costs a full re-pair.
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
	// Both are no-ops once the rename has landed.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := write(tmp); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}

func WriteFileBytesAtomic(path string, data []byte) error {
	return WriteFileAtomic(path, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// Every element type is a plain value except Token.Scopes; a new slice or
// map inside one must be cloned here too.
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

func (cfg *Config) FindSite(name string) *Site {
	for i := range cfg.Sites {
		if cfg.Sites[i].Name == name {
			return &cfg.Sites[i]
		}
	}
	return nil
}

func (cfg *Config) RenameGallery(from, to string) int {
	moved := 0
	if cfg.Monbooru.DefaultGallery == from {
		cfg.Monbooru.DefaultGallery = to
		moved++
	}
	for i := range cfg.Sites {
		if cfg.Sites[i].Gallery == from {
			cfg.Sites[i].Gallery = to
			moved++
		}
	}
	return moved
}

type Secret struct {
	Name  string
	Value string
}

// A shorter value, such as a two-digit user id, would also blank out
// timestamps and post ids.
const minSecretLen = 6

// Every credential belongs here: gallery-dl's debug output carries
// request URLs, query-string keys included.
func (cfg *Config) Secrets() []Secret {
	out := make([]Secret, 0, len(cfg.Sites)*3+2)
	add := func(name, value string) {
		if value != "" {
			out = append(out, Secret{Name: name, Value: value})
		}
	}
	for _, s := range cfg.Sites {
		add(s.Name+".api_key", s.APIKey)
		add(s.Name+".password", s.Password)
		add(s.Name+".user_id", s.UserID)
	}
	add("monbooru.api_token", cfg.Monbooru.APIToken)
	add("ptr.access_key", cfg.PTR.AccessKey)
	return out
}

func RedactSecrets(text string, secrets []Secret) (string, int) {
	removed := 0
	for _, s := range secrets {
		if len(s.Value) < minSecretLen {
			continue
		}
		removed += strings.Count(text, s.Value)
		text = strings.ReplaceAll(text, s.Value, "<redacted:"+s.Name+">")
	}
	return text, removed
}

func validateJSONObject(s, what string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return fmt.Errorf("%s must be a JSON object: %w", what, err)
	}
	return nil
}

func ValidateRawConfig(s string) error { return validateJSONObject(s, "raw gallery-dl config") }

func ValidateSiteOptions(s string) error { return validateJSONObject(s, "site options") }

func applyEnvOverrides(cfg *Config) {
	envOverride("MONLOADER_SERVER_BIND_ADDRESS", &cfg.Server.BindAddress, parseString)
	envOverride("MONLOADER_SERVER_BASE_URL", &cfg.Server.BaseURL, parseString)
	envOverride("MONLOADER_MONBOORU_API_URL", &cfg.Monbooru.APIURL, parseString)
	envOverride("MONLOADER_MONBOORU_API_TOKEN", &cfg.Monbooru.APIToken, parseString)
	envOverride("MONLOADER_MONBOORU_WEB_URL", &cfg.Monbooru.WebURL, parseString)
	envOverride("MONLOADER_MONBOORU_DEFAULT_GALLERY", &cfg.Monbooru.DefaultGallery, parseString)
	envOverride("MONLOADER_DOWNLOADER_DEFAULT_FOLDER", &cfg.Downloader.DefaultFolder, parseString)
	envOverride("MONLOADER_GALLERYDL_BINARY_PATH", &cfg.GalleryDL.BinaryPath, parseString)
	envOverride("MONLOADER_GALLERYDL_CONFIG_PATH", &cfg.GalleryDL.ConfigPath, parseString)
	envOverride("MONLOADER_GALLERYDL_ARCHIVE_PATH", &cfg.GalleryDL.ArchivePath, parseString)
	envOverride("MONLOADER_GALLERYDL_COOKIES_DIR", &cfg.GalleryDL.CookiesDir, parseString)
	envOverride("MONLOADER_GALLERYDL_SUPPORTEDSITES_PATH", &cfg.GalleryDL.SupportedSitesPath, parseString)
	envOverride("MONLOADER_AUTH_PASSWORD_HASH", &cfg.Auth.PasswordHash, parseString)
	envOverride("MONLOADER_LOG_LEVEL", &cfg.Log.Level, parseString)
	envOverride("MONLOADER_PTR_DATA_PATH", &cfg.PTR.DataPath, parseString)
	envOverride("MONLOADER_PTR_ADDRESS", &cfg.PTR.Address, parseString)
	envOverride("MONLOADER_PTR_ACCESS_KEY", &cfg.PTR.AccessKey, parseString)
	envOverride("MONLOADER_LOOKUP_SAUCENAO_API_KEY", &cfg.Lookup.Saucenao.APIKey, parseString)
	envOverride("MONLOADER_DOWNLOADER_CONCURRENCY", &cfg.Downloader.Concurrency, strconv.Atoi)
	envOverride("MONLOADER_DOWNLOADER_MAX_ITEMS_PER_JOB", &cfg.Downloader.MaxItemsPerJob, strconv.Atoi)
	envOverride("MONLOADER_DOWNLOADER_HISTORY_RETENTION_DAYS", &cfg.Downloader.HistoryRetentionDays, strconv.Atoi)
	envOverride("MONLOADER_DOWNLOADER_SUCCESS_RETENTION_DAYS", &cfg.Downloader.SuccessRetentionDays, strconv.Atoi)
	envOverride("MONLOADER_PTR_MIN_FREE_GB", &cfg.PTR.MinFreeGB, strconv.Atoi)
	envOverride("MONLOADER_LOOKUP_MIN_SIMILARITY", &cfg.Lookup.MinSimilarity, strconv.Atoi)
	envOverride("MONLOADER_GALLERYDL_SLEEP_REQUEST", &cfg.GalleryDL.SleepRequest, parseFloat)
	envOverride("MONLOADER_PTR_FETCH_SLEEP", &cfg.PTR.FetchSleep, parseFloat)
	envOverride("MONLOADER_PTR_COMMIT_SLEEP", &cfg.PTR.CommitSleep, parseFloat)
	envOverride("MONLOADER_AUTH_ENABLE_PASSWORD", &cfg.Auth.EnablePassword, strconv.ParseBool)
	envOverride("MONLOADER_PTR_ENABLED", &cfg.PTR.Enabled, strconv.ParseBool)
}

func envOverride[T any](env string, dst *T, parse func(string) (T, error)) {
	raw := os.Getenv(env)
	if raw == "" {
		return
	}
	v, err := parse(raw)
	if err != nil {
		logx.Warnf("config: ignoring %s=%q: %v", env, raw, err)
		return
	}
	*dst = v
}

func parseString(v string) (string, error) { return v, nil }

func parseFloat(v string) (float64, error) { return strconv.ParseFloat(v, 64) }

func validate(cfg *Config) error {
	if cfg.Server.BindAddress == "" {
		return fmt.Errorf("server.bind_address must not be empty")
	}
	if !strings.Contains(cfg.Server.BindAddress, ":") {
		return fmt.Errorf("server.bind_address %q is not a valid host:port", cfg.Server.BindAddress)
	}
	// An empty hash would let the password-update handler skip its
	// current-password check.
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
	cfg.Downloader.Concurrency = max(cfg.Downloader.Concurrency, 1)
	if cfg.Downloader.MaxItemsPerJob <= 0 {
		cfg.Downloader.MaxItemsPerJob = 200
	}
	cfg.Downloader.HistoryRetentionDays = max(cfg.Downloader.HistoryRetentionDays, 0)
	cfg.Downloader.SuccessRetentionDays = max(cfg.Downloader.SuccessRetentionDays, 0)
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
	if cfg.Lookup.MinSimilarity < 1 || cfg.Lookup.MinSimilarity > 100 {
		cfg.Lookup.MinSimilarity = defaultMinSimilarity
	}
	cfg.Lookup.Iqdb.Order = max(cfg.Lookup.Iqdb.Order, 0)
	cfg.Lookup.Saucenao.Order = max(cfg.Lookup.Saucenao.Order, 0)
	cfg.Lookup.ScheduledDailyBudget = max(cfg.Lookup.ScheduledDailyBudget, 0)
	return nil
}
