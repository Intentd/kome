# Version stamping — reads from git; CI injects the same flags.
GIT_VERSION := $(shell git describe --tags --match 'v*' --always 2>/dev/null || echo "dev")
GIT_COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
GIT_DATE    := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

KOME_LDFLAGS := -X github.com/intentd/tools/version.Version=$(GIT_VERSION) \
                -X github.com/intentd/tools/version.Commit=$(GIT_COMMIT) \
                -X github.com/intentd/tools/version.Date=$(GIT_DATE)

OUTPUT_DIR ?= $(HOME)/.kome/bin

.PHONY: build test vet fmt

build:
	go build -ldflags "$(KOME_LDFLAGS)" -o $(OUTPUT_DIR)/kome ./cmd/kome

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .
