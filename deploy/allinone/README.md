# Zaentrum all-in-one (`ghcr.io/zaentrum/appliance`)

The whole Zaentrum platform in **one container** — a neutral media client +
server for a library you own and are entitled to stream. The image bundles a
single-node [k3s](https://k3s.io) (real Kubernetes), the operator's install and
a `Zaentrum` resource. k3s applies them on boot, and the operator brings up the
platform from that resource as it would on any cluster. This is the zero-clone
option: nothing to check out, one `docker run`.

## Run it

```bash
docker run -d --privileged --restart unless-stopped --name zaentrum -p 80:80 \
  ghcr.io/zaentrum/appliance:latest
```

Then open <http://zaentrum.localhost> — modern browsers resolve `*.localhost` to
`127.0.0.1`, with no `/etc/hosts` edit. First boot pulls the application images
(see below) and runs the database migrations, so give it a few minutes;
`--restart unless-stopped` brings the container back after a reboot or a Docker
restart.

**Port 80, and that one name.** The `Zaentrum` the appliance boots
([`manifests/20-zaentrum.yaml`](manifests/20-zaentrum.yaml)) sets
`hostname: zaentrum.localhost`, and the platform binds both its ingress and its
sign-in to it: the Ingress answers that host only, and Keycloak issues its
tokens for, and sends every sign-in back to, `http://zaentrum.localhost` on
port 80. Publish the container on another host port (`-p 8080:80`) or open it
by another name (`http://localhost`, the machine's IP) and the pages answer
404, or the sign-in redirects to a port nothing listens on.

**linux/amd64 only.** No arm64 image is published yet.

**This machine only.** The appliance serves plain http, and sign-in works over
plain http on `localhost` names alone: Keycloak marks its login cookies
`Secure`, which a browser keeps over http only for `localhost`. A phone, a TV or
another computer cannot sign in to it — and the Android phone and TV apps
refuse plain http outright. For other devices, run the operator on a cluster
under a real hostname with https
([self-hosting](https://github.com/zaentrum/zaentrum/blob/main/docs/self-hosting.md#b-self-host-with-the-operator)).

### Why `--privileged`?

The container runs **k3s**, which needs to mount filesystems, manage cgroups,
and run an embedded container runtime (containerd) for the application pods.
That requires privileges a normal container does not get. `--privileged` is the
simple, reliable way to grant them. (Hardened setups can instead pass the
narrower set of capabilities + mounts k3s documents, but `--privileged` is the
supported default here.)

## First run

There is **no setup wizard**. The appliance comes up configured — for
`http://zaentrum.localhost`, with the bundled Keycloak (realm `zaentrum`) and an
empty library — and three steps make it yours
([self-hosting → first run](https://github.com/zaentrum/zaentrum/blob/main/docs/self-hosting.md#first-run)):

1. **Sign in.** <http://zaentrum.localhost> is the portal, whose launchpad opens
   the video app and, for an admin, the catalog consoles. Sign in as `admin`
   with the **first admin password**, which the operator generated once for this
   install into the Secret `zaentrum-keycloak-admin` — no two appliances share
   one. Keycloak then has you choose a password of your own:

   ```bash
   docker exec zaentrum kubectl -n zaentrum get secret zaentrum-keycloak-admin \
     -o jsonpath='{.data.realm-admin-password}' | base64 -d; echo
   ```

2. **Add a TMDB key — before the first scan.** Titles, posters and plots come
   from TMDB, and the images carry no key of their own. In **Catalog
   Management** on the launchpad (`/katalog-manage/`), open **settings** and
   enter a TMDB v4 read access token as **TMDB api key**; it applies from the
   next lookup, with no restart.
3. **Fill the library, then scan.** The catalog reads the `media/` folder of the
   platform's `media` volume, which on the appliance is a directory under k3s's
   storage path inside the container. Once the platform is up, copy your files
   there, then press **trigger scan** in Catalog Management:

   ```bash
   lib=$(docker exec zaentrum sh -c 'echo /var/lib/rancher/k3s/storage/pvc-*_zaentrum_media')/media
   docker exec zaentrum mkdir -p "$lib"
   docker cp ./my-library/. zaentrum:"$lib/"
   ```

### Keycloak's admin console

Further accounts are made in Keycloak's admin console, which is not on the
published port: the Ingress sends only `/auth/realms` and `/auth/resources` to
Keycloak. The console answers through a port-forward on `localhost:8080` — its
links and its sign-in point there — so the container has to publish that port.
Docker publishes a port only when it creates a container, and a re-created
appliance starts an empty platform ([persistence](#persistence)): if you will
want the console, start the container with `-p 127.0.0.1:8080:8080` beside
`-p 80:80`. Then:

```bash
docker exec -d zaentrum kubectl -n zaentrum port-forward --address 0.0.0.0 svc/keycloak 8080:80
docker exec zaentrum kubectl -n zaentrum get secret zaentrum-keycloak-admin \
  -o jsonpath='{.data.password}' | base64 -d; echo
open http://localhost:8080/auth/admin/    # as admin, with that password
```

That is the master realm's console, signed in as its bootstrap admin; the realm
`zaentrum`, with its users, is one switch away.

An appliance started without that port can publish the console on its own port
instead, with `spec.identity.exposeAdminConsole: true`
([the operator's README](../../operator/README.md#the-admin-console)):

```bash
docker exec zaentrum kubectl -n zaentrum patch zaentrum zaentrum --type merge \
  -p '{"spec":{"identity":{"exposeAdminConsole":true}}}'
```

Once Keycloak has restarted, the realm's own console is
<http://zaentrum.localhost/auth/admin/zaentrum/console/>, for the realm's
`admin`.

## Persistence

The platform keeps its data on two claims, which k3s's `local-path`
StorageClass makes directories under `/var/lib/rancher/k3s/storage` — inside
the Docker volume the image declares for `/var/lib/rancher/k3s`:

- `media` — the library, and with the pipeline on, its packaged streams;
- `postgres-data` — the bundled Postgres: users and their watch state, the
  catalog, Keycloak's accounts and the portal's settings. A new install starts
  its Postgres on this claim (`spec.storage.postgres`).

Kafka's log and the HLS cache are `emptyDir`s, which last as long as their
pods.

- **A restart keeps the platform.** `docker stop` / `docker start`, a Docker
  restart, or a reboot with `--restart unless-stopped` bring back the same
  cluster with its data, and so do pods restarted or rescheduled inside it.
- **Replacing the container starts a new, empty platform.** `docker rm` and a
  new `docker run` start a new cluster in a new volume, whose claims get new
  directories — `local-path` names each one after its claim's UID — so even a
  volume mounted at k3s's storage path keeps the old files without attaching
  them. The old volume stays behind, unattached.

To be able to re-create the container, keep the whole k3s state on a named
volume from the first `docker run`, and give the container a fixed host name —
k3s names its node after it, and a `local-path` volume belongs to the node it
was made on:

```bash
docker run -d --privileged --restart unless-stopped --name zaentrum -h zaentrum -p 80:80 \
  -v zaentrum:/var/lib/rancher/k3s \
  ghcr.io/zaentrum/appliance:latest
```

A container re-created on that volume with the same host name comes back as
the same cluster, its database and its generated Secrets included. The volume
then also keeps the operator install the first container brought. To take a
newer one from a newer image, copy it over before you re-create:

```bash
docker run --rm -v zaentrum:/state --entrypoint sh ghcr.io/zaentrum/appliance:latest \
  -c 'cp /var/lib/rancher/k3s/server/manifests/10-operator.yaml /state/server/manifests/'
```

An appliance made before Postgres moved onto a claim keeps it on an
`emptyDir` — which anything that recreates its pod empties — until a copy
moves it
([the operator's README](../../operator/README.md#the-bundled-postgres-keeps-its-data-specstoragepostgres)):

```bash
docker exec zaentrum kubectl -n zaentrum patch zaentrum zaentrum --type merge \
  -p '{"spec":{"storage":{"postgres":{"migrate":true}}}}'
docker exec zaentrum kubectl -n zaentrum get zaentrum zaentrum \
  -o jsonpath='{.status.conditions[?(@.type=="DatabasePersistent")].message}'; echo
```

## What's inside

k3s applies the manifests in its auto-apply directory
(`/var/lib/rancher/k3s/server/manifests`) in filename order. The image carries
three:

- `00-namespace.yaml` — the namespace `zaentrum`;
- `10-operator.yaml` — the operator's install from `operator/config`: its CRDs,
  its RBAC and its controller (`ghcr.io/zaentrum/operator:latest`), which
  reports the install as `appliance` in `status.controller.source`;
- `20-zaentrum.yaml` — the `Zaentrum`: bundled identity, a 50Gi library, Kafka
  on, the media pipeline off, manual updates.

The entrypoint adds `coredns-custom.yaml`, a CoreDNS entry that sends
`zaentrum.localhost` to the ingress inside the cluster too, so the services
validate tokens against the issuer the browser signs in at.

From that `Zaentrum` the operator brings up:

- the portal (`zaentrum-portal`, `portal-api`) at `/` and `/portal`, whose
  launchpad opens the apps;
- `chino-web`, the video app, at `/chino`, and `chino-api` (the product BFF) at
  `/api`;
- the Catalog and Catalog Management consoles at `/katalog` and
  `/katalog-manage`, and `katalog-manager-api` (the neutral management/write
  API) at `/api/manage`;
- `katalog-api` (the neutral catalog read API) and `chino-stream` (the
  HLS/CMAF origin), inside the cluster;
- Keycloak (realm `zaentrum`) at `/auth/realms` and `/auth/resources`,
  Postgres, Valkey, and a single-node KRaft Kafka broker for the internal event
  stream.

The media pipeline (analyzer, packager, transcoder, katalog-ingest) does not
run: the appliance's `Zaentrum` leaves `features.pipeline` off.

Inspect it like any cluster — the k3s image ships `kubectl`:

```bash
docker exec zaentrum kubectl -n zaentrum get zaentrum      # PHASE Ready once it is up
docker exec zaentrum kubectl -n zaentrum get pods
docker exec zaentrum kubectl -n zaentrum logs deploy/katalog-manager-api
```

## Where the images come from

Pods pull their images as they start: the operator's
`ghcr.io/zaentrum/operator`; the platform's `ghcr.io/zaentrum/<service>`
images — `zaentrum-portal`, `portal-api`, `chino-web`, `chino-api`,
`chino-stream`, `katalog-api`, `katalog-manager`, `katalog-manager-ui`, and
`zae` for the platform's check of itself; and the upstream `postgres`,
`valkey/valkey`, `apache/kafka` and `quay.io/keycloak/keycloak` images.

The `ghcr.io/zaentrum` images run on the moving tag `latest`, so the box needs
`ghcr.io` for longer than the first start. The operator re-resolves each of
them to its current digest as it reconciles, and rolls a component whose
digest moved; the platform's services are pulled with
`imagePullPolicy: Always` whenever their pods start; and the operator's own pod
pulls `:latest` again whenever it restarts. The appliance follows every push to
`latest`.

There is no offline or air-gapped mode today: an image tarball in k3s's airgap
directory is not enough, as `Always` asks the registry before a container
starts.

## Build

```bash
./deploy/allinone/build.sh            # write manifests/, then docker build :latest
IMAGE=ghcr.io/zaentrum/appliance:v1 ./deploy/allinone/build.sh
./deploy/allinone/build.sh render     # just re-write manifests/
```

`build.sh` writes the three manifests the Dockerfile copies into k3s's
auto-apply directory: `00-namespace.yaml`; `10-operator.yaml`, the CRDs, the
RBAC and the manager (its namespace and Deployment) of `operator/config`, the
manager stamped `ZAENTRUM_INSTALL_SOURCE=appliance`; and `20-zaentrum.yaml`, a
copy of `operator/config/samples/zaentrum_v1alpha1_zaentrum.yaml`. Nothing in
it comes from `deploy/base`.

CI ([`all-in-one.yml`](../../.github/workflows/all-in-one.yml)) builds and
pushes `ghcr.io/zaentrum/appliance:latest` and `:sha-<commit>` from `main`.
The runs that follow the component images' build then boot it as a user would
— `docker run --privileged -p 80:80` — and wait for the platform to be Ready
and to pass its own verification.

## Stop / remove

```bash
docker rm -f zaentrum
```

That leaves the container's volume — the cluster, the library, the database —
behind, unattached; `docker rm -fv zaentrum` removes it with the container. A
named volume (`-v zaentrum:…`) stays until `docker volume rm zaentrum`.
