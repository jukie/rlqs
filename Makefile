BINARY    := rlqs-server
MODULE    := github.com/jukie/rlqs
BUILD_DIR := bin
GO        := go
GOFLAGS   :=

.PHONY: all build test lint proto-gen clean fmt vet

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
