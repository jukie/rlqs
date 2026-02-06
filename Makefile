BINARY    := rlqs-server
MODULE    := github.com/jukie/rlqs
BUILD_DIR := bin
GO        := go
GOFLAGS   :=

.PHONY: all build test lint proto-gen clean fmt vet e2e e2e-up e2e-down

all: lint test build

build:
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/rlqs-server

test:
	$(GO) test ./... -race -count=1

lint:
	golangci-lint run ./...

proto-gen:
	@echo "Using pre-generated protos from go-control-plane; nothing to generate."

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BUILD_DIR)

e2e: e2e-up
	$(GO) test -tags=e2e -v -count=1 -timeout=5m ./test/e2e/ || ($(MAKE) e2e-down; exit 1)
	$(MAKE) e2e-down

e2e-up:
	docker compose -f test/e2e/testdata/docker-compose.yaml up -d --build --wait

e2e-down:
	docker compose -f test/e2e/testdata/docker-compose.yaml down -v
