# Zaentrum all-in-one: a whole Zaentrum cluster in a single container.
#
# Bundles a single-node k3s (real Kubernetes), the operator's install and a
# Zaentrum (manifests/, which build.sh writes from operator/config). k3s
# auto-applies its server manifests dir on boot, so starting the container
# installs the operator, which brings up the platform. The platform's images are
# pulled from ghcr.io/zaentrum/<service> as their pods start; there is no
# offline mode (README "Where the images come from").
#
# Build it with deploy/allinone/build.sh (renders manifests first).
FROM rancher/k3s:v1.31.5-k3s1

# The operator's install and the Zaentrum -> k3s auto-apply dir. k3s applies
# every *.yaml here at startup and re-applies a file when it changes.
COPY manifests/ /var/lib/rancher/k3s/server/manifests/

COPY entrypoint.sh /usr/local/bin/zaentrum-entrypoint.sh

# Traefik answers on :80 inside the container, and on :443 once the platform has
# a certificate of its own (README "Phones and TVs"); publish them with -p.
EXPOSE 80 443

ENTRYPOINT ["/usr/local/bin/zaentrum-entrypoint.sh"]
