# Zaentrum operator — OLM bundle

This directory is the [Operator Lifecycle Manager](https://olm.operatorframework.io/)
(OLM) bundle for the Zaentrum operator: the packaging that lets it appear in
OperatorHub and be installed via a `Subscription` on OpenShift or any OLM
cluster.

## What this is

OLM installs operators from a *bundle image*: a `registry+v1` payload made of a
ClusterServiceVersion (the operator's install metadata, RBAC, and managed
Deployment), the CRDs it owns, and a small set of annotations.

The Zaentrum operator (the Go controller-manager under `operator/`) reconciles a
single `Zaentrum` custom resource into the whole platform, rendered from the
Helm chart it embeds (`operator/platform/chart`): the portal, the catalog and
its consoles, chino, the bundled Postgres, Kafka and Keycloak, the Ingress or
the OpenShift Routes. This bundle is how that operator gets installed by OLM.

It is content-neutral, like the rest of the repo: it ships only the platform,
with no downloaders or indexer integrations.

## Layout

```
operator/bundle/
├── bundle.Dockerfile     # builds the registry+v1 bundle image (scratch + labels)
├── manifests/            # the installable payload
│   ├── zaentrum-operator.clusterserviceversion.yaml   # CSV (install spec + RBAC)
│   ├── zaentrum.io_zaentrums.yaml                         # owned Zaentrum CRD
│   └── zaentrum.io_zaentrumaddons.yaml                    # owned ZaentrumAddon CRD
├── metadata/
│   └── annotations.yaml  # package=zaentrum-operator, channel=stable
└── README.md             # this file
```

The CSV's install spec is derived directly from the operator's own manifests:

- the managed **Deployment** is copied from `operator/config/manager/manager.yaml`,
- the **clusterPermissions** are the rules from `operator/config/rbac/role.yaml`
  (plus leader-election leases/events), bound to the `serviceAccountName` from
  `operator/config/rbac/service_account.yaml`,
- the owned **CRDs** (`zaentrums.zaentrum.io`, `zaentrumaddons.zaentrum.io`) are
  copied from `operator/config/crd/`.

This is distinct from the operator **controller** image
(`ghcr.io/zaentrum/operator`), which is built from `operator/Dockerfile` by
the `operator` matrix leg in
[`.github/workflows/build-images.yml`](../../.github/workflows/build-images.yml).
The committed CSV is main's: `zaentrum-operator.v0.1.0`, its managed Deployment
on `ghcr.io/zaentrum/operator:latest`. A release tag's build stamps the release
into it first ([`stamp.sh`](stamp.sh)): `zaentrum-operator.vX.Y.Z`, version
`X.Y.Z`, the controller on `operator:vX.Y.Z`, and `olm.skipRange: "<X.Y.Z"` so
an install of any older version can move to it. This `bundle.Dockerfile`
packages the *metadata*, not the controller binary.

## Building

```bash
# From operator/bundle (in a scratch checkout: stamp.sh edits manifests/):
./stamp.sh v0.4.0
docker build -f bundle.Dockerfile -t ghcr.io/zaentrum/operator-bundle:v0.4.0 .
operator-sdk bundle validate .
```

CI builds and publishes the bundle image (and the catalog/index image that
references it) as a step separate from the controller image build: from `main`
as `:latest` and `:sha-<commit>`, from a release tag, stamped, as `:vX.Y.Z` and
`:X.Y`. A release's catalog serves its bundle on the `stable` channel.

## Installing

See the front door's
[operator guide](https://github.com/zaentrum/zaentrum/blob/main/docs/operator.md)
for how to install on OpenShift / any OLM cluster and create a `Zaentrum` CR.
