---
title: "Install"
sidebar_position: 1
---

# Install

Harness is one Go binary, `harness`, that is both the daemon and the client.
Pick one of the two methods below, then verify with `harness doctor`.

## Homebrew (macOS and Linux)

```sh
brew tap stump-wtf/tap
brew install --HEAD harness
```

`--HEAD` builds the latest `main` from the
[GitHub repository](https://github.com/stump-wtf/harness). The tap's tagged
formula can lag behind, and these guides use features that only exist on `main`.
The formula compiles from source, so Homebrew pulls in Go as a build dependency
and there is no Gatekeeper prompt on macOS.

To update a `--HEAD` install later:

```sh
brew upgrade --fetch-HEAD harness
```

## From source

You need Go 1.26 or newer. An older Go (1.21+) downloads the right toolchain
automatically the first time you build.

```sh
git clone https://github.com/stump-wtf/harness.git
cd harness
go install ./cmd/harness
```

That puts `harness` in `$(go env GOPATH)/bin`, usually `~/go/bin`; make sure
that is on your `PATH`. Alternatively, `make install` builds with version
metadata baked in and installs to `~/.local/bin`.

:::caution Use the clone, not `go install …@latest`

The module's import path is not its GitHub URL, so
`go install github.com/stump-wtf/harness/cmd/harness@latest` fails. Clone and
build from the checkout as shown above. Every dependency resolves from the
public Go module proxy.

:::

## Verify

```sh
harness --version
harness doctor
```

On a fresh install, `doctor` fails two checks, and both are expected. It prints
a hint for each, plus a `SETTING` table showing where every setting came from:

```
CHECK         STATUS      DETAIL
config        error       not found at /home/you/.config/harness/harness.toml
                          → create one (see `harness daemon -h`) or pass --config PATH
daemon        error       unreachable at /run/user/1000/harness.sock
                          → start it with: harness daemon
summary       0 passed · 0 warning(s) · 2 failed

SETTING       SOURCE      VALUE
socket        default     /run/user/1000/harness.sock
config        default     /home/you/.config/harness/harness.toml
…
```

The binary works. There is simply no config and no daemon yet. The next two
guides add both, after which every check should read `ok`.

To take it for a spin before setting up a service, run the daemon in one
terminal and the dashboard in another:

```sh
harness daemon          # terminal 1: the supervisor, in the foreground
harness                 # terminal 2: the dashboard (q to quit)
```

## Where things live

| What | Default path |
|------|--------------|
| Config | `~/.config/harness/harness.toml` (honors `$XDG_CONFIG_HOME`) |
| State, intent and run history | `~/.local/state/harness/state.json` (honors `$XDG_STATE_HOME`) |
| Per-harness logs | `~/.local/state/harness/logs/NAME.log` |
| Per-run logs of scheduled harnesses | `~/.local/state/harness/jobs/NAME/RUN_ID.log` |
| Control socket (Linux) | `$XDG_RUNTIME_DIR/harness.sock`, usually `/run/user/UID/harness.sock` |
| Control socket (macOS, no `$XDG_RUNTIME_DIR`) | `~/.local/state/harness/harness.sock` |

The client finds the daemon through that socket, so both must agree on it. The
[service guide](./run-as-a-service#the-client-has-to-find-the-socket) covers the
one case where they don't.

Next: [run the daemon as a service](./run-as-a-service).
