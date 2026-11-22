# zaentrum-operator

The platform's meta-repository. `operator/` is the operator, a controller-runtime manager
that reconciles the whole platform from one `Zaentrum` resource: it renders the Helm chart
it embeds, `operator/platform/chart/`, and applies the result with server-side apply. A
second reconciler installs addon charts from `ZaentrumAddon` resources.
`operator/bundle/` is the OLM bundle, `deploy/operator-install.yaml` the pinned cluster
install, and `deploy/allinone/` the appliance: k3s, the operator and a `Zaentrum` in one
container. A cluster-admin installs the operator; portal-api, for the portal's operator
console and `zae platform`, reads and patches the `Zaentrum` and writes `ZaentrumAddon`s.
The chart runs the `ghcr.io/zaentrum/<name>` images the other repositories publish.
`deploy/{base,compose,k3s,overlays}`, `apps/admin/` and `platform/keycloak/` are older
profiles and their images, and are not supported.
How a change is proposed and what a pull request has to pass is in the
[guide to contributing](https://github.com/zaentrum/.github/blob/main/CONTRIBUTING.md);
what may be built is in [building from the specification](https://github.com/zaentrum/zaentrum/blob/main/AGENTS.md).

## Build

```sh
(cd operator && go build ./...)
```

Go 1.24 (`operator/go.mod`). The chart is compiled into the binary
(`operator/platform/embed.go`), so a change to the chart is a change to the operator.
The images and a release's install, as the workflows build them (Docker; `kubectl` for
the kustomize build):

```sh
docker build -t zaentrum-operator operator                          # build-images.yml
docker build -f operator/bundle/bundle.Dockerfile operator/bundle    # operator-bundle.yml
./deploy/allinone/build.sh          # all-in-one.yml: writes deploy/allinone/manifests/, builds
./scripts/render-release.sh vX.Y.Z  # release.yml: dist/operator-install.yaml
```

`build.sh render` writes the appliance's manifests without building; with
`VERSION=vX.Y.Z` it writes a release's, which are never committed.

## Test

As `ci.yml` runs them on a pull request, in its `operator` job:

```sh
(cd operator && go vet ./... && go test ./...)
helm lint operator/platform/chart
./deploy/allinone/build.sh render && git diff --exit-code -- deploy/allinone/manifests
```

The Go tests need Go alone: the reconcilers run on controller-runtime's fake client, the
chart is rendered per profile through the operator and as `helm template` and
`helm install` render it, and registries, chart servers and release channels are local
test servers. On the Go 1.24.0 that `operator/go.mod` names,
`TestVPNSchemaChecksWhatTheOperatorChecks` fails; `ci.yml` and the image take the newest
1.24. `helm lint` needs Helm on the PATH. The render fails the job when the appliance's
committed manifests (`deploy/allinone/manifests`) are not what a render of the tree makes. On
a pull request `build-images.yml` also builds the operator's image when `operator/` or `deploy/` changed, and
`operator-bundle.yml` the bundle when `operator/bundle/` or `operator/config/` changed;
neither pushes.

## Run it

The appliance runs the whole platform on one machine, as the boot job of
`all-in-one.yml` runs it (Docker, linux/amd64):

```sh
docker run -d --privileged --restart unless-stopped --name zaentrum -p 80:80 \
  ghcr.io/zaentrum/appliance:latest
docker exec zaentrum kubectl -n zaentrum get zaentrum   # PHASE Ready once it is up
```

Then open `http://zaentrum.localhost` (that name, on port 80) and sign in as `admin`
([first run](deploy/allinone/README.md#first-run)). It pulls `operator:latest` and the
platform's images from `ghcr.io`: it runs what `main` has published, not a checkout. On a
cluster, a cluster-admin runs `kubectl apply -k operator/config` and then applies a
`Zaentrum` in the namespace `zaentrum`, such as
`operator/config/samples/zaentrum_v1alpha1_zaentrum.yaml`
([self-hosting](https://github.com/zaentrum/zaentrum/blob/main/docs/self-hosting.md)).
The manager reads `RELEASES_URL` (by default the front door's `releases.json`) and
`PIN_DIGESTS` (`false` keeps bare tags), and serves its probes on `:8081` and its metrics
on `:8080`. The reference demo is a `Zaentrum` this operator reconciles, with the profile
in `operator/platform/chart/values-demo.yaml`
([how it is deployed](https://github.com/zaentrum/zaentrum/blob/main/docs/reference-demo.md)).

## What the tests leave unchecked

- A real API server: a create or replace stands in for server-side apply, the CRD's CEL
  rule is compared as text, and the ClusterRoles are read from the files.
- A cluster, on a pull request. The appliance's boot in `all-in-one.yml` (the platform
  Ready, and its own verification passed) runs on `main` and on a release tag; `ci.yml`'s
  `manifests` job applies only `deploy/base`, an unsupported profile, on kind.
- The CRD against the Go types: no test regenerates it.
- The chart's scripts in `operator/platform/chart/files/`, checked as rendered into their
  Jobs and never run, and the Keycloak theme.
- The OLM bundle: `operator-bundle.yml`'s `operator-sdk bundle validate` reports warnings
  only.
- `apps/admin/` and `platform/keycloak/`: CI only builds their images.

## Before a pull request

- **The CR and the chart move together.** A `Zaentrum` field maps onto the chart's values
  in `operator/internal/templates` (`chartValues`), and a chart change comes with a
  render test there.
- **A CRD change goes into every copy.** Generate the CRD from `operator/` with
  `go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5 crd paths=./api/... output:crd:dir=/tmp/crd`
  (`object paths=./api/...` for the deepcopy code). Splice the new field's block into
  `operator/config/crd/`, some of whose descriptions are shortened by hand, copy it to
  `operator/bundle/manifests/`, and render the appliance's manifests.
  `deploy/operator-install.yaml` moves only when it is re-pinned; until then the field is
  listed in `sinceThePin` (`operator/internal/controller/install_bundles_test.go`).
- **The appliance's manifests are committed.** After a change to `operator/config/`, run
  `./deploy/allinone/build.sh render` and commit what it changes in
  `deploy/allinone/manifests/`; on a tree that is in step, it changes nothing.
- **Neutrality.** The organisation's shared check, the `neutrality-shared` job of `ci.yml`,
  is the one neutrality check.
