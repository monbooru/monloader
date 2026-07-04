# Metadata mapping

gallery-dl returns each post's fields in the source booru's own vocabulary.
monloader maps that onto monbooru's data model: tags with category prefixes, a
rating tag, a source label, and a canonical URL. The mapping is driven by
curated site profiles plus a generic fallback, with user override tables for
the edge cases.

## Tags by category

monbooru's built-in categories are general, character, artist, copyright, meta,
medium, person, and year. gallery-dl exposes per-category tags as
`tags_<category>` fields. Each is routed:

| gallery-dl suffix | monbooru category | notes |
|---|---|---|
| general | general | |
| artist | artist | |
| character | character | |
| copyright | copyright | |
| meta / metadata | meta | gelbooru uses `metadata` |
| species (e621) | general | no monbooru equivalent (lossy by default) |
| lore (e621) | meta | |
| contributor (e621) | artist | |
| circle / group | artist | doujin circle / group |
| parody / series | copyright | the parodied / source work |
| studio | copyright | production studio |
| model | person | real-person model (realbooru, photo boorus) |
| faults (moebooru) | meta | |
| invalid, deprecated | dropped | not real tags |
| anything else | general | best-effort fallback |

Each mapped tag is sent as `category:name` (e.g. `artist:paruperu`,
`copyright:original`) or bare `name` for general. monbooru auto-creates missing
tags; any it rejects come back as `tag_warnings`, recorded on the item without
failing the push.

Tag names are normalized to monbooru's `lower_snake_case` first - lowercased,
with any run of characters monbooru's tag rules reject collapsed to an
underscore and the ends trimmed (`fate/grand_order` -> `fate_grand_order`). A name with nothing
representable left (CJK-only for instance) is dropped.

## Rating

monbooru ratings are ordered general < sensitive < questionable < explicit. The
full-word forms (general, sensitive, questionable, explicit, and safe) map 1:1
across every family, case-insensitively (safe -> general). The catch is the
single letter `s`: on Danbooru it means "sensitive", everywhere else it means
"safe". So the letter mapping is per family, not a global letter map.

**NSFW manga.** Manga/comic galleries carry no per-post rating. Leaving them
unrated would surface them under a safe ceiling, so NSFW only profiles
(nhentai, hitomi, exhentai, the *hentai* sites, 8muses, ...) set a
`default_rating` of `explicit` that applies only when the source gives no
rating. A real source rating always wins.

## Source, URL, and origin

| monbooru field | value |
|---|---|
| `url` | the booru post page, built from the profile's `post_url_template` (e.g. `https://danbooru.donmai.us/posts/{id}`) |
| `source` | the site name, so `source:danbooru` filtering works in monbooru |
| `via` | `monloader` (stored on `images.origin` and each initial tag's `tagger_name`) |
| target gallery | the per-source gallery, sent as `?gallery=<name>` |
| `collection` / `collection_order` | the pool name and page order, when pushing a pool as a collection |

A bare media URL (gallery-dl's `directlink` extractor) has no booru post: the
`source` is the file's host and the `url` is the file URL itself.

## Commentary and notes

Danbooru-family posts carry the artist's **commentary** and some families carry positional
**notes** boxes overlaid on the image. Note bodies are reduced to plain text and pushed with their pixel coordinates.
Monbooru overwrites a source's commentary and notes on each re-pull.

## Override tables

When a profile's default is wrong for your library, override it in
`monloader.toml`.
An override wins over the profile and takes effect on the next download:

```toml
# route a gallery-dl tag category to a different monbooru category for one site
[[tag_overrides]]
site = "e621"
from = "species"
to   = "general"

# route a booru rating value to a monbooru rating for one site
[[rating_overrides]]
site = "somebooru"
from = "x"
to   = "explicit"
```

`site` is the gallery-dl category. A `tag_override` matches a `tags_<category>`
suffix (its `from`) and reroutes it; a `rating_override` matches a raw booru
rating value (case-insensitively) and remaps it. Either wins over both the
curated profile and the built-in family rule.

## Adding a site

Mappings are data, not code, so tracking gallery-dl's growing list is a data
change. There are two levels:

- **No profile needed.** Any gallery-dl category without a curated entry uses
  the generic fallback: `tags_<category>` route by name where they match a
  monbooru category (else general), ratings use the tolerant rules with `s`
  treated as safe, and the post URL is whatever gallery-dl reports. So a site
  works the day gallery-dl supports it; a profile only sharpens the result. The
  override tables above are the escape hatch for a generic site that needs a
  tweak, and they take effect with no release.

- **A curated profile.** To sharpen a site, add one entry to
  `internal/mapping/profiles.json`, keyed by the gallery-dl category. Profiles
  are embedded at build time, so a new one ships with a rebuild (unlike the
  override tables). A profile's fields:

  | field | purpose |
  |---|---|
  | `family` | `danbooru`, `e621`, `moebooru`, `gelbooru_v02`, `philomena`, or `generic` - picks the rating semantics and the per-category tag regime |
  | `kind` | `booru` (default) or `manga` - a manga gallery bundles its pages into one cbz |
  | `post_url_template` | the canonical post URL with `{id}` substituted (e.g. `https://danbooru.donmai.us/posts/{id}`) |
  | `auth` | `none`, `api_optional`, `api_required`, or `cookies` - drives the settings login indicator |
  | `example` | a representative URL for the per-site test probe |
  | `category_overrides` | per-suffix tag-category remaps baked into the profile |
  | `rating_overrides` | per-value rating remaps baked into the profile |
  | `default_rating` | rating used only when the source gives none  |
  | `needs_tags` | a generic-family site that needs gallery-dl's `tags: true` to emit per-category tags (one extra request per post, e.g. sankaku) |
  | `has_notes` | a generic-family site whose extractor emits note boxes with gallery-dl's `notes: true` (e.g. sankaku); the note-carrying booru families get it by family |