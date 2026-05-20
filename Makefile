BIN ?= bin
GOFLAGS ?= -trimpath

.PHONY: all build server watcher tidy test fmt vet clean

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
