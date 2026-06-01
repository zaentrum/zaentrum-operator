# platform/ — neutral catalog core

Operates on a library you already have. **No acquisition** — nothing here fetches content;
how files arrive on disk is out of scope (and out of this repo). See
[docs/architecture.md](../docs/architecture.md#scope).

| Dir | What | State |
|---|---|---|
| `katalog-api` | Read-only catalog REST API. Serves clients. | ✅ migrating |
| `transcoder` | Transcode (NVENC primary, ffmpeg fallback). | ✅ migrating |
| `packager` | CMAF/HLS packaging (shaka-style). | ✅ migrating |
| `metadata-enricher` | Metadata (e.g. TMDB) enrichment. | ✅ migrating |
| `analyzer` | Media probe / packaging steps. | ✅ migrating |
| `artwork` | Artwork blobs/derivatives. | ✅ migrating |
| _import_ | **NEW (to build):** scan a folder of files you own → catalog. Neutral replacement for the acquisition-coupled write path. | 🚧 |

The single new neutral component this project needs is **import**: the catalog write/
ingest path minus any acquisition knowledge. The private operator's acquisition stack
becomes just one more producer behind it.
