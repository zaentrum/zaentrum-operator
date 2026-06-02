# stube-admin

The **admin UI for Stube** — first-run setup and day-2 management for a neutral media
client + server for a library you own and are entitled to stream.

Served under **`/manage`**. It is a static SPA (React 18 + Vite 6 + TypeScript +
TailwindCSS 3 + lucide-react + react-router-dom 6) that talks to the **manage-API**
(implemented by `katalog-manager-api`) over `/api/manage`.

## Authentication

The whole app is gated behind an OIDC login against Stube's **bundled identity
provider** (Keycloak realm `stube`), reusing the public `chino-web` client
(Authorization Code + PKCE). It mirrors `chino-web`'s runtime-config pattern:

- On boot the SPA fetches the unauthenticated, CORS-open discovery document
  **`GET /api/config`** (served by `chino-api`, same origin) to learn the OIDC
  `oidcIssuer` + the public web client id. The manage-API's own
  `GET /api/manage/config` can't be used for discovery — it's behind the OIDC
  verifier (chicken-and-egg). See [`src/auth/runtimeConfig.ts`](src/auth/runtimeConfig.ts).
- `RuntimeAuthProvider` mounts `react-oidc-context`'s `AuthProvider` once an
  issuer is known (polling `/api/config` while the API is still starting up),
  and `AuthGate` redirects unauthenticated visitors to Keycloak. Redirect URI is
  `<origin>/manage/callback`.
- The bundled `admin` account's password is set by **Keycloak's own
  `UPDATE_PASSWORD` action on first login**, not by this app — so login happens
  *before* the first-run wizard.
- Every `/api/manage/*` request carries `Authorization: Bearer <access_token>`
  (the token is bridged from the auth context into the `api` client; see
  [`src/auth/token.ts`](src/auth/token.ts) + `AuthTokenBridge`).

Build-time pins (`VITE_OIDC_AUTHORITY`, `VITE_OIDC_CLIENT_ID`,
`VITE_OIDC_REDIRECT_URI`, `VITE_OIDC_POST_LOGOUT_REDIRECT_URI`) let a
single-tenant deployment hard-pin a realm; when unset the server-reported values
win.

> Stube is content-neutral. This UI catalogs and configures a library you already have;
> it never acquires content. How files arrive on disk is out of scope.

## Stack

| | |
|---|---|
| Framework | React 18 |
| Build | Vite 6 (`base: '/manage/'`) |
| Language | TypeScript 5 (strict) |
| Styling | TailwindCSS 3, theme mapped to the nalet.cloud design tokens |
| Icons | lucide-react |
| Routing | react-router-dom 6 (`basename="/manage"`) |

Design tokens are vendored from the design system into
[`src/styles/tokens.css`](src/styles/tokens.css) and exposed to Tailwind via
[`tailwind.config.ts`](tailwind.config.ts) (`bg`, `surface`, `fg`, `cloud-blue`, …) and
the 4px spacing scale (`s-1`…`s-8`). Fonts: Inter (UI) + JetBrains Mono (mono / the
`> stube` wordmark).

## Develop

```bash
npm install
npm run dev        # http://localhost:5174/manage/
```

The dev server proxies `/api/*` to `VITE_DEV_API_TARGET` (default
`http://localhost:8080`) so you can run against a local `katalog-manager-api`.

```bash
npm run build      # tsc --noEmit && vite build  ->  dist/
npm run preview    # serve the production build locally
```

## Routes

| Path | Page |
|---|---|
| `/manage` | **Launchpad** — status + tile grid (Library, Import, Jobs, Metadata, Users, Settings). Redirects to `/manage/setup` when the server is unconfigured. |
| `/manage/setup` | **First-run wizard** (login-first) — 4 steps: Welcome (server name + advanced OIDC) → Library → Streaming → Review. No admin-password step: that's owned by Keycloak's `UPDATE_PASSWORD` at first login. |
| `/manage/callback` | OIDC redirect landing — handled by the AuthProvider, then normalised back to `/manage/`. |
| `/manage/library` | Catalogued items (search, table, empty/loading/error states). |
| `/manage/import` | Start a folder scan. |
| `/manage/jobs` | Background work (scan / enrich / analyze / artwork / package). |
| `/manage/settings` | Edit identity / OIDC / library path; view streaming-key status. |

## manage-API contract

`katalog-manager-api` implements these; this UI consumes them. Base path `/api/manage`
(override with `VITE_MANAGE_API_BASE`). Types live in
[`src/lib/api.ts`](src/lib/api.ts).

```
GET  /api/manage/setup/status
       -> { configured: boolean, version: string,
            checks: { database: boolean, kafka: boolean, library: boolean } }

POST /api/manage/setup   (requires a bearer token)
       body { displayName, oidcIssuer, oidcClientId, libraryPath, streamSigningKey? }
       -> persists config (generates streamSigningKey if absent)
       -> { configured: true }
       NB: oidcIssuer + oidcClientId are REQUIRED by the server. For the
           bundled IdP the wizard echoes back the issuer + web client it
           discovered from GET /api/config; the advanced path supplies an
           external provider.

GET  /api/manage/config   -> current non-secret config
       -> { displayName, oidcIssuer, oidcClientId, libraryPath,
            streamSigningKeySet: boolean, version }
PUT  /api/manage/config   body Partial<config> -> updated config

GET  /api/manage/library?q=&limit=&offset=  -> { items: LibraryItem[], total }
POST /api/manage/import/scan { path }       -> { jobId, path, state }
GET  /api/manage/jobs                       -> { jobs: Job[] }
```

The stream signing key is a secret: it is accepted on `POST /setup` and generated by the
server when omitted, but **never echoed back** — `GET /config` only reports
`streamSigningKeySet`.

## Deploy

`Dockerfile` builds the SPA and serves it on `:8080` via unprivileged nginx, with the
bundle under `/manage` and an SPA fallback so deep links resolve. Behind the platform
ingress, `/api` routes to the backend and the static app is served at `/manage`.

```bash
docker build -t ghcr.io/nalet/stube/stube-admin:latest .
```
