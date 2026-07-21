# harness — local dev Makefile.
#
# Mirrors .gitea/workflows/ci.yaml so what passes locally passes in CI.
# Common flows:
#
#   make              # build + check + test (the "did I break it" target)
#   make run          # build and launch the TUI against the local daemon
#   make daemon       # run the daemon in the foreground
#   make tidy         # go mod tidy + gofumpt
#   make check        # just the CI gates (fmt, vet, build, test, race)
#
# Override the binary path / version via:
#   make VERSION=v0.1.0
#   make BIN_DIR=/usr/local/bin install

GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BIN_DIR   ?= $(HOME)/.local/bin
PKG       := gitea.stump.rocks/stump.wtf/harness
LDFLAGS   := -X $(PKG)/internal/buildinfo.Version=$(VERSION)
GOFLAGS   := -trimpath -ldflags "$(LDFLAGS)"
BIN       := harness

.PHONY: all build check fmt vet test race tidy clean run daemon install version

# The default "did I break it" loop.
all: build check

# Build the harness binary into ./bin/ with version metadata baked in.
build:
	$(GO) build $(GOFLAGS) -o bin/$(BIN) ./cmd/harness

# Full CI gate: formatting, vet, build, tests, and the race detector.
check: fmt vet test race

fmt:
	@unformatted=$$(gofmt -l $$(git ls-files '*.go' | grep -v '^vendor/')); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt would reformat:"; echo "$$unformatted"; exit 1; \
	fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# -race needs CGO enabled.
race:
	CGO_ENABLED=1 $(GO) test -race ./...

# Apply formatting in place. Uses gofumpt if installed, falls back to gofmt.
tidy:
	@command -v gofumpt >/dev/null 2>&1 && gofumpt -w . || gofmt -w .
	$(GO) mod tidy

# Build and run the TUI against the default socket.
run: build
	./bin/$(BIN)

# Build and run the supervision daemon in the foreground.
daemon: build
	./bin/$(BIN) daemon

# Install the freshly-built binary into BIN_DIR (default ~/.local/bin).
install: build
	@mkdir -p $(BIN_DIR)
	install -m 0755 bin/$(BIN) $(BIN_DIR)/$(BIN)
	@echo "installed → $(BIN_DIR)/$(BIN)"

version:
	@echo $(VERSION)

clean:
	rm -rf bin/
