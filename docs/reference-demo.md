# Reference demo: how the public demo is deployed

This page documents exactly how the public reference demo at
**`https://zaentrum.demo.nalet.cloud`** is deployed end to end. It is the worked
example of a real GitOps deploy of an operator-managed Zaentrum platform, and a
companion to the more general [self-hosting.md](./self-hosting.md) and
[operator.md](./operator.md).

The demo runs in namespace **`zaentrum-demo`** on an OKD cluster. The whole
~16-service platform is rendered by the [zaentrum-operator](./operator.md) from a
single `Zaentrum` CR out of its embedded canonical Helm chart. Deploys are driven
by a **GitLab CI job** in a deploy-only repository — the manifests referenced
throughout live in **`gitlab.nalet.cloud/zaentrum/deploy`** under
`zaentrum-demo/`. This repo is public source-of-truth for the operator and chart;
the deploy manifests are GitLab-hosted because they are deploy-only.

The deploy is best understood as four layers, from the outside in:

| Layer | What | Who / when |
|---|---|---|
| 0 | External, pre-existing infrastructure | Not managed here |
| 1 | One-time cluster-admin bootstrap | Cluster-admin, run once |
| 2 | The repeatable CI deploy | GitLab CI, every deploy |
| 3 | Verify | Read-only checks, after every deploy |

---

## Layer 0 — External / pre-existing

These exist independently of the demo and are not created by the deploy:

