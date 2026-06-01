#!/usr/bin/env bash
# Stube all-in-one appliance: real Kubernetes, packed in one.
# Uses k3d (k3s in Docker) to boot a single-node cluster and apply deploy/base.
#
#   ./deploy/k3s/up.sh          # create + deploy
#   ./deploy/k3s/up.sh down     # tear the cluster down
set -euo pipefail

CLUSTER="${STUBE_CLUSTER:-stube}"
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
kubectl -n stube rollout status deploy/chino-web --timeout=180s || true

cat <<EOF

Stube is starting on http://localhost:8080
(add '127.0.0.1 stube.example.com' to /etc/hosts, or set your host in deploy/base/ingress.yaml)

  kubectl -n stube get pods
  ./deploy/k3s/up.sh down   # when finished
EOF
