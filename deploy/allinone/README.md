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

Keycloak's own admin console — where further accounts are made — is not on the
published port. It answers through a port-forward on `localhost:8080`, so
publish that port when you start the container (`-p 127.0.0.1:8080:8080` beside
`-p 80:80`), then:

```bash
docker exec -d zaentrum kubectl -n zaentrum port-forward --address 0.0.0.0 svc/keycloak 8080:80
docker exec zaentrum kubectl -n zaentrum get secret zaentrum-keycloak-admin \
  -o jsonpath='{.data.password}' | base64 -d; echo
open http://localhost:8080/auth/admin/    # as admin, with that password
```

Or publish it on the appliance's own port with `spec.identity.exposeAdminConsole:
true` ([the operator's README](../../operator/README.md#the-admin-console)).

## Persistence

Postgres (users, watch state, the catalog), the media library and the HLS cache
live on PersistentVolumeClaims backed by k3s's `local-path` StorageClass, so
they outlive a pod's restart or reschedule and a `docker restart`. They are
still inside the container, though: `docker rm` takes them with it. To keep
them across a re-created container, keep the whole k3s state on a volume, and
give the container a fixed host name — k3s names its node after it, and a
`local-path` volume belongs to the node it was made on:

```bash
docker run -d --privileged --name zaentrum -h zaentrum -p 80:80 \
  -v zaentrum:/var/lib/rancher/k3s \
  ghcr.io/zaentrum/appliance:latest
```

The volume then also keeps the operator install the first container brought.
To take a newer one from a newer image, copy it over before you re-create:

```bash
docker run --rm -v zaentrum:/state --entrypoint sh ghcr.io/zaentrum/appliance:latest \
  -c 'cp /var/lib/rancher/k3s/server/manifests/10-operator.yaml /state/server/manifests/'
```

An appliance made before Postgres moved onto a claim keeps it on an emptyDir
until it is copied over: see
[the operator's README](../../operator/README.md#the-bundled-postgres-keeps-its-data-specstoragepostgres).

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

Application images are pulled from **`ghcr.io/zaentrum/<service>`** on first
boot (`chino-web`, `chino-api`, `chino-stream`, `katalog-api`,
`katalog-manager-api`, `admin`), plus the upstream `postgres`, `valkey`, and
`apache/kafka` images. The box needs outbound network for the first start;
after that the images are cached in the container's containerd store.

## Offline / airgap

To run with **no registry access**, bake the image tarball into the k3s airgap
directory. k3s imports anything in `/var/lib/rancher/k3s/agent/images/` before
it tries to pull:

```bash
# 1. Collect the images this release uses (on a connected machine):
imgs="ghcr.io/zaentrum/chino-web:latest \
ghcr.io/zaentrum/chino-api:latest \
ghcr.io/zaentrum/chino-stream:latest \
ghcr.io/zaentrum/katalog-api:latest \
ghcr.io/zaentrum/katalog-manager:latest \
ghcr.io/zaentrum/admin:latest \
postgres:16-alpine valkey/valkey:8-alpine apache/kafka:3.8.0"
for i in $imgs; do docker pull "$i"; done
docker save $imgs -o zaentrum-airgap.tar

# 2. Bake it into a custom all-in-one image:
mkdir -p deploy/allinone/airgap && mv zaentrum-airgap.tar deploy/allinone/airgap/
#    then add to the Dockerfile, before the ENTRYPOINT line:
#      COPY airgap/zaentrum-airgap.tar /var/lib/rancher/k3s/agent/images/
./deploy/allinone/build.sh
```

The resulting image is large (it carries every layer) but starts with zero
registry traffic.

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
