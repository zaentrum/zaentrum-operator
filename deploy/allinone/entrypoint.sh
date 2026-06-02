#!/bin/sh
# All-in-one entrypoint: boot a single-node k3s server with the bundled
# Traefik ingress on :80, let k3s auto-apply the Stube manifests, and wait
# until the web app is ready.
set -eu

MANIFEST_DIR=/var/lib/rancher/k3s/server/manifests

# The bundled Ingress carries a placeholder host (stube.example.com). For the
# appliance we want it to answer on ANY host (localhost, the box's IP, a LAN
# name), so strip the host line before k3s applies it. Traefik then treats the
# rule as a host-less catch-all.
if [ -f "$MANIFEST_DIR/stube.yaml" ]; then
  sed -i '/- host: stube\.example\.com/d' "$MANIFEST_DIR/stube.yaml" || true
fi

echo ">> starting k3s (single node, Traefik ingress on :80)"
# Disable components an appliance does not need; keep Traefik (our :80 ingress)
# and local-path (the default StorageClass that backs every PVC).
k3s server \
  --disable=servicelb \
  --disable=metrics-server \
  --write-kubeconfig-mode=644 \
  &
K3S_PID=$!

KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export KUBECONFIG

echo ">> waiting for the API server"
i=0
until k3s kubectl get --raw='/readyz' >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 120 ] && { echo "!! API server never came up"; exit 1; }
  sleep 2
done

echo ">> waiting for the stube namespace (k3s auto-applies bundled manifests)"
i=0
until k3s kubectl get ns stube >/dev/null 2>&1; do
  i=$((i+1)); [ "$i" -gt 120 ] && { echo "!! stube namespace never appeared"; exit 1; }
  sleep 2
done

echo ">> waiting for chino-web to become available"
k3s kubectl -n stube rollout status deploy/chino-web --timeout=600s || \
  echo "!! chino-web not ready yet — it may still be pulling images from ghcr.io"

cat <<'EOF'

  Stube is up. With the container started as:
      docker run -d --privileged --name stube -p 8080:80 ghcr.io/nalet/stube:latest
  open http://localhost:8080

  Inspect the cluster:
      docker exec -it stube k3s kubectl -n stube get pods

EOF

# Hand the foreground back to k3s so the container lives as long as k3s does.
wait "$K3S_PID"
