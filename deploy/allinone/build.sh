#!/usr/bin/env bash
# Build the Stube all-in-one image.
#
# Renders deploy/base with kustomize into ./manifests/, which the Dockerfile
# COPYs into the k3s auto-apply directory (/var/lib/rancher/k3s/server/manifests).
# k3s applies everything in that directory on first boot, so the rendered
# manifest IS the install.
#
#   ./deploy/allinone/build.sh                 # render + docker build :latest
#   IMAGE=ghcr.io/nalet/stube:v1 ./build.sh    # custom tag
#   ./deploy/allinone/build.sh render          # render manifests only (no build)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
BASE="$ROOT/deploy/base"
OUT="$HERE/manifests"
IMAGE="${IMAGE:-ghcr.io/nalet/stube:latest}"

# kustomize ships inside kubectl (kubectl kustomize); fall back to standalone.
render() {
  mkdir -p "$OUT"
  echo ">> rendering $BASE -> $OUT/stube.yaml"
  if command -v kustomize >/dev/null 2>&1; then
    kustomize build "$BASE" > "$OUT/stube.yaml"
  else
    kubectl kustomize "$BASE" > "$OUT/stube.yaml"
  fi
  echo ">> rendered $(grep -c '^kind:' "$OUT/stube.yaml") resources"
}

render
[ "${1:-build}" = "render" ] && exit 0

echo ">> docker build $IMAGE"
docker build -t "$IMAGE" "$HERE"
echo ">> built $IMAGE"
echo "   run: docker run -d --privileged --name stube -p 8080:80 $IMAGE"
