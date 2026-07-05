# katalog-manager-api

The neutral **management API** and **first-run backend** for Stube — the
catalog write/management plane that the admin UI (served at `/manage`) talks
to.

Stube is a neutral media client + server for a library you already own and are
entitled to stream. This service is **content-neutral**: it never acquires,
downloads, requests, or indexes content. How files arrive on disk is out of
scope. The only write path into the catalog here is the **import scan**, which
registers files that already exist on disk.

Written in Go with the chi router, matching the read-only catalog API
(`platform/katalog-api`).

## Responsibilities

- **First-run setup** — the contract the admin wizard consumes to configure a
  fresh install (display name, identity provider, library path, optional
  stream signing key).
- **Configuration** — persist and serve the live non-secret service config.
- **Catalog write surface** — edit and delete catalog items.
- **Import** — scan a directory of files the operator owns and register them as
  catalog items with a primary playback asset. This is the neutral replacement
  for any acquisition-coupled write path.
- **Processing dispatch** — enqueue transcode / package / metadata-enrich work
  by **emitting Kafka task events** on `stube.processing.task.*`. The workers
  consume the events; this service never runs the work synchronously.

The read path (browse, detail, search, asset resolution for the stream plane)
lives in the separate read-only catalog API. This service is the only writer to
the catalog item tables.

## HTTP surface

All routes are under the `/api/manage` prefix (so the edge proxy forwards the
management plane and the admin UI without path rewriting). Everything except
`GET /setup/status`, `/healthz`, and `/metrics` requires a valid OIDC bearer
token.

### First-run / config contract

The admin UI and this service share this contract verbatim — keep them
identical.

| Method | Path | Body | Returns |
|---|---|---|---|
| `GET`  | `/api/manage/setup/status` | — | `{ configured, version, checks: { database, kafka, library } }` |
| `POST` | `/api/manage/setup` | `{ displayName, oidcIssuer, oidcClientId, libraryPath, streamSigningKey? }` | `{ configured: true }` |
| `GET`  | `/api/manage/config` | — | current non-secret config |
| `PUT`  | `/api/manage/config` | non-secret config | updated config |

`POST /setup` generates and persists a `streamSigningKey` when one is not
supplied. `GET /setup/status` is intentionally reachable without a token so the
wizard can render before any identity provider is configured.

### Import / library / jobs

| Method | Path | Purpose |
|---|---|---|
| `POST`   | `/api/manage/import/scan` | Scan a directory (within the library root) of owned files and register new items. |
| `GET`    | `/api/manage/library` | List catalog items, paged (`?type=&q=&limit=&offset=`). |
| `PUT`    | `/api/manage/items/{id}` | Partial-patch an item's editable fields. |
| `DELETE` | `/api/manage/items/{id}` | Delete a catalog item (file on disk untouched). |
| `GET`    | `/api/manage/jobs` | Recent job history, newest first. |
| `POST`   | `/api/manage/items/{id}/transcode` | Emit `stube.processing.task.transcode`. |
| `POST`   | `/api/manage/items/{id}/package` | Emit `stube.processing.task.package`. |
| `POST`   | `/api/manage/items/{id}/enrich` | Emit `stube.processing.task.enrich`. |

The full schema is in [`openapi.yaml`](./openapi.yaml).

## Configuration

| Env | Default | Purpose |
|---|---|---|
| `ADDR` | `:8080` | Listen address. |
| `KATALOG_API_BASE_URL` | `http://katalog-api` | In-cluster base URL of the read-only catalog API. |
| `KAFKA_BROKERS` | `kafka:9092` | Comma-separated bootstrap broker list for task events. |
| `PG_URL` | — | Postgres connection string (config + catalog write DB). |
| `OIDC_ISSUER` | — | OIDC discovery issuer. Resolved from the operator's environment. |
| `OIDC_AUDIENCE` | `stube` | Comma-separated audience accept-list. |
| `STREAM_SIGNING_KEY` | — | Optional shared HMAC key for the stream plane. Generated at first-run if absent. |

With no `PG_URL` / `KAFKA_BROKERS` the service still boots in **scaffold mode**:
`/healthz` and `/setup/status` answer, the DB-backed routes return `503`, and
processing dispatch is a logged no-op.

## Build & run

```sh
go build ./...
go test ./...
go run .
```

Container image: `ghcr.io/zaentrum/katalog-manager`, listens on `:8080`.

```sh
docker build -t ghcr.io/zaentrum/katalog-manager:dev .
```

## Service contract notes

- **admin UI** — `ghcr.io/zaentrum/admin`, nginx static SPA on `:8080`,
  served at `/manage`. Consumes the first-run / config contract above.
- **kafka** — single-node KRaft (no ZooKeeper), service `kafka:9092`. The
  producer here publishes `stube.processing.task.*`.
