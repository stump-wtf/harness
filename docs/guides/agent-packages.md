---
title: "Install shared agents from a stable"
sidebar_label: "Shared agents (stables)"
sidebar_position: 8.5
---

# Install shared agents from a stable

A **stable** is a git repository of agent **packages**, and `harness agent`
installs from it the way `brew tap` and `brew install` work. A package is
someone else's harness definition: an adapter, the values it needs, and
usually a prompt. Installing one writes a harness table you then run like any
other.

This guide installs the generic PR reviewer from the
[stump-wtf stable](https://github.com/stump-wtf/harness-stable), wires it to a
schedule, and covers upgrades, removal, and writing a package of your own. The
design is ADR-0044 and the contract is SPEC-0026.

## What a package can and cannot do

A package carries a `package.toml` and the files it names. Its `[harness]`
table may only select an adapter and supply values:

- **Allowed:** `harness`, `args`, `argv`, `model`, `auto_accept`,
  `max_turns`, `quiet`, a single prompt source (`prompt`, `prompt_file`,
  `prompt_template` or `prompt_template_file`), `system_prompt_file`,
  `mcp_config`, `allowed_tools`, and `skill_paths`.
- **Forbidden** (any of them fails the install): `schedule`, `triggers`,
  `enabled`, `restart`, `restart_delay`, `operating_hours`, `workdir`,
  `env_file` and `secrets_env`. So is any string containing `${`, and any
  other table.
- **Paths stay inside the package.** A path a package names must be relative
  to it. An absolute path, a `~` path, or one that climbs out with `..` is
  refused.

So installing a package never makes anything run on its own, never reads your
secrets, and never points an agent at a file on your machine. When it runs,
whom it runs as, and what credentials it sees stay on **your** harness table.

## 1. Trust a stable

```sh
harness agent stable add stump-wtf https://github.com/stump-wtf/harness-stable.git
```

This clones the remote into `$XDG_STATE_HOME/harness/agents/stables/stump-wtf/`
and only then writes a `[stable.stump-wtf]` table to your **global**
`harness.toml`. If the clone fails, no table is written. A project file can
never add a stable.

Nothing else fetches. `search`, `info`, `install` and `upgrade` read the local
clone. `harness agent stable update [NAME]` is the one command that fetches,
and it fast-forwards only.

```sh
harness agent stable list
harness agent search reviewer           # name or description, case-insensitive
harness agent info stump-wtf/pr-reviewer
```

`info` prints the manifest, the itemised `[requests]`, the content-scan
findings and the bundled files, without installing anything.

It also prints the package's SPDX `license`. If the package declares none,
`info` prints `none declared` and adds a `package.no-license` low finding.
The finding is shown but never blocks, so you can still install the package
knowingly.

## 2. Install it

```sh
harness agent install stump-wtf/pr-reviewer
```

Install does the following, in order:

1. **Pins.** It resolves the package to an exact commit and copies it into
   an immutable, content-addressed store under
   `$XDG_STATE_HOME/harness/agents/installed/`.
2. **Scans.** It runs a fixed content scan over the manifest, every `.md` and
   `.txt` file, and every prompt file the manifest names. A `high` finding
   blocks the install. `--force-unsafe` overrides it, and asks you to retype
   the package name.
3. **Confirms.** It shows the manifest, the requests and the findings, then
   asks. Without a terminal it refuses unless you pass `--yes`, and `--yes`
   never clears a `high` finding or a `mcp_allow = ["write"]` request.
4. **Binds.** It writes `source = "stump-wtf/pr-reviewer@<sha>"` onto a
   `[harness.pr-reviewer]` table. `--as NAME` picks another table name.

A clean scan is a tripwire, not a guarantee. Read the bundled prompt before
you run it.

## 3. Wire it up

The package ships the instruction; your table says when to run it and with
what. Add a schedule (or `triggers`) and a working directory next to the
`source` line. Any key you set on the table overrides the package's:

```toml
[harness.pr-reviewer]
source   = "stump-wtf/pr-reviewer@0123456789abcdef0123456789abcdef01234567"
schedule = "CRON_TZ=UTC 30 9 * * *"
workdir  = "~/sweeps/pr-reviewer"
# Run it on another adapter or model than the package picked:
# harness = "crush"
# model   = "litellm/Qwen3.8-27B"
```

- **Prompt overrides.** The four prompt keys override as one group. If your
  table sets any of them, the package's prompt is dropped whole, whatever
  form either side uses.
- **Load.** `harness reload` picks the table up. `harness describe
  pr-reviewer` marks each value the package supplied with `(package)`.

## 4. Upgrade

```sh
harness agent stable update stump-wtf
harness agent list                       # the NEWER column shows what moved
harness agent upgrade stump-wtf/pr-reviewer
```

Upgrade re-scans the new commit and shows you three things:

- the manifest diff, where a `package.license` change is flagged as a
  change to the package's terms;
- any **new** scan finding, flagged as new;
- every change to the harness's effective values.

Review those effective-value changes one by one:

- **A value the package moves.** If the package supplied it, you choose:
  keep the old value (pinned onto your table) or take the new one.
- **A value you overrode.** If the package moved a value your table
  overrides, you choose whether to drop your override.
- **No silent apply.** `--yes` and unattended runs refuse to apply such a
  diff. They name the changes instead.

## 5. Remove

```sh
harness agent uninstall pr-reviewer     # removes the table; the pin stays
harness agent prune                      # removes pins nothing references
harness agent stable remove stump-wtf    # lists harnesses still sourced from it
```

`prune` considers only the global `harness.toml`. A pin that only a project
file uses is not protected, and that project's next `harness up` reports it
missing.

## Writing a package

A stable is a repository with `packages/<name>/` directories:

```text
packages/pr-reviewer/
  package.toml
  prompts/review.md
  skills/<slug>/SKILL.md     # optional; requires [requests] skill_paths = true
```

```text
[package]
name        = "pr-reviewer"            # must match the directory
version     = "0.1.0"
description = "Reviews PRs that request your review"
homepage    = "https://github.com/you/your-stable"
license     = "MIT"                    # SPDX expression, e.g. "MIT OR Apache-2.0"

[harness]
harness     = "claude-code"            # an existing adapter, never a new one
prompt_file = "prompts/review.md"      # relative to package.toml
auto_accept = true
max_turns   = 80

[requests]                              # shown verbatim at install
network = true
# mcp_allow   = ["read"]               # "write" makes the installer retype the name
# skill_paths = true
```

`license` is an SPDX license expression. It may use only `AND`, `OR`,
`WITH` and parentheses. Every ID must be on the
[SPDX License List](https://spdx.org/licenses/), at the version this
Harness release vendors, or be a custom `LicenseRef-<name>`. An unknown ID
fails the manifest load, and the error names that ID. A package without a
license still installs, but it carries the `package.no-license` low
finding.

Keep the manifest to keys every adapter accepts if you want operators to
override `harness`. `system_prompt_file`, `mcp_config` and `allowed_tools`
are Claude Code one-shot keys.

[stump-wtf/harness-stable](https://github.com/stump-wtf/harness-stable) is a
working example. Its `make test` applies the manifest rules and the
high-severity scan patterns in CI, so a package Harness would refuse fails
there first.

## See also

- [CLI reference: Agent packages](../usage/cli#agent-packages)
- [Configuration: Agent packages and stables](../usage/configuration#agent-packages-and-stables)
