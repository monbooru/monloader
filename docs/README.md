# monloader docs

Setup and operation guides for monloader; the API reference is self-served in the app at
`/api/v1/docs`.

- [Installing](INSTALL.md) - Docker, volumes, environment variables, custom CSS, log
  levels.
- [How a download flows](PIPELINE.md) - the resolve -> download -> map -> push
  pipeline, per-item outcomes and error codes, pools and
  cbz bundling.
- [Sites and credentials](SITES.md) - supported sites, the per-family auth
  kinds, cookies files, the per-site test probe, and per-source target
  galleries.
- [Metadata mapping](MAPPING.md) - how booru tags, ratings, and sources become
  monbooru fields, the override tables, and adding a site.
- [Hydrus PTR tag lookup](PTR.md) - sha256-keyed tag lookup from a
  local copy of the Hydrus Public Tag Repository
- [REST API](API.md) - the JSON API, the optional UI password, and
  the bearer token.
- [Building from source](BUILDING.md) - Go build, CLI flags, the gallery-dl
  dependency.
