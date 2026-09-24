---
title: "Install"
sidebar_position: 1
---

# Install

:::tip Paste this to your agent

```text
Read https://stump-wtf.github.io/harness/llms.txt and
https://stump-wtf.github.io/harness/guides/install. Install Harness on this
machine with the method that page recommends for my OS, then run `harness
doctor` and show me its output. Done means doctor reports the config and
daemon checks as ok, not that the install command exited 0.
```

:::

Harness is one Go binary, `harness`, that is both the daemon and the client.
Pick one of the two methods below, then verify with `harness doctor`.

## Homebrew (macOS and Linux)

```sh
brew install stump-wtf/tap/harness
```

This installs the latest tagged release and reports its version. Installing by
the fully qualified name is a one-liner and the only form that works out of the
box: since [Homebrew 6.0.0](https://brew.sh/2026/06/11/homebrew-6.0.0/)
non-official taps require explicit trust, and a fully qualified name trusts just
that one formula rather than the tap and everything it may ever contain. The
older `brew tap stump-wtf/tap` + `brew install harness` form now fails, because
the short name needs the tap loaded and the tap is untrusted.

The formula compiles from source, so Homebrew pulls in Go as a build dependency
and there is no Gatekeeper prompt on macOS.

To update later:

```sh
brew update && brew upgrade stump-wtf/tap/harness
```

### Opting in to the latest `main`

`--HEAD` builds the tip of `main` instead of the tagged release, and is worth
using only when a guide needs a feature that has not shipped yet:

```sh
brew install --HEAD stump-wtf/tap/harness
```

A `--HEAD` build reports the version it was built from, so it is no longer
indistinguishable from an unreleased build. To update one:

```sh
brew upgrade --fetch-HEAD stump-wtf/tap/harness
```

### Running the daemon as a service

The formula ships a `brew services` definition, which is the easiest way to keep
the daemon running without hand-authoring a launchd plist:

```sh
brew services start harness
```

`brew services` starts the daemon in your GUI login session, so agents it
launches inherit your login Keychain and a `claude-code` harness needs no
`env_file`. One caveat: a service does not read your shell profile, and agent
CLIs are looked up on the **daemon's** `PATH`, so an agent installed outside
Homebrew (`~/.local/bin`, an npm or bun global prefix, mise or asdf shims) will
not be found and the harness fails at spawn. See
[the service guide](./run-as-a-service#agent-clis-resolve-on-the-daemons-path)
for how to fix that on each platform. On Linux, use the
`systemd --user` unit instead; the same guide covers it.

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

### Or install by module path

Since v0.4.0 the module declares itself as `github.com/stump-wtf/harness`, so
the one-liner works:

```sh
go install github.com/stump-wtf/harness/cmd/harness@latest
```

:::note Versions before v0.4.0

Up to and including v0.3.0 the module declared a private path, so installing by
module path failed with a `version constraints conflict` — the GitHub mirror is
a byte copy, and its `go.mod` carried the original path. Pin `@v0.4.0` or later,
or clone and build as above.

Note that `go install` alone reports `harness dev` for its version: the version
string is injected at link time, which `go install` does not do. Use `make
install`, a release binary, or the Homebrew formula if you want
`harness --version` to report a real version.

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
