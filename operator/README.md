# Zaentrum Operator

A controller-runtime operator (Operator SDK / kubebuilder layout) that
reconciles the **whole Zaentrum platform** from a single `Zaentrum` custom resource.

```
apiVersion: zaentrum.io/v1alpha1
kind: Zaentrum
metadata: { name: zaentrum, namespace: zaentrum }
spec:
  channel: stable          # stable | edge  (Stage-2 auto-update train)
  version: latest          # tag applied to every ghcr.io/zaentrum/* image
  hostname: zaentrum.localhost
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
   (`zaentrum-env-bc6ctgt9bg`, …). The operator owns and applies the full set
   atomically, so it uses **stable** names (`zaentrum-env`, `zaentrum-db`,
   `zaentrum-stream-signing`, `zaentrum-keycloak`, `zaentrum-keycloak-admin`) and
   references them by plain name from every pod (`envFrom` / `configMapKeyRef`
   / `secretKeyRef`).
2. **CR-driven parameterization.** Image tag → `{{ image "<svc>" }}` (resolves
   to `ghcr.io/zaentrum/<svc>:{{.Version}}`) on all 7 zaentrum images; issuer
   host → `{{.Hostname}}` (`OIDC_ISSUER`, `KC_HOSTNAME`, ingress host);
   issuer/clientId/audience from `spec.identity`; media PVC size from
   `spec.storage.mediaSize` (+ `storageClassName` when set); GPU overlay gated
   on `spec.features.gpu`; kafka resources gated on `spec.features.kafka`;
   bundled-identity resources (Keycloak Deployment/Service/realm + the
   `wait-for-oidc` initContainers + the `/auth` ingress path) gated on
   `identity.mode == bundled`.

Every **boot fix** from `deploy/base` is preserved verbatim: Keycloak `Service`
on `:80`, management-port (`:9000`) `/auth/health/{ready,live}` probes,
`wait-for-oidc` init containers, base64 `zaentrum-stream-signing` key, the `zaentrum`
realm import `ConfigMap`, and the CoreDNS-friendly
`http://<host>/auth/realms/zaentrum` issuer.

### Reconcile

`internal/controller/zaentrum_controller.go`:

1. Render the embedded templates with the CR's (defaulted) values.
2. Decode each rendered document into an `unstructured.Unstructured`.
3. Set the `Zaentrum` as controller owner on every **namespaced** object (so they
   cascade-delete with the CR; the cluster-scoped `Namespace` is skipped).
4. **Server-side apply** each object: `Patch(ctx, obj, client.Apply,
   client.FieldOwner("zaentrum-operator"), client.ForceOwnership)`. SSA makes the
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

### The controller reports itself (`status.controller`)

Everything above is the **platform**. `status.controller` is the **operator**:

```yaml
status:
  controller:
    image:           ghcr.io/zaentrum/operator:sha-a3d32ba…   # as the pod spec writes it
    version:         sha-a3d32ba…      # the tag, else a 12-char short digest, else "unknown"
    source:          manifest          # olm | manifest | appliance | unknown
    availableUpdate: ""                # a newer version on the channel; "" when none/unknown
    observedAt:      2026-09-22T08:14:03Z
```

Reported, never acted on. Replacing a control plane is a cluster-admin / OLM /
GitOps job, and an operator that can upgrade itself mid-reconcile is a failure
mode, not a feature — Flux, Argo CD, cert-manager and every OLM-managed
operator draw the line in the same place. What the product owes its operator is
the *fact*, so "which operator is this cluster running, and is it current?"
stops being a question only `kubectl` can answer.

- **image / version** come from the pod the controller runs in, found through
  `POD_NAME` / `POD_NAMESPACE` (downward API, injected by every install
  bundle). Never a constant stamped in at build time: that is a claim about the
  past, and it goes stale the moment someone repoints a tag.
- **source** is derived, not configured. A `ClusterServiceVersion` owning the
  controller's Deployment (or pod) means `olm`. Otherwise
  `ZAENTRUM_INSTALL_SOURCE`, which only the all-in-one image sets — it bakes
  the same manifests a cluster-admin would apply, so nothing in the API tells
  the two apart. Otherwise `manifest`. `unknown` when the pod is unreadable.
  OLM outranks the env: it owns the upgrade path of what it installed.
