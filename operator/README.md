# Stube Operator

A controller-runtime operator (Operator SDK / kubebuilder layout) that
reconciles the **whole Stube platform** from a single `Stube` custom resource.

```
apiVersion: stube.io/v1alpha1
kind: Stube
metadata: { name: stube, namespace: stube }
spec:
  channel: stable          # stable | edge  (Stage-2 auto-update train)
  version: latest          # tag applied to every ghcr.io/zaentrum/* image
  hostname: stube.localhost
  identity: { mode: bundled, clientId: chino-web, audience: chino }
  storage:  { mediaSize: 50Gi }
  features: { gpu: false, kafka: true }
  update:   { mode: manual }   # manual | auto  (Stage-2)
```

## How it works

The platform's deployable manifests live in `../deploy/base` (35 objects via
`kubectl kustomize deploy/base`). The operator does **not** hand-rewrite those
35 resources as Go structs. Instead, `internal/templates/data/*.yaml` are Go
`text/template` files derived 1:1 from the rendered `deploy/base`, embedded via
`go:embed`, with two deliberate edits:

1. **Un-kustomized names.** Kustomize hashes `ConfigMap`/`Secret` names
   (`stube-env-bc6ctgt9bg`, …). The operator owns and applies the full set
   atomically, so it uses **stable** names (`stube-env`, `stube-db`,
   `stube-stream-signing`, `stube-keycloak`, `stube-keycloak-admin`) and
   references them by plain name from every pod (`envFrom` / `configMapKeyRef`
   / `secretKeyRef`).
2. **CR-driven parameterization.** Image tag → `{{ image "<svc>" }}` (resolves
   to `ghcr.io/zaentrum/<svc>:{{.Version}}`) on all 7 stube images; issuer
   host → `{{.Hostname}}` (`OIDC_ISSUER`, `KC_HOSTNAME`, ingress host);
   issuer/clientId/audience from `spec.identity`; media PVC size from
   `spec.storage.mediaSize` (+ `storageClassName` when set); GPU overlay gated
   on `spec.features.gpu`; kafka resources gated on `spec.features.kafka`;
   bundled-identity resources (Keycloak Deployment/Service/realm + the
   `wait-for-oidc` initContainers + the `/auth` ingress path) gated on
   `identity.mode == bundled`.

Every **boot fix** from `deploy/base` is preserved verbatim: Keycloak `Service`
on `:80`, management-port (`:9000`) `/auth/health/{ready,live}` probes,
`wait-for-oidc` init containers, base64 `stube-stream-signing` key, the `stube`
realm import `ConfigMap`, and the CoreDNS-friendly
`http://<host>/auth/realms/stube` issuer.

### Reconcile

`internal/controller/stube_controller.go`:

1. Render the embedded templates with the CR's (defaulted) values.
2. Decode each rendered document into an `unstructured.Unstructured`.
3. Set the `Stube` as controller owner on every **namespaced** object (so they
   cascade-delete with the CR; the cluster-scoped `Namespace` is skipped).
4. **Server-side apply** each object: `Patch(ctx, obj, client.Apply,
   client.FieldOwner("stube-operator"), client.ForceOwnership)`. SSA makes the
   operator the declarative owner of exactly the fields it sets — the API
   server merges intent, prunes fields the operator dropped, and re-applying an
   identical object is a no-op (no read-modify-write conflicts). `ForceOwnership`
   reclaims any field a prior manager (e.g. `kubectl`) touched.
5. Refresh `status`: `phase`, `currentVersion` (= `spec.version`),
   `components[]` readiness (read live `Deployments`), `conditions[]`
   (`ResourcesApplied`, `Ready`), `observedGeneration`. Requeue every 30s.

**Stage 2 (auto-update)** is stubbed: `spec.channel` and `spec.update.mode` are
stored and surfaced into `status.availableUpdate`; the tag-discovery + image
bump logic is marked `TODO(S2)` in the reconciler.

## Build / test

```sh
go build ./...     # compiles
go test  ./...     # template render + boot-fix assertions
```

The render test asserts the default `Stube` produces **35 objects** including a
`Deployment` named `keycloak` with a `:80` `Service` and a `/auth` health probe
on the management port.

## Layout

```
api/v1alpha1/            CRD types + deepcopy + scheme
internal/templates/      go:embed manifests + renderer + tests
internal/controller/     the reconciler (server-side apply)
config/crd/              generated CRD
config/rbac/             ServiceAccount + ClusterRole/Binding (CRUD on all kinds)
config/manager/          operator Deployment + namespace
config/samples/          example Stube CR
Dockerfile               multi-stage, distroless → ghcr.io/zaentrum/operator
```

## Install

```sh
kubectl apply -f config/crd/
kubectl apply -f config/rbac/
kubectl apply -f config/manager/manager.yaml
kubectl apply -f config/samples/   # creates ns 'stube' worth of platform
```
