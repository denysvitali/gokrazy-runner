SHELL := /usr/bin/env bash

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_DATE := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.BuildDate=$(BUILD_DATE)

GO ?= go
GOFLAGS ?= -trimpath

GOKRAZY_INSTANCE ?= gokrazy-runner
GOKRAZY_PARENT_DIR ?= $(CURDIR)/gokrazy
IMAGE_DIR ?= $(CURDIR)/ota

.PHONY: all
all: build

.PHONY: build
build:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o dist/perm-init ./cmd/perm-init
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o dist/runner-init ./cmd/runner-init
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o dist/runner-webui ./cmd/runner-webui

.PHONY: build-arm64
build-arm64:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o dist/perm-init-linux-arm64 ./cmd/perm-init
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o dist/runner-init-linux-arm64 ./cmd/runner-init
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o dist/runner-webui-linux-arm64 ./cmd/runner-webui

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test:
	$(GO) test -race -v ./...

.PHONY: test-short
test-short:
	$(GO) test -short ./...

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: ota
ota:
	GOKRAZY_INSTANCE=$(GOKRAZY_INSTANCE) \
	GOKRAZY_PARENT_DIR=$(GOKRAZY_PARENT_DIR) \
	IMAGE_DIR=$(IMAGE_DIR) \
	bash scripts/build-ota-image.sh

.PHONY: clean
clean:
	rm -rf dist ota gokrazy
