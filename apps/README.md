# apps/ — clients

Clients are **skins over one shared core** (`apps/core`). The neutral-client model: a
client is told its server at runtime (first-run setup → `/api/config` → OIDC discovery →
device-code sign-in), so it ships with no baked backend.

| Dir | Client | Stack | State |
|---|---|---|---|
| `core/` | shared client core (data layer, design system) | KMP / TS | 🚧 to extract |
| `chino/` | video (web · mobile · androidtv) | React · KMP · Compose-for-TV | ✅ migrating first |
| `musig/` | music | (shared core) | 🚧 slot reserved |
| `tv/` | live TV | (shared core) | 🚧 slot reserved |

Migration order: land `chino` real and de-nalet'd, factor the shared `core`, then `musig`
and `tv` become thin skins rather than copies.

## De-nalet checklist (applied as each client lands here)

- [ ] no hardcoded `*.nalet.cloud` issuer/host (runtime config only)
- [ ] sign-in copy is neutral (no "your nalet.cloud account")
- [ ] telemetry off by default / opt-in, never posts identity to arbitrary servers
- [ ] device-code flow is the default sign-in (no per-server redirect URIs)
- [ ] no store screenshots with copyrighted artwork
