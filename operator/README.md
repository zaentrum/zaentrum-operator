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

### The platform checks itself (`status.verification`)

After every update that leaves the platform Ready, the operator checks it the
way a user meets it — from outside, through the public URL, with a real sign-in —
and reports the verdict on the CR:

```yaml
status:
  verification:
    result:      Passed           # Passed | Failed | Running | Skipped | Error
    trigger:     update           # update | request
    request:     ""               # the last zaentrum.io/verify-request value answered
    fingerprint: 7dc90b436e94     # which platform was verified (below)
    version:     latest           # status.currentVersion when the run started
    startedAt:   2026-10-03T09:00:10Z
    finishedAt:  2026-10-03T09:00:15Z
    job:         zaentrum-verify-cxklt
    passed: 5
    failed: 0
    warned: 0
    skipped: 0
    checks:
    - { name: tls,            status: ok, detail: "certificate valid, 61 days left" }
    - { name: routes,         status: ok, detail: "3 public paths answer" }
    - { name: oidc issuer,    status: ok, detail: "https://media.example.org/auth/realms/zaentrum serves discovery (advertised by /api/config)" }
    - { name: sign-in,        status: ok, detail: "zaentrum-verify signed in through the zae client" }
    - { name: image registry, status: ok, detail: "ghcr.io/zaentrum/portal-api:latest pulls anonymously" }
    message: 5 of 5 checks passed
  conditions:
  - { type: Verified, status: "True", reason: Passed, message: 5 of 5 checks passed }
```

A failed run names what failed, in `checks` and in one line:

```yaml
    result: Failed
    trigger: request
    request: "1759482305"
    passed: 3
    failed: 1
    warned: 0
    skipped: 1
    checks:
    - { name: routes,  status: fail, detail: "not serving: /portal (502)" }
    - { name: sign-in, status: skip, detail: "the portal is down, nothing to sign in to" }
    # …
    message: "1 of 5 checks failed: routes; skipped: sign-in"
  conditions:
  - { type: Verified, status: "False", reason: Failed, message: "1 of 5 checks failed: routes; skipped: sign-in" }
```

`kubectl get zaentrum` shows the result in its `VERIFIED` column, and `zae
platform status` shows the whole of it.

**What runs.** The checks ship the way Helm says a chart's checks should: as a
chart test, [`templates/tests/verify.yaml`](platform/chart/templates/tests/verify.yaml)
(`helm.sh/hook: test`), so a plain Helm install runs them with `helm test`. The
operator never applies a test hook with the platform. It splits them off every
render, pins their images to digests like the rest, and starts the verification
Job itself, as a Job of its own (`zaentrum-verify-<suffix>`, owned by the
Zaentrum). The Job runs `zae doctor --url <public URL> --sign-in --report
/dev/termination-log` from `ghcr.io/zaentrum/zae` on the platform's tag: TLS,
the published routes, the issuer, a real sign-in and whatever else the doctor
checks. Its compact report is the container's termination message, which the
operator reads back from the pod — no log scraping, nothing in the run that may
write to the API. The public URL is `https://<hostname>` where the edge
terminates TLS (OpenShift Routes, or `identity.issuerScheme: https`), else
`http://<hostname>`. The Job gets the same `hostAliases` as the services that
validate tokens (`network.issuerHostAliasIP`), so the public host resolves from
inside the cluster the way it does for them. It runs under the restricted SCC:
non-root, no privilege escalation, every capability dropped, `RuntimeDefault`
seccomp, a read-only root filesystem, no service account token, one attempt
(`backoffLimit: 0`) and a ten-minute deadline.

**When a run happens.**

- *After an update.* The fingerprint is the first 12 hex characters of the
  sha256 over the sorted `deployment/container=image` lines of every platform
  Deployment, init containers included, with images as applied after digest
  pinning. When it differs from `status.verification.fingerprint` and the
  platform is Ready, a run starts. A replica count or a configuration change
  moves no image, so it starts no run by itself; ask for one.
- *On request.* Any value of the annotation `zaentrum.io/verify-request` that
  differs from `status.verification.request` asks for a run, once the platform
  is Ready. `zae platform verify` sets a fresh value, and so can you:

  ```sh
  kubectl -n zaentrum annotate zaentrum zaentrum --overwrite \
    zaentrum.io/verify-request="$(date +%s)"
  ```

  Only a run a request started answers it: an update-triggered run keeps the
  last answered value, so an annotation left in place never reads as a new
  request.
