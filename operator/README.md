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
  storage:  { mediaSize: 50Gi, postgres: { size: 10Gi } }
  features: { gpu: false, kafka: true }
  update:   { mode: manual }   # manual | auto  (Stage-2)
```

## How it works

The platform is the Helm chart in [`platform/chart`](platform/chart) — the
chart a plain `helm install` takes — compiled into the binary (`go:embed`,
`platform/embed.go`). The operator renders it with Helm's template engine
alone, client-side: no release, no Helm apply (`internal/templates`). The CR's
spec maps onto the chart's values (`chartValues`; the keys `values.yaml`
documents), with what the operator decides each pass: the tag to run, whether
the cluster is OpenShift and serves cert-manager, where the bundled Postgres
keeps its data, whether a restore runs, the platform's certificate for the
Routes.

### Reconcile

`internal/controller/zaentrum_controller.go`, every 30 seconds — every 10 while
a run is in flight:

1. Read the Zaentrum from the API server, not the cache: the pass starts Jobs
   from what status says about the last ones.
2. Decide what the render needs: the release channel's tag
   (`internal/updates`), OpenShift (asked once), where the bundled Postgres
   keeps its data (`database.go`), a restore in flight (`restore.go`), the
   platform's certificate (`tls.go`).
3. Render the chart and split off its hooks — the verification, the realm
   Job, the database copy, the restore — which are never applied with the
   platform: the operator starts each itself, as a Job of its own, when its
   time comes.
4. Pin every `ghcr.io/zaentrum/*` image to its current digest
   (`internal/digest`), so a new push on a moving tag rolls.
5. Make the platform's Secrets that are missing, once (`secrets.go`).
6. **Server-side apply** every object as field manager `zaentrum-operator`
   with `ForceOwnership`: the operator owns exactly the fields it sets, the API
   server prunes the ones it stopped setting, and an identical apply is a
   no-op. The Zaentrum is controller owner of every namespaced object — but
   what the chart marks `helm.sh/resource-policy: keep`, the claim with the
   backups — so they go with the CR. An object the render no longer carries is
   not removed; the backups' CronJob, once backups are off, is removed
   explicitly.
7. Take the restore's step, read the backups and the certificate, refresh
   `status` — `phase`, `currentVersion`, `components[]`, the conditions,
   `status.controller` — and then keep the realm in step and verify the
   platform.

**Release channels.** `spec.channel` resolves through the front door's
`releases.json` (`internal/updates`) to a tag: `stable` to the newest release,
`edge` to `latest`. With `spec.update.mode: auto` that tag is rendered. With
`manual` an install keeps the version it runs (`status.currentVersion`) and
reports the channel's tag in `status.availableUpdate`; a new install starts on
the channel's tag, and a channel on `latest` is followed in both modes. When
the document cannot be read, an install keeps what it runs. Digest pinning
resolves the rendered tag — the release's, or `latest` push by push — so a push
to `main` moves only installs that render `latest`.

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
checks. A platform tag the zae image does not carry — a release cut without a
zae image of the same tag — would leave the run unable to start, so when the
registry says that tag does not exist, the run checks with `zae:latest` (pinned
like every image) and its message ends with "checked with zae:latest, as no zae
image is tagged <tag>". The checks are outside-in and skip what an instance does
not offer, so a newer checker suits an older platform. A registry that cannot be
asked keeps the tag. Its compact report is the container's termination message, which the
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
  is the backstop. "In flight" is what the API server holds, not what status
  says: a run Job that has not finished is never replaced, even by a pass whose
  status does not name it, and a run whose Job is gone while another run's Job
  is there is followed to that one, whose verdict is read. (A push that moved
  two images a second apart once had a pass, working from a stale read of the
  Zaentrum, replace the run in flight; the first was reported as an Error and
  the second's result never read.)
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

### The platform's Secrets

Unless `spec.secrets.external` says someone else provides them, the operator
makes every Secret the platform reads, before anything that reads it is
applied, and once:

| Secret | Keys | |
|---|---|---|
| `zaentrum-db` | `user` (`zaentrum`), `password` | the bundled Postgres's superuser |
| `zaentrum-stream-signing` | `key` | 32 random bytes, base64 |
| `zaentrum-keycloak` | `client-secret` | the `zaentrum-manager` client (bundled identity) |
| `zaentrum-keycloak-admin` | `username` (`admin`), `password`, `realm-admin-password` | the master realm's bootstrap admin; the first administrator's one-time password (bundled identity) |
| `zaentrum-demo-user` | `password` | the realm import's `${DEMO_USER_PASSWORD}` (bundled identity) |
| `zaentrum-people` | `client-secret`, `deletion-token` | the `zaentrum-people` client, which portal-api manages the realm's people with; the token chino-api and portal-api delete an account with ([below](#people-and-the-rating-cap)) (bundled identity) |

Every value comes from `crypto/rand` and is alphanumeric. Each Secret is owned
by the Zaentrum (it goes with the platform) and labelled
`zaentrum.io/generated=true`. Nothing in one is ever rotated by a reconcile: a
key that went missing is filled in, a value that is there stays, because it
also lives where the Secret does not reach — the database's role, the realm's
client, Keycloak's admin user. The chart renders none of them for the
operator; a plain `helm install` makes its own, random, and reads them back on
every upgrade (`templates/secrets.yaml`).

**The first administrator** signs in as `admin` with a one-time password; the
realm import marks it temporary, so Keycloak asks for a new one at that first
sign-in. Status says where it is, never what it is:

```sh
kubectl get zaentrum zaentrum -n zaentrum \
  -o jsonpath='{.status.conditions[?(@.type=="SecretsGenerated")].message}'
kubectl -n zaentrum get secret zaentrum-keycloak-admin -o jsonpath='{.data.realm-admin-password}' | base64 -d
```

The bootstrap admin's `password` is a machine credential: the operator's Jobs
sign in to the admin API with it (`kcadm`). It is also how a person reaches the
admin console through a port-forward ([below](#the-admin-console)).

**An install made from an earlier chart** still holds the values that chart
shipped to every install. `SecretsGenerated` is then `False`, reason
`PublishedDefaults`, and names each Secret and key. They are reported, not
replaced: each has to change where it is used first. Through a port-forward or
`kubectl exec`, for instance:

```sh
ns=zaentrum
new() { LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32; }

# The database: its role first, then the Secret.
pw=$(new)
kubectl -n $ns exec deploy/postgres -- psql -U zaentrum -c "ALTER ROLE zaentrum PASSWORD '$pw'"
kubectl -n $ns patch secret zaentrum-db -p "{\"stringData\":{\"password\":\"$pw\"}}"

# The signing key: nothing else holds it.
kubectl -n $ns patch secret zaentrum-stream-signing -p "{\"stringData\":{\"key\":\"$(openssl rand -base64 32)\"}}"

# zaentrum-manager's secret (Clients → zaentrum-manager → Credentials) and the
# bootstrap admin's password (master realm → Users → admin → Credentials): in the
# admin console through a port-forward, then the same values in zaentrum-keycloak
# and zaentrum-keycloak-admin. realm-admin-password only matters until the first
# sign-in: if admin has never signed in, sign in now and choose a password.

# Then restart what reads them — never the Postgres itself while it runs on an
# emptyDir: its restart would empty the database.
kubectl -n $ns rollout restart deploy -l 'app!=postgres'
```

With `secrets.external` (the demo, beta) the operator makes, reads and reports
none of them.

### The bundled Postgres keeps its data (`spec.storage.postgres`)

```yaml
spec:
  storage:
    postgres:
      size: 10Gi            # the claim postgres-data, ReadWriteOnce
      className: ""         # default storage.className, else the cluster's default
      claimName: ""         # an existing claim to keep it on instead
      migrate: false        # copy a database that lives elsewhere onto the claim
```

A new install keeps users, watch state and the catalog on a claim, so they
outlive the pod. Where the data is, though, is read from the running Postgres
before every render, and a Postgres that already runs somewhere else — on an
`emptyDir`, as the chart had it before, or on another claim — stays there: a
Postgres started on a new volume starts empty. `DatabasePersistent` says which:

| Reason | |
|---|---|
| `OnClaim` (True) | on its claim |
| `EmptyDir` / `OtherClaim` (False) | where it ran before; `migrate` moves it |
| `Migrating` (False) | a copy is running |
| `Migrated` (True) | the copy succeeded; the Postgres switches now |
| `MigrationFailed` (False) | the copy failed, the reason beside it; the Postgres stays |

**Moving it.** Set `spec.storage.postgres.migrate: true`. The operator applies
the claim and starts `postgres-migrate-<suffix>` (the chart's
`templates/postgres-migrate.yaml`), from the Postgres's own image and Secret,
the claim mounted where the Postgres mounts it, while the Postgres keeps
serving. The Job initialises the claim as the image does at a first start,
then copies the roles and every database from the running Postgres —
`pg_dump` into `psql`, stopping at the first error — checks each copy holds its
source's tables, and records the checkpoint it stopped at. It writes only into
an empty claim or an earlier copy of its own nothing has run on, and refuses
anything else. Once it has succeeded, the next pass switches the Postgres onto
the claim; a copy older than five minutes is taken again instead, since it has
missed what was written since. While it copies, neither a realm run nor a
verification run starts.

Writes made while the copy runs and until the Postgres has switched — a
minute, typically — are not carried across: do it when the platform is quiet.
A failed copy is not repeated by itself; `kubectl logs job/postgres-migrate-…`
says what happened, and deleting the Job asks for another. `migrate` may stay
set: on its claim there is nothing to move.

A plain Helm release moves the same way, in two upgrades: one with
`storage.postgres.migrate=true` (the copy is a post-upgrade hook, the Postgres
stays), then, once it succeeded, one with `storage.postgres.current=postgres-data`
and `migrate=false` (the switch). Without `current`, an upgrade looks at the
running Postgres and keeps it where it is.

On a cluster without dynamic provisioning the claim cannot come by itself:
make a PersistentVolume and a claim bound to it, as for `storage.kafkaPvc`,
and name the claim in `claimName` — the chart then creates none, and the copy
and the Postgres follow the volume to its node.

With `databases.mode: external` the databases are the tenant's: no claim, no
copy, no condition.

### The platform's certificate (`spec.tls`)

```yaml
spec:
  hostname: media.example.org
  tls:
    secretName: zaentrum-tls         # default; kubernetes.io/tls: tls.crt, tls.key (ca.crt)
    issuerRef:                       # optional: cert-manager issues it into that Secret
      name: letsencrypt
      kind: ClusterIssuer            # Issuer (default) | ClusterIssuer
```

With `spec.tls` the platform serves its hosts — `hostname`, and in subdomains
routing `routing.hosts.chino` — over https with a certificate of its own, and
every URL it derives is https: the issuer, Keycloak's `KC_HOSTNAME`, the
sign-in redirects (the realm Job writes them), the public URL its checks
use. That holds whatever `identity.issuerScheme` says, which keeps its
default, `http`, for an install without TLS.

- **A Secret you bring.** The Ingresses' `tls` name it. OpenShift Routes
  cannot name a Secret: the operator reads it every pass and the Routes carry
  it inline, edge-terminated as before; a renewed certificate reaches them on
  the next pass. (Anyone who may read the namespace's Routes may read the key
  there, as with any Route that carries its own certificate.)
- **cert-manager.** With `issuerRef`, where the cluster serves cert-manager's
  API, the chart renders a `Certificate` named `zaentrum` for the hosts, and
  cert-manager writes and renews the Secret. The operator's ClusterRole holds
  `cert-manager.io/certificates`; `deploy/operator-install.yaml` gains the rule
  with its next re-pin.

Keep `network.issuerHostAliasIP` (or a DNS entry) pointing the hostname at the
ingress from inside the cluster, so the services validating tokens reach the
https issuer; they trust the public certificate authorities their images
carry, so use a certificate from one — as the phone and TV apps only trust
those too.

The `TLS` condition says how the hosts are served, and never moves the phase:

| Reason | | |
|---|---|---|
| `Certificate` | True | the platform's certificate, for every host, valid until the date it names |
| `Router` | True | no `spec.tls`; the OpenShift Routes, with the router's certificate and an https issuer |
| `TerminatedInFront` | True | no `spec.tls`; `identity.issuerScheme: https`, TLS ends in a proxy in front |
| `Issuing` | Unknown | cert-manager has not written the Secret yet; what cert-manager says |
| `PlainHTTP` | False | plain http: a browser signs in only on a `localhost` name, phones and TVs not at all |
| `IssuerSchemeHTTP` | False | Routes serve https, but the issuer is http |
| `SecretMissing`, `BadCertificate`, `WrongHost`, `Expired` | False | no Secret, no certificate or key in it, one for another host, one past its date |
| `NoCertManager` | False | an `issuerRef`, and no cert-manager in the cluster |

### Backups of the bundled Postgres (`spec.backup`)

```yaml
spec:
  backup:
    enabled: true          # unset: on wherever the bundled Postgres is on a claim
    schedule: "@daily"     # a CronJob schedule, the controller manager's time zone
    retention: 7           # dumps kept, newest first
    size: 5Gi              # the claim backups, ReadWriteOnce
    className: ""          # empty: the Postgres's StorageClass
    claimName: ""          # an existing claim instead, e.g. one kept from before
```

The CronJob `zaentrum-backup` (the chart's `templates/backup.yaml`) dumps every
platform database — `databases.chino`, `.katalog`, `.keycloak`, `.portal` —
over the network while the platform serves, from the image the Postgres runs
([`files/postgres-backup.sh`](platform/chart/files/postgres-backup.sh)). One
run at a time; one that was missed while the cluster was down runs within six
hours. Each run writes one dump, a directory on the claim `backups` named
after when it began, in UTC:

```
backups/2026-10-04T00-00-05Z/
  globals.sql     the roles and their grants, but the superuser's
  chino.dump      each database, pg_dump's custom format, compressed
  katalog.dump
  keycloak.dump
  portal.dump
  SHA256SUMS      the sha256 of each file above
```

It is written as `.partial-<name>` and renamed once whole; then the newest
`retention` dumps stay and the rest go. Each database's dump is one consistent
snapshot of it; the databases are dumped one after another.

Backups are on by default wherever the bundled Postgres keeps its data on a
claim — every new install — and off where it still runs on an `emptyDir`
(move it with `spec.storage.postgres.migrate`, or set `enabled: true`) and
with `databases.mode: external`, whose backups are the tenant's. Turned off,
the operator removes the CronJob; the claim stays. The claim is not owned by
the Zaentrum (nor by a Helm release: `helm.sh/resource-policy: keep`), so
deleting the platform leaves the backups; delete the claim yourself.

The operator reads each run's summary from its pod, as it reads a
verification run's:

```yaml
status:
  backup:
    lastSuccess: 2026-10-04T00:01:12Z
    lastDump:    2026-10-04T00-00-05Z      # the name a restore asks for
    lastFailure: 2026-10-02T00:00:40Z
    dumps: [2026-10-04T00-00-05Z, 2026-10-03T00-00-04Z, …]
    job: zaentrum-backup-29324160
  conditions:
  - { type: Backup, status: "True", reason: Succeeded,
      message: "2026-10-04T00-00-05Z: 4 databases, 12.0 MiB; 7 dumps kept, 4.1 GiB free" }
```

`Backup` is `False` with reason `Failed` — the run's own reason, and the last
dump that succeeded — when the latest run failed, `Disabled` when backups are
off, and `Unknown` (`Scheduled`) until the first run. RBAC: the operator's
ClusterRole holds `batch/cronjobs`; `deploy/operator-install.yaml` gains the
rule with its next re-pin.

**Restoring.** Annotate the Zaentrum with the dump's name:

```sh
kubectl -n zaentrum get zaentrum zaentrum -o jsonpath='{.status.backup.dumps}'; echo
kubectl -n zaentrum annotate zaentrum zaentrum --overwrite \
  zaentrum.io/restore-request=2026-10-04T00-00-05Z
kubectl -n zaentrum get zaentrum zaentrum -o jsonpath='{.status.backup.restore}'; echo
```

The operator then:

1. stops every client of the database — chino-api, katalog-api,
   katalog-manager-api, keycloak, portal-api, and the pipeline's workers and
   katalog-ingest, which write through katalog-manager-api — and suspends the
   backups (`result: Stopping`; the phase is `Restoring`, the platform is
   down meanwhile);
2. once none of their pods is left and no backup runs, starts
   `postgres-restore-<suffix>` ([`files/postgres-restore.sh`](platform/chart/files/postgres-restore.sh),
   `result: Running`), which makes sure the dump is whole — the directory is
   there, `SHA256SUMS` names `globals.sql` and a dump of every database and
   nothing else is beside them, every checksum matches, `pg_restore` reads
   each dump — and **refuses**, changing nothing, when it is not; waits for any
   other session to leave the databases; makes the roles of the dump the
   Postgres lacks (never the superuser, whose password stays Secret
   `zaentrum-db`'s); recreates each database from its dump, stopping at the
   first error; and checks each holds the tables its dump lists;
3. reads the result — `Succeeded`, `Refused` (nothing was changed) or
   `Failed` (one database may already be restored: restore again) — and starts
   the clients again; after a success the realm Job runs again over the
   restored realm.

`status.backup.restore` and the `Restore` condition say where it is. A request
is answered once: to restore the same dump again, give it a value of its own,
`2026-10-04T00-00-05Z#2`. A value that names no dump, external databases, or
no claim `backups` are refused at once. A restore waits for a database copy
(`spec.storage.postgres.migrate`) to end, and neither a verification nor a
realm run starts while one runs. What was written after the dump was made is
gone once it is restored.

With plain Helm, the same in two upgrades: `--set backup.restore=<dump>`
stops the clients, suspends the backups and runs the restore as a post-upgrade
hook (`kubectl logs job/postgres-restore`); an upgrade without it starts them
again.

**What a backup does not hold.**

- *The platform's Secrets* — `zaentrum-db`, `zaentrum-keycloak` (the
  `zaentrum-manager` client's secret, which the restored realm also holds),
  `zaentrum-keycloak-admin` (the bootstrap admin, whose password the restored
  master realm also holds), `zaentrum-demo-user`, `zaentrum-people` (the
  `zaentrum-people` client's secret, which the realm Job sets again, and the
  account deletion token) and `zaentrum-stream-signing`.
  Restoring into the install that made the dump needs
  none of them; restoring into a new one — another cluster, a re-created
  appliance — needs those it had. Keep them once, somewhere safe, as you would
  the dumps, which hold the realm's password hashes and client secrets:

  ```sh
  kubectl -n zaentrum get secret zaentrum-db zaentrum-keycloak zaentrum-keycloak-admin \
    zaentrum-demo-user zaentrum-people zaentrum-stream-signing -o yaml > zaentrum-secrets.yaml
  ```

  Into a new install: create the namespace, apply them (drop `ownerReferences`,
  `uid` and `resourceVersion` first) before the Zaentrum, so the operator finds
  them and makes none; bring the claim with the dumps along
  (`spec.backup.claimName`); once the platform is up, restore. With
  `secrets.external` they are yours already.
- *The media library.* It lives on the `media` volume: back that up as you
  back up your files. The packaged streams beside it are made again by the
  pipeline.
- Kafka's topics and Valkey's cache, which the platform makes again.

### The media pipeline (`spec.pipeline`)

`features.pipeline` runs the workers that make a title playable everywhere:
the analyzer, the transcoder, the packager and katalog-ingest. `spec.pipeline`
says how:

```yaml
spec:
  features: { pipeline: true }
  pipeline:
    encoder: gpu               # gpu | cpu
    ladder: ""                 # e.g. "source,720p" (the transcoder's LADDER)
    segmentSeconds: 6          # 1..30 (SEGMENT_SECONDS of both workers)
    surroundAudio: "off"       # off | eac3 | ac3
    hlsSubtitles: false
    preferredLanguages: []     # e.g. [de, en]
```

| Field | Default | |
|---|---|---|
| `encoder` | `gpu` | `gpu`: NVENC — the transcoder asks for `nvidia.com/gpu: 1` and is placed on a node labelled `nvidia.com/gpu.present=true`, tolerating the GPU taint, as before this field existed. `cpu`: libx264/libx265 on any node, no GPU asked for, tolerated or looked for, the transcoder asking for 500m CPU and 1Gi (limits 4 CPUs, 8Gi) |
| `ladder` | one rendition a title | extra renditions, `<source\|NNNp>[:<hevc\|h264>][:<maxrate>]` separated by commas; checked by the CRD, as the transcoder would refuse a typo at its start |
| `segmentSeconds` | the workers' 6 | the HLS segment length and the transcoder's keyframe interval |
| `surroundAudio` | off | a 5.1 rendition beside each surround track's stereo one; leave it off until chino-stream keeps it from players that cannot decode it |
| `hlsSubtitles` | false | name the WebVTT renditions in the HLS master; the clients draw the sidecars themselves |
| `preferredLanguages` | the catalog's language list | the order the default audio track is picked in |

An empty field is not passed on, so each worker keeps its own default and an
install that sets nothing renders its workers exactly as before. Either
encoder passes through a source the clients play as it is — HEVC, or H.264 a
browser decodes (8-bit 4:2:0, up to High) — which costs a remux. Anything else
(MPEG-2, VC-1, AV1, 10-bit H.264) is an encode: minutes on NVENC, hours a
title with x265 on a few cores, one title at a time. The packaged streams live
beside the library on the `media` volume, about as large again as what they
were made from, and more with a ladder.

### Sign-in redirects, and the realm Job

The clients people sign in through return only to the platform's own origins
— derived from `hostname` and the routing (`z.realmClients`) — and allow only
the flows their apps use, each a public client with PKCE (S256):

| Client | Who signs in | Flows | Redirect URIs | Post-logout |
|---|---|---|---|---|
| `chino-web` | the web app | code, device | `<origin>/auth/callback` | `<origin>` |
| `zaentrum-web` | the portal and the catalog consoles | code | `<origin>/portal/`, `/katalog/`, `/katalog-manage/` + `auth/callback` | `<origin>` + those bases |
| `chino-mobile` | the phone and tablet apps | code | `cloud.nalet.chino:/oauth/callback` | its redirect |
| `chino-tv` | the Android TV and Tizen apps | device | none | |
| `zae` | the command line | code, device | `http://127.0.0.1/*`, `http://localhost/*` | its redirects |

The apps learn which client is theirs from chino-api's `GET /api/config`
(`oidcClientId.web`, `.tv`, `.mobile`, `.portal`) and use chino-api's default,
`chino`, only where it names none. The bundled realm has no `chino`, so a
bundled install names `chino-tv` and `chino-mobile` there
(`spec.identity.tvClientId` / `mobileClientId` name others). With an external
provider they are named only when those fields are set: give your provider a
public client with the device grant and PKCE for the TV apps, and one with the
authorization code, PKCE and the redirect `cloud.nalet.chino:/oauth/callback`
— exactly that string; a wildcard does not cover it — for the phone apps, both
with `offline_access` and an audience mapper for `identity.audience`; or one
`chino` client that does all of it.

`<origin>` is the public URL, plain `http` beside it where OpenShift Routes
serve the host (they allow it), and for `chino-web` also `https://<hosts.chino>`
in subdomains routing. Web origins are those origins. A new realm gets them
from the import. A realm that exists keeps what was imported when it was made,
so the operator runs the chart's realm Job, `zaentrum-realm-<suffix>`
(`templates/realm.yaml`, `files/realm-config.sh`), whenever what it sets
changes — and once a day besides, as a run's Job is kept a day — while Keycloak
is available, never beside a verification run. It sets exactly those lists
and the flows above with `kcadm` — public, which of the authorization code,
the implicit flow, the password grant and the device grant are on, and the
PKCE method, each as the realm import has it — nothing else of a client, and
leaves alone what is already so. A client the realm lacks, one imported before
the chart had it or deleted since, it makes from the realm import's own
representation. `RealmConfigured` reports its summary, or why it failed; a
failed run is tried again after ten minutes.

The same run retires a password every bundled install once shared: a realm
imported without Secret `zaentrum-demo-user` gave its `demo` user the
password `${DEMO_USER_PASSWORD}`, the placeholder itself. If `demo` still
signs in with it, it gets the Secret's password (the operator makes the Secret
unless secrets are external), or is disabled where there is none. A `demo`
user with a password of its own is left alone.

### People, and the rating cap

One account per person: an admin adds people on the portal's People page and
sends each an invite link, where they choose their own password (zaentrum-
portal). portal-api does it through one client of the bundled realm,
`zaentrum-people` — confidential, the client credentials grant and no other
way in — whose service account holds `view-users`, `query-users` and
`manage-users` of `realm-management`, and nothing else: never `realm-admin`,
`manage-realm` or `manage-clients`. Its secret is `client-secret` in Secret
`zaentrum-people`; the realm import carries none (Keycloak makes one nobody
knows) and the realm Job sets the Secret's. portal-api reaches Keycloak
in-cluster (`http://keycloak:80/auth`), never through the public host.

The realm Job keeps it so in a realm that exists: it makes the client as the
import makes it when the realm lacks it, sets how it signs in as the import
says, sets the Secret's secret, and gives its service account exactly those
three roles — a role granted by hand, `realm-admin` or a realm role, goes
again with the next run. Without the Secret it says so in `RealmConfigured`,
and the People page says it is not set up. `manage-users` may change any
user of the realm, the realm's own administrators too; portal-api refuses to
touch an account that holds a `realm-management` role (the first `admin`), so
an admin of the platform cannot make themselves one of Keycloak's.

A person's **rating cap** is the user attribute `max_rating`, an age from 0
to 21. The realm's user profile declares it so that only an admin sees or
changes it — Keycloak keeps no attribute its profile does not declare — and
the clients people watch through (`chino-web`, `chino-tv`, `chino-mobile`,
`zaentrum-web`) map it into the access token as the integer claim
`max_rating`. No attribute, no claim: no cap. The realm Job declares the
attribute, puts the mapper on each of those clients and makes anew one whose
config differs. It also turns off the required action `VERIFY_PROFILE`, which
would stop a person without an email or a last name at sign-in to ask for
them, and gives a realm without a password policy the import's,
`length(8) and notUsername and notEmail`.

**Deleting an account.** chino-api's `DELETE /api/v1/me` deletes the
signed-in person's data and asks portal-api to delete their account, with
`deletion-token` and the person's own bearer; an admin deleting someone on
the People page has portal-api ask chino-api for that person's data the same
way, with the admin's bearer. Both read the token from Secret
`zaentrum-people`, optionally.

With `secrets.external` (the demo) whoever makes the Secrets makes
`zaentrum-people` too, both keys random. With an external provider none of
this exists: the People page says people live in that provider.

### The admin console

Keycloak's admin console and admin API are not on the public host: the Routes
and the Ingress send only `/auth/realms` (login, account, device and OIDC
endpoints) and `/auth/resources` to Keycloak. Reach the console through a
port-forward, signed in as the bootstrap admin:

```sh
kubectl -n zaentrum port-forward svc/keycloak 8080:80
kubectl -n zaentrum get secret zaentrum-keycloak-admin -o jsonpath='{.data.username}' | base64 -d; echo
kubectl -n zaentrum get secret zaentrum-keycloak-admin -o jsonpath='{.data.password}' | base64 -d; echo
open http://localhost:8080/auth/admin/
```

In the master realm's console, the realm `zaentrum` — its users, its clients
— is one switch away; the realm's own console (`/auth/admin/zaentrum/console/`)
signs in on the public host and does not work through the port-forward. On
the all-in-one appliance, publish `127.0.0.1:8080:8080` and port-forward with
`--address 0.0.0.0` inside it (`deploy/allinone/README.md`).

The console and its sign-in live at `http://localhost:8080/auth` then:
`KC_HOSTNAME_ADMIN` points the console there, and the realm Job sets the master
realm's frontend URL to the same, so its sign-in pages stay on the
port-forward too. Use local port 8080. `spec.identity.exposeAdminConsole: true`
publishes `/auth` whole on the public host instead, as before, and the realm
Job unsets the master realm's frontend URL again.

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
go build ./... && go vet ./... && go test ./...
helm lint platform/chart && helm lint platform/chart -f platform/chart/values-demo.yaml
../scripts/check-neutrality.sh       # the public boundary; it checks its own patterns first
../deploy/allinone/build.sh render   # must leave deploy/allinone/manifests unchanged
```

`internal/templates` renders the chart per profile — self-host, the demo's,
a shared-services install — through the operator and as `helm template` and
`helm install` would (a fake cluster answering the chart's lookups);
`internal/controller` runs the reconciler's flows on controller-runtime's fake
client; `install_bundles_test.go` holds the shipped CRD copies and
ClusterRoles to the canonical ones.

The CRD is controller-gen v0.16.5 output (`controller-gen crd
paths=./api/... output:crd:dir=config/crd`, `controller-gen object
paths=./api/...`). A few descriptions of the committed copy are shortened by
hand, so a new field's block is spliced into it and copied to
`bundle/manifests`; `build.sh render` carries it into the appliance.
`deploy/operator-install.yaml` changes only when it is re-pinned
(`sinceThePin`).

## Layout

```
api/v1alpha1/            CRD types + deepcopy + scheme
platform/chart/          the platform's Helm chart, embedded (platform/embed.go)
internal/templates/      the chart's renderer (Helm's engine, client-side) + render tests
internal/controller/     the reconcilers: the platform's (server-side apply) and the addons'
internal/addon/          addon charts: fetch, render, guardrails
internal/digest/         image digest pinning
internal/updates/        release channels
config/crd/              generated CRDs
config/rbac/             ServiceAccount + ClusterRole/Binding
config/manager/          operator Deployment + namespace
config/samples/          the example Zaentrum, which the appliance boots
bundle/                  the OLM bundle
Dockerfile               multi-stage, distroless → ghcr.io/zaentrum/operator
```

## Install

Once, as a cluster-admin, from an install manifest — the CRDs, the cluster
RBAC and the controller — then a Zaentrum. A release's (`operator:vX.Y.Z`,
rendered by `scripts/render-release.sh` and attached to the GitHub release)
goes with the `stable` channel; main's pinned one (`operator:sha-<commit>`)
with `edge`:

```sh
kubectl apply -f https://github.com/zaentrum/zaentrum-operator/releases/latest/download/operator-install.yaml
#   or main's: https://raw.githubusercontent.com/zaentrum/zaentrum-operator/main/deploy/operator-install.yaml
kubectl create namespace zaentrum
kubectl apply -f config/samples/zaentrum_v1alpha1_zaentrum.yaml   # or a Zaentrum of your own
kubectl -n zaentrum get zaentrum                                  # PHASE Ready once it is up
```

From a checkout, `kubectl apply -k config` installs the same, its image tag
pinned in `config/kustomization.yaml`; on OpenShift or any OLM cluster, the
bundle in [`bundle/`](bundle).
