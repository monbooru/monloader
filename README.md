# monloader

Download images from the web into [monbooru](https://github.com/leqwin/monbooru).  
Paste a direct URL, an image or search from an online booru/gallery or [any site supported by gallery-dl](https://github.com/mikf/gallery-dl/blob/master/docs/supportedsites.md) and monloader fetches the files and per-post metadata, maps it onto monbooru's data model, and pushes each file into a monbooru gallery over the REST API.

<table>
  <tr>
    <td><img src=".github/assets/add.webp" width="400"/></td>
    <td><img src=".github/assets/queue.webp" width="400"/></td>
  </tr>
</table>

---

## Features

- **Download images** A direct link to an image, or anything else a gallery-dl extractor matches : booru post, pool, tag search, artist page. 
- **Pools and manga.** A booru pool's pages import as an ordered collection; a manga or comic gallery bundles into a single `.cbz` for monbooru's reader.
- **Metadata mapped to monbooru.** Tags by category (artist / character / copyright / meta / ...), rating, and source, normalized across booru families so tags land the way monbooru expects them.
- **50+ curated sites, plus a fallback.** Profiles for the danbooru, e621, moebooru, gelbooru, and philomena families and a set of manga/comic sites; anything else gallery-dl supports still works through a generic fallback.
- **Queue management.** Each download shows whether it was added, was already in your library, got skipped, or failed so you can see at a glance what happened to every item. Re-submitting a search skips posts already fetched, while re-submitting a single post refreshes it, folding any new tags into the image monbooru already holds. Pause and resume downloads globally from the topbar (or the monsender popup) to queue up a batch before it starts.

---

## Related applications

monloader is a companion downloader for monbooru. It fetches images from the web and pushes them into your library :

```mermaid
flowchart LR
    web["- Any booru or gallery supported by gallery-dl<br/>- Direct image URL"]
    sender["<b>monsender</b><br/>browser extension"]
    loader(["<b>monloader</b><br/>downloader"])
    booru["<b>monbooru</b><br/>Your self-hosted booru"]

    web -->|browse| sender
    sender -->|REST API| loader
    web -.->|paste URL| loader
    loader -->|REST API| booru

    classDef hub  fill:#5c6bc0,stroke:#9fa8da,stroke-width:3px,color:#ffffff;
    classDef tool fill:#16161c,stroke:#5c6bc0,stroke-width:1.5px,color:#e2e2e8;
    classDef src  fill:#16161c,stroke:#8888a0,stroke-width:1px,color:#8888a0;

    class loader hub;
    class sender,booru tool;
    class web src;
```

- **[monbooru](https://github.com/leqwin/monbooru)** : self-hosted booru; organizes, tags, and serves your collection.
- **monloader** : this application; fetches files and per-post metadata (via gallery-dl) and pushes them into a monbooru gallery over the REST API.
- **[monsender](https://github.com/leqwin/monsender)** : browser extension; sends the URL of the page you're currently browsing to monloader.

---

## Quick start

monloader ships in monbooru's `docker/docker-compose.yml` :

1. Uncomment the `monloader` service in monbooru's compose and start it:
   ```bash
   docker compose up -d monloader
   ```
2. Open monloader at `http://localhost:8081`, go to **Settings -> monbooru**, and click **connect to monbooru** (the default api url `http://monbooru:8080` works on the shared compose network).
3. In monbooru, approve the pairing request in its monloader settings. monloader stores the token monbooru issues; there are no keys to copy by hand.
4. Back in monloader, pick a **default gallery** and **save**.
5. Paste a URL into the command bar on the home screen and press Enter.

See [docs/README.md](docs/README.md) for installation, configuration, sites and credentials, metadata mapping, the REST API, and building from source.

---

## Warning

> **Intended for local network use.** monloader's UI is not designed to be exposed to the public internet.

---

## Acknowledgements

monloader is mostly a wrapper for [gallery-dl](https://github.com/mikf/gallery-dl), which does the actual scraping. monloader adds queue management, maps that output onto monbooru's data model and pushes it to over the API.
