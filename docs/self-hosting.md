# Self-hosting Stube

Stube is a media server for a library **you own and are entitled to stream**. It ships no
content and no way to acquire content — you point it at your own files.

## Quick start (appliance)

```bash
git clone https://github.com/nalet/stube
cd stube
./deploy/k3s/up.sh
```

This boots a single-node Kubernetes cluster (k3d) and applies `deploy/base`. When it
finishes it prints the URL. On first run the client asks for your server address and
discovers the rest.

For a lighter setup, `docker compose -f deploy/compose/docker-compose.yml up -d`.

## Operator contract

Stube clients are neutral — they're told where the server is at runtime. You, the
operator, provide three things:

1. **A `/api/config` response** from `chino-api` (unauthenticated):
   ```json
   { "apiBase": "https://stube.example.com",
     "oidcIssuer": "https://auth.example.com/realms/stube",
     "oidcClientId": "chino" }
   ```
   Clients read this, then use OIDC `.well-known` discovery — any OIDC provider works,
   not just Keycloak.

2. **A public OIDC client** registered on your provider with:
   - the **device authorization grant** (RFC 8628) enabled — this is the default sign-in
     on every client (no per-device redirect URIs to register), and
   - `offline_access` so refresh tokens are issued.

3. **A shared `STREAM_SIGNING_KEY`**, set identically on `chino-api` (mints) and
   `chino-stream` (verifies):
   ```bash
   k=$(openssl rand -base64 32)
   kubectl -n stube create secret generic stube-stream-signing --from-literal=key="$k"
   ```
   Mismatch ⇒ playback returns 403 while artwork still loads — the classic symptom.

## Getting content in

Use the neutral **import** path: point Stube at a directory of media you own; it scans,
enriches metadata, and (optionally) transcodes/packages for adaptive streaming.

> Stube intentionally has **no** built-in downloaders or indexer integrations. How files
> arrive on disk is out of scope — and out of this project.

## Running the public demo (`demo-stube`)

The `overlays/demo` overlay stands up a public demo. **It must serve only content you can
distribute** — Creative-Commons (e.g. the Blender open movies: *Big Buck Bunny*, *Sintel*,
*Tears of Steel*), public-domain titles, or your own. **Never a private/licensed library.**

The demo also:
- uses an **isolated auth realm** with a shared demo login (not real user accounts),
- has **telemetry disabled**,
- ships content **pre-packaged as static CMAF**, so it needs no transcoder/GPU.

This same clean content set is what you hand app-store reviewers for sign-in access.

## GPU (optional)

The base runs software ffmpeg. For hardware transcoding, apply the GPU overlay and install
the NVIDIA device plugin on a GPU node. Without a GPU, everything still works — just slower
on large 4K transcodes.