- *One at a time, nothing dropped.* An update or a request that arrives while a
  run is in flight waits for it to end; its verdict is written first, and the
  next run starts on the next pass. The previous run's Job is deleted when the
  next one starts, so at most the latest is kept, for its logs
  (`kubectl logs job/zaentrum-verify-…`); `ttlSecondsAfterFinished` (one day)
  is the backstop.
- *Never again by itself.* A failed run, or one that could not start, is not
  retried for the same fingerprint — a broken platform is not mended by asking
  again every 30 seconds. A new update or a new request starts the next run.

While a run is in flight the operator polls it every 10 seconds instead of 30
(a Job watch would cache every Job in the cluster) and says in `message` what
holds it up, if anything does — an image that does not pull, say — long before
the deadline turns it into a verdict.

**Passed, Failed, Error.** A readable report decides: any failed check is
`Failed`; `Passed` takes no failed check *and* exit code 0. Without a verdict
the result is `Error`, and `message` says why — the run could not start, the
test account could not be prepared, the deadline passed, the runner was killed,
the report could not be read (the kubelet keeps only the last 4 KiB of a
termination message), or a report and an exit code disagree. `status.verification`
holds at most 40 checks (failures and warnings kept first) with 200 characters
of detail each. Verification never moves the phase or the `Ready` condition and
never fails a reconcile: a failed check is a reading beside the platform's
status, not a degraded platform. The `Verified` condition is `True` on Passed,
`False` with reason `Failed` or `Error`, `Unknown` while `Running` and with
reason `Disabled` when verification is off.

**The test account.** With bundled identity the checks sign in as
`zaentrum-verify`, a regular user of the `zaentrum` realm. Its credentials live
in the Secret `zaentrum-verify` (keys `username`, `password`), which is the
operator's own state: created once — even with `secrets.external` — with a
32-character password from `crypto/rand`, owned by the Zaentrum, and never
rotated; only a key that has gone missing is filled in. Before every run an
init container, from the same Keycloak image the platform runs, prepares the
account through the in-cluster admin API with the bootstrap admin from
`zaentrum-keycloak-admin` ([`files/verify-account.sh`](platform/chart/files/verify-account.sh)):
enabled, email and name set and verified so profile checks never interrupt the
sign-in, no required actions, the Secret's password (set only when it does not
already sign in, so a password-history policy is no obstacle), the realm role
`zaentrum-user` and nothing else, and no brute-force lockout. A password changed
by hand, a lockout or a role granted by mistake heals itself on the next run.
No password leaves the cluster: it is never printed, never an argument on a
command line (kcadm reads it from the environment), and blanked out of anything
copied into status. To rotate it, delete the Secret; the next run makes a new
one and the account follows.

**External identity.** There is no realm to prepare an account in, so the
account is yours to provide: create the Secret `zaentrum-verify` in the
platform's namespace with the keys `username` and `password` of an account in
your identity provider that holds the role the platform's users hold
(`zaentrum-user`) and needs no second factor. Without it, the sign-in checks
skip and the rest still run. The operator never makes up an account there, and
removes the Secret it generated if the platform moved off bundled identity.

```sh
kubectl -n zaentrum create secret generic zaentrum-verify \
  --from-literal=username=verify@example.org --from-literal=password='…'
```

**Turning it off.** `spec.verification.enabled: false` stops the runs (one in
flight is abandoned) and reports `result: Skipped`. Turning it back on verifies
the platform as it then stands, and answers a request made in the meantime.

Where the public host cannot reach the front door from inside the cluster, the
route checks fail there even though users are fine — for instance where an
in-cluster DNS rewrite sends the host to the identity provider alone. Resolve
the host to the edge instead: `network.issuerHostAliasIP`, or a DNS rewrite to
the ingress, as the all-in-one appliance does (its CoreDNS entry names Traefik),
so a pod takes the routes a browser takes.

RBAC needs nothing new: the ClusterRole already holds `jobs`, `secrets` and
`pods` `get`/`list`; reading a run's pod is the first use of `pods/list` by the
operator itself. Pods, Jobs and the Secret are read uncached.

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
