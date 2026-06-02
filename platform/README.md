# platform/ — neutral catalog core

Operates on a library you already have. **No acquisition** — nothing here fetches content;
how files arrive on disk is out of scope (and out of this repo). See
[docs/architecture.md](../docs/architecture.md#scope).

| Dir | What | State |
|---|---|---|
| `katalog-api` | Read-only catalog REST API. Serves clients. | real |
| `katalog-manager-api` | **Neutral management / write API + first-run backend.** | real |
| `transcoder` | Transcode (NVENC primary, ffmpeg fallback). | real |
| `packager` | CMAF/HLS packaging. | real |
| `metadata-enricher` | Metadata enrichment from public providers. | real |
| `analyzer` | Media probe / packaging steps. | real |
| `artwork` | Artwork blobs/derivatives. | real |

## katalog-manager-api — the write side, done neutrally

`katalog-api` reads the catalog; **`katalog-manager-api`** is the matching write/management
plane. It does two jobs:

- **First-run backend.** It implements `GET /api/manage/setup/status`, `POST
  /api/manage/setup`, and `GET`/`PUT /api/manage/config` — the contract the admin UI
  (`apps/admin`, served at `/manage`) consumes. Full shape in
  [docs/architecture.md](../docs/architecture.md#config-contract).
- **Catalog management.** It registers and manages library entries for media **already on
  disk**, and drives the processing core (transcoder, packager, enricher, analyzer,
  artwork) over the bundled event stream.

Crucially, it performs **no acquisition**: it never reaches out for content, never talks to
indexers or downloaders, and has no notion of fetching media. It manages what you already
have. That is the whole neutral line — the write path exists, but it knows nothing about how
files arrived.
