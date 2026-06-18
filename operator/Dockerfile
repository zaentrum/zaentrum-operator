# Multi-stage build for the Stube operator.
# Final image: distroless static (nonroot), published as ghcr.io/zaentrum/stube/operator.
FROM golang:1.24 AS build
WORKDIR /workspace

# Cache deps first.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Source. The platform manifests are embedded via go:embed, so they ship in
# the binary — no runtime files needed.
COPY main.go main.go
COPY api/ api/
COPY internal/ internal/

# Static, stripped binary.
RUN CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath \
    go build -ldflags="-s -w" -o /manager main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
