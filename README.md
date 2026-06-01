# Stube

**Self-hosted media platform.** Bring your own library; stream it to clean, neutral
clients on the web, your phone/tablet, and your TV.

Stube is the *platform*. **Chino** (video) is its first client. **Musig** (music) and
**TV** (live) are planned — the clients are skins over one shared core.

> Stube is a neutral media client + server, in the same category as Jellyfin, Plex,
> Emby and Kodi. It ships **no media and no content sources** — you point it at a
> library you own and are entitled to stream.

---

## Status

| Component | What it is | State |
|---|---|---|
| **chino** (web · mobile · androidtv) | Video client | ✅ Real — the reference product |
| **chino-api** / **chino-stream** | Product BFF + HLS/CMAF origin | ✅ Real |
| **katalog-api** + processing (transcoder, packager, enricher, analyzer, artwork) | Neutral catalog core | ✅ Real |
| **musig** / **tv** | Music / live-TV clients | 🚧 Scaffold — slots reserved, not yet built |
| One-command appliance (k3s/k3d) | All-in-one self-host | 🚧 In progress |

## Quick start

Two front doors, one source of truth (`deploy/`):

```bash
# A) All-in-one appliance — real Kubernetes, packed in one (k3d = k3s in Docker)
./deploy/k3s/up.sh            # boots a single-node cluster and applies deploy/base

# B) Docker Compose — lighter, for a NAS / small box
docker compose -f deploy/compose/docker-compose.yml up -d
```

Then open the printed URL, complete first-run server setup, and sign in via the
device-code flow. See **[docs/self-hosting.md](docs/self-hosting.md)**.

## Repository layout

```
apps/        core (shared client) · chino · musig · tv      ← clients
services/    chino-api · chino-stream                        ← per-product BFF + stream origin
platform/    katalog-api · transcoder · packager · ...       ← neutral catalog core
deploy/      base (vanilla k8s) · overlays · k3s · compose    ← single source of truth for deploy
docs/        architecture · self-hosting (operator contract)
```

## What is deliberately **not** here

Stube is content-neutral. Anything that *acquires* content (automated downloaders,
indexer integrations, the *arr stack) is **not** part of this project and never will be.
This repo is a media server + clients, nothing more. See
[docs/architecture.md](docs/architecture.md#scope).

## License

**TBD — decision required before the first public commit.** GPL/AGPL is the category
norm (Jellyfin, Kodi) but **conflicts with the Apple App Store** for the iOS client;
MPL-2.0 or Apache-2.0 are store-safe. See [docs/architecture.md](docs/architecture.md#license).
