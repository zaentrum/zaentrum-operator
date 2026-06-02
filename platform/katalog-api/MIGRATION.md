# MIGRATION

`katalog-api` is a **from-scratch** Go service. It is **not** lifted from the
CAP-Java codebase (`katalog-manager-api`) or any chino service.

## Why a fresh repo?

- CAP-Java + Spring Boot is the right tool for the admin/write surface (the
  Fiori UI, complex business logic, audit hooks). It is **not** the right tool
  for a read-only, HPA-scalable, low-tail-latency API on the hot path.
- `katalog-api` only ever runs SELECTs under the `cloud_katalog_ro` Postgres
  role. The DB-level grant is the strongest possible invariant guaranteeing
  read-only behaviour.

## Pattern source

The service skeleton (chi router, pgx pool, go-oidc verifier, prom metrics,
slog) mirrors `/projects/chino/chino-api/cmd/server/main.go`.

## Open items

- Run `sqlc generate` once the data model is finalised; for now
  `internal/store/queries.sql` is a stub.
- Pin Postgres version + connection string format.
- Decide on caching layer (Valkey or pgx LRU).
- Confirm OpenAPI spec at `api/openapi.yaml` matches the handlers as they
  land.
