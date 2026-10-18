BIN     := bin/kcond
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test lint fmt clean

build:
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/kcond

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt: files need formatting"; exit 1; }
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin
