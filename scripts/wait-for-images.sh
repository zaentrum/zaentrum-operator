#!/usr/bin/env bash
# Wait until ghcr.io holds the images of a release, at the release's tag.
#
#   scripts/wait-for-images.sh v0.4.0              # the operator + the platform's images
#   scripts/wait-for-images.sh v0.4.0 operator     # just these
#   WAIT_MINUTES=0 scripts/wait-for-images.sh v0.4.0   # look once, do not wait
#
# A release is tagged in every repository at once, and each builds its own
# images, so a build that needs another repository's images — the appliance's
# boot, the release's install — waits for them here. With no images named, it
# waits for the operator and for every ghcr.io/zaentrum image the platform's
# chart runs, read from the chart's templates so the list cannot drift.
#
# Each tag is looked up as a pull would look it up, anonymously; the images
# are public. It looks every 30 seconds until all are there or WAIT_MINUTES
# (default 40) have passed.
set -euo pipefail

tag="${1:?usage: wait-for-images.sh vX.Y.Z [image ...]}"
shift
root="$(cd "$(dirname "$0")/.." && pwd)"

images=("$@")
if [ ${#images[@]} -eq 0 ]; then
  images=(operator)
  while IFS= read -r name; do images+=("$name"); done < <(
    grep -rhoE 'ghcr\.io/zaentrum/[a-z0-9-]+:[{][{] *\.Values\.global\.version *[}][}]' \
      "$root/operator/platform/chart/templates" |
      sed -E 's#ghcr\.io/zaentrum/([a-z0-9-]+):.*#\1#' | sort -u)
  [ ${#images[@]} -gt 1 ] || { echo "wait-for-images.sh: the chart names no ghcr.io/zaentrum image" >&2; exit 1; }
fi

accept='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

# present <image>: the registry holds <image>:<tag>.
present() {
  local token
  token="$(curl -fsS "https://ghcr.io/token?scope=repository:zaentrum/$1:pull" 2>/dev/null |
    sed -n 's/.*"token":"\([^"]*\)".*/\1/p')" || return 1
  [ -n "$token" ] || return 1
  [ "$(curl -s -o /dev/null -w '%{http_code}' -I -H "Authorization: Bearer $token" -H "Accept: $accept" \
    "https://ghcr.io/v2/zaentrum/$1/manifests/$tag")" = 200 ]
}

deadline=$(( $(date +%s) + ${WAIT_MINUTES:-40} * 60 ))
echo "waiting for ${#images[@]} images at $tag: ${images[*]}"
while :; do
  missing=()
  for image in "${images[@]}"; do
    present "$image" || missing+=("$image")
  done
  if [ ${#missing[@]} -eq 0 ]; then
    echo "every image is published at $tag"
    exit 0
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "::error::not published at $tag: ${missing[*]}"
    exit 1
  fi
  echo "$(date -u +%H:%M:%S) still missing at $tag: ${missing[*]}"
  sleep 30
done
