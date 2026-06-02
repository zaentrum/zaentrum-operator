# OLM bundle image for the Stube operator.
#
# WIP / Stage 3: this builds a structurally-valid registry+v1 bundle image, but
# the bundle CONTENTS are still a skeleton (placeholder CSV, no CRD manifest
# yet). Do not push this to a catalog as a real channel entry until Stage 3
# fills it in. See operator/bundle/README.md.
#
# This is the OLM *bundle* image, distinct from the operator *controller* image
# (ghcr.io/nalet/stube/operator, built by .github/workflows/build-images.yml).
# The CI `operator` matrix leg builds the controller from operator/Dockerfile;
# this file is built separately when Stage 3 wires bundle publishing.
FROM scratch

# Core bundle labels (mirror metadata/annotations.yaml — OLM reads both).
LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1=stube-operator
LABEL operators.operatorframework.io.bundle.channels.v1=stable
LABEL operators.operatorframework.io.bundle.channel.default.v1=stable
LABEL com.redhat.openshift.versions="v4.14"

# Marker so this stub is obvious in image metadata.
LABEL stube.io.bundle-stage=wip-stage-3

# Bundle payload: CSV (+ CRD, added in Stage 3) and the annotations metadata.
COPY manifests /manifests/
COPY metadata /metadata/
