# Stage 1: build the Go binary.
FROM golang:1.24-alpine AS build
WORKDIR /build

# Pull the module graph first so Docker layer caching survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# Strip + trimpath so the image stays small and binaries don't leak paths.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/chino-stream ./cmd/chino-stream

# Stage 2: runtime. Debian slim with the distro ffmpeg package — the
# default software (libx264 / libfdk-free aac) path. ffmpeg is the only
# hard runtime dependency; the service shells out to ffmpeg + ffprobe
# for on-demand transcode / remux / passthrough.
#
# GPU acceleration is opt-in: setting USE_NVENC=true makes the transcode
# path emit h264_nvenc + CUDA hwaccel arguments. That requires an ffmpeg
# build with NVENC linked in and a GPU visible to the container; this
# image ships the software path only. Build a CUDA-enabled variant on
# top of an NVIDIA base image if you want hardware encoding.
FROM debian:bookworm-slim
WORKDIR /app

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/*

# Run as a non-root user. The container orchestrator may override the
# UID; create a placeholder so /app and /tmp work either way.
RUN useradd --system --uid 1001 --gid 0 --home-dir /app --no-create-home --shell /usr/sbin/nologin appuser \
    && mkdir -p /var/lib/stube/media /var/lib/stube/packages /var/cache/stube-hls \
    && chown -R 1001:0 /app /var/cache/stube-hls

COPY --from=build --chown=1001:0 /out/chino-stream /app/chino-stream

USER 1001
EXPOSE 8080
CMD ["/app/chino-stream"]

LABEL org.opencontainers.image.source="https://github.com/zaentrum/stube"
LABEL org.opencontainers.image.title="chino-stream"
