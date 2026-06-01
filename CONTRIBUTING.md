# Contributing to Stube

Thanks for your interest! Stube is a **neutral, self-hostable media platform** — a media
server and clients, nothing more.

## Ground rules

- **No acquisition.** PRs that add downloaders, indexer/tracker integrations, scrapers, or
  links to such tools will be declined. Stube catalogs and streams a library you already
  own; it never fetches content. See [docs/architecture.md](docs/architecture.md#scope).
- **Neutral by default.** No hardcoded servers, issuers, or branding for any one operator.
  Configuration is runtime data (env / `/api/config`), not code.
- **No secrets, ever.** This is a public repo. No keys, keystores, tokens, kubeconfigs —
  the `.gitignore` guards the common cases but you are the last line.

## Layout

`apps/` clients · `services/` per-product backends · `platform/` neutral catalog core ·
`deploy/` one source of truth for deployment · `docs/` architecture + self-hosting.

## Dev loop

```bash
./deploy/k3s/up.sh     # whole platform on a local k3d cluster
# or
docker compose -f deploy/compose/docker-compose.yml up -d
```

## License

See [README](README.md#license) — license selection is pending and gates the first public
release. Until then, treat the code as all-rights-reserved.
