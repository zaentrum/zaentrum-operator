# OLM bundle image for the Stube operator.
#
# This builds a registry+v1 bundle image: the CSV, the owned Stube CRD, and the
# annotations metadata. It is the OLM *bundle* image, distinct from the operator
# *controller* image (ghcr.io/nalet/stube/operator, built by
# .github/workflows/build-images.yml). The CI `operator` matrix leg builds the
# controller from operator/Dockerfile; this file packages the *metadata*, not
# the controller binary.
#
# Build (from operator/bundle):
#   docker build -f bundle.Dockerfile -t ghcr.io/nalet/stube/operator-bundle:v0.1.0 .
FROM scratch

# Core bundle labels (mirror metadata/annotations.yaml — OLM reads both).
LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1=stube-operator
LABEL operators.operatorframework.io.bundle.channels.v1=stable
LABEL operators.operatorframework.io.bundle.channel.default.v1=stable
LABEL com.redhat.openshift.versions="v4.14"

# Bundle payload: CSV + owned CRD, and the annotations metadata.
COPY manifests /manifests/
COPY metadata /metadata/
