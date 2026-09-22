#!/usr/bin/env bash
# Build the Zaentrum all-in-one image.
#
# Renders deploy/base with kustomize into ./manifests/, which the Dockerfile
# COPYs into the k3s auto-apply directory (/var/lib/rancher/k3s/server/manifests).
# k3s applies everything in that directory on first boot, so the rendered
# manifest IS the install.
#
#   ./deploy/allinone/build.sh                 # render + docker build :latest
#   IMAGE=ghcr.io/zaentrum/appliance:v1 ./build.sh    # custom tag
#   ./deploy/allinone/build.sh render          # render manifests only (no build)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
BASE="$ROOT/deploy/base"
OUT="$HERE/manifests"
IMAGE="${IMAGE:-ghcr.io/zaentrum/appliance:latest}"

# The appliance boots the Zaentrum OPERATOR, which reconciles the whole platform
# from a Zaentrum CR — rather than baking the rendered platform manifests directly.
# k3s auto-applies the files below in filename order: the zaentrum namespace, then
# the operator install (CRD + RBAC + manager), then the Zaentrum CR. The operator
# then creates/owns everything (so /manage can talk to it + auto-update works).
# deploy/base stays the operator's template source AND the kustomize path for
# non-appliance / external-cluster installs.
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

render() {
  mkdir -p "$OUT"; rm -f "$OUT"/*.yaml
  printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: zaentrum\n' > "$OUT/00-namespace.yaml"
  {
    for f in "$OP"/crd/*.yaml "$OP"/rbac/*.yaml; do echo "---"; cat "$f"; done
    for f in "$OP"/manager/*.yaml; do echo "---"; stamp_appliance "$f"; done
  } > "$OUT/10-operator.yaml"
  grep -q 'ZAENTRUM_INSTALL_SOURCE' "$OUT/10-operator.yaml" ||
    { echo "build.sh: appliance install source not stamped — refusing to ship an appliance that reports itself as a manifest apply"; exit 1; }
  cp "$OP/samples/zaentrum_v1alpha1_zaentrum.yaml" "$OUT/20-zaentrum.yaml"
  echo ">> baked operator install + Zaentrum CR ($(grep -c '^kind:' "$OUT/10-operator.yaml") operator objects)"
}

render
[ "${1:-build}" = "render" ] && exit 0

echo ">> docker build $IMAGE"
docker build -t "$IMAGE" "$HERE"
echo ">> built $IMAGE"
echo "   run: docker run -d --privileged --name zaentrum -p 8080:80 $IMAGE"
