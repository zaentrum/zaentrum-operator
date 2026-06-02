# Stube bundled Keycloak

A self-contained Keycloak image with the `stube` realm baked in. This is the
identity provider that ships *with* Stube — a single-box / self-host appliance
gets working OIDC out of the box, no external IdP required. Larger deployments
can point services at any compatible OIDC issuer instead and skip this image.

- **Image:** built from `quay.io/keycloak/keycloak:26.1` (pinned), optimized build.
- **Realm:** `stube` (imported on first start from `/opt/keycloak/data/import/`).
- **In-cluster Service:** `keycloak` on `:8080` (created by `deploy/`).
- **Public path:** fronted by Traefik at `/auth` (`KC_HTTP_RELATIVE_PATH=/auth`).
- **Issuer:** `<scheme>://<host>/auth/realms/stube`.

The Deployment, Service, ConfigMap and Secret live in `deploy/` (owned by the
deploy agent). This directory provides the **image + realm + the env contract**
documented below.

## Files

| File               | Purpose                                                       |
| ------------------ | ------------------------------------------------------------- |
| `Dockerfile`       | Two-stage optimized build; bakes the realm into the image.    |
| `stube-realm.json` | The `stube` realm: clients, scopes, roles, default admin user.|
| `README.md`        | This file — env contract + operational notes.                 |

## Realm contents

### Clients

| Client          | Type         | Flows                                   | Notes                                                              |
| --------------- | ------------ | --------------------------------------- | ------------------------------------------------------------------ |
| `chino-web`     | public       | Auth Code + PKCE (S256), Device Grant   | Browser SPA. `redirectUris: ["*"]`, `webOrigins: ["*"]`.           |
| `chino-tv`      | public       | Device Grant only                       | Living-room client; no browser on device.                          |
| `chino-mobile`  | public       | Auth Code + PKCE (S256), Device Grant   | System-browser login; custom-scheme redirect via wildcard.         |
| `stube-manager` | confidential | Service account (client credentials)    | Calls the Admin REST API from `manage-api` (user CRUD + `stube-admin` role grants). Secret injected by env.|

All three public clients:

- offer `offline_access` as an optional scope (request `scope=offline_access`
  for a refresh token that survives SSO-session idle timeout);
- emit access tokens with **`aud: chino`** via an audience protocol mapper, so
  every resource service keeps `OIDC_AUDIENCE=chino` (matches the deploy
  `stube-env` ConfigMap and `docker-compose`).

> Compatibility note: these mirror the historical `chino*` clients in
> `identity/idp-config/clients/` (public, PKCE S256, device grant, audience
> mapper) but are produced generically — no `nalet.cloud` redirect URIs, no
> real secrets. The public clients use wildcard redirect/origins so the realm
> works behind any host the appliance is deployed under. Tighten these per
> deployment if you front a single known host.

### `stube-manager` secret

The realm JSON ships a placeholder secret:

```json
"secret": "${STUBE_MANAGER_CLIENT_SECRET:change-me-in-deploy}"
```

Keycloak resolves `${ENV_VAR:default}` references in realm-import files from
the pod environment at import time. `deploy/` provides
`STUBE_MANAGER_CLIENT_SECRET` from a Kubernetes Secret; `manage-api` receives
the same value (also from that Secret) and uses it for the client-credentials
grant against the Admin REST API. The image never contains a real secret.

`service-account-stube-manager` is granted the `realm-management` client roles
**`manage-users`, `view-users`, `query-users`, `view-realm`** — exactly the scope
`manage-api` needs to create / list Stube users and to read and assign the
`stube-admin` realm role when promoting a user (`view-realm` lets it resolve the
realm role and its member list).

### Realm roles and the `stube-admin` gate

The realm defines two realm roles:

| Role          | Granted to                              | Meaning                                                                 |
| ------------- | --------------------------------------- | ----------------------------------------------------------------------- |
| `stube-user`  | interactive accounts (assign on create) | Plain end user of the products.                                         |
| `stube-admin` | the seed `admin` user only              | May manage the catalog and other users via the Stube `/manage` console. |

`stube-admin` is **not** in the realm `defaultRoles`, so newly-created users get
no admin rights by default — an existing admin promotes another account
explicitly (the Users API exposes an `admin` flag for this). The `roles` client
scope emits realm roles into the access token's `realm_access.roles` claim
(`access.token.claim: true`); `manage-api`'s auth middleware reads that claim and
rejects any token lacking `stube-admin` on `/api/manage/*` with **403** (a valid
token that is merely authenticated is not enough).

### Default admin user

The realm contains a user `admin` with the `realm-management` `realm-admin`
client role (full admin of the `stube` realm only). **No password is stored in
the JSON.** Set it one of two ways:

