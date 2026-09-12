# Harness

`systemctl` for your agents — a Go + Charmbracelet client-server TUI for
supervising, attaching to, and hopping between long-running harnesses (agent
CLIs, REPLs, watchers). Successor to `zsh-harnessd`. See `README.md`.

- Origin of truth: https://gitea.stump.rocks/stump.wtf/harness (Gitea). GitHub
  (https://github.com/stump-wtf/harness) is a read-only push mirror — issues,
  PRs, and pushes go to Gitea.
- The daemon (`harness daemon`) is deliberately agnostic about what runs inside a
  harness. Keep it that way; agent-awareness bolts on later as a detector.
- Visual direction lives in `docs/design/` — calm ops cockpit, state legibility
  over decoration, the "hop" between harnesses is the signature interaction.

## Verification — check the property, not a proxy

A check that passes because something *correlated* with the truth moved is
worse than no check: it reports success, and stops anyone looking further.

Before reporting something verified, ask what the check would do if the change
had silently not happened. If the answer is "pass", it is measuring a proxy.

| Proxy | What it actually proves | The property |
|---|---|---|
| `harness --version` on a box | a binary changed | `strings <binary> \| grep -c <symbol the change adds or removes>` |
| a deploy script's `exit 0` | the last stage of a pipe succeeded | print the after-state and diff it against the before |
| the rendered `harness.toml` | what the *next* spawn will use | the running process's argv — it is fixed at spawn |
| a watcher exiting 0 | its loop ended | the run's own `conclusion` field |
| a count (bytes, matches, rows) | something moved | the content, and whether the count could ever be non-zero |

Traps in this repo specifically, each of which has already cost a wrong
"verified":

- **Deploy.** A version string advances whenever the binary is rebuilt, so it
  cannot tell you the payload arrived. Grep the deployed binary for a symbol
  the change adds or removes.
- **Config vs process.** The daemon builds a harness's argv at spawn, so a
  correct `harness.toml` says nothing about a harness that is already running;
  it keeps its old argv until it restarts. Read the process, not the file.
- **CI.** `GET /actions/runs` (the list) requires auth even on this public
  repo and returns null anonymously, while `GET /actions/runs/<id>` does not —
  so an anonymous list poll is indistinguishable from "no CI ran". Use
  `$GITEA_TOKEN`, and read `conclusion`, never an exit code.
- **Tests.** A test that builds its own fixture where production builds the
  real thing passes against broken code too. `TestDaemonManagerOptionsEnableGiveUp`
  exists because every give-up test constructed its own `Policy`, so none of
  them covered the wiring `cmd/harness/daemon.go` performs (#315).
- **A zero.** "No matches" and "never fired" read identically whether a rule is
  correct-and-quiet or never exercised at all. Show the check can fire before
  trusting its silence.

When you report a result, say which property you checked.

## Architecture Context

This project uses the [SDD plugin](https://github.com/joestump/claude-plugin-sdd) for architecture governance.

- Architecture Decision Records are in `docs/adrs/`
- Specifications are in `docs/openspec/specs/`

Run `/sdd:prime [topic]` at the start of a session to load relevant ADRs and specs into context.

### SDD Configuration

#### Tracker

- Type: Gitea
- Owner: stump.wtf
- Repo: harness
- Host: https://gitea.stump.rocks

#### Branch Conventions

- Prefix: feature
- Epic Prefix: epic

#### PR Conventions

- Close Keyword: Closes
- Ref Keyword: Part of
- Include Spec Reference: yes

### Governing Comments

When implementing code governed by ADRs or specs, leave comments referencing the governing artifacts:

```
// Governing: ADR-0003 (native multiplexer, tmux as backend), SPEC-0002 REQ "Backpressure"
```
