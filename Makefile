GOLANGCI_VERSION ?= v2.15.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ungarscool1/goblins/internal/app.Version=$(VERSION)

.PHONY: all build test race integration lint fmt image mock tidy

all: lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/goblins ./cmd/goblins

test:
	go test ./...

race:
	go test -race ./...

integration:
	go test -tags integration ./test/integration/...

lint:
	go vet ./...
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

image:
	docker build --build-arg VERSION=$(VERSION) -t goblins:$(VERSION) -t goblins:dev .

mock: build
	./bin/goblins serve --mock
