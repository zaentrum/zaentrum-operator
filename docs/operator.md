# Running Zaentrum with the operator

The Zaentrum operator is a Kubernetes controller that turns a single `Zaentrum` custom
resource into the whole platform — the catalog core, the per-product streaming
backends, the clients, the bundled Postgres/Valkey/Kafka, and OIDC. You declare
the platform you want; the operator reconciles it and reports progress on the CR
status.

The operator is published as an [OLM](https://olm.operatorframework.io/) bundle
(`ghcr.io/zaentrum/operator-bundle`), so it installs the same way on
OpenShift and on any OLM-enabled Kubernetes cluster.

## Two ways to run Zaentrum

There are two supported deployment modes, and `/manage` (the admin UI + setup
wizard) talks to the operator in both:

1. **All-in-one appliance.** A single container (`ghcr.io/zaentrum/appliance:latest`)
   runs a full Kubernetes (k3s) in-process with the operator already bundled
   inside it. You start one container and you have the whole product — see
   [self-hosting.md](self-hosting.md). The operator runs inside the appliance and
   reconciles a `Zaentrum` CR that the image ships with; `/manage` drives that same
   CR.

2. **This operator on your own cluster.** If you already run OpenShift or another
   OLM cluster, install this operator into it and create a `Zaentrum` CR yourself.
   The operator reconciles the platform into your cluster using your storage,
   ingress, and (optionally) GPU. `/manage` talks to the operator the same way it
   does in the appliance — the only difference is *where* the cluster lives.

Either way, the unit of configuration is the `Zaentrum` CR, and `/manage` reads and
writes it through the operator. Pick the appliance for the fastest path, or this
operator when you want Zaentrum to live alongside your other workloads on a cluster
you already operate.

## Install on OpenShift / any OLM cluster

### Quickest: run the bundle directly

If you have [`operator-sdk`](https://sdk.operatorframework.io/) and are logged in
to the cluster, install the operator straight from its bundle image:

```bash
kubectl create namespace zaentrum-operator
operator-sdk run bundle ghcr.io/zaentrum/operator-bundle:v0.1.0 -n zaentrum-operator
```

This creates an ephemeral catalog, a `Subscription`, and installs the
ClusterServiceVersion. When it reports succeeded, the controller is running.

Then create a namespace for the platform and apply a `Zaentrum` CR:

```bash
kubectl create namespace zaentrum
kubectl apply -f - <<'EOF'
apiVersion: zaentrum.io/v1alpha1
kind: Zaentrum
metadata:
  name: zaentrum
  namespace: zaentrum
spec:
  channel: stable
  version: latest
  hostname: zaentrum.localhost
  identity:
    mode: bundled
    clientId: chino-web
    audience: chino
  storage:
    mediaSize: 50Gi
  features:
    gpu: false
    kafka: true
  update:
    mode: manual
EOF
```

Watch it converge:

```bash
kubectl -n zaentrum get zaentrum zaentrum -w
# NAME    PHASE        VERSION   HOST              AGE
# zaentrum   Reconciling  ...       zaentrum.localhost   30s
# zaentrum   Ready        ...       zaentrum.localhost   3m
```

Set `spec.hostname` to the public host you will reach Zaentrum at — the OIDC issuer
host, the ingress host, and the in-cluster validation host must all match it. The
operator wires all of them to that single name. When `phase` reaches `Ready`,
open `https://<hostname>` and finish first-run setup at `/manage/setup`.

Uninstall (when you used `run bundle`):

```bash
operator-sdk cleanup zaentrum-operator -n zaentrum-operator
```

### Via a CatalogSource / OperatorHub

For a persistent install (the normal production path), add the operator's catalog
to the cluster and install it from OperatorHub. CI builds and publishes a catalog
(index) image that references the bundle above.

Create a `CatalogSource` pointing at that index image:

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: zaentrum-catalog
  namespace: openshift-marketplace   # use "olm" on upstream OLM clusters
spec:
  sourceType: grpc
  image: ghcr.io/zaentrum/operator-catalog:v0.1.0
  displayName: Zaentrum
  publisher: Zaentrum
  updateStrategy:
    registryPoll:
      interval: 30m
```

Once the catalog is `READY`, "Zaentrum" appears in the OpenShift OperatorHub
console. Install it there (it supports AllNamespaces install), or create a
`Subscription` directly:

```yaml
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: zaentrum-operator
  namespace: zaentrum-operator
spec: {}
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: zaentrum-operator
  namespace: zaentrum-operator
spec:
  channel: stable
  name: zaentrum-operator
  source: zaentrum-catalog
  sourceNamespace: openshift-marketplace
  installPlanApproval: Automatic
```

After the ClusterServiceVersion installs, create a `Zaentrum` CR exactly as in the
quick-start above.

## The Zaentrum CR

One CR drives the whole platform. The fields:

| Field | Meaning | Default |
|---|---|---|
| `spec.channel` | Release train consulted by auto-update (`stable` or `edge`). | `stable` |
| `spec.version` | Image tag applied to every `ghcr.io/zaentrum/*` image. | `latest` |
| `spec.hostname` | Public host — issuer host + ingress host + in-cluster validation host. | `zaentrum.localhost` |
| `spec.identity.mode` | `bundled` (ship Keycloak) or `external` (your own OIDC). | `bundled` |
| `spec.identity.issuer` | Issuer URL when `mode: external`. | — |
| `spec.identity.clientId` | Public OIDC client id. | `chino-web` |
| `spec.identity.audience` | Token audience. | `chino` |
| `spec.storage.mediaSize` | PVC size for the media library. | `50Gi` |
| `spec.storage.className` | StorageClass for the media PVC. | cluster default |
| `spec.features.gpu` | Enable hardware transcoding (needs a GPU node + device plugin). | `false` |
| `spec.features.kafka` | Run the bundled Kafka. | `true` |
| `spec.update.mode` | `manual` or `auto` — whether the operator applies updates from `channel`. | `manual` |

Status reports back: `phase`, `currentVersion`, `availableUpdate`, plus per-
component `ready`/`image` so you can see the rollout converge.

## Updating

With `spec.update.mode: manual`, bump `spec.version` (or `spec.channel`) and the
operator rolls the new images out. With `auto`, the operator watches the
configured channel and applies new releases itself, surfacing the next available
version on `status.availableUpdate` before it rolls.

The operator itself is upgraded through OLM — a new bundle published to the
catalog produces a new ClusterServiceVersion that OLM installs per your
`Subscription` approval policy.