- **availableUpdate** reuses the same channel document the platform reads
  (`internal/updates`), against the controller's own repository. Where the
  registry answers, the comparison is by **digest** through the shared cache in
  `internal/digest` — an operator pinned to `:sha-<commit>` that *is* the
  channel's current image must not be told to update forever, and a
  digest-pinned one has no tag to compare at all. Discovery failure costs the
  field, never the reconcile.
- **observedAt** marks when the reading last *changed*. A timestamp that moved
  every pass would make every status write a real change, and the CR watch
  would turn each one straight back into another reconcile.

A pinned `spec.version` opts the CR out of channel tracking for the platform
*and* the controller: an air-gapped pinned install makes no outbound call, so
`availableUpdate` stays `""`.

RBAC is unchanged — `pods/get` and `deployments/get` were already in the
ClusterRole (held so the operator may grant them to `portal-api`). The
Deployment's name is derived from the pod's ReplicaSet owner rather than read,
so this report does not add a `replicasets` rule.

## Addons (`ZaentrumAddon`)

A second, isolated reconciler installs a standard Helm chart next to the
platform, one namespaced `ZaentrumAddon` per addon (`internal/addon`,
`internal/controller/zaentrumaddon_controller.go`). The chart is fetched,
rendered in memory, checked against guardrails and applied with server-side
apply as field manager `zaentrum-addon`; an addon error never touches the
platform phase.

### Trust boundary

An addon chart is **untrusted input**. The operator, not the chart, decides what
may run:

- Only a small set of namespaced kinds; pods run non-root, no host namespaces,
  no privilege escalation, no control-plane placement, no escape-hatch security
  context, and no ServiceAccount token unless the chart opts in.
- A rendered object may only reference the platform Secrets/ConfigMaps the chart
  itself renders, plus the ones the platform hands the addon in the reserved
  `zaentrum` values (events TLS secret, image pull secrets). Secret `type`
  `kubernetes.io/service-account-token` and the `service-account.name/uid`
  annotations are refused, so a chart cannot mint a platform SA's token.
- `valuesFrom` may read only the addon's **own** values objects
  (`zaentrum-addon-<name>-*` labelled `zaentrum.io/addon=<name>`), so the
  operator's cluster-wide read access cannot be turned into a confused deputy.
  A chart may render nothing named `zaentrum-addon-*`, so it cannot squat any
  addon's values or generated Secret names.
- Render errors report only a location, never the chart-controlled message body,
  so a secret input cannot be echoed back through `status`.
- Chart fetch (https + OCI, redirects included) refuses loopback, link-local,
  the cloud-metadata addresses and the in-cluster API server (SSRF), checked at
  the dialer so DNS rebinding cannot bypass it.

The chart-rendered `portal-api` Role is granted `secrets` **create only**. Each
secret write creates a new immutable Secret (`generateName`
`zaentrum-addon-<addon>-values-`, labelled `zaentrum.io/addon=<addon>`) and
repoints the addon's `valuesFrom` at it; `portal-api` never reads, patches or
deletes a Secret. The operator collects values Secrets nothing references any
more (after a 10-minute grace for the create-then-update) and sweeps those of
addons that no longer exist (after an hour). `portal-api` still holds
`deployments` patch, so the namespace remains the trust boundary.

Removing an addon with the annotation `zaentrum.io/keep-values: "true"` keeps its
values and generated Secrets (the operator's `zaentrum.io/addon-values`
finalizer strips their owner references and labels them `zaentrum.io/keep=true`).
Keep relies on the default (background) deletion propagation. If the operator is
no longer running, that finalizer has to be removed by hand for a removal to
finish.

## Build / test

```sh
go build ./...     # compiles
go test  ./...     # template render + boot-fix assertions
```

The render test asserts the default `Zaentrum` produces **35 objects** including a
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
config/samples/          example Zaentrum CR
Dockerfile               multi-stage, distroless → ghcr.io/zaentrum/operator
```

## Install

```sh
kubectl apply -f config/crd/
kubectl apply -f config/rbac/
kubectl apply -f config/manager/manager.yaml
kubectl apply -f config/samples/   # creates ns 'zaentrum' worth of platform
```
