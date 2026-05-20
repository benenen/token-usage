# syntax=docker/dockerfile:1.7

# The image only ships the server — watchers run on developer machines
# and are distributed as plain binaries via the GitHub Release.

# ----- build stage: cross-compile the server for $TARGETPLATFORM -----
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
ARG TARGETOS
ARG TARGETARCH

# Proxy passthrough — `make docker` forwards these from the host env if set.
# Only the build stage sees them; the final image inherits nothing.
ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG NO_PROXY
ARG GOPROXY
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=${NO_PROXY} \
    GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}

WORKDIR /src

# warm module cache before copying full source
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/token-usage-server ./cmd/server

# ----- runtime: distroless, nonroot, ~2 MB base -----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/token-usage-server /usr/local/bin/token-usage-server

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/token-usage-server"]
