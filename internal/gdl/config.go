package gdl

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/shared/fsx"
	"github.com/monbooru/monloader/internal/shared/logx"
)

type SiteLists struct {
	FlatTag   []string
	Metadata  []string
	Notes     []string
	UserAgent map[string][]string
	Hosts     map[string][]string
	Options   map[string]map[string]any
}

type siteListSource interface {
	FlatTagSites() []string
	MetadataSites() []string
	NotesSites() []string
	UserAgentSites() map[string][]string
	SiteHosts() map[string][]string
	SiteOptions() map[string]map[string]any
}

func ListsFrom(src siteListSource) SiteLists {
	return SiteLists{
		FlatTag:   src.FlatTagSites(),
		Metadata:  src.MetadataSites(),
		Notes:     src.NotesSites(),
		UserAgent: src.UserAgentSites(),
		Hosts:     src.SiteHosts(),
		Options:   src.SiteOptions(),
	}
}

// Per site, later layers win: profile behavior keys, profile options,
// credentials, then the block's own options. raw_config merges last, over
// everything.
func WriteManagedConfig(cfg *config.Config, lists SiteLists) error {
	directlink := map[string]any{"filename": "{filename}.{extension}"}
	extractor := map[string]any{
		"directory": []any{},
		"postprocessors": []any{
			// On skip too: an archive skip's sidecar names its post, which
			// is how the pipeline pairs it with its row.
			map[string]any{"name": "metadata", "mode": "json", "event": "file,skip"},
		},
		// The default directlink name also carries {domain}/{path}, which a
		// long URL pushes past the 255-byte limit once flattened.
		"directlink": directlink,
		// Every file of a job lands in one flat directory, where the default
		// skip reads a second file of the same name as already fetched.
		"skip": "enumerate",
		// gallery-dl's default timeout and retries would block a worker
		// on an unreachable host.
		"timeout": 20.0,
		"retries": 2,
	}
	if cfg.GalleryDL.ArchivePath != "" {
		extractor["archive"] = cfg.GalleryDL.ArchivePath
	}
	if cfg.GalleryDL.SleepRequest > 0 {
		// sleep-request paces listing requests and sleep each file
		// download, so a page of files is not fetched in a burst.
		extractor["sleep-request"] = cfg.GalleryDL.SleepRequest
		extractor["sleep"] = cfg.GalleryDL.SleepRequest
	}

	// Even without a [[sites]] block: tags:true is what yields
	// per-category tags.
	for _, site := range lists.FlatTag {
		extractor[site] = map[string]any{"tags": true}
	}

	// The danbooru family emits artist commentary and notes only with
	// this include.
	for _, site := range lists.Metadata {
		blockFor(extractor, site)["metadata"] = "artist_commentary,notes"
	}

	// The gelbooru family emits note boxes only behind notes:true, read
	// off the page the tags fetch already loads.
	for _, site := range lists.Notes {
		blockFor(extractor, site)["notes"] = true
	}

	// directlink sends a browser agent, which Danbooru's Cloudflare
	// challenges; gallery-dl keys its config by the host's last two labels.
	for site, hosts := range lists.UserAgent {
		agent := "+gallery-dl"
		if s := cfg.FindSite(site); s != nil {
			agent = cmp.Or(s.UserAgent(), agent)
		}
		for _, host := range hosts {
			blockFor(directlink, registrableSuffix(host))["user-agent"] = agent
		}
	}

	for site, opts := range lists.Options {
		mergeMaps(blockFor(extractor, site), opts)
	}

	for _, site := range cfg.Sites {
		if site.Name == config.SitePTR {
			continue
		}
		block := blockFor(extractor, site.Name)
		// A username without a key makes the danbooru and e621 family
		// prompt for a password, which aborts without a TTY.
		if site.Username != "" && (site.Password != "" || site.APIKey != "") {
			block["username"] = site.Username
		}
		if site.APIKey != "" {
			block["api-key"] = site.APIKey
		}
		// The danbooru and e621 family reads the key as the Basic
		// Auth "password"; gelbooru reads "api-key".
		if password := cmp.Or(site.Password, site.APIKey); password != "" {
			block["password"] = password
		}
		if site.UserID != "" {
			block["user-id"] = site.UserID
		}
		agent := site.Agent
		if _, ok := lists.UserAgent[site.Name]; ok {
			agent = site.UserAgent()
		}
		if agent != "" {
			block["user-agent"] = agent
		}
		if site.Agent != "" {
			for _, host := range lists.Hosts[site.Name] {
				blockFor(directlink, registrableSuffix(host))["user-agent"] = site.Agent
			}
		}
		if site.Cookies != "" {
			block["cookies"] = site.Cookies
		}
		// Validated at save; one corrupted since is skipped so a bad
		// block cannot poison the whole file.
		if site.Options != "" {
			var opts map[string]any
			if err := json.Unmarshal([]byte(site.Options), &opts); err != nil {
				logx.Warnf("site %s options are not a JSON object, skipping them: %v", site.Name, err)
			} else {
				mergeMaps(block, opts)
			}
		}
		if len(block) == 0 {
			delete(extractor, site.Name)
		}
	}

	managed := map[string]any{
		"extractor": extractor,
		// Login sessions are secrets, and gallery-dl's default keeps them in the
		// user's cache directory.
		"cache": map[string]any{"file": filepath.Join(filepath.Dir(cfg.GalleryDL.ConfigPath), "gallery-dl-cache.sqlite3")},
		// reportDownloads reads file paths from stdout, and a Windows pipe
		// otherwise carries the ANSI code page.
		"output": map[string]any{"stdout": "utf-8", "stderr": "utf-8"},
	}

	if raw := cfg.GalleryDL.RawConfig; raw != "" {
		var rawMap map[string]any
		if err := json.Unmarshal([]byte(raw), &rawMap); err != nil {
			return fmt.Errorf("raw gallery-dl config is not valid JSON: %w", err)
		}
		mergeMaps(managed, rawMap)
	}

	data, err := json.MarshalIndent(managed, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding managed config: %w", err)
	}
	path := cfg.GalleryDL.ConfigPath
	if path == "" {
		return fmt.Errorf("gallerydl.config_path is empty")
	}
	if err := fsx.WriteFile(path, data); err != nil {
		return fmt.Errorf("writing managed config: %w", err)
	}
	return nil
}

func blockFor(extractor map[string]any, site string) map[string]any {
	block, _ := extractor[site].(map[string]any)
	if block == nil {
		block = map[string]any{}
		extractor[site] = block
	}
	return block
}

func mergeMaps(dst, src map[string]any) {
	for k, sv := range src {
		if sm, ok := sv.(map[string]any); ok {
			mergeMaps(blockFor(dst, k), sm)
			continue
		}
		dst[k] = sv
	}
}
