package gdl

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/monbooru/monloader/internal/config"
	"github.com/monbooru/monloader/internal/logx"
)

// Per site, later layers win: profile behavior keys, profile options,
// credentials, then the block's own options. raw_config merges last, over
// everything.
func WriteManagedConfig(cfg *config.Config, flatTagSites, metadataSites, notesSites []string, profileOptions map[string]map[string]any) error {
	extractor := map[string]any{
		"directory": []any{},
		"postprocessors": []any{
			// On skip too: an archive skip's sidecar names its post, which
			// is how the pipeline pairs it with its row.
			map[string]any{"name": "metadata", "mode": "json", "event": "file,skip"},
		},
		// The default directlink name also carries {domain}/{path}, which a
		// long URL pushes past the 255-byte limit once flattened.
		"directlink": map[string]any{"filename": "{filename}.{extension}"},
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
	for _, site := range flatTagSites {
		extractor[site] = map[string]any{"tags": true}
	}

	// The danbooru family emits artist commentary and notes only with
	// this include.
	for _, site := range metadataSites {
		blockFor(extractor, site)["metadata"] = "artist_commentary,notes"
	}

	// The gelbooru family emits note boxes only behind notes:true, read
	// off the page the tags fetch already loads.
	for _, site := range notesSites {
		blockFor(extractor, site)["notes"] = true
	}

	for site, opts := range profileOptions {
		mergeMaps(blockFor(extractor, site), opts)
	}

	for _, site := range cfg.Sites {
		block := blockFor(extractor, site.Name)
		if site.Password != "" {
			if site.Username != "" {
				block["username"] = site.Username
			}
			block["password"] = site.Password
			if site.APIKey != "" {
				block["api-key"] = site.APIKey
			}
		} else {
			// A username without a key makes the danbooru and e621 family
			// prompt for a password, which aborts without a TTY.
			if site.Username != "" && site.APIKey != "" {
				block["username"] = site.Username
			}
			if site.APIKey != "" {
				block["api-key"] = site.APIKey
				// The danbooru and e621 family reads the key as the Basic
				// Auth "password"; gelbooru reads "api-key".
				block["password"] = site.APIKey
			}
		}
		if site.UserID != "" {
			block["user-id"] = site.UserID
		}
		if site.Cookies != "" {
			block["cookies"] = site.Cookies
		}
		if slices.Contains(flatTagSites, site.Name) {
			block["tags"] = true
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

	managed := map[string]any{"extractor": extractor}

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
	if err := config.WriteFileBytesAtomic(path, data); err != nil {
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
			if dm, ok := dst[k].(map[string]any); ok {
				mergeMaps(dm, sm)
				continue
			}
		}
		dst[k] = sv
	}
}
