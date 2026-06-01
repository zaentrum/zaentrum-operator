# services/ — per-product backends

| Dir | What | Talks to | State |
|---|---|---|---|
| `chino-api` | Video BFF. Read consumer; owns its own watch-history DB. Serves `/api/config`, `/api/v1/*`, proxies playback. | `katalog-api` (read), `chino-stream` (bytes) | ✅ migrating |
| `chino-stream` | HLS/CMAF origin. Pre-packaged playback; ffmpeg fallback. Mints/verifies `?stream=` tokens with `chino-api`. | `katalog-api`, media store | ✅ migrating |

`musig-*` / `tv-*` services land here when those products are real.

**Contract that must hold (operator-facing):** `STREAM_SIGNING_KEY` is shared between
`chino-api` (mint) and `chino-stream` (verify). Mismatch ⇒ playback 403 while artwork
loads. See [docs/self-hosting.md](../docs/self-hosting.md#operator-contract).
