# Harness

`systemctl` for your agents — a Go + Charmbracelet client-server TUI for
supervising, attaching to, and hopping between long-running harnesses (agent
CLIs, REPLs, watchers). Successor to `zsh-harnessd`. See `README.md`.

- Origin of truth: https://gitea.stump.rocks/stump.wtf/harness (Gitea). GitHub
  (https://github.com/stump-wtf/harness) is a read-only push mirror — issues,
  PRs, and pushes go to Gitea.
- The daemon (`harnessd`) is deliberately agnostic about what runs inside a
  harness. Keep it that way; agent-awareness bolts on later as a detector.
- Visual direction lives in `docs/design/` — calm ops cockpit, state legibility
  over decoration, the "hop" between harnesses is the signature interaction.

## Architecture Context

This project uses the [SDD plugin](https://github.com/joestump/claude-plugin-sdd) for architecture governance.

- Architecture Decision Records are in `docs/adrs/`
- Specifications are in `docs/openspec/specs/`

Run `/sdd:prime [topic]` at the start of a session to load relevant ADRs and specs into context.

### Governing Comments

When implementing code governed by ADRs or specs, leave comments referencing the governing artifacts:

```
// Governing: ADR-0003 (native multiplexer, tmux as backend), SPEC-0002 REQ "Backpressure"
```
