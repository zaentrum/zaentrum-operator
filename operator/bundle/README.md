# Stube operator — OLM bundle

This directory is the [Operator Lifecycle Manager](https://olm.operatorframework.io/)
(OLM) bundle for the Stube operator: the packaging that lets it appear in
OperatorHub and be installed via a `Subscription` on OpenShift or any OLM
cluster.

## What this is

OLM installs operators from a *bundle image*: a `registry+v1` payload made of a
ClusterServiceVersion (the operator's install metadata, RBAC, and managed
Deployment), the CRDs it owns, and a small set of annotations.

The Stube operator (the Go controller-manager under `operator/`) reconciles a
single `Stube` custom resource into the same resource set that
`kubectl kustomize deploy/base` renders — namespace, the bundled
data/Kafka/Keycloak, `katalog-*`, `chino-*`, `admin`, the ingress, and the boot
fixes. This bundle is how that operator gets installed by OLM.

It is content-neutral, like the rest of the repo: it ships only the platform,
with no downloaders or indexer integrations.

## Layout

```
operator/bundle/
├── bundle.Dockerfile     # builds the registry+v1 bundle image (scratch + labels)
├── manifests/            # the installable payload
│   ├── stube-operator.clusterserviceversion.yaml   # CSV (install spec + RBAC)
│   └── stube.io_stubes.yaml                         # owned Stube CRD
├── metadata/
│   └── annotations.yaml  # package=stube-operator, channel=stable
└── README.md             # this file
```

The CSV's install spec is derived directly from the operator's own manifests:

- the managed **Deployment** is copied from `operator/config/manager/manager.yaml`,
- the **clusterPermissions** are the rules from `operator/config/rbac/role.yaml`
  (plus leader-election leases/events), bound to the `serviceAccountName` from
  `operator/config/rbac/service_account.yaml`,
- the owned **CRD** (`stubes.stube.io`) is copied from
  `operator/config/crd/stube.io_stubes.yaml`.

This is distinct from the operator **controller** image
(`ghcr.io/zaentrum/stube/operator`), which is built from `operator/Dockerfile` by
the `operator` matrix leg in
[`.github/workflows/build-images.yml`](../../.github/workflows/build-images.yml).
The CSV's managed Deployment references that controller image at the release tag
(`v0.1.0`); this `bundle.Dockerfile` packages the *metadata*, not the controller
binary.

## Building

```bash
# From operator/bundle:
docker build -f bundle.Dockerfile -t ghcr.io/zaentrum/stube/operator-bundle:v0.1.0 .
operator-sdk bundle validate .
```

CI builds and publishes the bundle image (and the catalog/index image that
references it) as a step separate from the controller image build.

## Installing

See [`docs/operator.md`](../../docs/operator.md) for how to install on
OpenShift / any OLM cluster and create a `Stube` CR.
