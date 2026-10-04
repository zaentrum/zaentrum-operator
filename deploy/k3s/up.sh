#!/usr/bin/env bash
# UNSUPPORTED. deploy/base, which this applies, does not bring up a working
# platform (see its kustomization.yaml); it is kept for reference. For the
# platform, run the appliance — k3s, the operator and a Zaentrum in one
# container, no checkout:
#   docker run -d --privileged --restart unless-stopped --name zaentrum -p 80:80 ghcr.io/zaentrum/appliance:latest
# (deploy/allinone/README.md), or the operator on a cluster (README.md).
#
# Zaentrum's old base on real Kubernetes, locally: k3d (k3s in Docker) boots a
# single-node cluster and deploy/base is applied, with random credentials
# (deploy/base/make-secrets.sh; the base ships none).
#
#   ./deploy/k3s/up.sh          # create + deploy
#   ./deploy/k3s/up.sh down     # tear the cluster down
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
  # Port 80, as the issuer the base names (http://zaentrum.localhost) has no port.
  k3d cluster create "$CLUSTER" -p "80:80@loadbalancer" --wait
fi

"$ROOT/deploy/base/make-secrets.sh"
kubectl apply -k "$ROOT/deploy/base"
kubectl -n zaentrum rollout status deploy/chino-web --timeout=180s || true

cat <<EOF

Zaentrum is starting — open http://zaentrum.localhost
(*.localhost resolves to 127.0.0.1 in modern browsers; for a LAN name set it in
 deploy/base/ingress.yaml + OIDC_ISSUER + KC_HOSTNAME to the same host)

  kubectl -n zaentrum get pods
  ./deploy/k3s/up.sh down   # when finished
EOF
