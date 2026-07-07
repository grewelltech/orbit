BIN     := bin/orbit
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test integration vet fmt tidy gen clean install install-service

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/orbit

# Install the binary to $(PREFIX)/bin (default /usr/local).
PREFIX ?= /usr/local
install: build
	install -Dm755 $(BIN) $(DESTDIR)$(PREFIX)/bin/orbit

# Install and enable orbit as a systemd service (see scripts/install-service.sh).
install-service:
	./scripts/install-service.sh

test:
	go test ./...

# Requires live-core reachability (integration-CI tier, DESIGN §6).
# Override the AMF with ORBIT_AMF_N2=host:port.
integration:
	go test -tags=integration -count=1 -v ./...

vet:
	go vet ./...
	go vet -tags=integration ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

# Regenerates gen/ from proto/ (requires buf, protoc-gen-go,
# protoc-gen-connect-go on PATH; `go install` each or see CI).
gen:
	buf lint
	buf generate

clean:
	rm -rf bin
