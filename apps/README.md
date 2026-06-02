# apps/ — clients

Clients are **skins over one shared core** (`apps/core`). The neutral-client model: a client
is told its server at runtime (first-run setup → OIDC discovery → device-code sign-in), so
it ships with no baked backend.

| Dir | Client | Route | Stack | State |
|---|---|---|---|---|
| `core/` | shared client core (data layer, design system) | — | KMP / TS | shared |
| `chino/` | video (web · mobile · androidtv) | `/` | React · KMP · Compose-for-TV | real |
| `admin/` | **management launchpad** | `/manage` | React SPA | real |
| `musig/` | music | `/` | (shared core) | planned |
| `tv/` | live TV | `/` | (shared core) | planned |

## admin — the `/manage` launchpad

A first-class client, not a hidden settings page. It is a React SPA served at `/manage`
(router basename `/manage`) and is the home for two jobs:

- **First-run setup.** On a fresh install it owns the wizard at `/manage/setup`. It reads
  `GET /api/manage/setup/status`; while `configured` is `false` the whole product points
  visitors here. The wizard collects display name, OIDC issuer + client id, and library
  path, then `POST`s to `/api/manage/setup`.
- **Day-2 management.** Once configured, it surfaces library and config management via
  `GET`/`PUT /api/manage/config`.

It talks only to **`katalog-manager-api`** under `/api/manage` — the neutral management /
write API. See the contract in
[docs/architecture.md](../docs/architecture.md#config-contract).

## Neutral-client checklist (applied as each client lands here)

- [ ] no hardcoded issuer/host — runtime config only (first-run setup / `/api/manage/config`)
- [ ] sign-in copy is neutral (no operator-specific account names)
- [ ] telemetry off by default / opt-in, never posts identity to arbitrary servers
- [ ] device-code flow is the default sign-in (no per-server redirect URIs)
- [ ] no store screenshots with copyrighted artwork
