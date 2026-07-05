# Zaentrum all-in-one: a whole Zaentrum cluster in a single container.
#
# Bundles a single-node k3s (real Kubernetes) plus the rendered deploy/base
# manifests. k3s auto-applies anything under its server manifests dir on boot,
# so starting the container installs Zaentrum. Application images are pulled from
# ghcr.io/zaentrum/appliance on first boot (see README for offline/airgap baking).
#
# Build it with deploy/allinone/build.sh (renders manifests first).
FROM rancher/k3s:v1.31.5-k3s1

# Rendered Zaentrum manifests -> k3s auto-apply dir. k3s applies every *.yaml here
# at startup and re-applies on change.
COPY manifests/ /var/lib/rancher/k3s/server/manifests/

COPY entrypoint.sh /usr/local/bin/zaentrum-entrypoint.sh

# OPTIONAL airgap: drop the app image tarball(s) into agent/images and k3s
# imports them before pulling from a registry. Left empty by default so the
# image stays small; see README "Offline / airgap".
#   /var/lib/rancher/k3s/agent/images/zaentrum-airgap.tar

# Traefik ingress listens on :80 inside the container; publish it with -p.
EXPOSE 80

ENTRYPOINT ["/usr/local/bin/zaentrum-entrypoint.sh"]
