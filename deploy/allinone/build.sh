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
render() {
  mkdir -p "$OUT"; rm -f "$OUT"/*.yaml
  printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: zaentrum\n' > "$OUT/00-namespace.yaml"
  { for f in "$OP"/crd/*.yaml "$OP"/rbac/*.yaml "$OP"/manager/*.yaml; do echo "---"; cat "$f"; done; } > "$OUT/10-operator.yaml"
  cp "$OP/samples/zaentrum_v1alpha1_zaentrum.yaml" "$OUT/20-zaentrum.yaml"
  echo ">> baked operator install + Zaentrum CR ($(grep -c '^kind:' "$OUT/10-operator.yaml") operator objects)"
}

render
[ "${1:-build}" = "render" ] && exit 0

echo ">> docker build $IMAGE"
docker build -t "$IMAGE" "$HERE"
echo ">> built $IMAGE"
echo "   run: docker run -d --privileged --name zaentrum -p 8080:80 $IMAGE"
