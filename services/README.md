# services/ — per-product backends

| Dir | What | Talks to | State |
|---|---|---|---|
| `chino-api` | Video BFF. Read consumer; owns its own watch-history DB. Serves `/api/config`, `/api/v1/*`, proxies playback. | `katalog-api` (read), `chino-stream` (bytes) | real |
| `chino-stream` | HLS/CMAF origin. Pre-packaged playback; ffmpeg fallback. Mints/verifies `?stream=` tokens with `chino-api`. | `katalog-api`, media store | real |

`musig-*` / `tv-*` services land here when those products are real.

## Related: the management API

The neutral **management / write API** is **`katalog-manager-api`**, which lives in
[`platform/`](../platform/README.md). It is the first-run backend (it implements
`/api/manage/setup/*` and `/api/manage/config`) and the catalog write path for files that
are already on disk. It performs **no acquisition** — it only registers and manages the
library you already have. The admin UI (`apps/admin`, served at `/manage`) is its client;
the contract they share is in
[docs/architecture.md](../docs/architecture.md#config-contract).

**Contract that must hold (operator-facing):** the stream-signing key is shared between
`chino-api` (mint) and `chino-stream` (verify). First-run setup generates it via
`katalog-manager-api`; a mismatch ⇒ playback 403 while artwork loads. See
[docs/self-hosting.md](../docs/self-hosting.md#operator-contract).
