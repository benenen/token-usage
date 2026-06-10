BIN     ?= bin
GOFLAGS ?= -trimpath

# `make release` cross-compiles ./cmd/watcher for these targets and packages
# each as tar.gz (unix) or zip (windows) under $(RELEASE_DIR). Override
# VERSION to stamp the archive filename (defaults to `git describe`).
RELEASE_DIR     ?= dist
RELEASE_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
RELEASE_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# Docker image config (override at the CLI, e.g. `make docker IMAGE_TAG=v1.0.0`)
IMAGE_NAME ?= token-usage-server
IMAGE_TAG  ?= dev
IMAGE      := $(IMAGE_NAME):$(IMAGE_TAG)

# Forward host proxy env vars into `docker build`. Lazy assignment so make
# picks up the values at recipe-run time.
#   HTTPS_PROXY=http://proxy:8080 make docker
#   GOPROXY=https://goproxy.cn,direct make docker
DOCKER_PROXY_ARGS = $(if $(HTTPS_PROXY),--build-arg HTTPS_PROXY=$(HTTPS_PROXY)) \
                    $(if $(HTTP_PROXY),--build-arg HTTP_PROXY=$(HTTP_PROXY)) \
                    $(if $(NO_PROXY),--build-arg NO_PROXY=$(NO_PROXY)) \
                    --build-arg GOPROXY=$(GOPROXY)

.PHONY: all build server watcher tidy test fmt vet clean release docker docker-multi docker-run docker-prepull

all: build

build: server watcher

server:
	go build $(GOFLAGS) -o $(BIN)/token-usage-server ./cmd/server

watcher:
	go build $(GOFLAGS) -o $(BIN)/token-usage-watcher ./cmd/watcher

tidy:
	go mod tidy

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -rf $(BIN) $(RELEASE_DIR)

release:
	@command -v zip >/dev/null || { echo "zip not found — needed for the windows archive"; exit 1; }
	@rm -rf $(RELEASE_DIR) && mkdir -p $(RELEASE_DIR)
	@set -eu; for target in $(RELEASE_TARGETS); do \
	    goos=$${target%/*}; goarch=$${target#*/}; \
	    ext=""; [ "$$goos" = "windows" ] && ext=".exe"; \
	    bin="token-usage-watcher$$ext"; \
	    archive="token-usage-watcher_$(RELEASE_VERSION)_$${goos}_$${goarch}"; \
	    echo "==> $$goos/$$goarch"; \
	    GOOS=$$goos GOARCH=$$goarch CGO_ENABLED=0 \
	        go build -trimpath -ldflags="-s -w" \
	        -o "$(RELEASE_DIR)/$$bin" ./cmd/watcher; \
	    if [ "$$goos" = "windows" ]; then \
	        (cd $(RELEASE_DIR) && zip -q "$$archive.zip" "$$bin"); \
	    else \
	        tar -C $(RELEASE_DIR) -czf "$(RELEASE_DIR)/$$archive.tar.gz" "$$bin"; \
	    fi; \
	    rm -f "$(RELEASE_DIR)/$$bin"; \
	done
	@(cd $(RELEASE_DIR) && sha256sum *.tar.gz *.zip > SHA256SUMS)
	@echo; ls -lh $(RELEASE_DIR)

# Pre-pull every base image used by the Dockerfile into the local Docker
# daemon. Useful in restricted networks where the daemon can't reach
# docker.io but skopeo can (different routes, proxy, mirror, etc.).
#
# We go via a docker-archive tarball + `docker load`, which sidesteps the
# Docker remote-API version negotiation (some skopeo builds ship with an
# older client lib than the daemon's minimum API).
DOCKER_PREPULL_IMAGES := \
	docker.io/library/golang:1.23-alpine \
	gcr.io/distroless/static-debian12:nonroot \
	docker.io/docker/dockerfile:1.7

docker-prepull:
	@command -v skopeo >/dev/null || { echo "skopeo not found — apt install skopeo (or brew install skopeo)"; exit 1; }
	@command -v docker >/dev/null || { echo "docker not found"; exit 1; }
	@set -eu; for img in $(DOCKER_PREPULL_IMAGES); do \
	    tag=$${img#docker.io/library/}; \
	    tmp=$$(mktemp /tmp/tu-prepull.XXXXXX.tar); \
	    echo "==> $$img -> $$tag"; \
	    skopeo copy "docker://$$img" "docker-archive:$$tmp:$$tag" || { rm -f $$tmp; exit 1; }; \
	    docker load -i "$$tmp" || { rm -f $$tmp; exit 1; }; \
	    rm -f $$tmp; \
	done
	@echo "==> local images now contain:"
	@docker images --format 'table {{.Repository}}\t{{.Tag}}\t{{.Size}}' | grep -E 'golang|distroless|dockerfile' || true

# Build the server Docker image for the host architecture (fastest, good for
# local dev). Use docker-multi for a release-style linux/amd64+arm64 build.
# Run `make docker-prepull docker` if your daemon can't reach docker.io.
docker:
	docker build $(DOCKER_PROXY_ARGS) -t $(IMAGE) .

# Multi-arch build via buildx (matches what release.yml does). Stays local —
# add --push to actually push to a registry.
docker-multi:
	docker buildx build $(DOCKER_PROXY_ARGS) --platform linux/amd64,linux/arm64 -t $(IMAGE) .

# Convenience: build + run with the DSN from the environment, port 8080.
docker-run: docker
	docker run --rm -p 8080:8080 \
	    -e TOKENUSAGE_DSN \
	    $(IMAGE)

watch: server
	TOKENUSAGE_DSN="postgres://tokenuser:tokenpass@localhost:5432/tokenusage?sslmode=disable" $(BIN)/token-usage-server
