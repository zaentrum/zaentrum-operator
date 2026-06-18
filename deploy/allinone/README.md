# Stube all-in-one (`ghcr.io/zaentrum/stube`)

A whole Stube cluster in **one container** — a neutral media client + server for
a library you own and are entitled to stream. The image bundles a single-node
[k3s](https://k3s.io) (real Kubernetes) and the rendered `deploy/base`
manifests. k3s auto-applies those manifests on boot, so starting the container
installs Stube. This is the zero-clone option: nothing to check out, one
`docker run`.

## Run it

```bash
docker run -d --privileged --name stube -p 8080:80 ghcr.io/zaentrum/stube:latest
```

Then open <http://localhost:8080>. First boot pulls the application images
(see below) and runs the database migrations, so give it a minute.

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

Everything (Postgres, the Kafka log, the media library, the HLS cache) lives on
PersistentVolumeClaims backed by k3s's `local-path` StorageClass, i.e. inside
the container's writable layer. To keep your data across `docker rm`, mount a
host directory at k3s's storage path:

```bash
docker run -d --privileged --name stube -p 8080:80 \
  -v stube-data:/var/lib/rancher/k3s/storage \
  ghcr.io/zaentrum/stube:latest
```

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
docker exec -it stube k3s kubectl -n stube get pods
docker exec -it stube k3s kubectl -n stube logs deploy/katalog-manager-api
```

## Where the images come from

Application images are pulled from **`ghcr.io/zaentrum/stube/<service>`** on first
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
imgs="ghcr.io/zaentrum/stube/chino-web:latest \
ghcr.io/zaentrum/stube/chino-api:latest \
ghcr.io/zaentrum/stube/chino-stream:latest \
ghcr.io/zaentrum/stube/katalog-api:latest \
ghcr.io/zaentrum/stube/katalog-manager-api:latest \
ghcr.io/zaentrum/stube/admin:latest \
postgres:16-alpine valkey/valkey:8-alpine apache/kafka:3.8.0"
for i in $imgs; do docker pull "$i"; done
docker save $imgs -o stube-airgap.tar

# 2. Bake it into a custom all-in-one image:
mkdir -p deploy/allinone/airgap && mv stube-airgap.tar deploy/allinone/airgap/
#    then add to the Dockerfile, before the ENTRYPOINT line:
#      COPY airgap/stube-airgap.tar /var/lib/rancher/k3s/agent/images/
./deploy/allinone/build.sh
```

The resulting image is large (it carries every layer) but starts with zero
registry traffic.

## Build

```bash
./deploy/allinone/build.sh            # render deploy/base -> manifests/, then docker build
IMAGE=ghcr.io/zaentrum/stube:v1 ./deploy/allinone/build.sh
./deploy/allinone/build.sh render     # just re-render manifests/stube.yaml
```

`build.sh` renders `deploy/base` with kustomize into `manifests/stube.yaml`,
which the Dockerfile copies into the k3s auto-apply directory. Re-run it
whenever `deploy/base` changes so the bundled manifest stays in sync.

## Stop / remove

```bash
docker rm -f stube
```
