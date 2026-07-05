#!/usr/bin/env bash
# Zaentrum on real Kubernetes, locally: uses k3d (k3s in Docker) to boot a
# single-node cluster and apply deploy/base.
#
#   ./deploy/k3s/up.sh          # create + deploy
#   ./deploy/k3s/up.sh down     # tear the cluster down
#
# Zero-clone alternative: the all-in-one image bundles k3s + deploy/base in one
# container — no checkout, no k3d, just:
#   docker run -d --privileged --name zaentrum -p 8080:80 ghcr.io/zaentrum/appliance:latest
# (see deploy/allinone/README.md). This script is for hacking on the manifests.
set -euo pipefail

CLUSTER="${ZAENTRUM_CLUSTER:-zaentrum}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1 — install it first ($2)"; exit 1; }; }
need docker "https://docs.docker.com/get-docker/"
need k3d    "https://k3d.io/#installation"
need kubectl "https://kubernetes.io/docs/tasks/tools/"

if [ "${1:-up}" = "down" ]; then
  k3d cluster delete "$CLUSTER"; exit 0
fi

if ! k3d cluster list 2>/dev/null | grep -q "^${CLUSTER}\b"; then
  # 8080→80 maps the cluster ingress to localhost.
  k3d cluster create "$CLUSTER" -p "8080:80@loadbalancer" --wait
fi

kubectl apply -k "$ROOT/deploy/base"
kubectl -n zaentrum rollout status deploy/chino-web --timeout=180s || true

cat <<EOF

Zaentrum is starting — open http://zaentrum.localhost:8080
(*.localhost resolves to 127.0.0.1 in modern browsers; for a LAN name set it in
 deploy/base/ingress.yaml + OIDC_ISSUER + KC_HOSTNAME to the same host)

  kubectl -n zaentrum get pods
  ./deploy/k3s/up.sh down   # when finished
EOF
