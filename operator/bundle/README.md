# Stube operator — OLM bundle (WIP / Stage 3)

> **Status: skeleton.** This directory lays out the [Operator Lifecycle
> Manager](https://olm.operatorframework.io/) (OLM) bundle structure so a later
> stage can publish the Stube operator to OperatorHub / OpenShift. It is **not a
> complete or installable bundle yet** — the ClusterServiceVersion (CSV) is a
> stub and the owned CRD is referenced by name only. Do not `operator-sdk run
> bundle` or add this to a catalog until Stage 3 finishes it.

## What this is

OLM installs operators from a *bundle image*: a `registry+v1` payload made of a
CSV (the operator's install metadata, RBAC, and managed Deployment), the CRDs it
owns, and a small set of annotations. This is the standard packaging an operator
needs to appear in OperatorHub and be installable via a `Subscription`.

The Stube operator (the Go controller-manager under `operator/`, owned by the
operator-core work) reconciles a single `Stube` custom resource into the **same
35-resource set** that `kubectl kustomize deploy/base` renders — namespace, the
bundled data/Kafka/Keycloak, `katalog-*`, `chino-*`, `admin`, the ingress, and
all the boot fixes. This bundle is how that operator gets installed by OLM.

It is content-neutral, like the rest of the repo: it ships only the platform,
with no downloaders or indexer integrations.

## Layout

```
operator/bundle/
├── bundle.Dockerfile     # builds the registry+v1 bundle image (scratch + labels)
├── manifests/            # CSV (+ CRD in Stage 3) — the installable payload
│   └── stube-operator.clusterserviceversion.yaml   # STUB
├── metadata/
│   └── annotations.yaml  # package=stube-operator, channel=stable
└── README.md             # this file
```

This is distinct from the operator **controller** image
(`ghcr.io/nalet/stube/operator`), which is built from `operator/Dockerfile` by
the `operator` matrix leg in
[`.github/workflows/build-images.yml`](../../.github/workflows/build-images.yml).
The CSV's managed Deployment references that controller image; this
`bundle.Dockerfile` packages the *metadata*, not the controller binary.

## What Stage 3 must finish

Everything below is marked `WIP` / `wip-stage-3` inline. None of it is
authoritative yet.

1. **Add the CRD manifest** to `manifests/` — copy the real `Stube` CRD the
   operator-core work ships under `operator/config`. The CSV's
   `customresourcedefinitions.owned` (group/kind/version) must match it exactly.
   The `stube.io/v1alpha1 Stube` naming used here is the *expected* convention,
   not yet pinned.
2. **Replace the CSV install spec** (`spec.install.spec.deployments` and
   `permissions`/`clusterPermissions`) with the operator's *real* manager
   Deployment and RBAC — those are owned by operator-core. The blocks in the
   stub are illustrative shape only.
3. **Set real version metadata** — `metadata.name` (`stube-operator.v<semver>`),
   `spec.version`, `spec.replaces`/`skips` upgrade graph, `containerImage`
   digest, `createdAt`, and `relatedImages` (the full neutral
   `ghcr.io/nalet/stube/<service>` set, pinned, for disconnected installs).
4. **Confirm channels / OCP range** in `metadata/annotations.yaml` and the
   matching `LABEL`s in `bundle.Dockerfile`.
5. **Validate**: `operator-sdk bundle validate ./operator/bundle` (or
   `opm alpha bundle validate`) must pass, then wire bundle build + publish into
   CI as a separate step from the controller image build.

## Building (once Stage 3 is done)

```bash
# From the repo root, after the CRD + real CSV are in place:
docker build -f operator/bundle/bundle.Dockerfile -t ghcr.io/nalet/stube/operator-bundle:<ver> operator/bundle
operator-sdk bundle validate operator/bundle
```
