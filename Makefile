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
#   make scan         # the secret scan (gitleaks) the pipeline runs first
#   make check        # just the CI gates (scan, lint, test, race, fuzz)
#   make fuzz         # run the fuzz targets for a bounded time (FUZZTIME)
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
PKG       := github.com/stump-wtf/harness
# BUILD_FLAGS, deliberately not GOFLAGS: a file-scope `GOFLAGS :=` is passed
# down to every recipe whenever the caller exports GOFLAGS, and go aborts
# parsing the leaked -ldflags before a single test runs (issue #1024). make
# never exports a variable assigned only in the Makefile, so the build flags
# stay on the build recipe's command line and the go gates see only the
# caller's own GOFLAGS.
LDFLAGS   := -X $(PKG)/internal/buildinfo.Version=$(VERSION)
BUILD_FLAGS := -trimpath -ldflags "$(LDFLAGS)"
BIN       := harness

.PHONY: all build check lint fmt vet tidy-check goflags-check test race fuzz scan tidy clean run daemon install version release-snapshot release-check

# The default "did I break it" loop.
all: build check

# Build the harness binary into ./bin/ with version metadata baked in.
build:
	$(GO) build $(BUILD_FLAGS) -o bin/$(BIN) ./cmd/harness

# Full CI gate: the secret scan first — a leaked credential must not wait
# behind a compile, the same ordering as the pipeline — then the static
# checks, tests, the race detector, and a bounded fuzz run. CI's pipeline
# calls `make check`, so adding `fuzz` here is what puts it in CI.
check: scan lint test race fuzz

# The secret scan: the same gitleaks call the pipeline's first stage runs
# (stump.wtf/ci gitleaks.yaml) — full history, the repo's .gitleaksignore,
# redacted output. It exists so a literal the scan reads as a credential is
# caught locally, where the finding is on screen, instead of as a PR that
# dies in five seconds reading like a CI outage: a failed `uses:` callee is
# recorded on the no-step wrapper job as "job not started" (issue #1015).
# Scans history, so run it from a branch that carries the branch's history.
# Locally a missing gitleaks fails the gate with an install hint; the `git`
# subcommand needs gitleaks v8.19+, and an older one fails the gate with
# its own unknown-command error, which is loud enough. On a runner
# (RUNNER_OS is set in the job container) it warns loudly and
# skips, because the shared runner image ships no gitleaks and the
# pipeline's dedicated secrets stage already ran the same scan earlier in
# the run.
scan:
	@if command -v gitleaks >/dev/null 2>&1; then \
		gitleaks git . --no-banner --verbose --redact --gitleaks-ignore-path .gitleaksignore; \
	elif [ -n "$$RUNNER_OS" ]; then \
		echo "scan: WARNING: gitleaks is not installed in this runner image, so the check skips the scan here; the pipeline's dedicated secrets stage (stump.wtf/ci gitleaks.yaml) runs the same scan on this run"; \
	else \
		echo "scan: gitleaks is not installed; the local gate is incomplete without it. Install gitleaks v8.19 or newer (https://github.com/gitleaks/gitleaks) and re-run"; \
		exit 1; \
	fi

# Static checks only (the uniform `make lint` entry point).
lint: fmt vet tidy-check

fmt:
	@unformatted=$$(gofmt -l $$(git ls-files '*.go' | grep -v '^vendor/')); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt would reformat:"; echo "$$unformatted"; exit 1; \
	fi

vet: goflags-check
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

# Guard for the go gate recipes (issue #1024): when the caller exports
# GOFLAGS, a file-scope `GOFLAGS :=` assignment rides into every recipe's
# environment, and go dies parsing the leaked -ldflags before a single test
# runs. The build flags therefore live in BUILD_FLAGS (which make never
# exports); here we reject any -ldflags in the gate's GOFLAGS, so a leak
# turns the gate red with a message instead of an opaque go parse error.
goflags-check:
	@case "$$GOFLAGS" in \
	*-ldflags*) echo "goflags-check: -ldflags must not reach the go gate (GOFLAGS=$$GOFLAGS)"; exit 1;; \
	esac

test: goflags-check
	$(GO) test ./...

# -race needs CGO enabled.
race: goflags-check
	CGO_ENABLED=1 $(GO) test -race ./...

# Fuzz targets CI runs, as <package>:<target>. `go test -fuzz` takes one
# target in one package per invocation, hence the loop. `make test` already
# runs every target's seed corpus; this is the part that searches for new
# inputs. FUZZTIME bounds each target so the gate stays predictable; raise it
# locally (`make fuzz FUZZTIME=5m`) to dig. A failing input is written under
# the package's testdata/fuzz/ — commit it with the fix as a regression seed.
#
# A failed target is retried once. The fuzz coordinator gives its workers a
# short stop window after FUZZTIME, and on a contended runner a worker can
# miss it, failing a clean run with "context deadline exceeded" and no
# failing input — seen on the merge train for PR #962 (run 18263) where the
# same tree was green on its PR run and on main. A real crasher does not get
# past the retry: its input is already in testdata/fuzz/, so the second run
# fails at the seed corpus and the gate still goes red, at the cost of a few
# seconds per target.
#
# internal/tmpl FuzzParse: SPEC-0017 REQ-6, issue #501.
# internal/attach FuzzRing: the byte-bounded scrollback ring against a flat
# model (ADR-0007), https://github.com/stump-wtf/harness/issues/18.
# internal/modelerr FuzzResetAfter: an untrusted error note never yields a
# reset outside (now, now+8d] (SPEC-0021 REQ-12, issue #473).
# internal/budget FuzzDay: the budget day bounds every instant across DST and
# odd zones (SPEC-0021 REQ-3, stump.wtf/harness#470).
FUZZTIME    ?= 30s
FUZZ_TARGETS := ./internal/tmpl:FuzzParse ./internal/attach:FuzzRing ./internal/modelerr:FuzzResetAfter ./internal/budget:FuzzDay

fuzz: goflags-check
	@set -e; for t in $(FUZZ_TARGETS); do \
		pkg=$${t%%:*}; fn=$${t##*:}; \
		echo "fuzz $$pkg $$fn ($(FUZZTIME))"; \
		if ! $(GO) test -run '^$$' -fuzz "^$$fn\$$" -fuzztime $(FUZZTIME) $$pkg; then \
			echo "fuzz $$pkg $$fn failed once; retrying (a stop-window timeout fails clean runs; a real crasher is re-found at the seed corpus and fails the retry too)"; \
			$(GO) test -run '^$$' -fuzz "^$$fn\$$" -fuzztime $(FUZZTIME) $$pkg; \
		fi; \
	done

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
