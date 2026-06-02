# Bundled Stube identity provider: Keycloak with the 'stube' realm baked in.
#
# Two-stage optimized build. The builder runs `kc.sh build` to bake the
# Postgres JDBC store + health/metrics into an immutable, pre-augmented
# distribution; the runtime stage starts with `--optimized` so no build
# step runs at pod start (fast, deterministic boot).
#
# The 'stube' realm is imported on first start from
# /opt/keycloak/data/import/. The OAuth 2.0 Device Authorization Grant is
# GA in Keycloak 26.x (no --features flag needed); it is enabled per-client
# in the realm import via the oauth2.device.authorization.grant.enabled
# client attribute.

# ---------------------------------------------------------------------------
# Stage 1 — optimized build
# ---------------------------------------------------------------------------
FROM quay.io/keycloak/keycloak:26.1 AS builder

# Build-time options. These get frozen into the augmented distribution and
# must match the runtime DB vendor, or `start --optimized` refuses to boot.
ENV KC_DB=postgres
ENV KC_HEALTH_ENABLED=true
ENV KC_METRICS_ENABLED=true
# Persisted user/session state is not part of the build; only the feature
# set + provider augmentation is. Device grant is GA so it is omitted from
# --features.

RUN /opt/keycloak/bin/kc.sh build \
      --db=postgres \
      --health-enabled=true \
      --metrics-enabled=true

# ---------------------------------------------------------------------------
# Stage 2 — runtime
# ---------------------------------------------------------------------------
FROM quay.io/keycloak/keycloak:26.1

# Carry over the pre-augmented distribution from the builder.
COPY --from=builder /opt/keycloak/ /opt/keycloak/

# Bake the realm import. On first start (--import-realm) Keycloak reads every
# *.json in this directory and creates/updates the realm. The
# ${STUBE_MANAGER_CLIENT_SECRET} placeholder in the JSON is resolved from the
# pod environment at import time (Keycloak performs env substitution on
# realm-import files), so no real secret is ever committed to the image.
COPY stube-realm.json /opt/keycloak/data/import/stube-realm.json

# Run as the non-root keycloak user shipped in the base image.
USER 1000

# Default runtime command. The deploy/ Deployment may append/override flags,
# but the image is self-sufficient: optimized start + first-run realm import.
# Hostname / proxy / relative-path settings are supplied as env by deploy/
# (KC_PROXY_HEADERS=xforwarded, KC_HOSTNAME_STRICT=false,
#  KC_HTTP_RELATIVE_PATH=/auth, KC_HTTP_ENABLED=true) — see README.md.
ENTRYPOINT ["/opt/keycloak/bin/kc.sh"]
CMD ["start", "--optimized", "--import-realm"]