| Thing | Detail |
|---|---|
| OKD cluster | Edge-terminated TLS at the router; public host `*.demo.nalet.cloud` already routes to the edge load balancer. |
| Media NFS export | `nas001:/media-demo`, exported RWX with permissive squash (the seed Job writes as an arbitrary UID). **Creative-Commons / own content ONLY** — never point the demo at production media. |
| Split-horizon DNS | In-cluster `zaentrum.demo.nalet.cloud` must resolve to the OKD router IP so the in-cluster OIDC validators can reach the edge-terminated issuer TLS. The CR pins this via `network.issuerHostAliasIP` (see [Layer 2](#the-cr)). |
| Container images | `ghcr.io/zaentrum/*:latest` on the public GitHub Container Registry, built by GitHub Actions from the app repos. |
| GHCR pull credentials | A GitHub PAT with `read:packages`, supplied to CI (see [CI variables](#ci-variables)). |
| The `stube` namespace pull secret | The demo CI copies the `gitlab-registry` pull secret out of the live OKD `stube` namespace. That namespace/secret is unrelated to the GitLab `stube` group and stays valid. |

---

## Layer 1 — One-time cluster-admin bootstrap

The CI deploy token is a namespace-scoped ServiceAccount and **cannot** create
cluster-scoped resources (CRDs, `Namespace`, `PersistentVolume`) or bind a cluster
SCC. So a cluster-admin runs these steps once, out of band from CI. Source:
`zaentrum-demo/bootstrap.yaml` and the operator install manifest in this repo.

### 1a. Install the operator (CRD + ClusterRoles + controller)

```bash
oc apply -f https://raw.githubusercontent.com/zaentrum/zaentrum-operator/main/deploy/operator-install.yaml
oc -n zaentrum-operator-system rollout status deploy/zaentrum-operator-controller-manager
```

This installs the `zaentrums.zaentrum.io` CRD, the operator ClusterRoles, and the
controller-manager in namespace `zaentrum-operator-system`.

### 1b. Bootstrap the namespace (`bootstrap.yaml`)

```bash
oc apply -f zaentrum-demo/bootstrap.yaml
```

This single file creates everything cluster-scoped or privileged that the
namespaced CI deployer cannot:

| Resource | Purpose |
|---|---|
| `Namespace/zaentrum-demo` | The target namespace. |
| `PersistentVolume/zaentrum-demo-media-nfs` | Static NFS PV → `nas001:/media-demo` (200Gi, RWX, `Retain`), `claimRef` pinned to the `media` PVC. |
| `PersistentVolume/pv-worker1-zaentrum-demo-kafka` | Node-local PV (20Gi, RWO, `local-temp-rwo`) for the bundled Kafka log dir, `nodeAffinity` pinned to `worker1.okd.nalet.cloud`, `claimRef` to the `kafka-data` PVC. Backs persistent Kafka so **topics + consumer offsets survive a broker restart**. |
| `RoleBinding/deployer-admin` | Grants the `stube:deployer` SA the `admin` ClusterRole on `zaentrum-demo`. |
| `Role`+`RoleBinding/zaentrum-deployer` | Grants the deployer explicit `zaentrums` rights (the built-in `admin` role does not cover custom resources) so CI can apply the CR. |
| `RoleBinding/kafka-anyuid` | Grants the bundled `kafka` SA the `anyuid` SCC (apache/kafka writes config as uid 1000). |
| `ClusterRole`+`ClusterRoleBinding/zaentrum-demo-ns-get` | Grants the deployer two cluster-scoped reads — `get namespaces/zaentrum-demo` and `get customresourcedefinitions/zaentrums.zaentrum.io`, both `resourceNames`-scoped — for the deploy job's pre-flight guards. |

### 1c. Create the Kafka PV host directory

The node-local Kafka PV points at a host path that must exist and be writable
before the PVC can bind. On the pinned node:

```bash
oc debug node/worker1.okd.nalet.cloud
# in the debug shell:
chroot /host mkdir -p /var/local-storage/a/pv/zaentrum-demo-kafka
chroot /host chmod 0777 /var/local-storage/a/pv/zaentrum-demo-kafka   # kafka runs as uid 1000
```

### CI variables

Set once, in GitLab. Group-level variables are shared across the `zaentrum` group;
project-level variables live on the `deploy` project.

| Scope | Variable | What it is |
|---|---|---|
| Group (`zaentrum`) | `OC_SERVER` | OKD API server URL. |
| Group (`zaentrum`) | `OC_TOKEN` | A **long-lived deployer SA token**: `oc create token deployer -n stube --duration=87600h`. |
| Project (`deploy`) | `DEMO_DB_PW` | Bundled Postgres password (throwaway). |
| Project (`deploy`) | `DEMO_MANAGER_SECRET` | Keycloak client secret for the manager/pipeline client. |
| Project (`deploy`) | `DEMO_KC_ADMIN_PW` | Bundled Keycloak admin password. |
| Project (`deploy`) | `DEMO_REALM_ADMIN_PW` | `zaentrum` realm admin password. |
| Project (`deploy`) | `DEMO_USER_PW` | Optional demo end-user password (seeds the realm `demo` user on a fresh import only). |
| Project (`deploy`) | `GHCR_PULL_TOKEN` | GitHub PAT with `read:packages` for `ghcr.io/zaentrum/*` pulls. |
| Project (`deploy`) | `GHCR_PULL_USER` | GitHub username for the GHCR pull secret (defaults to `zaentrum`). |
| Group / project | `CI_ENABLED` | Must be `"true"` or the whole pipeline stays dormant. |

> **Trap:** if `OC_TOKEN` expires the deploy fails pre-flight with `ns
> zaentrum-demo missing`. Refresh it with a new long-lived deployer token. See
> [troubleshooting.md](./troubleshooting.md).

---

## Layer 2 — The repeatable deploy

Everything namespaced is applied by the GitLab CI job **`deploy:zaentrum-demo`**
(source: `.gitlab-ci.yml`). It is **main-only**, **manual**, and gated on
`CI_ENABLED == "true"`. It runs a public `origin-cli` image and authenticates as
the deployer SA from the group `OC_SERVER` / `OC_TOKEN`.

### The CR

The platform is one `Zaentrum` custom resource, `okd/zaentrum.yaml`
(`apiVersion: zaentrum.io/v1alpha1`). The demo profile maps 1:1 onto the chart's
`values-demo.yaml`. Key fields:

```yaml
apiVersion: zaentrum.io/v1alpha1
kind: Zaentrum
metadata:
  name: zaentrum
  namespace: zaentrum-demo
spec:
  version: latest                       # every ghcr.io/zaentrum/* image tracks :latest
  hostname: zaentrum.demo.nalet.cloud   # OIDC issuer host + Route host + KC_HOSTNAME
  partOf: zaentrum-demo
  imagePullSecrets: [ghcr-pull, gitlab-registry]
  identity:
    issuerScheme: https                 # TLS terminates at the OKD router
    loginTheme: zaentrum
  features:
    kafka: true
    pipeline: true                      # full media pipeline: analyzer/packager/transcoder/katalog-ingest
  storage:
    provisionMedia: false               # the external media PVC is applied by CI, not the operator
    kafkaPvc: kafka-data                # persistent Kafka log dir → node-local PV
    kafkaNode: worker1.okd.nalet.cloud
  network:
    issuerHostAliasIP: "77.109.148.13"  # in-cluster split-horizon to the router for OIDC validation
  routing:
    provisionIngress: false
    provisionRoutes: true               # single-origin OpenShift Routes
  secrets:
    external: true                      # zaentrum-* secrets are pre-created by CI
  databases:
    mode: perApp
```

### What the operator does NOT render

The demo deliberately keeps a few resources outside the operator; the CI overlay
(`kustomization.yaml`) carries them alongside the CR:

| Kept out | CR flag | Why |
|---|---|---|
| The `media` PVC (`okd/media-nfs.yaml`) | `storage.provisionMedia: false` | The operator consumes the existing PVC bound to the NFS PV. |
| The `zaentrum-*` secrets | `secrets.external: true` | CI creates them from the `DEMO_*` variables (the only secret path allowed from CI). |
| The `kafka-data` PVC (`okd/kafka-pvc.yaml`) | `storage.kafkaPvc: kafka-data` | Binds the node-local Kafka PV from bootstrap. |
| The seed / scan / enqueue / kafka-topics Jobs | operator forces `jobs.seed=false` | Demo choreography (populate + drive the pipeline), never rendered by the operator. |

### Job steps

The `deploy:zaentrum-demo` script, in order:

1. **Pre-flight guards.** Require the `DEMO_*` + `OC_SERVER` + `OC_TOKEN`
   variables; then `kubectl get ns zaentrum-demo` and `kubectl get crd
   zaentrums.zaentrum.io` — both fail with a clear message pointing at the Layer 1
   bootstrap if missing.
2. **Create the demo secrets** with `kubectl create secret --dry-run=client -o
   yaml | kubectl apply -f -`: `zaentrum-db`, `zaentrum-stream-signing` (random
   32-byte key), `zaentrum-keycloak`, `zaentrum-keycloak-admin`, and optionally
   `zaentrum-demo-user`.
3. **Copy the `gitlab-registry` pull secret** from the OKD `stube` namespace into
   `zaentrum-demo`.
4. **Create the `ghcr-pull` secret** from `GHCR_PULL_TOKEN` / `GHCR_PULL_USER`
   (if set) and add it to the `default` SA's `imagePullSecrets`. If unset, an
   existing in-namespace `ghcr-pull` from a prior deploy persists.
5. **Delete the finished choreography Jobs** (`kafka-topics`, `seed-demo-content`,
   `scan-catalog`, `enqueue-processing`) so `apply -k` recreates them fresh — Jobs
   are immutable.
6. **`kubectl apply -k zaentrum-demo`** — applies the CR, the two external PVCs,
   and the choreography Jobs. The operator picks up the CR and reconciles the
   platform via server-side apply (async).
7. **Wait for the operator to render** — poll up to ~2 min for
   `deploy/zaentrum-portal` to appear.
8. **Rollout-restart the app tiers** to pick up config, **excluding** the stateful
   backers (`postgres` / `valkey` / `kafka` — restarting them would wipe DB / topic
   / session state), then `rollout status deploy/keycloak`.
9. **Print CR status + workloads** (`kubectl get zaentrum,pods,routes`).

The choreography Jobs recreated by step 6 run once the platform is up:

| Job | Source | Does |
|---|---|---|
| `kafka-topics` | `okd/kafka-topics.yaml` | Creates the pipeline topics with deterministic partitions/retention: `stube.catalog.item.discovered/enriched/analyzed/transcoded` (idempotent, `--if-not-exists`). |
| `seed-demo-content` | `okd/seed.yaml` | Downloads Creative-Commons / public-domain titles to `/var/lib/katalog/media` on the NFS export (skip-if-present, tolerant of dead links). |
| `scan-catalog` | `okd/scan.yaml` | Mints a client-credentials token from bundled Keycloak and triggers the filesystem scan; the scan emits `stube.catalog.item.discovered` and the event-driven pipeline (enrich → analyze → transcode → package) takes over. |
| `enqueue-processing` | `okd/enqueue.yaml` | Harmless backfill for items that predate the event flow. |

### Trigger the deploy

The job is manual, so trigger a pipeline on `main` and then play the manual job:

```bash
glab ci run -b main -R zaentrum/deploy
# then, in the GitLab UI or CLI, play the manual `deploy:zaentrum-demo` job
```

---

## Layer 3 — Verify

Read-only checks after the deploy (no cluster mutation needed):

```bash
# 1. The operator reports the platform Ready.
oc -n zaentrum-demo get zaentrum
#   → PHASE should be Ready; status.components all ready.

# 2. All pods up (postgres → keycloak → services).
oc -n zaentrum-demo get pods

# 3. Pipeline topics exist.
oc -n zaentrum-demo exec deploy/kafka -- \
  /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:9092 --list | grep '^stube.catalog.item.'

# 4. Seed landed the CC titles.
oc -n zaentrum-demo logs job/seed-demo-content

# 5. Issuer resolves (confirms split-horizon DNS).
curl -s https://zaentrum.demo.nalet.cloud/auth/realms/zaentrum/.well-known/openid-configuration
```

Then in a browser: open `https://zaentrum.demo.nalet.cloud`, log in with the
bundled Keycloak admin (a **fresh realm import forces an `UPDATE_PASSWORD` on first
login**), browse the catalog, and play a title.

---

## Update flows

| Change | Path |
|---|---|
| **An app change** | Push the app's GitHub repo → GitHub Actions builds `ghcr.io/zaentrum/<app>:latest` (+ `:sha-<gitsha>`) → run the CI deploy; step 8 `rollout restart`s the app tiers to pull `:latest`. |
| **A chart / operator change** | Push `zaentrum-operator` → the build-images workflow builds `ghcr.io/zaentrum/operator:latest` (+ `:sha-<gitsha>`) → bump the image in `deploy/operator-install.yaml` to that sha → cluster-admin `oc apply -f operator-install.yaml` to roll the operator (its embedded chart re-renders). |
| **A CR value change** | Edit `okd/zaentrum.yaml` → run the CI deploy. |

See [updating.md](./updating.md) for the full update model and channels, and
[troubleshooting.md](./troubleshooting.md) for known traps (OC_TOKEN expiry, the
Kafka `emptyDir`→PVC volume-type conflict, NVENC/driver mismatch, in-cluster OIDC
split-horizon, and the fresh-realm password change).
