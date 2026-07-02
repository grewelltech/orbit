BIN     := bin/orbit
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet fmt tidy clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/orbit

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	rm -rf bin
