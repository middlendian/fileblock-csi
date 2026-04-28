# Makefile for fileblock-csi.
#
# Common targets:
#   make build           # compile both binaries into ./bin
#   make test            # run unit tests
#   make test-race       # run unit tests with -race
#   make cover           # run tests with -race and coverage; emits cover.out
#   make vet             # go vet
#   make fmt             # gofmt -w (modifies files in place)
#   make fmt-check       # fail if any file needs formatting
#   make lint            # golangci-lint run
#   make tidy            # go mod tidy
#   make tidy-check      # fail if go.mod / go.sum need tidying (CI gate)
#   make check           # full CI gate locally (run before every push)
#   make smoke           # local end-to-end (requires root, loop devices)
#   make sanity          # csi-sanity (requires root, loop devices, csi-sanity)
#   make e2e             # kind + go test ./test/e2e (local backing store)
#   make e2e-nfs         # kind + go test ./test/e2e (NFSv3 backing store)
#   make docker          # build the container image
#   make clean           # remove ./bin and ./dist

SHELL := /bin/bash

# Use the module path so -ldflags can target the right symbol.
MODULE       := github.com/middlendian/fileblock-csi
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS      := -s -w -X $(MODULE)/pkg/driver.Version=$(VERSION)
GOFLAGS      ?= -trimpath
BIN_DIR      := bin
COVER_OUT    := cover.out

# Tooling pinned for reproducibility. CI installs these the same way.
GOLANGCI_LINT_VERSION ?= v2.5.0

GO_PACKAGES := ./...

.PHONY: all
all: fmt-check vet lint test build

# Mirrors the ci.yml gate (every job except the Docker build, which needs a
# daemon). Run this before pushing — it's the cheapest way to catch the same
# failures CI will catch on the PR.
.PHONY: check
check: fmt-check vet lint tidy-check test build

.PHONY: build
build: $(BIN_DIR)/fileblock-controller $(BIN_DIR)/fileblock-node

$(BIN_DIR)/fileblock-controller: $(shell find cmd/controller pkg -name '*.go') go.mod go.sum
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/controller

$(BIN_DIR)/fileblock-node: $(shell find cmd/node pkg -name '*.go') go.mod go.sum
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/node

.PHONY: test
test:
	go test $(GO_PACKAGES)

.PHONY: test-race
test-race:
	go test -race $(GO_PACKAGES)

.PHONY: cover
cover:
	@pkgs=$$(go list -f '{{if (or .TestGoFiles .XTestGoFiles)}}{{.ImportPath}}{{end}}' ./...); \
	go test -race -covermode=atomic -coverprofile=$(COVER_OUT) -coverpkg=./... $$pkgs
	@go tool cover -func=$(COVER_OUT) | tail -n 1

.PHONY: vet
vet:
	go vet $(GO_PACKAGES)

.PHONY: fmt
fmt:
	gofmt -s -w $(shell find . -name '*.go' -not -path './vendor/*')

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -s -l $(shell find . -name '*.go' -not -path './vendor/*')); \
	if [[ -n "$$unformatted" ]]; then \
	  echo "the following files are not gofmt-clean:"; \
	  echo "$$unformatted"; \
	  exit 1; \
	fi

.PHONY: lint
lint:
	@if ! command -v golangci-lint >/dev/null; then \
	  echo "golangci-lint not on PATH; install with:"; \
	  echo "  curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \\"; \
	  echo "    | sh -s -- -b \$$(go env GOPATH)/bin $(GOLANGCI_LINT_VERSION)"; \
	  exit 1; \
	fi
	golangci-lint run

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: tidy-check
tidy-check:
	go mod tidy
	@if ! git diff --quiet -- go.mod go.sum; then \
	  echo "go.mod or go.sum is not tidy. Run 'make tidy' and commit the result."; \
	  git --no-pager diff -- go.mod go.sum; \
	  exit 1; \
	fi

.PHONY: smoke
smoke:
	sudo -E hack/smoke.sh

.PHONY: sanity
sanity:
	sudo -E hack/csi-sanity.sh

.PHONY: e2e
e2e:
	hack/e2e.sh

.PHONY: e2e-nfs
e2e-nfs:
	BACKING_KIND=nfs hack/e2e.sh

.PHONY: docker
docker:
	docker build -t fileblock-csi:$(VERSION) --build-arg VERSION=$(VERSION) .

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) dist $(COVER_OUT)
