GO        ?= go
BIN       := bin/mountenant
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)

SQLC_VERSION         := v1.31.1

.PHONY: all build test race lint fmt vuln generate live-test clean

all: lint test build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/mountenant

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

fmt:
	gofmt -w .

lint:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run make fmt"; exit 1; }
	$(GO) vet ./...
	$(GO) vet -tags live ./...
	golangci-lint run

# Generated code is committed; CI fails when this changes anything.
generate:
	$(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Contract tests against real backends; see docs/spike-backend-adapter.md.
live-test:
	$(GO) test -tags live -count=1 -v ./internal/jobs/adapters/sabdav/

clean:
	rm -rf bin
