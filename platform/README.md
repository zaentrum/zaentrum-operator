# platform/

One image lives here: **`keycloak/`**, a Keycloak build with the `zaentrum` realm baked in,
which CI still publishes as `ghcr.io/zaentrum/keycloak` for the older, unsupported profiles
(`deploy/base`, `deploy/compose`).

The platform the operator brings up does not use it. Its Keycloak is the upstream image
(`spec.keycloak.image`, `quay.io/keycloak/keycloak`), with the realm import, the login theme and
the realm Job from the chart (`operator/platform/chart`).

The catalog and its pipeline — `katalog-api` (the read API), `katalog-manager` (the neutral
management / write API, `katalog-manager-api` in the chart), `katalog-ingest`, `analyzer`,
`transcoder`, `packager` — live in their own repos at `github.com/zaentrum/<svc>` and publish
`ghcr.io/zaentrum/<svc>`, which the chart references. The platform catalogs, prepares and
streams a library already on disk; it fetches no content. See the front door's
[architecture → scope](https://github.com/zaentrum/zaentrum/blob/main/docs/architecture.md#scope).
