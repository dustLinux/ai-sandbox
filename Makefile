SHELL := /bin/sh
GO ?= go
DIST := dist

.PHONY: all build release test vet fmt clean

all: build

build:
	$(GO) build ./...

release:
	mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o $(DIST)/sandboxd-linux-amd64 ./cmd/sandboxd
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags='-s -w' -o $(DIST)/sandboxd-linux-arm64 ./cmd/sandboxd
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags='-s -w' -o $(DIST)/sandbox-pool-linux-amd64 ./cmd/sandbox-pool
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags='-s -w' -o $(DIST)/sandbox-pool-linux-arm64 ./cmd/sandbox-pool
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o $(DIST)/sandboxd ./cmd/sandboxd
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags='-s -w' -o $(DIST)/sandbox-pool ./cmd/sandbox-pool
	ls -lh $(DIST)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf $(DIST)
