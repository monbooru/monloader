# Changelog

## [v1.9.0] - 2026-08-13
### Added
- User can install or roll back a gallery-dl release from Settings, without waiting for a new image. ([#24](https://github.com/monbooru/monloader/issues/24))
- Successful downloads clear from the history sooner (3 days by default). ([#23](https://github.com/monbooru/monloader/issues/23))
- Add a "clear succeeded" button on the queue. ([#23](https://github.com/monbooru/monloader/issues/23))
- The container image now runs on arm64 as well as amd64.

### Changed
- The default full-history window is now 14 days, up from 7. (So by default succeeded downloads disappear after 3 days, and failed ones after 14 days)
- Each settings field's hint includes its built-in default.
- Some perf improvement for PTR tag batch.
- Bundled gallery-dl updated to 1.32.9.

### Fixed
- monloader builds on Windows again, with the PTR free-space check working there. ([#22](https://github.com/monbooru/monloader/issues/22))
- A manga gallery that re-resolves short is no longer pushed as a complete `.cbz`.
- A job canceled at shutdown stays canceled instead of running again on the next start.
- The add bar and the API accept an upper-case `md5:` prefix and a padded URL.
- Saving a site clears an emptied credential and leaves fields the form omitted alone.
- The iqdb chain row names whichever danbooru credential is missing, username or api key.
- Clicking connect twice in the extension no longer creates two pairings.

Thanks to @gary-host-laptop for the suggestion (https://github.com/monbooru/monloader/issues/24).
Thanks to @CeareDelafont for the suggestion (https://github.com/monbooru/monloader/issues/23).
Thanks to @QiE2035 for the fix (https://github.com/monbooru/monloader/issues/22).

Co-authored-by: 企鹅2035 <qie2035@qq.com>

## [v1.8.0] - 2026-08-10
### Added
- Backend for scheduled lookups: monbooru can check up to 100 hashes against the PTR index in one call, scheduled lookups run behind any active lookups, daily budget for scheduled lookups (25 images by default). ([monbooru#65](https://github.com/monbooru/monbooru/issues/65))
- A batch PTR lookup files one history row with its asked and matched counts.

### Changed
- The queue orders rows by when each last changed, matching the timestamp it shows.

### Fixed
- Canceling a lookup, refetch or replace tells monbooru instead of leaving it waiting.
- A complete PTR index answers lookups right after a restart instead of refusing them.
- The sites API no longer lists gallery-dl base extractors that name no site.

Thanks to @gary-host-laptop for the suggestion (https://github.com/monbooru/monbooru/issues/65).

## [v1.7.0] - 2026-07-31
### Added
- Add any of gallery-dl's ~300 sites from Settings, not just the shipped profiles. ([#21](https://github.com/monbooru/monloader/issues/21))
- Edit a site's mapping profile in Settings: family, rating map, tag categories, templates. ([#21](https://github.com/monbooru/monloader/issues/21))
- Per-site tag rules suppress a tag, rename it, or retarget its category. ([#21](https://github.com/monbooru/monloader/issues/21))
- Paste a browser cookies export into the site dialog instead of placing a file yourself. ([#16](https://github.com/monbooru/monloader/issues/16))
- Per-site gallery-dl options.
- Choose the source label a site's pushes carry, per site or per host. ([monsender#3](https://github.com/monbooru/monsender/issues/3))
- Export a site's profile as a shareable file; `/config/profiles/` overrides a shipped mapping.
- Add a PTR contributions chart for daily contribution activity.
- The sites API reports each site's display name, kind, and configured state.

### Changed
- Settings lists only the sites you customized, grouped into boorus, manga and other.
- A push from a site's CDN or mirror host records the site, not the bare host.

### Fixed
- Pausing the monbooru link also holds retry, force download, and get next / get all.
- The queue table stays readable on a narrow window instead of breaking the url apart. ([#18](https://github.com/monbooru/monloader/issues/18))
- An API enqueue with no monbooru configured is refused instead of accepted and failed.
- The monbooru URLs and the default folder are validated instead of saved as typed.
- A job still running at shutdown settles interrupted, so its requeue survives the restart.
- PTR enable, disable and delete no longer overlap when clicked in quick succession.
- PTR sync bounds a manifest entry by size, so a big update stops spiking memory.
- A crash during a settings save can no longer leave the config file truncated.

Thanks to @gary-host-laptop for the suggestions (https://github.com/monbooru/monbooru/issues/73, https://github.com/monbooru/monloader/issues/21) and the report (https://github.com/monbooru/monsender/issues/3).
Thanks to @JustRoxy for the suggestion (https://github.com/monbooru/monloader/issues/16).
Thanks to @CeareDelafont for the report (https://github.com/monbooru/monloader/issues/18).

## [v1.6.0] - 2026-07-26
### Added
- Replace jobs download a post's file and push it over an existing monbooru image.
- API enqueues can pass a `page_url` recorded as a direct file link's source.
- Click the footer light to pause the monbooru link; new URLs are refused until resumed.
- PTR sync can be turned off from Settings and resumed later without deleting the index.
- The PTR page shows when the last update was applied.
- Docs include a link to download a pre-synced PTR database. ([#17](https://github.com/monbooru/monloader/issues/17))

### Changed
- PTR lookups wait for a caught-up index instead of answering a partial tag set.
- The queue's clear and cancel-pending buttons show only when they have work to act on.
- The PTR free-space floor now defaults to 70 GB.
- Bundled gallery-dl updated to 1.32.8.

### Fixed
- Force download on a collapsed search re-fetches the whole series, not just the newest window.
- Cancelling a job that never started keeps a canceled row instead of deleting it.
- A rejected raw gallery-dl config returns for editing instead of being wiped.
- Cleared or malformed download settings are refused instead of reporting saved over the old values.
- The contributions card refreshes after account creation and while a send is in flight.
- A rebuilt PTR index reads as provisional again until its first sync catches up.
- Booru lookups and metadata refetches link their queue row at the exact image they enriched.
- The site probe rejects non-http(s) URLs instead of passing them to gallery-dl.
- Contribution previews no longer repeat hundreds of index queries per submitted tag.
- A stranded pairing attempt can be canceled or reset from Settings.
- A cancel landing mid-resolve or mid-enrich labels the item canceled instead of failed.
- PTR sync progress resumes from the cursor after a restart instead of restarting at zero. ([#7](https://github.com/monbooru/monloader/issues/7))
- Sites without a profile record the extractor's permalink as the pushed source.
- A similarity match records the post's md5, so later hash lookups find the image.

## [v1.5.0] - 2026-07-21
### Added
- Contribute tags, aliases, and implications back to the hydrus PTR from monbooru. ([#11](https://github.com/monbooru/monloader/issues/11))
- The PTR page creates a personal contribution account and logs every contribution sent. ([#11](https://github.com/monbooru/monloader/issues/11))
- Queue rows link out to the source post and to the monbooru image. ([#14](https://github.com/monbooru/monloader/issues/14))
- The queue and its history survive a restart; an interrupted job comes back with a requeue.
- Cancel every pending job at once from the queue toolbar.

### Changed
- PTR sync progress tracks the work actually left, with separate download and index speeds.  ([#7](https://github.com/monbooru/monloader/issues/7))
- Queue rows gain a type column, one aligned line per item, and only non-zero counts.
- e621 and PTR species tags land in monbooru's species category instead of general.
- monbooru and monsender pairing share one Settings section.

### Fixed
- PTR sync no longer stalls for good on the large manifest entries the repository serves. ([#7](https://github.com/monbooru/monloader/issues/7))
- Long URLs no longer push the queue's actions column off-screen. ([#12](https://github.com/monbooru/monloader/issues/12))
- Accented and CJK tags now reach monbooru instead of being dropped.
- PTR lookups no longer import bookkeeping namespaces as opaque general tags.
- PTR tag suggestions no longer offer implications the repository never declared.
- Retrying a canceled or interrupted job no longer skips posts that never reached monbooru.
- Cancel on a queue row no longer deletes the history of a job that just finished.
- A lookup's view link opens the image it enriched, whichever gallery holds it.
- A pool that resolves exactly at the item cap no longer finishes as "no posts matched".
- A download that fails partway pushes the files that landed instead of failing them all.

### Internal
- Contributing to the PTR requires monbooru v1.15.0 or newer.

## [v1.4.1] - 2026-07-14
### Added
- Queue rows show how long a job has been in its current status.
- Finished jobs age out of history after `downloader.history_retention_days` (default 7).

### Changed
- Displayed timestamps follow the TZ timezone instead of always showing UTC.
- User documentation moved.

### Fixed
- A fully-synced PTR index no longer holds ~500 MB resident while sitting idle.

## [v1.4.0] - 2026-07-13
### Added
- A new PTR page syncs the hydrus Public Tag Repository to a local hash-and-tag index. ([#7](https://github.com/monbooru/monloader/issues/7))
- Imported posts now record their original artist source and link derivatives to their parent post. ([#8](https://github.com/monbooru/monloader/issues/8))
- Look up a post by its md5 hash to import it or enrich an existing image. 
- Reverse-image lookup finds candidate posts on iqdb and SauceNAO when no hash matches.
- Order the lookup sources into a chain and test each one from Settings.

### Changed
- Bundled gallery-dl updated to 1.32.6.

### Fixed
- Bottom bar is always visible ([#10](https://github.com/monbooru/monloader/issues/10))
- Manga imports no longer split multi-word artist or group names into separate tags.
- Imported commentary and notes no longer drop text or leave stray HTML markup.
- Confirming a queue action no longer silently fails when the queue list refreshes.

## [v1.3.0] - 2026-07-04
### Added
- Queue rows show batch progress next to the item count. ([#3](https://github.com/monbooru/monloader/issues/3))
- Pause and resume downloads from the topbar. ([#4](https://github.com/monbooru/monloader/issues/4))
- Re-submit a post to refresh its metadata, merging new tags into the existing image. ([#6](https://github.com/monbooru/monloader/issues/6))
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
