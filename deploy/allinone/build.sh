#!/usr/bin/env bash
# Build the Zaentrum all-in-one image.
#
# Writes ./manifests/ from operator/config — the namespace, the operator's
# install (CRDs, RBAC, manager) and the sample Zaentrum — which the Dockerfile
# COPYs into the k3s auto-apply directory (/var/lib/rancher/k3s/server/manifests).
# k3s applies everything in that directory on first boot, so those manifests
# ARE the install.
#
#   ./deploy/allinone/build.sh                 # render + docker build :latest
#   IMAGE=ghcr.io/zaentrum/appliance:v1 ./build.sh    # custom tag
#   ./deploy/allinone/build.sh render          # render manifests only (no build)
#   VERSION=v0.4.0 ./deploy/allinone/build.sh render   # a release's appliance
#
# Which platform the appliance boots follows what it is built from. A build of
# main bakes the operator's :latest and boots the sample Zaentrum on the edge
# channel, so the platform runs the latest images that operator is built with.
# A release (VERSION=vX.Y.Z, the tag's build) bakes operator:vX.Y.Z and boots
# the sample pinned to spec.version vX.Y.Z: the operator, its CRDs and every
# image of the platform are that release's, whatever the channels say later.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
OUT="$HERE/manifests"
IMAGE="${IMAGE:-ghcr.io/zaentrum/appliance:latest}"
VERSION="${VERSION:-}"
if [ -n "$VERSION" ] && ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "build.sh: VERSION=$VERSION is not a release tag (vX.Y.Z)"; exit 1
fi

# The appliance boots the Zaentrum OPERATOR, which reconciles the whole platform
# from a Zaentrum CR — rather than baking the rendered platform manifests directly.
# k3s auto-applies the files below in filename order: the zaentrum namespace, then
# the operator install (CRD + RBAC + manager), then the Zaentrum CR. The operator
# then renders the platform from the chart it embeds, and creates and owns it.
OP="$ROOT/operator/config"

# The appliance is the ONE install the cluster cannot be asked about: it bakes
# the same manifests a cluster-admin would apply by hand, so nothing in the API
# tells the two apart (an OLM install, by contrast, is derivable from the
# ClusterServiceVersion that owns the Deployment). So the appliance declares
# itself here, and the operator reports it in status.controller.source.
#
# Applied only to the manager Deployment — the CRDs and RBAC are copied
# verbatim — so no other `env:` in the bundle can be hit by accident.
stamp_appliance() {
  awk '
    /^          env:$/ && !done {
      print
      print "            # Stamped by deploy/allinone/build.sh: this controller"
      print "            # ships inside the all-in-one image."
      print "            - name: ZAENTRUM_INSTALL_SOURCE"
      print "              value: appliance"
      done = 1
      next
    }
    { print }
    END { if (!done) { print "build.sh: no env: block in the manager Deployment to stamp" > "/dev/stderr"; exit 1 } }
  ' "$1"
}

# The operator the appliance runs (a manager manifest on stdin): :latest for
# main, the release's tag for a release — its one image line, exactly once.
pin_operator() {
  if [ -z "$VERSION" ]; then cat; return; fi
  awk -v img="ghcr.io/zaentrum/operator:$VERSION" '
    /^ *image: ghcr\.io\/zaentrum\/operator:latest$/ { sub(/ghcr\.io\/zaentrum\/operator:latest/, img); n++ }
    { print }
    END { if (n != 1) { print "build.sh: the manager names ghcr.io/zaentrum/operator:latest " n " times, not once" > "/dev/stderr"; exit 1 } }
  '
}

# The Zaentrum the appliance boots: the sample, on edge for main, pinned to
# the release for a release.
zaentrum_cr() {
  local from to
  if [ -n "$VERSION" ]; then from='  version: latest' to="  version: $VERSION"
  else from='  channel: stable' to='  channel: edge'; fi
  awk -v from="$from" -v to="$to" '
    $0 == from { print to; n++; next }
    { print }
    END { if (n != 1) { print "build.sh: the sample Zaentrum has \"" from "\" " n " times, not once" > "/dev/stderr"; exit 1 } }
  ' "$1"
}

render() {
  mkdir -p "$OUT"; rm -f "$OUT"/*.yaml
  printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: zaentrum\n' > "$OUT/00-namespace.yaml"
  {
    for f in "$OP"/crd/*.yaml "$OP"/rbac/*.yaml; do echo "---"; cat "$f"; done
    for f in "$OP"/manager/*.yaml; do echo "---"; stamp_appliance "$f" | pin_operator; done
  } > "$OUT/10-operator.yaml"
  grep -q 'ZAENTRUM_INSTALL_SOURCE' "$OUT/10-operator.yaml" ||
    { echo "build.sh: appliance install source not stamped — refusing to ship an appliance that reports itself as a manifest apply"; exit 1; }
  zaentrum_cr "$OP/samples/zaentrum_v1alpha1_zaentrum.yaml" > "$OUT/20-zaentrum.yaml"
  echo ">> baked operator install + Zaentrum CR ($(grep -c '^kind:' "$OUT/10-operator.yaml") operator objects, ${VERSION:-edge})"
}

render
[ "${1:-build}" = "render" ] && exit 0

echo ">> docker build $IMAGE"
docker build -t "$IMAGE" "$HERE"
echo ">> built $IMAGE"
echo "   run: docker run -d --privileged --restart unless-stopped --name zaentrum -p 80:80 $IMAGE"
echo "   then open http://zaentrum.localhost"
