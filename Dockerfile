# syntax=docker/dockerfile:1

# ---- Build stage ------------------------------------------------------------
# Pure-Go build (no CGO), statically linked so it can run on distroless/static.
FROM golang:1.26 AS build

WORKDIR /src

# Download dependencies first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source and build a small static binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /paperplane ./cmd/paperplane

# Pre-create the data dir here so it can be copied into the runtime image with
# nonroot ownership. distroless has no shell, so we cannot mkdir/chown there.
RUN mkdir -p /data

# ---- Runtime stage ----------------------------------------------------------
# distroless/static: no shell, no package manager, runs as an unprivileged user.
# HEALTHCHECK therefore uses the binary's own `healthcheck` subcommand instead
# of curl.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /paperplane /paperplane

# Data dir owned by the nonroot uid (65532) so the runtime user can write the
# SQLite DB and site files. An anonymous or freshly-created named volume mounted
# at /data inherits this ownership from the image directory.
COPY --from=build --chown=65532:65532 /data /data

# ADMIN_PASSWORD and APP_URL are supplied at runtime, not baked into the image.
ENV DATA_DIR=/data \
    PORT=8080

EXPOSE 8080
VOLUME ["/data"]

USER nonroot

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/paperplane", "healthcheck"]

ENTRYPOINT ["/paperplane"]

# Example run (data persisted in a named volume; secrets passed at runtime):
#   docker run -d -p 8080:8080 -v paperplane-data:/data \
#     -e ADMIN_PASSWORD=change-me -e APP_URL=https://example.com \
#     ghcr.io/kalfian/paper-plane
