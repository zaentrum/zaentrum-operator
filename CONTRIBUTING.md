# Contributing to Stube

Thanks for your interest! Stube is a **neutral media client + server** for a library you own
and are entitled to stream — a media server, an admin launchpad, and clients, nothing more.

## Ground rules

- **No acquisition.** PRs that add downloaders, indexer/tracker integrations, scrapers, or
  links to such tools will be declined. Stube catalogs and streams a library you already
  own; it never fetches content, and how files arrive on disk is out of scope. See
  [docs/architecture.md](docs/architecture.md#scope).
- **Neutral by default.** No hardcoded servers, issuers, or branding for any one operator.
  Configuration is runtime data (first-run setup / `/api/manage/config`), not code.
- **Keep the config contract in sync.** `katalog-manager-api` and the admin UI share the
  setup/config endpoints documented in
  [architecture.md](docs/architecture.md#config-contract). Change one, change the other in
  the same PR.
- **No secrets, ever.** This is a public repo. No keys, keystores, tokens, or kubeconfigs —
  `.gitignore` guards the common cases, but you are the last line.

## Layout

`apps/` clients (incl. `admin`, the `/manage` launchpad) · `services/` per-product backends ·
`platform/` neutral catalog core (incl. `katalog-manager-api`) · `deploy/` one source of
truth for deployment · `docs/` architecture + self-hosting.

## Dev loop

```bash
# whole product in one container (k3s in-process, all services bundled)
docker run -d --privileged --name stube -p 8080:80 ghcr.io/zaentrum/stube:latest

# or the same manifests on any cluster you have a kubeconfig for
kubectl apply -k deploy/base
```

Open http://localhost:8080 and the first-run wizard at `/manage/setup` takes it from there.

## License

This project is licensed under [MPL-2.0](LICENSE). By contributing you agree your changes
are released under the same license.
