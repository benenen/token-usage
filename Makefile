BIN     ?= bin
GOFLAGS ?= -trimpath

# Docker image config (override at the CLI, e.g. `make docker IMAGE_TAG=v1.0.0`)
IMAGE_NAME ?= token-usage-server
IMAGE_TAG  ?= dev
IMAGE      := $(IMAGE_NAME):$(IMAGE_TAG)

.PHONY: all build server watcher tidy test fmt vet clean docker docker-multi docker-run

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
	rm -rf $(BIN)

# Build the server Docker image for the host architecture (fastest, good for
# local dev). Use docker-multi for a release-style linux/amd64+arm64 build.
docker:
	docker build -t $(IMAGE) .

# Multi-arch build via buildx (matches what release.yml does). Stays local —
# add --push to actually push to a registry.
docker-multi:
	docker buildx build --platform linux/amd64,linux/arm64 -t $(IMAGE) .

# Convenience: build + run with the DSN from the environment, port 8080.
docker-run: docker
	docker run --rm -p 8080:8080 \
	    -e TOKENUSAGE_DSN \
	    $(IMAGE)
