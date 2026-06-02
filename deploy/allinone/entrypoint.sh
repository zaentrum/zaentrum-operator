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

# --- ingress host: keep it (issuer host == ingress host) --------------------
# The bundled Ingress now carries a REAL default host (stube.localhost). Modern
# browsers auto-resolve *.localhost to 127.0.0.1, so `docker run -p 80:80 …` +
# http://stube.localhost reaches Traefik with Host: stube.localhost and Traefik
# matches the rule — no /etc/hosts edit, no host-strip. Crucially this keeps the
# ingress host EQUAL to the issuer host (stube.localhost), the same invariant a
# real cluster holds, so a token minted by the browser-facing Keycloak validates
# unchanged in-cluster. We therefore DO NOT rewrite the Ingress here.
#
# Reaching the box by a different name (raw IP, a LAN hostname): set your real
# name in all four places — stube-env OIDC_ISSUER, stube-keycloak-config
# KC_HOSTNAME, the deploy/base/ingress.yaml host, AND STUBE_ISSUER_HOST below
# (the latter drives the in-cluster rewrite). They must agree, because the
# issuer host is both the browser host and the in-cluster validation host.

# --- in-cluster split-horizon for the OIDC issuer host ----------------------
# The bundled Keycloak pins its issuer to the PUBLIC host (KC_HOSTNAME ->
# http://stube.localhost/auth), so tokens carry iss=http://stube.localhost/auth/
# realms/stube. The Go services validate with coreos/go-oidc, which fetches
# {OIDC_ISSUER}/.well-known/openid-configuration and requires the doc `issuer`
# AND the token `iss` to equal OIDC_ISSUER. For that to work from inside the
# cluster, the issuer host must resolve to Keycloak. We add a coredns-custom
# entry rewriting stube.localhost straight to the keycloak Service — pods then
# reach the issuer directly (no ingress hop, Keycloak serves /auth natively), so
# discovery + JWKS succeed while the browser reaches the SAME host on 127.0.0.1
# (the *.localhost auto-resolution). To run under a different name, set your real
# host in deploy/base/ingress.yaml + KC_HOSTNAME + OIDC_ISSUER + STUBE_ISSUER_HOST
# (this rewrite follows STUBE_ISSUER_HOST). k3s applies anything in the manifest
# dir, so drop the ConfigMap there. CoreDNS hot-reloads the import on change.
ISSUER_HOST="${STUBE_ISSUER_HOST:-stube.localhost}"
cat > "$MANIFEST_DIR/coredns-custom.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns-custom
  namespace: kube-system
data:
  stube.server: |
    ${ISSUER_HOST}:53 {
      template IN A ${ISSUER_HOST} {
        match ^${ISSUER_HOST}\.\$
        answer "{{ .Name }} 5 IN CNAME keycloak.stube.svc.cluster.local"
        upstream
      }
      forward . /etc/resolv.conf
    }
EOF

echo ">> starting k3s (single node, Traefik ingress on :80)"
# Keep servicelb (klipper): it is what binds Traefik's LoadBalancer Service to
# the container's :80 — without it the Service stays <pending> and nothing
# answers on :80, so `-p 80:80` reaches a dead port. Keep Traefik (our ingress)
# and local-path (default StorageClass backing every PVC). metrics-server is the
# only thing an appliance can safely drop.
k3s server \
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
    echo ">> Stube is up — open http://stube.localhost"
  else
    echo ">> Stube is starting — pods may still be pulling images; open http://stube.localhost shortly"
  fi
  echo ">> inspect: docker exec -it stube k3s kubectl -n stube get pods"
) &

# Hand the foreground back to k3s so the container lives as long as k3s does.
wait "$K3S_PID"
