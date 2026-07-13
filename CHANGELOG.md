# Changelog

## [v1.4.0] - 2026-07-13
### Added
- A new PTR page syncs the hydrus Public Tag Repository to a local hash-and-tag index. ([#7](https://github.com/leqwin/monloader/issues/7))
- Imported posts now record their original artist source and link derivatives to their parent post. ([#8](https://github.com/leqwin/monloader/issues/8))
- Look up a post by its md5 hash to import it or enrich an existing image. 
- Reverse-image lookup finds candidate posts on iqdb and SauceNAO when no hash matches.
- Order the lookup sources into a chain and test each one from Settings.

### Changed
- Bundled gallery-dl updated to 1.32.6.

### Fixed
- Bottom bar is always visible ([#10](https://github.com/leqwin/monloader/issues/10))
- Manga imports no longer split multi-word artist or group names into separate tags.
- Imported commentary and notes no longer drop text or leave stray HTML markup.
- Confirming a queue action no longer silently fails when the queue list refreshes.

## [v1.3.0] - 2026-07-04
### Added
- Queue rows show batch progress next to the item count. ([#3](https://github.com/leqwin/monloader/issues/3))
- Pause and resume downloads from the topbar. ([#4](https://github.com/leqwin/monloader/issues/4))
- Re-submit a post to refresh its metadata, merging new tags into the existing image. ([#6](https://github.com/leqwin/monloader/issues/6))
- Imported posts now carry artist commentary and notes from boorus that provide them.

### Fixed
- Manga and comic imports no longer put CBZ pages out of order on some sites.
- The footer connection light no longer flickers to "checking" on every page load.

### Internal
- Metadata refresh and commentary/notes import require monbooru v1.13.0 or newer.

## [v1.2.0] - 2026-07-01
This update include a breaking change with the monbooru and monsender pairing mechanism. Update monbooru to >v1.12.0 and monsender to >v1.2.0 and use the pairing mechanism.

### Added
- Named, scoped API tokens: create, name, and set per-token privileges in Settings.
- One-click pairing to connect and disconnect your monbooru instance.
- Approve or deny browser-extension pairing requests from Settings.

### Changed
- Every API endpoint now requires a scoped bearer token.
- Settings reorganized with a section nav sidebar and monbooru-matched layout.

### Fixed
- Authenticated danbooru/e621-family downloads work again instead of returning nothing.
- Wide queue and sites tables no longer scroll the whole page sideways.

### Removed
- The `auth.api_token` config value and `MONLOADER_AUTH_API_TOKEN` env; create a named token instead.
- The manual monbooru API-token field in Settings; pair instead (env override still works).

## [v1.1.2] - 2026-06-23
### Changed
- Links to your monbooru instance open in the same tab instead of a new one.
- Topbar logo, spacing, and labels restyled to match monbooru.
- Bundled gallery-dl updated to 1.32.4.

## [v1.1.1] - 2026-06-13
### Added
- Settings warns when the default gallery is unset or not found on monbooru.

### Changed
- DNS or unreachable-host failures now report as `network_unreachable` instead of a generic download failure.
- API rejects non-http(s) URLs and unknown status filters with a 400 instead of accepting them.

### Fixed
- Imports now use monbooru's active gallery instead of requiring one named "default".
- A plain 403 no longer mislabels a download failure as needing credentials.
- Testing the monbooru connection no longer clears the token you just typed.
- Queue rows show live counts while a job runs instead of zeros until it finishes.
- A per-job destination folder now applies to manga/CBZ imports instead of being ignored.
- Continuing a capped search no longer re-fetches windows an earlier continuation already took.

## [v1.1.0] - 2026-06-11
### Added
- Topbar and landing-page links to your monbooru instance.
- The footer connection indicator shows the connected monbooru's version.
- "Get all" action fetches a capped search to the end, continuing automatically.
- `monloader healthcheck` subcommand, so the container healthcheck no longer needs curl.
- API: queue jobs expose their continuation-series root and a continue-all endpoint.

### Changed
- Long job item lists fold into a "+N more" toggle.
- A capped search and its continuation windows now show as one queue row.
- Retry and force-download actions appear only when they would do something.
- Destructive actions confirm through an in-page dialog instead of the browser popup.
- UI nav, contrast, and settings cards restyled to match monbooru.
- Container image rebased on Debian 13 (Python 3.13).

### Fixed
- Retrying or re-adding a URL that failed to import re-downloads it instead of being skipped.
- Very long URLs or sources are trimmed to monbooru's limits instead of failing the import.
- Queue "view" links no longer open the wrong image after a deletion in monbooru.
- The politeness delay now throttles each file download, so a multi-file fetch no longer bursts.

## [v1.0.1] - 2026-06-07
### Added
- Import a manga or comic title as one CBZ per chapter.

### Changed
- Queue and history items now show as compact, aligned one-line rows.

### Fixed
- Forum threads and dispatcher pages with off-site images now download instead of silently matching nothing.
- Media monbooru cannot ingest (audio, SVG, AVIF) is skipped or declined instead of failing the job.

## [v1.0.0] - 2026-06-06
### Added
- Initial release.
