#!/usr/bin/env bash
# Render a release's operator install: what deploy/operator-install.yaml is
# for main, for the release.
#
#   scripts/render-release.sh v0.4.0           # -> dist/operator-install.yaml
#   scripts/render-release.sh v0.4.0 /tmp/out  # -> /tmp/out/operator-install.yaml
#
# It is the kustomize build of operator/config at this checkout — the CRDs,
# the cluster RBAC and the controller the release's operator was built with —
# with the controller's image set to ghcr.io/zaentrum/operator:<tag>. The
# release workflow (.github/workflows/release.yml) attaches it to the GitHub
# release, so a cluster installs a release with
#
#   kubectl apply -f https://github.com/zaentrum/zaentrum-operator/releases/download/v0.4.0/operator-install.yaml
#
# The committed deploy/operator-install.yaml is untouched: it stays the pin a
# cluster-admin moves by hand.
set -euo pipefail

tag="${1:?usage: render-release.sh vX.Y.Z [outdir]}"
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
  { echo "render-release.sh: $tag is not a release tag (vX.Y.Z or vX.Y.Z-pre)" >&2; exit 1; }
root="$(cd "$(dirname "$0")/.." && pwd)"
out="${2:-$root/dist}"
mkdir -p "$out"

image="ghcr.io/zaentrum/operator:$tag"
{
  printf '%s\n' \
    "# The Zaentrum operator $tag: its CRDs, its cluster RBAC and its controller," \
    "# $image. Apply it as cluster-admin:" \
    "#   kubectl apply -f operator-install.yaml" \
    "# Rendered by scripts/render-release.sh from operator/config at $tag."
  kubectl kustomize "$root/operator/config" |
    awk -v img="$image" '
      /^ *image: ghcr\.io\/zaentrum\/operator[:@]/ { sub(/ghcr\.io\/zaentrum\/operator[:@][^ ]*/, img); n++ }
      { print }
      END { if (n != 1) { print "render-release.sh: expected the controller image once, found " n > "/dev/stderr"; exit 1 } }
    '
} > "$out/operator-install.yaml.tmp"
mv "$out/operator-install.yaml.tmp" "$out/operator-install.yaml"
echo "rendered $out/operator-install.yaml ($(grep -c '^kind:' "$out/operator-install.yaml") objects, $image)"
