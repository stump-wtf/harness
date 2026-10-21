---
status: proposed
date: 2026-10-18
decision-makers: [joestump]
extends: [ADR-0023, ADR-0044]
related: [ADR-0011, ADR-0038, ADR-0006]
---

# ADR-0047: Package-relative argv — a `command` harness locates its bundled script through `{{package.dir}}`, a path the daemon derives from the harness's own pin

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#957 by the
> m lane; no maintainer decision exists yet. The issue is an imported outside
> report, and the only upstream reply was the mirror-intake ack.

## Context and Problem Statement

A package whose harness is `harness = "command"` can ship a script and cannot
run it. Install copies every bundled file into the pin directory
(`$XDG_STATE_HOME/harness/agents/installed/<stable>/<package>/<sha>/`), and the
report's repro — `argv = ["bash", "watch.sh"]` with `watch.sh` beside
`package.toml` — starts and exits 127: the file is on disk, and nothing resolves
the name against the directory holding it.

Every other path a manifest names is anchored on the package (SPEC-0026 REQ-3:
`prompt_file`, `system_prompt_file`, `mcp_config`, `skill_paths`), so this is an
inconsistency, not a policy. The other three doors are deliberately shut:
`workdir` is an operator key, `${…}` is ADR-0038's secret-reference grammar and
is refused anywhere in a manifest, and no run-context variable names the pin
directory (`HARNESS_RUN_ID`, `_TRIGGER`, `_SOURCE`, `_EVENT_FILE` are all there
is). The only workaround is to hard-code the content SHA into `argv`, which
breaks on every upgrade — the one thing ADR-0044's pins exist to prevent.

`docs/guides/agent-packages.md` already tells readers that paths a package names
resolve relative to it. For `argv` that sentence is false. Either the guide or
the code has to move; this ADR moves the code, because the guide states the rule
the rest of the schema already follows.

## Decision

Add one path to the SPEC-0017 REQ-7 template context, daemon tier:

| Path | Tier | Present when |
| --- | --- | --- |
| `package.dir` | daemon | The harness is defined by an installed package pin (it has a `source`) |

A `command` package then ships its script and names it:

```toml
[harness]
harness = "command"
argv = ["bash", "{{package.dir}}/watch.sh"]
```

At spawn, the daemon renders `package.dir` by deriving the pin directory from
the harness's own `source` value (`agentpkg.InstalledPackageDir`), the same way
the skill merge already derives the bundle directory from it. The manifest never
supplies the string.

Details that were choices:

* **The value is derived, never read.** It comes from the harness table's
  `source` and the pin store's layout, so a manifest cannot point a render at
  anything that is not its own installed files. The REQ-3 rule is untouched: an
  absolute path, a `~` path or a `..` climb in a manifest path value still
  fails the load.
* **Load refuses what can never render.** A required `{{package.dir}}` on a
  harness with no `source` fails validation, in the same shape as the existing
  rule for a required `{{run.id}}` on a resident harness. The optional form
  `{{package.dir?}}` renders empty, so one table can serve both a packaged and
  a hand-written harness.
* **Only argv[1:], like every other template path.** `argv[0]` stays the
  executable, resolved by PATH lookup or against the workdir; a package's script
  is an argument to an interpreter that is on PATH (`bash`, `python3`), which is
  what the report's own repro does.
* **Absence is absence.** A harness with no pin has no `package.dir` in its
  render context, so a required reference fails the render and nothing is exec'd
  with a literal placeholder in its argv.

## Alternatives rejected

* **Resolve every relative `argv` entry against the pin.** Silent and wrong half
  the time: `-c`, `--file`, and any value that is not a path would be rewritten
  into a nonexistent one, and the meaning of every existing package's `argv`
  would change underneath it.
* **Export `HARNESS_PACKAGE_DIR` instead.** It does not fix the report. A
  manifest cannot name it in `argv`, because `${` is the secret-reference
  grammar and is rejected in every manifest string, so the package author would
  still need an operator-written override; and a path a script discovers at
  runtime is invisible to `describe`, to a diff, and to whoever reads the config.
  It remains available as an addition later if someone needs it for a
  non-argv purpose.
* **Let a package set `workdir`.** SPEC-0026 REQ-3 forbids it, and correctly:
  the working directory belongs to the machine and the operator, not to the
  package.

## Consequences

* A bundled script works with no SHA in any config, and survives an upgrade
  untouched.
* The guide's "paths stay inside the package" sentence becomes true for `argv`
  as well, and the guide now says how to name one.
* Nothing about the "data, never code" boundary changes here, and this ADR does
  not claim otherwise: a bundled script is listed by `agent info` and the
  install confirmation, because install copies the whole package tree into the
  pin, and it is marked `not scanned` there, since the scan reads only the
  manifest, `.md`/`.txt` and the named prompts. That visibility is
  stump.wtf/harness#956; what it does not do is certify the script, so reading
  it before you run the package stays the operator's job.
* `package.dir` joins a closed allow set. Any future package-scoped path is a
  decision of the same kind, not a new key on the manifest.
