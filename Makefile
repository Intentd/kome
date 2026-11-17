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

# The tests run against envtest's API server; setup-envtest fetches it:
#   go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.25
ENVTEST_K8S_VERSION ?= 1.37.0
ENVTEST_BIN_DIR     ?= $(HOME)/.envtest-bins

test:
	KUBEBUILDER_ASSETS="$$(setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_BIN_DIR) -p path)" go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .
