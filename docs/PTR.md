# Hydrus PTR tag lookup

monloader can answer "what are this file's tags?" from its sha256 alone, using
a local copy of the [Hydrus Public Tag Repository](https://hydrusnetwork.github.io/hydrus/PTR.html)
(the PTR) - a community tag database of billions of tag-to-file mappings. It is
**off by default** and downloads nothing until you enable it.

This is the sha256-keyed companion to the booru md5 lookup in
[PIPELINE.md](PIPELINE.md): the booru lookup is instant and needs no local
data but only finds files still hosted on a booru; the PTR also knows files
that were deleted or never on a booru, at the cost of a large local index.

## What it costs

The PTR has no per-hash query API - by design, so its server cannot learn which
files you have. Using it means streaming the repository's whole tag history once
and keeping a local index:

- **Initial download:** fetched as thousands of small update files over hours.
- **Local index:** tens of GB of SQLite under a dedicated volume (expect >70GB). monloader
  refuses to start the initial sync when that volume has less than
  `[ptr].min_free_gb` (default 80) free.
- **Steady state:** one small update roughly every day once caught up.

The index lives on its own volume and can be deleted and re-synced at any time.

## Enabling it

1. Mount a dedicated volume at `/ptr` (see the commented block in
   `docker/docker-compose.yml` or `docker/monloader.container`). The PTR page
   warns while the data path does not exist: without a volume mounted there,
   the index lands inside the container and is lost when the container is
   recreated.
2. Open **PTR** in the top bar and read the disabled-state summary (the
   repository, the data path, and your free space against the ~80 GB the
   initial sync needs).
3. Click **enable ptr sync**. The page then shows live progress; leave it to
   catch up (hours). You can **pause** and **resume**; deleting the index
   (settings -> **ptr**) turns the lookup off and reclaims the disk. 


## Using it

Once the index has data (it answers during the sync, on whatever it has so
far), a sha256 lookup resolves the file's tags through the PTR's alias
(sibling) and implication (parent) graph, mapping hydrus namespaces onto
monbooru categories, and folds them into the image:

```bash
curl -H "Authorization: Bearer <token>" \
     -X POST http://localhost:8081/api/v1/lookup \
     -d '{"image_id":42,"backend":"ptr","sha256":"<64 hex>"}'
```

This is the `ptr` backend of the lookup endpoint monbooru's "fetch tags" button
calls; monbooru shows that button only when `GET /api/v1/ptr/status` reports the
PTR enabled. Each lookup shows up as a job on the **Queue** page like any other.

The synced alias / implication graph is also queryable, so monbooru can sweep
its own tag list and propose aliases and implications:

```bash
curl -H "Authorization: Bearer <token>" \
     -X POST http://localhost:8081/api/v1/ptr/tags \
     -d '{"tags":["blonde_hair","artist:some_name"]}'
```

Each tag comes back with its PTR ideal spelling, the tags that alias to it, and
its implications, all in monbooru form; up to 500 tags per call.

## Configuration

The `[ptr]` block in `monloader.toml` (all keys have `MONLOADER_PTR_*` env
overrides):

```toml
[ptr]
enabled     = false                          
data_path   = "/ptr"                          # dedicated index volume 
address     = "https://ptr.hydrus.network:45871"
access_key  = ""                              # empty = the PTR's public read-only key
fetch_sleep = 1.0                             # seconds between update downloads
min_free_gb = 80                              # refuse the initial sync below this free space
```

These keys are also editable from the settings page's **ptr** section; the sync engine reads them at boot, so changes apply on restart. The confirmed "delete ptr data" button lives there too.