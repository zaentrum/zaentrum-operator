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

That is the newest build of `main`. A release's appliance is the same command
on the release's tag, `ghcr.io/zaentrum/appliance:vX.Y.Z` (or `:X.Y`, its
newest patch): the operator and every image of the platform are that release's
([releases](https://github.com/zaentrum/zaentrum/blob/main/docs/releases.md)).

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
refuse plain http outright. For other devices, give the appliance a name and a
certificate ([below](#phones-and-tvs-a-name-and-a-certificate)), or run the
operator on a cluster under a real hostname with https
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

## Phones and TVs: a name and a certificate

What another device needs is the platform under a name that resolves to this
machine, served over https with a certificate it trusts — the Android apps
trust public certificate authorities only — on port 443. Five steps.

1. **A name and a certificate.** A DNS name you control, `media.example.org`
   here, that resolves to this machine on your network and on the machine
   itself. A certificate for it from a public authority, such as Let's Encrypt
   through a DNS-01 challenge, which needs nothing of the box to be reachable
   from the internet: the chain (`fullchain.pem`) and its key (`privkey.pem`).
2. **Port 443 and the name inside the cluster.** Docker publishes a port only
   when it creates a container, and the entrypoint writes the in-cluster DNS
   entry for the platform's name (the CoreDNS rewrite that lets the services
   validate tokens against the https issuer) from `STUBE_ISSUER_HOST` when the
   container starts. So the container is created anew — which keeps the
   platform only when it runs on a named volume with a fixed host name
   ([persistence](#persistence)). An appliance that does not starts empty
   then: copy the library, the backups and the platform's Secrets off it
   first, and bring them into the new one
   ([restoring](../../operator/README.md#backups-of-the-bundled-postgres-specbackup)).
   On such a volume:

   ```bash
   docker rm -f zaentrum
   docker run -d --privileged --restart unless-stopped --name zaentrum -h zaentrum \
     -p 80:80 -p 443:443 -e STUBE_ISSUER_HOST=media.example.org \
     -v zaentrum:/var/lib/rancher/k3s ghcr.io/zaentrum/appliance:latest
   ```

   k3s's Traefik takes 443 as it takes 80.
3. **The certificate, as a Secret** in the platform's namespace:

   ```bash
   docker cp fullchain.pem zaentrum:/tmp/tls.crt
   docker cp privkey.pem zaentrum:/tmp/tls.key
   docker exec zaentrum sh -c 'kubectl -n zaentrum create secret tls zaentrum-tls \
     --cert=/tmp/tls.crt --key=/tmp/tls.key && rm /tmp/tls.crt /tmp/tls.key'
   ```
4. **The platform on that name, with it**
   ([`spec.tls`](../../operator/README.md#the-platforms-certificate-spectls)):

   ```bash
   docker exec zaentrum kubectl -n zaentrum patch zaentrum zaentrum --type merge \
     -p '{"spec":{"hostname":"media.example.org","tls":{"secretName":"zaentrum-tls"}}}'
   docker exec zaentrum kubectl -n zaentrum get zaentrum zaentrum \
     -o jsonpath='{.status.conditions[?(@.type=="TLS")].message}'; echo
   ```

   The Ingress answers `media.example.org` with the certificate; the issuer,
   Keycloak's hostname and the sign-in redirects become
   `https://media.example.org` (the realm Job writes the redirects into the
   realm), and the TLS condition reads "media.example.org served over https
   with the certificate in Secret zaentrum-tls, valid until …". It no longer
   answers `zaentrum.localhost`, and everyone signs in again, once.
5. **The apps.** In the phone or TV app, add the server
   `https://media.example.org`; the apps learn their clients from it.

Renew by replacing the Secret the same way (`kubectl create secret tls …
--dry-run=client -o yaml | kubectl apply -f -`); the ingress serves the new
certificate at once. k3s applies `20-zaentrum.yaml` again when that file
changes — as when you copy a newer image's manifests into the volume — and so
puts its `hostname`, `zaentrum.localhost`, back: patch again after (the TLS
condition says `WrongHost` meanwhile).

## Persistence

The platform keeps its data on three claims, which k3s's `local-path`
StorageClass makes directories under `/var/lib/rancher/k3s/storage` — inside
the Docker volume the image declares for `/var/lib/rancher/k3s`:

- `media` — the library, and with the pipeline on, its packaged streams;
- `postgres-data` — the bundled Postgres: users and their watch state, the
  catalog, Keycloak's accounts and the portal's settings. A new install starts
  its Postgres on this claim (`spec.storage.postgres`);
- `backups` — a dump of every database each night, the newest seven kept
  ([the operator's README](../../operator/README.md#backups-of-the-bundled-postgres-specbackup)).
  They are in the same Docker volume as the database, so they undo a mistake
  inside the platform, not the loss of the volume: copy them off the box too,

  ```bash
  dumps=$(docker exec zaentrum sh -c 'echo /var/lib/rancher/k3s/storage/pvc-*_zaentrum_backups')
  docker cp zaentrum:"$dumps"/. ./zaentrum-backups/
  ```

  and the platform's generated Secrets with them, as that README says.

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
  its RBAC and its controller (`ghcr.io/zaentrum/operator:latest`; a release's
  appliance, `operator:vX.Y.Z`), which reports the install as `appliance` in
  `status.controller.source`;
- `20-zaentrum.yaml` — the `Zaentrum`: bundled identity, a 50Gi library, Kafka
  on, the media pipeline on the CPU, manual updates, on the `edge` channel —
  the latest images, as the operator is `:latest`. A release's appliance pins
  it to `spec.version: vX.Y.Z` instead.

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

- the media pipeline — the analyzer, the transcoder, one packager and
  katalog-ingest — which prepares and packages every title for every client,
  on the CPU ([below](#the-media-pipeline-on-the-cpu)).

Inspect it like any cluster — the k3s image ships `kubectl`:

```bash
docker exec zaentrum kubectl -n zaentrum get zaentrum      # PHASE Ready once it is up
docker exec zaentrum kubectl -n zaentrum get pods
docker exec zaentrum kubectl -n zaentrum logs deploy/katalog-manager-api
```

### The media pipeline, on the CPU

The appliance's `Zaentrum` turns the pipeline on with `pipeline.encoder: cpu`
([the operator's README](../../operator/README.md#the-media-pipeline-specpipeline)):
no GPU is asked for, and the transcoder encodes with libx264/libx265. What it
costs the box:

- **Time.** A title whose video the clients play as it is — HEVC Main or
  Main 10 (4:2:0, up to 10-bit), or H.264 a browser decodes (8-bit 4:2:0, up
  to High), as most files are — passes through: a remux, minutes. Anything
  else (MPEG-2, VC-1, AV1, 10-bit H.264) is an x265 encode, hours a title on
  a few cores. One title is prepared at a time. A packaged title starts at
  once and costs next to nothing to play; until a title is packaged,
  chino-stream transcodes it on the fly while it plays, at the cost of the
  CPU then.
- **Disk.** Every title is packaged as HLS beside the library on the `media`
  volume, which then holds about twice the library.
- **First boot.** About 3.9 GiB more of images to pull, compressed — the
  analyzer 2.2 GiB, the transcoder 1.4 GiB, the packager 0.2 GiB — and about
  twice that on disk once unpacked.
- **CPU and memory.** The pipeline's pods ask for 1.55 CPUs and 3.1 GiB of
  the platform's 2.3 CPUs and 5.1 GiB, so the box wants four cores and 8 GiB;
  an encode takes up to four cores when they are free, and what it asked for
  when they are not.

Turn it off — titles are then served as chino-stream finds them, transcoded on
the fly for a client that needs it — and, as the operator removes nothing it no
longer renders, delete the workers it ran:

```bash
docker exec zaentrum kubectl -n zaentrum patch zaentrum zaentrum --type merge \
  -p '{"spec":{"features":{"pipeline":false}}}'
docker exec zaentrum kubectl -n zaentrum delete deploy,svc analyzer transcoder packager katalog-ingest
```

## Where the images come from

Pods pull their images as they start: the operator's
`ghcr.io/zaentrum/operator`; the platform's `ghcr.io/zaentrum/<service>`
images — `zaentrum-portal`, `portal-api`, `chino-web`, `chino-api`,
`chino-stream`, `katalog-api`, `katalog-manager`, `katalog-manager-ui`, the
pipeline's `analyzer`, `transcoder`, `packager` and `katalog-ingest`, and
`zae` for the platform's check of itself; and the upstream `postgres`,
`valkey/valkey`, `apache/kafka` and `quay.io/keycloak/keycloak` images.

In the appliance built from `main`, the `ghcr.io/zaentrum` images run on the
moving tag `latest`, so the box needs `ghcr.io` for longer than the first
start. The operator re-resolves each of
them to its current digest as it reconciles, and rolls a component whose
digest moved; the platform's services are pulled with
`imagePullPolicy: Always` whenever their pods start; and the operator's own pod
pulls `:latest` again whenever it restarts. The appliance follows every push to
`latest`.

A release's appliance runs the release's tags instead — `operator:vX.Y.Z`, and
every platform image at `vX.Y.Z` — so nothing it runs moves on its own; a newer
release is a newer appliance image.

There is no offline or air-gapped mode today: an image tarball in k3s's airgap
directory is not enough, as `Always` asks the registry before a container
starts.

## Build

```bash
./deploy/allinone/build.sh            # write manifests/, then docker build :latest
IMAGE=ghcr.io/zaentrum/appliance:v1 ./deploy/allinone/build.sh
./deploy/allinone/build.sh render     # just re-write manifests/
VERSION=v0.4.0 ./deploy/allinone/build.sh render   # a release's manifests (never committed)
```

`build.sh` writes the three manifests the Dockerfile copies into k3s's
auto-apply directory: `00-namespace.yaml`; `10-operator.yaml`, the CRDs, the
RBAC and the manager (its namespace and Deployment) of `operator/config`, the
manager stamped `ZAENTRUM_INSTALL_SOURCE=appliance`; and `20-zaentrum.yaml`,
`operator/config/samples/zaentrum_v1alpha1_zaentrum.yaml` on the `edge`
channel. With `VERSION=vX.Y.Z` the manager runs `operator:vX.Y.Z` and the
Zaentrum is pinned to `spec.version: vX.Y.Z` — what a release's build bakes;
the committed manifests are always main's. Nothing in it comes from
`deploy/base`.

CI ([`all-in-one.yml`](../../.github/workflows/all-in-one.yml)) builds and
pushes `ghcr.io/zaentrum/appliance:latest` and `:sha-<commit>` from `main`,
and `:vX.Y.Z` and `:X.Y` from a release tag (no `:X.Y` for a pre-release).
The runs that follow the component images' build, and every release's, then
boot it as a user would — `docker run --privileged -p 80:80` — and wait for
the platform to be Ready and to pass its own verification; a release's boot
first waits until every image of the release is published.

## Stop / remove

```bash
docker rm -f zaentrum
```

That leaves the container's volume — the cluster, the library, the database —
behind, unattached; `docker rm -fv zaentrum` removes it with the container. A
named volume (`-v zaentrum:…`) stays until `docker volume rm zaentrum`.