1. **Bootstrap env (recommended for first start).** Provide
   `KC_BOOTSTRAP_ADMIN_USERNAME` / `KC_BOOTSTRAP_ADMIN_PASSWORD` (the 26.x
   replacement for `KEYCLOAK_ADMIN`/`KEYCLOAK_ADMIN_PASSWORD`) from a Secret in
   `deploy/`. This creates the temporary *master*-realm bootstrap admin; use it
   once to set the `stube`-realm `admin` user's password, then remove it.
2. **Initial password via Secret + first-run reset.** The realm `admin` user
   carries the `UPDATE_PASSWORD` required action, so whatever initial password
   you set must be changed on first interactive login. In practice Stube users
   (including the admin) are managed through the **Stube `/manage` console**,
   which talks to `manage-api` → Admin REST API; the Keycloak admin console is
   not the day-to-day surface (see below).

Either way the password is supplied at deploy time, never committed here.

## Behind Traefik — env contract for `deploy/`

The image is self-sufficient (`kc.sh start --optimized --import-realm`). The
Deployment must supply these env vars so Keycloak trusts the proxy and serves
under `/auth`:

| Env var                  | Value         | Why                                                                 |
| ------------------------ | ------------- | ------------------------------------------------------------------- |
| `KC_PROXY_HEADERS`       | `xforwarded`  | Trust `X-Forwarded-*` from Traefik (26.x form; supersedes `KC_PROXY=edge`). |
| `KC_HTTP_ENABLED`        | `true`        | TLS terminates at Traefik; Keycloak speaks plain HTTP in-cluster.   |
| `KC_HOSTNAME_STRICT`     | `false`       | Derive hostname/issuer from the forwarded request, not a fixed host.|
| `KC_HTTP_RELATIVE_PATH`  | `/auth`       | Serve everything (and the issuer) under `/auth`.                    |
| `KC_DB`                  | `postgres`    | Must match the build-time DB vendor.                                |
| `KC_DB_URL`              | (deploy)      | JDBC URL for the Stube Postgres.                                    |
| `KC_DB_USERNAME` / `KC_DB_PASSWORD` | (Secret) | DB credentials.                                                  |
| `STUBE_MANAGER_CLIENT_SECRET` | (Secret) | Resolved into `stube-manager` at realm import.                      |
| `KC_BOOTSTRAP_ADMIN_USERNAME` / `KC_BOOTSTRAP_ADMIN_PASSWORD` | (Secret, first run) | One-time master bootstrap admin. |

> `KC_PROXY_HEADERS=xforwarded` is the 26.x replacement for the deprecated
> `KC_PROXY=edge`. If you are on a Traefik that emits `Forwarded` instead of
> `X-Forwarded-*`, use `KC_PROXY_HEADERS=forwarded`. Either is fine; do not set
> both `KC_PROXY` and `KC_PROXY_HEADERS`.

With the above, the public issuer is:

```
<scheme>://<host>/auth/realms/stube
```

and that is exactly what services should set as `OIDC_ISSUER` (with
`OIDC_AUDIENCE=chino`). OIDC discovery lives at:

```
<scheme>://<host>/auth/realms/stube/.well-known/openid-configuration
```

The Traefik IngressRoute should route `PathPrefix(`/auth`)` to the in-cluster
`keycloak:8080` Service.

### Health / readiness

The build enables management health endpoints (on the management port,
`9000` by default in 26.x):

- liveness: `/health/live`
- readiness: `/health/ready`
- startup:  `/health/started`

## Admin console is internal-only / headless

Stube manages its users through the **Stube `/manage` console** (a Stube
surface that calls `manage-api`, which in turn uses `stube-manager` to drive
the Keycloak Admin REST API). The Keycloak admin console itself is **not**
exposed publicly:

- Do **not** add a Traefik route for `/auth/admin`.
- Operators reach the admin console only by port-forwarding the in-cluster
  Service when something needs hand-fixing:
  `kubectl port-forward svc/keycloak 8080:8080 -n <ns>` then
  `http://localhost:8080/auth/admin`.

This keeps the realm's day-to-day operation behind Stube's own UI and keeps the
public surface limited to the OIDC endpoints the clients need.

## Build (validation)

You cannot `docker build` in this repo context, but the artifacts are
self-validating:

```bash
# realm JSON is well-formed
jq empty platform/keycloak/stube-realm.json

# Dockerfile sanity (optional, if hadolint is available)
hadolint platform/keycloak/Dockerfile
```

CI builds the image (two-stage optimized) and pushes it; `deploy/` references
the resulting tag.
