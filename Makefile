# harness — local dev Makefile.
#
# Mirrors .gitea/workflows/ci.yaml so what passes locally passes in CI.
# Common flows:
#
#   make              # build + check + test (the "did I break it" target)
#   make run          # build and launch the TUI against the local daemon
#   make daemon       # run the daemon in the foreground
#   make tidy         # go mod tidy + gofumpt
#   make lint         # static checks only (fmt, vet, go.mod/go.sum tidiness)
#   make check        # just the CI gates (lint, test, race)
#
# Override the binary path / version via:
#   make VERSION=v0.1.0
#   make BIN_DIR=/usr/local/bin install

# Private module host: agent-trace lives on gitea.stump.rocks, not
# proxy.golang.org, so go mod download needs GOPRIVATE in every environment
# (local and CI). Exporting it here means CI's `make check` inherits it
# without a workflow change (issue #87 / #94).
export GOPRIVATE = gitea.stump.rocks/*

GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BIN_DIR   ?= $(HOME)/.local/bin
PKG       := gitea.stump.rocks/stump.wtf/harness
LDFLAGS   := -X $(PKG)/internal/buildinfo.Version=$(VERSION)
GOFLAGS   := -trimpath -ldflags "$(LDFLAGS)"
BIN       := harness

.PHONY: all build check lint fmt vet tidy-check test race tidy clean run daemon install version release-snapshot release-check

# The default "did I break it" loop.
all: build check

# Build the harness binary into ./bin/ with version metadata baked in.
build:
	$(GO) build $(GOFLAGS) -o bin/$(BIN) ./cmd/harness

# Full CI gate: the static checks, tests, and the race detector.
check: lint test race

# Static checks only (the uniform `make lint` entry point).
lint: fmt vet tidy-check

fmt:
	@unformatted=$$(gofmt -l $$(git ls-files '*.go' | grep -v '^vendor/')); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt would reformat:"; echo "$$unformatted"; exit 1; \
	fi

vet:
	$(GO) vet ./...

# go.mod / go.sum must already be tidy. Without this gate a dependency bump
# leaves the superseded version's `h1:` and `/go.mod` lines behind in go.sum
# (PR #173 shipped two stale charm.land/ssh v0.4.2 lines that way), and the
# next person to run `make tidy` picks up that churn in an unrelated PR.
# `-diff` needs Go 1.23+; the module requires 1.25.
tidy-check:
	@$(GO) mod tidy -diff || { \
		echo "go.mod/go.sum are not tidy — run 'make tidy' and commit the result"; exit 1; \
	}

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

# Dry-run the release locally: cross-compiles every target in .goreleaser.yaml
# into dist/ without a tag and without publishing anything. Run this before
# pushing a v* tag — the release workflow has no other rehearsal.
release-snapshot:
	goreleaser release --snapshot --clean

# Validate .goreleaser.yaml without building.
release-check:
	goreleaser check

clean:
	rm -rf bin/ dist/
