# Contributing to Zaentrum

Thanks for your interest! Zaentrum is a **neutral media client + server** for a library you own
and are entitled to stream — a media server, a portal, and clients, nothing more.

## Ground rules

- **No acquisition.** PRs that add downloaders, indexer/tracker integrations, scrapers, or
  links to such tools will be declined. Zaentrum catalogs and streams a library you already
  own; it never fetches content, and how files arrive on disk is out of scope. See the front
  door's [architecture → scope](https://github.com/zaentrum/zaentrum/blob/main/docs/architecture.md#scope).
  CI's neutrality job (`scripts/check-neutrality.sh`) holds the repo to it.
- **Neutral by default.** No hardcoded servers, issuers, or branding for any one operator. The
  platform learns its host and its identity provider from the `Zaentrum` resource
  (`spec.hostname`, `spec.identity`); the clients learn theirs from the server they are pointed
  at (`/api/config`).
- **The CR and the chart move together.** A `Zaentrum` field maps onto the chart's values
  (`operator/internal/templates`); a chart change comes with a render test, a CRD change with
  its block spliced into every copy — see [operator/README.md → Build / test](operator/README.md#build--test).
- **No secrets, ever.** This is a public repo. No keys, keystores, tokens, or kubeconfigs —
  `.gitignore` guards the common cases, but you are the last line.

## Layout

The platform's **meta-repo**: `operator/` the controller · `operator/platform/chart/` the
platform's Helm chart, which the operator embeds · `operator/bundle/` the OLM bundle ·
`deploy/operator-install.yaml` the pinned cluster install · `deploy/allinone/` the appliance.
`apps/admin/` and `platform/keycloak/` are images CI still builds for the older profiles in
`deploy/{base,compose,k3s,overlays}`, which are not supported. The service and client sources
live in their own repos at `github.com/zaentrum/<svc>`; the chart references their published
`ghcr.io/zaentrum/<svc>` images. Deployment docs live in the front door,
[github.com/zaentrum/zaentrum/docs](https://github.com/zaentrum/zaentrum/tree/main/docs).

## Dev loop

```bash
# the whole platform in one container: k3s, the operator, a Zaentrum
docker run -d --privileged --restart unless-stopped --name zaentrum -p 80:80 \
  ghcr.io/zaentrum/appliance:latest
open http://zaentrum.localhost   # sign in as admin — deploy/allinone/README.md#first-run

# the operator, the chart and the appliance's manifests
cd operator && go vet ./... && go test ./...
helm lint platform/chart && ../scripts/check-neutrality.sh && ../deploy/allinone/build.sh render
```

There is no setup wizard: a fresh install comes up configured, and its first run is a sign-in,
a TMDB key and the library.

## License

This project is licensed under [MPL-2.0](LICENSE). By contributing you agree your changes
are released under the same license.
