BINARY_NAME := code-review-graph
BUILD_DIR := bin
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: all build clean test test-race lint vet fmt run help

all: build

build:
	CGO_ENABLED=1 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/code-review-graph

build-mcp:
	CGO_ENABLED=1 go build $(LDFLAGS) -o $(BUILD_DIR)/mcp-server ./cmd/mcp-server

clean:
	rm -rf $(BUILD_DIR)
	go clean -testcache

test:
	CGO_ENABLED=1 go test ./... -v

test-race:
	CGO_ENABLED=1 go test -race ./... -v

lint:
	golangci-lint run ./...

vet:
	go vet ./...

fmt:
	gofmt -w .
	goimports -w .

run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

help:
	@echo "Targets:"
	@echo "  build       - Build the CLI binary"
	@echo "  build-mcp   - Build the MCP server binary"
	@echo "  clean       - Remove build artifacts"
	@echo "  test        - Run all tests"
	@echo "  test-race   - Run tests with race detector"
	@echo "  lint        - Run golangci-lint"
	@echo "  vet         - Run go vet"
	@echo "  fmt         - Format code"
	@echo "  run         - Build and run"
