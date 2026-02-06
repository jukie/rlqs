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

e2e-up:
	docker compose -f e2e/docker-compose.yaml up -d --build --wait

e2e-down:
	docker compose -f e2e/docker-compose.yaml down -v --remove-orphans

e2e: e2e-up
	E2E_SKIP_COMPOSE=1 E2E_DIR=e2e $(GO) test -tags=e2e -v -timeout=5m ./e2e/
	$(MAKE) e2e-down

clean:
	rm -rf $(BUILD_DIR)
