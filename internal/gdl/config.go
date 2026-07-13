package gdl

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/leqwin/monloader/internal/config"
)

// WriteManagedConfig writes the gallery-dl config file the app controls: the
// download-archive, the metadata postprocessor that writes `<file>.json`
// sidecars, and per-site blocks. The download location is set per run with
// `-D <workDir>`, so none is written here. flatTagSites lists the categories
// that need `tags: true` to emit per-category tags; notesSites those that need
// `notes: true` to emit note boxes. The validated raw passthrough
// is deep-merged last so operator options win; the file is rewritten from config,
// never hand-edited.
func WriteManagedConfig(cfg *config.Config, flatTagSites, metadataSites, notesSites []string) error {
	extractor := map[string]any{
		"directory": []any{},
		"postprocessors": []any{
			map[string]any{"name": "metadata", "mode": "json"},
		},
		// directlink's filename template is "{domain}/{path}/{filename}.{extension}";
		// with the directory flattened to the per-job workdir it folds into one
		// name, which a long URL pushes past the filesystem's 255-byte limit. Keep
		// just the file's own name so the download still lands.
		"directlink": map[string]any{"filename": "{filename}.{extension}"},
		// Cap an unreachable host; without these the worker blocks on gallery-dl's
		// long default timeout and retries. Overridable via raw_config.
		"timeout": 20.0,
		"retries": 2,
	}
	if cfg.GalleryDL.ArchivePath != "" {
		extractor["archive"] = cfg.GalleryDL.ArchivePath
	}
	if cfg.GalleryDL.SleepRequest > 0 {
		// sleep-request throttles extractor (listing) requests; sleep throttles
		// each file download, so a page of many files is not fetched in a burst.
		extractor["sleep-request"] = cfg.GalleryDL.SleepRequest
		extractor["sleep"] = cfg.GalleryDL.SleepRequest
	}

	// Every flat-tag family gets tags:true so its per-category tags appear,
	// even when the operator has not added credentials for it.
	for _, site := range flatTagSites {
		extractor[site] = map[string]any{"tags": true}
	}

	// Danbooru-family sites need the metadata include to emit artist commentary
	// and notes; merge it into whatever block flat-tag already created.
	for _, site := range metadataSites {
		blockFor(extractor, site)["metadata"] = "artist_commentary,notes"
	}

	// The gelbooru family emits its note boxes only behind notes:true, parsed
	// from the same post page the tags fetch already loads.
	for _, site := range notesSites {
		blockFor(extractor, site)["notes"] = true
	}

	// Overlay per-site credentials, keeping any tags:true already set.
	for _, site := range cfg.Sites {
		block := blockFor(extractor, site.Name)
		// A lone username (no key) makes the danbooru/e621 family prompt for a
		// password and abort in the non-TTY subprocess, so only send it with a key.
		if site.Username != "" && site.APIKey != "" {
			block["username"] = site.Username
		}
		if site.APIKey != "" {
			block["api-key"] = site.APIKey
			// The danbooru/e621 family authenticates by HTTP Basic Auth and reads the
			// key from "password"; gelbooru reads "api-key". Set both so either works.
			block["password"] = site.APIKey
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
		if len(block) == 0 {
			// A site row carrying only a gallery or lookup order sets nothing
			// here; keep the managed file free of empty blocks.
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
	if err := config.WriteFileAtomic(path, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	}); err != nil {
		return fmt.Errorf("writing managed config: %w", err)
	}
	return nil
}

// blockFor returns a site's extractor block, creating and storing an empty one
// when absent; mutations through the returned map land in place.
func blockFor(extractor map[string]any, site string) map[string]any {
	block, _ := extractor[site].(map[string]any)
	if block == nil {
		block = map[string]any{}
		extractor[site] = block
	}
	return block
}

// mergeMaps deep-merges src into dst: nested maps merge recursively, and a
// scalar or array in src overwrites dst. Operator (src) values win.
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
