# katalog-api

Read-only catalog REST API for the stube platform. Wraps the catalog
Postgres tables under the `cloud_katalog_ro` role and serves product apps
(`chino-web`, `tv-web`, `musig-web`) and edge consumers via OpenAPI 3.1.

## Status

Scaffold per the stube architecture review. Code lands in Phase 2 of the
migration plan.

## ADR cross-links

- ADR-007: Catalog read/write split. This service is the **read** half; all
  writes go through `katalog-manager-api`.
- ADR-014: `/metrics` Prometheus scrape conformance.

## Local development

```bash
go run ./cmd/server
curl localhost:8080/healthz
curl localhost:8080/api/v1/items
```

Set:

| Env | Default |
| --- | --- |
| `KATALOG_API_ADDR` | `:8080` |
| `KATALOG_API_PG_URL` | empty -> no DB |
| `KATALOG_API_OIDC_ISSUER` | empty -> auth disabled (set to your OIDC issuer URL) |
| `KATALOG_API_OIDC_AUDIENCE` | `chino` (comma-separated allowlist) |

## Deployment

Deploy manifests live under `deploy/base`. HPA-scalable (stateless, read-only).

## Owner

stube (single-maintainer personal value stream).
