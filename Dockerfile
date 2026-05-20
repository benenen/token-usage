# syntax=docker/dockerfile:1.7

# The image only ships the server — watchers run on developer machines
# and are distributed as plain binaries via the GitHub Release.

# ----- build stage: cross-compile the server for $TARGETPLATFORM -----
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
ARG TARGETOS
ARG TARGETARCH
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
