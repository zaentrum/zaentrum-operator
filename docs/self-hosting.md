# Self-hosting Stube

Stube is a media server for a library **you own and are entitled to stream**. It ships no
content and no way to acquire content — you point it at your own files.

## Quick start (all-in-one)

```bash
docker run -d --privileged -p 80:80 --name stube ghcr.io/nalet/stube:latest
open http://stube.localhost
```

Open **http://stube.localhost**. Modern browsers auto-resolve `*.localhost` to `127.0.0.1`,
so this works with **no `/etc/hosts` edit**, and the issuer host matches the host you reach
Stube at — the same invariant a real cluster holds.

Sign-in uses the **bundled Keycloak** (realm `stube`): log in with its admin account
(`admin` / `dev` by default), which forces a password change on first login. On first boot
nothing is configured, so the app sends you to the setup wizard at **`/manage/setup`** —
give it a display name, your OIDC provider, and your library path, and you are running.

### Running under a different name

If you reach the box by another name (a LAN hostname, a public domain, or its IP), the OIDC
issuer host must equal that name — both the browser and in-cluster validation use it. Set it
consistently in four places:

| Where | What | Default |
|---|---|---|
| `deploy/base/ingress.yaml` | `spec.rules[0].host` | `stube.localhost` |
| `stube-env` ConfigMap | `OIDC_ISSUER` | `http://stube.localhost/auth/realms/stube` |
| `stube-keycloak-config` ConfigMap | `KC_HOSTNAME` | `http://stube.localhost/auth` |
| all-in-one container | `STUBE_ISSUER_HOST` env (drives the in-cluster CoreDNS rewrite) | `stube.localhost` |

On the appliance the first three are baked into the image; for a one-off host change pass
`-e STUBE_ISSUER_HOST=my.host` only if you also rebuild with the matching ingress/issuer, or
rebuild the image with all four aligned. On a real cluster the operator sets all four to the
same name and lets cluster DNS resolve it.

That one container holds the whole product: a full Kubernetes (k3s) running in-process with
the web app, admin UI, catalog, transcode/package, streaming, and **bundled Postgres,
Valkey, and Kafka**. Nothing else to install.

## Scale out to a real cluster

The same manifests run anywhere:

```bash
kubectl apply -k deploy/base
```

`deploy/base` is vanilla Kubernetes (Deployment + Service + Ingress). The all-in-one image
is just this base wrapped around an in-process k3s — so what you test locally is what runs
in production.

## First-run setup

The first-run wizard is served by the admin UI at `/manage/setup` and backed by
`katalog-manager-api`. The flow is driven by one status endpoint:

```
GET /api/manage/setup/status
    -> { configured: false, version: "...",
         checks: { database: true, kafka: true, library: false } }
```

While `configured` is `false`, the proxy/app routes visitors to the wizard. The wizard
submits:

```
POST /api/manage/setup
     { "displayName":  "My Library",
       "oidcIssuer":   "https://auth.example.com/realms/stube",
       "oidcClientId": "chino",
       "libraryPath":  "/var/lib/stube/media" }
```

`katalog-manager-api` persists the config and, if you did not supply a `streamSigningKey`,
generates one so playback works immediately. It then returns `{ "configured": true }` and
the app opens to your catalog. You can revisit settings any time under `/manage`, which
reads and writes `GET`/`PUT /api/manage/config`.

## Operator contract

Stube clients are neutral — they learn where the server is at runtime. You provide:

1. **An OIDC provider.** Register a public client with:
   - the **device authorization grant** (RFC 8628) enabled — the default sign-in on every
     client, so there are no per-device redirect URIs to register, and
   - `offline_access` so refresh tokens are issued.

   Set the issuer and client id during first-run setup (or via `PUT /api/manage/config`).
   Clients then use OIDC `.well-known` discovery — any compliant provider works.

2. **A stream-signing key**, shared by `chino-api` (mints) and `chino-stream` (verifies).
   First-run setup generates one for you; supply your own only if you want to manage it
   externally. A mismatch ⇒ playback returns 403 while artwork still loads.

## Getting content in

Point Stube at a directory of media you own. `katalog-manager-api` registers and manages
those entries; the catalog core enriches metadata and (optionally) transcodes/packages for
adaptive streaming.

> Stube intentionally has **no** built-in downloaders or indexer integrations. It catalogs
> and streams files that are already on disk. How they got there is out of scope — and out
> of this project.

## Running the public demo {#demo}

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
the device plugin on a GPU node. Without a GPU everything still works — just slower on large
4K transcodes.
