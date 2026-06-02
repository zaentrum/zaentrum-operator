#!/bin/sh
# All-in-one entrypoint: boot a single-node k3s server with the bundled
# Traefik ingress on :80, let k3s auto-apply the Stube manifests, and wait
# until the web app is ready.
set -eu

# --- cgroup v2 nesting -------------------------------------------------------
# On a cgroup v2 host, running k3s inside a container leaves PID 1 in the root
# cgroup with domain controllers, so the kubelet cannot create the kubepods
# cgroup ("cannot enter cgroupv2 ... invalid state") and the API server never
# comes up. Move our processes into a leaf /init cgroup and delegate the
# controllers to subtrees — the same trick k3d/containerd use. No-op on v1.
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  mkdir -p /sys/fs/cgroup/init
  xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
  sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers \
    > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
fi

MANIFEST_DIR=/var/lib/rancher/k3s/server/manifests

# The bundled Ingress carries a placeholder host (stube.example.com). For the
# appliance we want it to answer on ANY host (localhost, the box's IP, a LAN
# name), so drop the host before k3s applies it — Traefik then treats the rule
# as a host-less catch-all. Replace "- host: stube.example.com" with a bare "-"
# (KEEP the list dash): deleting the whole line would turn rules: from a list
# into a mapping and k3s would reject the Ingress.
if [ -f "$MANIFEST_DIR/stube.yaml" ]; then
  sed -i 's/^\([[:space:]]*\)- host: stube\.example\.com[[:space:]]*$/\1-/' "$MANIFEST_DIR/stube.yaml" || true
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

# Best-effort readiness LOGGING only — runs in the background so a slow or
# degraded health check can never tear down the container. The container lives
# exactly as long as k3s (the `wait` below). Signal = `get ns stube` (true once
# the API is up and the bundled manifests applied); /readyz can stay degraded
# while the cluster is perfectly usable, so we don't gate on it.
(
  i=0
  until k3s kubectl get ns stube >/dev/null 2>&1; do
    i=$((i+1)); [ "$i" -gt 150 ] && { echo ">> still waiting for the API server / stube namespace…"; i=0; }
    sleep 2
  done
  if k3s kubectl -n stube rollout status deploy/chino-web --timeout=600s >/dev/null 2>&1; then
    echo ">> Stube is up — open http://localhost:8080"
  else
    echo ">> Stube is starting — pods may still be pulling images; open http://localhost:8080 shortly"
  fi
  echo ">> inspect: docker exec -it stube k3s kubectl -n stube get pods"
) &

# Hand the foreground back to k3s so the container lives as long as k3s does.
wait "$K3S_PID"
