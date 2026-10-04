#!/usr/bin/env bash
# Stamp a release into the OLM bundle: the CSV is named and versioned for the
# release, its controller is the release's operator image, and it may replace
# any older installed version.
#
#   operator/bundle/stamp.sh v0.4.0     # edits manifests/ in place
#
# The committed CSV is the one main builds: zaentrum-operator.v0.1.0 on
# ghcr.io/zaentrum/operator:latest. A release tag's bundle build
# (.github/workflows/operator-bundle.yml) runs this in its checkout before it
# builds the bundle image, so nothing release-specific is ever committed.
#
# olm.skipRange lets OLM move an install of any older version straight to this
# one, whether or not the catalog it subscribes to still lists that version: a
# release's catalog image carries the release's bundle alone.
set -euo pipefail

tag="${1:?usage: stamp.sh vX.Y.Z}"
[[ "$tag" =~ ^v([0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)$ ]] ||
  { echo "stamp.sh: $tag is not a release tag (vX.Y.Z or vX.Y.Z-pre)" >&2; exit 1; }
version="${BASH_REMATCH[1]}"

csv="$(cd "$(dirname "$0")" && pwd)/manifests/zaentrum-operator.clusterserviceversion.yaml"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

awk -v tag="$tag" -v version="$version" '
  /^  name: zaentrum-operator\.v/           { print "  name: zaentrum-operator." tag; name++; next }
  /^  version: /                            { print "  version: " version; ver++; next }
  /^  annotations:$/ && !skip               { print; print "    olm.skipRange: \"<" version "\""; skip++; next }
  /ghcr\.io\/zaentrum\/operator:latest/     { sub(/ghcr\.io\/zaentrum\/operator:latest/, "ghcr.io/zaentrum/operator:" tag); img++ }
  { print }
  END {
    if (name != 1 || ver != 1 || skip != 1 || img < 1) {
      printf "stamp.sh: expected one name, one version, one annotations block and the operator image; found %d, %d, %d, %d\n", name, ver, skip, img > "/dev/stderr"
      exit 1
    }
  }
' "$csv" > "$tmp"

if grep -q 'ghcr.io/zaentrum/operator:latest' "$tmp"; then
  echo "stamp.sh: the CSV still names ghcr.io/zaentrum/operator:latest" >&2
  exit 1
fi
cat "$tmp" > "$csv"
echo "stamped zaentrum-operator.$tag (version $version, ghcr.io/zaentrum/operator:$tag)"
