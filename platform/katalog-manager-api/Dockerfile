# syntax=docker/dockerfile:1.7
# Multi-stage build for the neutral management API. The final image is
# distroless/static (nonroot) and listens on :8080.
#
# Published as ghcr.io/nalet/stube/katalog-manager-api.
FROM golang:1.24-alpine AS build
WORKDIR /src

# Cache the module graph separately from the source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Stamp the version from the build arg (CI passes the git tag / short SHA).
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
	-trimpath \
	-ldflags="-s -w -X main.version=${VERSION}" \
	-o /out/katalog-manager-api ./

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/katalog-manager-api /katalog-manager-api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/katalog-manager-api"]
