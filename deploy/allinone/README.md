# Zaentrum all-in-one (`ghcr.io/zaentrum/appliance`)

A whole Zaentrum cluster in **one container** — a neutral media client + server for
a library you own and are entitled to stream. The image bundles a single-node
[k3s](https://k3s.io) (real Kubernetes) and the rendered `deploy/base`
manifests. k3s auto-applies those manifests on boot, so starting the container
installs Zaentrum. This is the zero-clone option: nothing to check out, one
`docker run`.

## Run it

```bash
docker run -d --privileged --name zaentrum -p 8080:80 ghcr.io/zaentrum/appliance:latest
```

Then open <http://localhost:8080>. First boot pulls the application images
(see below) and runs the database migrations, so give it a minute.

### Signing in the first time

The first administrator is `admin`, with a one-time password generated for this
container — no two appliances share one. Keycloak asks for a new password at
that first sign-in:

```bash
docker exec zaentrum kubectl -n zaentrum get secret zaentrum-keycloak-admin \
  -o jsonpath='{.data.realm-admin-password}' | base64 -d; echo
```

Keycloak's own admin console is not on the published port; see
[the operator's README](../../operator/README.md#the-admin-console) for the
port-forward that reaches it.

### Why `--privileged`?

The container runs **k3s**, which needs to mount filesystems, manage cgroups,
and run an embedded container runtime (containerd) for the application pods.
That requires privileges a normal container does not get. `--privileged` is the
simple, reliable way to grant them. (Hardened setups can instead pass the
narrower set of capabilities + mounts k3s documents, but `--privileged` is the
supported default here.)

### Ports

- `-p 8080:80` maps the bundled **Traefik** ingress (container `:80`) to
  `localhost:8080`. Use any host port you like (`-p 80:80` for the default web
  port). The ingress answers on **any** host name or IP, so the box's LAN
  address works too.

### Persistence

Postgres (users, watch state, the catalog), the media library and the HLS cache
live on PersistentVolumeClaims backed by k3s's `local-path` StorageClass, so
they outlive a pod's restart or reschedule and a `docker restart`. They are
still inside the container, though: `docker rm` takes them with it. To keep
them across a re-created container, keep the whole k3s state on a volume, and
give the container a fixed host name — k3s names its node after it, and a
`local-path` volume belongs to the node it was made on:

```bash
docker run -d --privileged --name zaentrum -h zaentrum -p 8080:80 \
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

Put your own library files where the stream service expects them (the `media`
PVC under `local-path`), or point `chino-stream` at a host path via an overlay
if you run the cluster form instead.

## What's inside

The container starts k3s, which applies the rendered `deploy/base` bundle:

- `chino-web` (the main app) at `/`,
- the management UI (`admin`) at `/manage` (first run opens the setup wizard at
  `/manage/setup`),
- `chino-api` (product BFF) at `/api`,
- `katalog-manager-api` (neutral management/write API) at `/api/manage`,
- `katalog-api` (neutral catalog read API), `chino-stream` (HLS/CMAF origin),
- Postgres, Valkey, and a single-node KRaft Kafka broker for the internal
  event stream.

Inspect it like any cluster:

```bash
docker exec -it zaentrum kubectl -n zaentrum get pods
docker exec -it zaentrum kubectl -n zaentrum logs deploy/katalog-manager-api
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
./deploy/allinone/build.sh            # render deploy/base -> manifests/, then docker build
IMAGE=ghcr.io/zaentrum/appliance:v1 ./deploy/allinone/build.sh
./deploy/allinone/build.sh render     # just re-render manifests/zaentrum.yaml
```

`build.sh` renders `deploy/base` with kustomize into `manifests/zaentrum.yaml`,
which the Dockerfile copies into the k3s auto-apply directory. Re-run it
whenever `deploy/base` changes so the bundled manifest stays in sync.

## Stop / remove

```bash
docker rm -f zaentrum
```
