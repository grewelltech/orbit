BIN     := bin/orbit
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test integration vet fmt tidy gen clean install install-service upgrade-service uninstall-service ui ui-dev ui-clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/orbit

# ── dashboard ────────────────────────────────────────────────────────────────
# Built assets live in internal/webui/dist and are committed, so `go build`
# and `go install` work without a Node toolchain. Rebuild after changing web/.
UI_SRC := $(shell find web/src web/index.html web/package.json web/vite.config.ts -type f 2>/dev/null)

ui: internal/webui/dist/index.html

internal/webui/dist/index.html: $(UI_SRC) web/package-lock.json
	cd web && npm ci --no-fund --no-audit
	cd web && npm run build

# Vite dev server with HMR, proxying API calls to a running `orbit serve`.
# Override the target with ORBIT_API=host:port.
ui-dev:
	cd web && npm install --no-fund --no-audit && npm run dev

ui-clean:
	rm -rf internal/webui/dist web/node_modules

# Install the binary to $(PREFIX)/bin (default /usr/local).
PREFIX ?= /usr/local
install: build
	install -Dm755 $(BIN) $(DESTDIR)$(PREFIX)/bin/orbit

# Manage orbit as a systemd service (scripts/orbit.sh — install/upgrade/uninstall
# in one script). These need root: run with sudo.
install-service:
	./scripts/orbit.sh install
upgrade-service:
	./scripts/orbit.sh upgrade
uninstall-service:
	./scripts/orbit.sh uninstall

# The race detector is on by default: several concurrency invariants (live
# load stats read while the run's worker pool writes them, per-session state
# read by API handlers while a handover mutates it) are enforced only by it,
# and the tests covering them are assertion-free without it.
test:
	go test -race ./...

# Requires live-core reachability (integration-CI tier, DESIGN §6).
# Override the AMF with ORBIT_AMF_N2=host:port.
integration:
	go test -race -tags=integration -count=1 -v ./...

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
