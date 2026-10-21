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
  refused. Every path key resolves against the package's own installed
  directory, and so does a bundled script named in `argv` — see
  [Running a script you bundle](#running-a-script-you-bundle).
- **Environment by name only.** A package can declare the environment
  variables it expects in `[[env]]` blocks, but never their values. You
  supply those in your harness table's `env_file` or the daemon's
  environment, and `harness doctor` tells you which are missing.
- **Suggests, never schedules.** Those forbidden keys are what a `[defaults]`
  table is for: a suggestion the installer offers and you choose, never a
  value a config load applies. `env_file`, `secrets_env` and `enabled` are
  not suggestible at all, so a package can neither point at a secret file
  nor switch itself on.

So installing a package never makes anything run on its own, never reads your
secrets, and never points an agent at a file on your machine. When it runs,
whom it runs as, and what credentials it sees stay on **your** harness table.

## 1. Trust a stable

```sh
harness stable add stump-wtf https://github.com/stump-wtf/harness-stable.git
```

This clones the remote into `$XDG_STATE_HOME/harness/agents/stables/stump-wtf/`
and only then writes a `[stable.stump-wtf]` table to your **global**
`harness.toml`. If the clone fails, no table is written. A project file can
never add a stable.

Nothing else fetches. `search`, `info`, `install` and `upgrade` read the local
clone. `harness stable update [NAME]` is the one command that fetches,
and it fast-forwards only.

```sh
harness stable list
harness agent search reviewer           # name or description, case-insensitive
harness agent info stump-wtf/pr-reviewer
```

`info` prints the manifest, the itemised `[requests]`, the environment
variables the package declares, the package's `README.md`, the content-scan
findings and every bundled file, each marked `scanned` or `not scanned`,
without installing anything.

Install copies the package's whole tree into the pin store, so that list is
everything you are about to have on disk, not only the files the scan opened.
The scan reads the manifest, every `.md` and `.txt` file, and every file the
manifest names as a prompt (`prompt_file`, `prompt_template_file`,
`system_prompt_file`) whatever its extension. Anything else, such as a script
a `command` harness runs through `{{package.dir}}`, is listed and marked
`not scanned`, with a note naming those files. The mark says nothing was
wrong with them: it says nobody read them, so they are the ones worth opening
yourself before you run the package.

The README is where a package says what you must provide that its manifest
cannot carry: environment variables, tokens, logins. Harness treats it as
untrusted text from the stable. It prints as plain text, with every terminal
control sequence removed: escape sequences, OSC 8 hyperlinks, and control
characters other than newline and tab. Nothing in it is ever run. The inline
render stops after 200 lines and ends with a footer saying where the full
file is. Pass `--readme-full` to `info`, `install` or `upgrade` to print the
whole README.

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
3. **Confirms.** It shows the manifest, the requests, the README, the
   findings and every file that will land in the store, each marked `scanned`
   or `not scanned`, then asks. Without a terminal it refuses unless you pass `--yes`, and `--yes`
   never clears a `high` finding or a `mcp_allow = ["write"]` request.
4. **Binds.** It writes `source = "stump-wtf/pr-reviewer@<sha>"` onto a
   `[harness.pr-reviewer]` table. `--as NAME` picks another table name.
5. **Offers its suggestions.** Every `[defaults]` key the package carries is
   offered once, in a fixed order: accept it, overwrite it with a value you
   type, or skip it. What you accept lands on your table as your own key, in
   the same write as the `source` line, and the output ends with one line per
   suggestion saying `written`, `skipped` and why, or `kept` because your
   table already set it. `--accept-defaults` accepts them all unattended;
   `--yes` accepts none and says so.
6. **Lists the environment.** When the package declares `[[env]]`
   variables, install ends with a ready-to-copy `env_file` skeleton: one
   `NAME=` line per variable with its description as a comment, and each
   optional one commented out. It holds names only; you fill in the values.

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
- **Environment.** Put the variables the package declares in an `env_file`
  on this table (a single file or a list), or in the daemon's environment.
  `harness doctor` adds an `agent_env` row per package-sourced harness. A
  missing required variable fails it, and a missing optional one warns with
  its description. `harness describe` shows each declared variable as `set
  (env_file)`, `set (environment)` or `unset`. Neither ever prints a value.

## 4. Upgrade

```sh
harness stable update stump-wtf
harness agent list                       # the NEWER column shows what moved
harness agent upgrade stump-wtf/pr-reviewer
```

Upgrade re-scans the new commit and shows you three things:

- the manifest diff, where a `package.license` change is flagged as a
  change to the package's terms, plus a `README.md` diff when the README
  changed between the two commits;
- any **new** scan finding, flagged as new;
- the candidate's full file list, each file marked `scanned` or
  `not scanned`, so a script the new version adds is visible even though
  nothing about it scans;
- every change to the harness's effective values.

Review those effective-value changes one by one:

- **A value the package moves.** If the package supplied it, you choose:
  keep the old value (pinned onto your table) or take the new one.
- **A value you overrode.** If the package moved a value your table
  overrides, you choose whether to drop your override.
- **No silent apply.** `--yes` and unattended runs refuse to apply such a
  diff. They name the changes instead.
- **A changed suggestion.** Upgrade also lists what the new commit's
  `[defaults]` added, changed or dropped, and offers only the added and
  changed keys, so one the package left alone is not asked about twice. A key
  your table already sets is never moved, even under `--accept-defaults`:
  the report shows it as `kept` with your value.

## 5. Remove

```sh
harness agent uninstall pr-reviewer     # removes the table; the pin stays
harness agent prune                      # removes pins nothing references
harness stable remove stump-wtf          # lists harnesses still sourced from it
```

`prune` considers only the global `harness.toml`. A pin that only a project
file uses is not protected, and that project's next `harness up` reports it
missing.

## Writing a package

A stable is a repository with `packages/<name>/` directories:

```text
packages/pr-reviewer/
  package.toml
  README.md                  # setup notes: what the operator must provide; required by `stable lint`
  prompts/review.md
  skills/<slug>/SKILL.md     # optional; requires [requests] skill_paths = true
```

Ship a `README.md` with every package. Use it to say what the agent needs
that the manifest cannot carry: environment variables, tokens and their
scopes, logins for the daemon's user. A package without one draws a `low`
`package.no-readme` scan finding on `info` and `install`. The finding does
not block. Keep the README plain Markdown. It is scanned like any other
`.md` file, and its control sequences are stripped before it is shown.

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

[[env]]                                 # one block per variable; names, never values
name        = "GH_TOKEN"
required    = false                     # gh may already be logged in
secret      = true
description = "Fine-grained PAT: Pull requests read/write, Contents read"

[[env]]
name        = "REVIEW_REPOS"
required    = true
description = "Space-separated <host>/<owner>/<repo> list"

[defaults]                              # suggestions, never applied values
schedule    = "0 9 * * 1-5"
timeout     = "45m"
on_overlap  = "queue"
```

`license` is an SPDX license expression. It may use only `AND`, `OR`,
`WITH` and parentheses. Every ID must be on the
[SPDX License List](https://spdx.org/licenses/), at the version this
Harness release vendors, or be a custom `LicenseRef-<name>`. An unknown ID
fails the manifest load, and the error names that ID. A package without a
license still installs, but it carries the `package.no-license` low
finding.

An `[[env]]` block takes only `name` (matching `^[A-Z_][A-Z0-9_]*$`),
`required` and `secret` (booleans, default `false`) and `description` (one
line). Any other key fails the install, and `value` or `default` fails it
with the reason: the value belongs to the operator.

A `[defaults]` table takes only `schedule`, `triggers`, `timeout`,
`on_overlap`, `catch_up`, `operating_hours`, `restart`, `restart_delay` and
`workdir`, each validated by the same parser your table's key uses. It is
inert at config load: nothing there reaches a running harness until you take
it at install or upgrade, when it becomes your own key. Two things gate an
offer: the table the write would produce must load (a `timeout` with nothing
to fire is skipped, naming the loader's error), and a `triggers` suggestion
needs every source it names to exist already, because install never creates
one. `env_file`, `secrets_env` and `enabled` are refused with a reason: the
first two are secret references (ADR-0038), and autostart is your call.

Keep the manifest to keys every adapter accepts if you want operators to
override `harness`. `system_prompt_file`, `mcp_config` and `allowed_tools`
are Claude Code one-shot keys.

### Running a script you bundle

A plain script — scrape a metric, page, restart something — is a `command`
harness, and its files travel with the package. `argv` entries are values, not
paths, so nothing in them is rewritten; name the script with `{{package.dir}}`,
which the daemon fills in from the pin your table points at:

```toml
[harness]
harness = "command"
argv = ["bash", "{{package.dir}}/watch.sh"]
```

The path is the daemon's, never the manifest's: it is derived from the
harness's own `source`, so it always names this package's own installed files,
and it survives an upgrade. Hard-coding the content SHA in `argv` works until
the next release, so don't. `{{package.dir}}` renders only for a harness with a
`source`; write `{{package.dir?}}` if the same `argv` should also load as a
hand-written harness.

Like any `command` harness, `argv[0]` is looked up on `PATH` (or against the
workdir when it has a separator), so the interpreter is `bash` or `python3`
rather than the script itself.

Validate the stable before you publish it. From the root of your checkout:

```sh
harness stable lint           # schema, scan, conventions
harness stable check          # lint, then load each package as config load would
harness stable check --json   # one {package, errors, warnings} per package, for CI
```

Both run offline: no daemon, no registered stable, no network. `lint` uses
the manifest parser and content scanner `install` uses, so a package it
passes is one an operator's install accepts. It fails on any of these:

- a manifest violation;
- a `high` scan finding;
- a `[package].name` that differs from its directory;
- a path key naming a missing or empty file;
- a symlink;
- a missing `README.md`.

`check` also catches an unknown adapter, or a prompt template that needs
context a scheduled run never supplies (write `{{run.source?}}` for an
optional value). Both exit 1 on any error, so either can gate your CI.

[stump-wtf/harness-stable](https://github.com/stump-wtf/harness-stable) is a
working example.

## See also

- [CLI reference: Agent packages](../usage/cli#agent-packages)
- [Configuration: Agent packages and stables](../usage/configuration#agent-packages-and-stables)
