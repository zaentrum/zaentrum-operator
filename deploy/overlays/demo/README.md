# Demo overlay → `demo-stube.nalet.cloud`

A public, link-shareable demo of the neutral platform.

**Content policy (non-negotiable):** Creative-Commons / public-domain / own content
**only**. A demo wired to a private or licensed library is a public window into it —
strictly worse than the problem the neutral pivot solves. `seed-content.yaml` defaults to
the Blender open movies (CC-BY).

What this overlay changes vs `base`:

- isolated namespace `stube-demo` (never collides with a real deployment),
- ingress host → `demo-stube.nalet.cloud`,
- a throwaway demo OIDC realm (`stube-demo`) with a shared demo login — not real users,
- telemetry off,
- a one-shot Job that seeds CC content.

## Deploying it (via the operator's GitOps pipeline — not `kubectl` by hand)

This overlay is public/neutral, but it deploys to a private cluster (OKD). It is applied
**through the GitLab CI pipeline** on the operator's side, which mirrors this repo and runs
the deploy stage. Do not `oc apply` it from a laptop.

```bash
# what the pipeline runs (vanilla clusters):
kubectl apply -k deploy/overlays/demo
```

On OKD the pipeline layers the private OKD overlay on top (Route instead of Ingress, SCCs,
ServiceMonitor) — that overlay is **not** in this repo.
