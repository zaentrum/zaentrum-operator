# apps/

One app lives here: **`admin/`**, the management console of the older, unsupported profiles
(`deploy/base`, `deploy/compose`), which serve it at `/manage`. CI still builds it as
`ghcr.io/zaentrum/admin` for them.

It is not part of the platform the operator brings up. There, the portal
(`zaentrum-portal` and `portal-api`) is the front door and the launchpad, the Catalog and
Catalog Management consoles (`katalog-manager-ui`, at `/katalog` and `/katalog-manage`) manage
the library, and accounts are made in Keycloak's admin console — see the
[README's route map](../README.md#route-map).

The clients — `chino-web`, `chino-mobile`, `chino-androidtv`, `chino-tizen` — live in their own
repos at `github.com/zaentrum/<client>`. Each is told its server at runtime: the address a person
types, then `/api/config` names the issuer and the client to sign in through (the
[operator's README](../operator/README.md#sign-in-redirects-and-the-realm-job)).
