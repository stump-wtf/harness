---
status: approved
date: 2026-09-27
implements: [ADR-0044]
extends: [SPEC-0006]
---

# SPEC-0026: Agent Package Stables

## Overview

This spec adds a package manager for harness definitions:

* A **stable**: a git remote the operator explicitly trusts, declared in a new
  global-only `[stable.*]` table, holding any number of installable
  **packages** under a fixed repository layout.
* A **package manifest**: a narrowed, declarative-only subset of the harness
  schema (ADR-0006/SPEC-0006), plus package metadata and an itemized
  `[requests]` table. It can select an existing adapter and supply values; it
  can never define an adapter, a secret reference, or any table other than
  `[package]`, `[harness]` and `[requests]`.
* A **content scan and confirmation gate** that runs on every install and
  every upgrade — never only the first — showing the human the manifest, the
  itemized requests, and (on upgrade) a full diff before anything a harness
  will run can change.
* A **`source` field** on a harness table, resolved at config load against an
  immutable, content-addressed local store. The daemon never fetches,
  clones, scans, or confirms anything; all of that lives in the
  `harness agent` CLI tree.

See ADR-0044 for the decision and the options it rejected. This spec extends
SPEC-0006's skill-path merge order (REQ-10) and otherwise adds new
requirements rather than amending existing ones.

Requirements are numbered. Cite them as `SPEC-0026 REQ-n`.

## Requirements

### Requirement: REQ-1 — Stable Registration And Global-Only Trust

The global configuration SHALL accept any number of `[stable.<name>]` tables,
each with a required `remote` (a git URL) and an optional `public` boolean
(unset is treated as `true`, following SPEC-0007's `skill_repo` convention).
`<name>` SHALL match `^[a-z][a-z0-9-]*$`.

`[stable.*]` SHALL be rejected in a project `harness.toml` and on the
project-up wire, joining `[adapter.*]` and `[skill_repo.*]` on the
global-only list, because a cloned repository must not be able to expand
what is trusted on the machine that clones it.

A `[stable.*]` table SHALL be created, modified, or removed only by
`harness agent stable add|remove <name> ...`, which writes the global
`harness.toml` directly; no other command SHALL add or change a stable
declaration. `stable add` SHALL clone the remote into
`$XDG_STATE_HOME/harness/agents/stables/<name>/` before the table is written,
and SHALL fail without writing the table if the clone fails. `stable remove`
SHALL delete the table and print every currently installed harness whose
`source` names that stable, without uninstalling them.

`harness agent stable update [name]` SHALL be the only operation that fetches
an already-added stable's remote. It SHALL fast-forward the local clone and
SHALL fail, without modifying the clone, on a non-fast-forward state. No
other command, and no daemon activity of any kind, SHALL fetch a stable's
remote.

#### Scenario: A project file cannot add a stable

- **WHEN** a project `harness.toml` declares `[stable.evil] remote =
  "https://attacker.example/stable.git"`
- **THEN** `harness up` fails, naming `[stable.*]` as global-only, and no clone
  is attempted

#### Scenario: Adding a stable clones before trusting

- **WHEN** `harness agent stable add stump-wtf
  https://forge.example/your-org/harness-stable.git` is run and the clone
  fails (network error, no such repository)
- **THEN** no `[stable.stump-wtf]` table is written to `harness.toml`

#### Scenario: Removing a stable reports its installed packages

- **WHEN** `harness agent stable remove stump-wtf` is run while
  `stump-wtf/pr-reviewer` is installed as harness `pr-reviewer`
- **THEN** the table is removed, the harness `pr-reviewer` is left exactly as
  it was, and the output names it as installed from the now-untrusted stable

#### Scenario: Nothing but stable update fetches

- **WHEN** `harness agent search`, `info`, `install`, or `upgrade` is run
  without an intervening `stable update`
- **THEN** none of them perform a network fetch; they operate on the stable's
  clone as it was left by the last `stable update`

### Requirement: REQ-2 — Stable Layout And Local Discovery

A stable's clone SHALL be expected to contain zero or more package directories
under `packages/<package-name>/`, each holding exactly one `package.toml`
manifest and, optionally, a `skills/` directory of skill directories
(ADR-0011 shape: `skills/<slug>/SKILL.md`) and a `prompts/` directory of
markdown files. `<package-name>` SHALL match the same pattern as a stable name.
A `packages/` directory with no valid subdirectories SHALL NOT be an error;
`harness agent search` on such a stable SHALL report zero packages.

`harness agent search [query] [--stable <name>]` SHALL list, across all
trusted stables or the named one, every package whose name or
`[package].description` matches `query` case-insensitively (or every
package, when `query` is omitted), reading only local clones.
`harness agent info <stable>/<package>` SHALL print the package's manifest,
its `[requests]` table, and the list of bundled files, without installing
it, reading only the local clone. Both commands SHALL fail, naming the stable,
when the named stable is not registered.

#### Scenario: Search across all trusted stables

- **WHEN** two stables are registered and `harness agent search reviewer` is run
- **THEN** every package from either stable whose name or description contains
  "reviewer" (case-insensitive) is listed, with its stable prefix

#### Scenario: Info reads without installing

- **WHEN** `harness agent info stump-wtf/pr-reviewer` is run and the package
  has never been installed
- **THEN** the manifest and bundled file list print, and no entry is created
  under the content-addressed store

#### Scenario: Unknown stable

- **WHEN** `harness agent info ghost/pr-reviewer` is run and no `[stable.ghost]`
  is registered
- **THEN** the command fails, naming `ghost` as not a registered stable

### Requirement: REQ-3 — Package Manifest Schema

A package manifest (`package.toml`) SHALL contain at most three top-level
tables: `[package]`, `[harness]`, and `[requests]`. Any other table SHALL
fail to load, naming the table and the manifest's path.

`[package]` SHALL require `name` (matching the package-name pattern) and
MAY carry `version`, `description`, `author`, and `homepage` as strings.

`[package]` MAY carry `license`, an SPDX license expression stating the
terms the package is offered under (issue #932). Its value SHALL be a
string; any other type SHALL fail to load, naming the key. The expression
syntax SHALL be limited to license identifiers joined by `AND`, `OR` and
`WITH` (operators uppercase, `WITH` binding tighter than `AND`, `AND`
tighter than `OR`) and grouped by parentheses. Every license identifier
SHALL be on the SPDX License List vendored into the binary at a pinned
version (matched case-insensitively), or SHALL be a custom
`LicenseRef-<idstring>`; every identifier after `WITH` SHALL be on the
vendored SPDX license-exception list. An unknown identifier, an empty
string, the `+` operator, a `DocumentRef-` reference, or any other
malformed expression SHALL fail to load, naming the key and the offending
identifier. The list is vendored, never fetched: validation is
reproducible from a Harness version alone, and bumping the pinned version
is a reviewed change regenerated from
https://github.com/spdx/license-list-data at a tagged release. The license
SHALL be shown by `harness agent info`, by the install and upgrade
confirmations (REQ-6, REQ-8), and by `harness agent list`, including its
`--json` output (REQ-12), where it is read from the installed pin's
manifest. An upgrade whose candidate declares a different license than
the installed pin (including one added or dropped) SHALL call it out as
its own row in the upgrade diff, marked as a change of the package's
terms. A package that declares no `license` SHALL still load; it
carries the `package.no-license` low finding (REQ-5).

`[harness]` SHALL require `harness`, naming an adapter exactly as
SPEC-0006/ADR-0039 validate it (an unknown adapter SHALL fail exactly as it
does for a hand-written harness). It MAY carry only the per-harness value
keys ADR-0039 closes over (`args`, `argv`, `model`, `model_pin`,
`auto_accept`, `max_turns`, `quiet`, `system_prompt_file`, `mcp_config`,
`allowed_tools`, `skill_paths`, `use_default_skill_paths`, `mcp_bridge`,
`mcp_exclusive`, `mcp_policy`), plus the one-shot prompt sources (`prompt`,
`prompt_file`, `prompt_template`, `prompt_template_file`; SPEC-0006 REQ
"Prompt Source", SPEC-0017 REQ-5). A manifest SHALL carry at most one prompt
source; two SHALL fail to load, naming both. A packaged prompt template is
held to SPEC-0017's grammar and context rules at config load exactly as a
hand-written one is. A package ships the instruction, never the firing: the
`schedule` and `triggers` that make the harness a one-shot stay on the
operator's harness table. It SHALL NOT carry `env_file`,
`secrets_env`, `workdir` as an absolute path outside the package directory,
`enabled`, `restart`, `restart_delay`, `operating_hours`, `schedule`, or
`triggers`; each SHALL fail to load, naming the key. Every string value
under `[harness]` SHALL be rejected if it contains the substring `${`,
because that is the secret-reference grammar ADR-0038 defines, and a
package manifest MUST NOT carry one. Every path value (`prompt_file`,
`prompt_template_file`, `system_prompt_file`, `mcp_config`, each
`skill_paths` entry) SHALL be relative and SHALL stay inside the package
directory: an absolute path, a `~` path, or one whose cleaned form climbs
out through `..` SHALL fail to load, naming the key. Those files are read
into the agent's context, so a path outside the package would hand the
agent any file on the installing machine without the content scan (REQ-5)
ever reading it. A relative path value SHALL resolve against the package's
own directory inside the content-addressed store, never against the
installing harness's `workdir`.

At config load the four prompt sources SHALL merge as one group, not key by
key: when the harness table sets any prompt source, the package's prompt
source SHALL NOT apply, whichever form either side uses (REQ-7's
local-override rule applied to the group SPEC-0017 REQ-5 makes mutually
exclusive).

`[requests]` MAY carry `skill_paths` (boolean), `mcp_allow` (a list whose
values are `"read"` and/or `"write"`), and `network` (boolean). Any other
key under `[requests]` SHALL fail to load, naming the key.

#### Scenario: An unknown table is rejected

- **WHEN** a manifest declares `[adapter.claude-code]` alongside `[harness]`
- **THEN** the package fails to load, naming `adapter` and the manifest's
  path

#### Scenario: env_file is rejected

- **WHEN** a manifest's `[harness]` table declares `env_file =
  "~/.config/vault/secrets.env"`
- **THEN** the package fails to load, naming `env_file`

#### Scenario: A secret reference in any value is rejected

- **WHEN** a manifest declares `system_prompt_file = "${HOME}/prompt.md"`
- **THEN** the package fails to load, naming the key and stating that `${`
  is not permitted in a package manifest

#### Scenario: A package ships a one-shot prompt

- **WHEN** a manifest declares `prompt_file = "prompts/review.md"` and the
  operator's table sets `source` to that package and `schedule = "@hourly"`
- **THEN** the harness loads as a scheduled one-shot whose `prompt_file` is
  `prompts/review.md` under the pin's directory, attributed to the package

#### Scenario: Two prompt sources in one manifest

- **WHEN** a manifest declares both `prompt_file` and `prompt_template`
- **THEN** the package fails to load, naming both keys

#### Scenario: A path outside the package is rejected

- **WHEN** a manifest declares `prompt_file = "/home/op/.ssh/id_ed25519"`,
  or `prompt_template_file = "../../x.tmpl"`
- **THEN** the package fails to load, naming the key

#### Scenario: A local prompt overrides the package's prompt as a group

- **WHEN** a manifest declares `prompt_template` and the operator's table
  sets `prompt`
- **THEN** the harness loads with the table's `prompt` alone, and describe
  attributes no prompt key to the package

#### Scenario: A valid license expression loads and is shown

- **WHEN** a manifest declares `license = "MIT OR Apache-2.0"`
- **THEN** the package loads, and `harness agent info`, the install
  confirmation and `harness agent list --json` all show
  `MIT OR Apache-2.0`

#### Scenario: A custom license reference loads

- **WHEN** a manifest declares `license = "LicenseRef-Acme-Internal"`
- **THEN** the package loads with that license

#### Scenario: An unknown license identifier is rejected

- **WHEN** a manifest declares `license = "MIT OR Bogus-1.0"`
- **THEN** the package fails to load, naming `license` and `Bogus-1.0`

#### Scenario: A non-string license is rejected

- **WHEN** a manifest declares `license = 1` or `license = ["MIT"]`
- **THEN** the package fails to load, stating that `[package].license`
  must be a string

#### Scenario: A license change on upgrade is called out

- **WHEN** the installed pin declares `license = "MIT"` and the upgrade
  candidate declares `license = "GPL-3.0-only"`
- **THEN** the upgrade diff shows
  `package.license: MIT -> GPL-3.0-only` marked as a license change before
  the confirmation

#### Scenario: An unknown request key

- **WHEN** `[requests]` declares `filesystem = true`
- **THEN** the package fails to load, naming `filesystem`

#### Scenario: Adapter selection is validated like any harness

- **WHEN** a manifest declares `harness = "nonexistent"`
- **THEN** the package fails to load with the same unknown-adapter error a
  hand-written harness table produces

### Requirement: REQ-4 — Capability Requests

The confirmation shown by `install` and `upgrade` (REQ-6, REQ-8) SHALL
render every entry present in `[requests]` as an itemized, human-readable
line, and SHALL state explicitly when a request key is absent (for example,
"does not declare needing network access" when `network` is unset or
`false`). A request of `mcp_allow` containing `"write"` SHALL be rendered
with a distinct warning stating that the installed harness would be able to
start, stop, or restart its siblings (ADR-0010), and installing or
upgrading such a package SHALL require the operator to retype
`<stable>/<package>` at the confirmation prompt, independent of any content
scan finding and independent of `--yes`. The confirmed `mcp_allow` SHALL be
written onto the installing `[harness.<name>]` table alongside `source`
(issue #882), so the effective scope lives in one visible, hand-editable
place; a package declaring no scopes writes nothing and the default grant
applies.

#### Scenario: A read-only package needs no extra confirmation

- **WHEN** a package requests `mcp_allow = ["read"]` and no content scan
  finding is `high`
- **THEN** `--yes` completes the install with the ordinary confirmation
  summary, no retyped name required

#### Scenario: A write request demands typed confirmation

- **WHEN** a package requests `mcp_allow = ["read", "write"]`
- **THEN** install refuses to proceed under `--yes` alone, and an
  interactive install only proceeds once the operator retypes
  `<stable>/<package>` exactly

### Requirement: REQ-5 — Content Scan And Severity

Before every install and every upgrade, the CLI SHALL run a content scan
over the manifest's string values, every bundled file with a `.md` or
`.txt` extension, and every file the manifest names as a prompt
(`prompt_file`, `prompt_template_file`, `system_prompt_file`) whatever its
extension. The scan SHALL classify each finding as `high` or `low`
severity and SHALL be documented as a heuristic tripwire, not a
certification: the CLI output and `harness agent info` SHALL both state
that a clean scan is not a guarantee of safety.

A `high`-severity finding SHALL block `install` and `upgrade` unless
`--force-unsafe` is given together with a re-typed `<stable>/<package>`; when
given, the install record SHALL retain the finding and the fact that it was
overridden. `--yes` alone SHALL NOT suppress a `high`-severity block. A
`low`-severity finding SHALL be shown in the confirmation output and SHALL
NOT block.

A manifest that declares no `[package].license` (REQ-3) SHALL produce
exactly one `low` finding with the pattern id `package.no-license`,
located at `package.toml`'s `[package]` header (issue #932). Like every
`low` finding it SHALL be shown and SHALL NOT block install or upgrade;
`harness agent stable lint` runs the same scan and treats it as an error,
so a stable cannot publish an unlicensed package while an operator can
still install one knowingly.

On an upgrade, the scan SHALL run against the new pin's content, and any
finding not present against the currently installed pin's content SHALL be
marked as new in the diff output (REQ-8), so a previously accepted package
cannot silently pick up an injected instruction on the next version without
it being called out specifically.

#### Scenario: A high finding blocks by default

- **WHEN** a bundled `SKILL.md` contains a phrase matching a high-severity
  pattern (for example, an instruction to disregard the system prompt)
- **THEN** `install` refuses without `--force-unsafe`, and the finding's
  file, line, and matched pattern name are shown

#### Scenario: --yes never bypasses a high block

- **WHEN** the same install is retried with `--yes` and no
  `--force-unsafe`
- **THEN** the install still refuses

#### Scenario: force-unsafe requires the typed name

- **WHEN** `--force-unsafe` is given without an interactive retype of
  `<stable>/<package>` on a non-interactive terminal
- **THEN** the install refuses, stating that an unattended session cannot
  override a high-severity finding

#### Scenario: A low finding does not block

- **WHEN** a bundled file contains only a low-severity finding (for example,
  an embedded link to a domain other than the package's declared
  `homepage`)
- **THEN** `install` completes under `--yes`, with the finding shown in the
  output

#### Scenario: A package without a license gets a low finding

- **WHEN** a manifest's `[package]` table declares no `license`
- **THEN** `harness agent info` and the install confirmation show a
  `package.no-license` `low` finding at `package.toml`, and `install`
  completes under `--yes`

#### Scenario: A new finding on upgrade is called out

- **WHEN** the currently installed pin has zero findings and the candidate
  pin has one `high` finding in a file unchanged by line count but altered
  in content
- **THEN** the upgrade's diff output marks that finding as new relative to
  the installed pin

### Requirement: REQ-6 — Install: Resolution And Pinning

`harness agent install <stable>/<package>[@<version>] [--as <name>]` SHALL
resolve `<version>` (default: the stable's current default branch tip) to an
exact, full 40-character commit SHA in the stable's local clone. When
`<version>` is already a full 40-character SHA present in the local
content-addressed store, resolution SHALL use that local copy directly and
SHALL NOT consult the stable's clone.

After the content scan (REQ-5) and confirmation (REQ-4), a successful
install SHALL copy the package directory's contents at that commit into
`$XDG_STATE_HOME/harness/agents/installed/<stable>/<package>/<sha>/`,
immutably; an existing directory at that exact path SHALL be treated as
already-materialized and SHALL NOT be rewritten. Install SHALL then write
or update a `[harness.<name>]` table in the target `harness.toml`
(`<name>` defaults to `<package>`; `--as` overrides it) with `source =
"<stable>/<package>@<sha>"`, preserving every other key already on that table.
Installing into a harness name that already has a `source` from a
*different* stable or package SHALL require an explicit `--replace` flag;
without it, the command SHALL fail, naming the existing source.

#### Scenario: A branch resolves to a pinned SHA

- **WHEN** `harness agent install stump-wtf/pr-reviewer` is run with no
  `@version` and the stable's default branch tip is commit `abc123...`
- **THEN** the written `source` reads
  `stump-wtf/pr-reviewer@abc123...` (the full SHA), never the branch name

#### Scenario: Repeated install is idempotent

- **WHEN** `install` is run twice in a row with no intervening `stable update`
- **THEN** both resolve to the same commit SHA and the second run reuses
  the existing content-addressed directory without rewriting it

#### Scenario: Installing twice under different names

- **WHEN** `harness agent install stump-wtf/pr-reviewer --as
  pr-reviewer-strict` is run after `pr-reviewer` is already installed
- **THEN** a second harness table, `pr-reviewer-strict`, is created with its
  own `source`, and the first is untouched

#### Scenario: A local SHA install needs no stable clone

- **WHEN** `harness agent install stump-wtf/pr-reviewer@<sha>` is run for a
  `<sha>` already present in the content-addressed store, and the stable's
  clone is unreachable
- **THEN** the install still succeeds, reading only the local store

#### Scenario: Overwriting a different source requires --replace

- **WHEN** `harness.pr-reviewer` already has `source =
  "other-stable/other-pkg@..."` and `harness agent install
  stump-wtf/pr-reviewer --as pr-reviewer` is run without `--replace`
- **THEN** the command fails, naming the existing source

### Requirement: REQ-7 — The Source Field And Config-Load Resolution

The harness schema SHALL accept an optional `source` key, a string of the
form `<stable>/<package>@<sha>` where `<sha>` is a full 40-character commit
SHA. `source` SHALL be legal on a `[harness.*]` table in the global
configuration file and in a project `harness.toml`, unlike `[stable.*]`.

At config load, a harness table declaring `source` SHALL be resolved by
reading `package.toml` from
`$XDG_STATE_HOME/harness/agents/installed/<stable>/<package>/<sha>/` on local
disk only — the daemon SHALL NOT perform a network request or a git
operation of any kind to resolve `source`. The package's `[harness]` values
SHALL be applied first, and any key also present directly on the harness
table SHALL override the package's value for that key, following the
precedence rule ADR-0011 already applies to `skill_paths`. The package's
`skill_paths` (if `[requests].skill_paths` was `true`) SHALL be added at the
lowest merge tier (REQ-10).

When the referenced pin is absent from local disk, config load SHALL fail
with a located error naming the harness, the full `source` value, and an
instruction to run `harness agent install`. This failure SHALL keep the
daemon on its last-good configuration (ADR-0006), exactly as an unknown
adapter does.

#### Scenario: A package-sourced harness resolves identically to a hand-written one

- **WHEN** a harness declares `source = "stump-wtf/pr-reviewer@abc123..."`
  with no other keys, and the pinned manifest declares
  `harness = "claude-code"` and `auto_accept = true`
- **THEN** the loaded, validated configuration for that harness is
  identical to a hand-written table declaring the same two keys directly

#### Scenario: A local override wins

- **WHEN** the same harness table also declares `model = "opus"` directly,
  and the pinned manifest does not set `model`
- **THEN** the effective configuration carries `model = "opus"`

#### Scenario: A missing pin fails the load, not the network

- **WHEN** a harness declares a `source` whose pin directory does not exist
  on disk
- **THEN** config load fails naming the harness and the full source string,
  and no network request or git operation is attempted

#### Scenario: A project file may reference an installed package

- **WHEN** a project `harness.toml` declares `[harness.agent] source =
  "stump-wtf/pr-reviewer@abc123..."`, and that pin is already installed on
  the machine running `harness up`
- **THEN** the project registers and starts the harness normally, with no
  fetch of any kind

#### Scenario: A project file cannot install what it references

- **WHEN** the same project file is used on a machine where that pin was
  never installed
- **THEN** `harness up` fails naming the missing pin, and nothing is
  fetched, scanned, or installed on its behalf

### Requirement: REQ-8 — Upgrade

`harness agent upgrade <stable>/<package>|--all [@<version>]` SHALL, for each
named harness whose `source` names that `<stable>/<package>`, resolve a new
pin exactly as REQ-6 describes for install (using the stable's
already-fetched local clone; `upgrade` SHALL NOT fetch). It SHALL then
produce a diff against the currently installed pin: every changed manifest
value, and, for each bundled file under 64 KiB, a unified text diff; a
larger or non-text file SHALL be reported as changed with its byte count,
not diffed inline. The diff output SHALL incorporate REQ-5's scan-on-new
findings.

Upgrade SHALL be refused, exactly as install is, on a `high`-severity
finding without `--force-unsafe`, and on an `mcp_allow` request including
`"write"` without the typed confirmation, whether or not that request was
already present and confirmed in the currently installed pin.

Before the confirmation, upgrade SHALL compute the effective diff: every
manifest-suppliable key whose effective value would move with the new pin
(package-supplied values the new pin changes or drops, keys it introduces),
every local override whose key the new pin changes from the installed pin's
value to one contradicting the override, and every requested
`mcp_allow` scope the table does not already grant (issue #882). When that
diff is non-empty, upgrade SHALL NOT auto-apply it: `--yes` and unattended
runs SHALL refuse loudly, naming the changes, and an interactive run SHALL
present the diff for an explicit per-change choice — keep the current value
(pinning it onto the table where it was not already the operator's own) or
take the new one (removing any local override it replaces, so the new pin
supplies it). A package-supplied key SHALL be compared manifest to manifest,
so a pin-relative path the new pin leaves unchanged is not a change. Keeping
a package-supplied path (`prompt_file`, `prompt_template_file`,
`system_prompt_file`, `mcp_config`) pins the old
pin's absolute path onto the table; the review SHALL say that this path now
holds the old pin, which REQ-9's prune keeps while it does. Package
metadata (version, description) SHALL NOT trigger the review: it changes
nothing the harness runs. The prompt sources are reviewed as REQ-3's one group: when
the table sets its own prompt source, no prompt-source row SHALL appear,
because nothing the new pin does to its prompt changes what the harness
runs; a package prompt that changes form SHALL read as the old key dropped
and the new key added.

On confirmation, upgrade SHALL update the `source` line's `@<sha>` on the
affected harness table plus exactly the changes the review's choices
require; every other key on that table SHALL be left untouched. The prior
pin's content-addressed directory SHALL NOT be deleted by upgrade.

#### Scenario: A trivial upgrade still requires confirmation

- **WHEN** `upgrade` resolves a new pin that changes only
  `[package].version` and no scanned content
- **THEN** the diff is shown and confirmation is still required (or `--yes`
  accepted, since no `high` finding exists)

#### Scenario: A local override the package did not move never blocks

- **WHEN** a harness table overrides `model = "opus"` over a package whose
  installed and candidate pins both declare `model = "sonnet"`, and
  `upgrade --yes` is run
- **THEN** the upgrade proceeds with no review row for `model`, and the
  override is left byte-identical
- **AND WHEN** the candidate pin instead declares `model = "haiku"`
- **THEN** `upgrade --yes` refuses, naming `model: opus -> haiku`

#### Scenario: Upgrade needs a prior stable update

- **WHEN** `upgrade` is run for a version newer than what the stable's local
  clone holds, with no intervening `stable update`
- **THEN** the command fails, naming the requested version as not present
  in the local clone and recommending `harness agent stable update`

#### Scenario: The prior pin survives an upgrade

- **WHEN** a harness is upgraded from `@sha1` to `@sha2`
- **THEN** `$XDG_STATE_HOME/harness/agents/installed/<stable>/<package>/sha1/`
  still exists on disk after the upgrade completes

#### Scenario: Rollback is a re-install of a retained pin

- **WHEN** `harness agent install <stable>/<package>@sha1 --as <name>
  --replace` is run after an upgrade to `sha2`, and `sha1`'s directory is
  still present
- **THEN** the harness's `source` is rewritten back to `@sha1` with no
  network access

### Requirement: REQ-9 — Uninstall And Prune

`harness agent uninstall <name>` SHALL, after confirmation, remove the
entire `[harness.<name>]` table from the file that declares it (global or,
when named there, project) when that table's `source` is set; it SHALL
refuse, naming the harness, when the table has no `source` (uninstall
never touches a hand-written harness).

`harness agent prune` SHALL remove every entry under
`$XDG_STATE_HOME/harness/agents/installed/` that the **global**
`harness.toml` does not reference. A pin is referenced by a `source` naming
it, or by a file path config load reads — `prompt_file`,
`prompt_template_file`, `system_prompt_file`, `mcp_config` — whose resolved
value lies inside the
pin's directory, as a package path kept through REQ-8's review does; for
each pin that only such a path references, prune SHALL say so in its
output, naming the harness and the key. It SHALL NOT scan project files (they
are ephemeral and not always present) and SHALL state this scope
limitation in its own output. A pin that a project file references but that
`prune` removed SHALL surface only as REQ-7's ordinary missing-pin load
error the next time that project is brought up, naming the pin and
recommending reinstall — the same behavior as any other missing pin.

#### Scenario: Uninstall removes the whole table

- **WHEN** `harness agent uninstall pr-reviewer` is run for a harness whose
  table is `source = "stump-wtf/pr-reviewer@abc123..." workdir =
  "~/src/reduit"`
- **THEN** the entire `[harness.pr-reviewer]` table is removed, not just the
  `source` line

#### Scenario: Uninstall refuses a hand-written harness

- **WHEN** `harness agent uninstall claude-src` is run for a harness with
  no `source` key
- **THEN** the command fails, stating that the harness was not installed
  from a package

#### Scenario: Prune removes only globally unreferenced pins

- **WHEN** a pin is referenced only by a project `harness.toml` that is not
  currently `up`, and by no global harness
- **THEN** `harness agent prune` removes it, and the next `harness up` for
  that project fails naming the missing pin rather than silently refetching
  it

#### Scenario: Prune keeps a pin a kept path points into

- **WHEN** a harness was upgraded from `@sha1` to `@sha2` and the operator
  kept the package's `system_prompt_file`, so the table carries
  `source = "<stable>/<package>@sha2"` and a `system_prompt_file` under
  `sha1`'s directory
- **THEN** `harness agent prune` keeps `sha1`, its output names
  `[harness.<name>] system_prompt_file` as what holds it, and the global
  configuration still loads; once that key no longer points inside `sha1`,
  the next prune removes it

### Requirement: REQ-10 — Skill Path Precedence Amendment

This requirement amends SPEC-0006 REQ "Ordered Merge and Shadowing". The
merge order becomes, lowest precedence first:

```
package bundle (installed source's skills/, when [requests].skill_paths
  was true) → adapter defaults → global skill_paths → project skill_paths →
  project-local dirs (highest)
```

A package's bundled skills SHALL contribute at the new lowest tier only
when the harness's effective configuration carries a `source`, and only
the skills bundled at that exact pinned commit. A name collision between
the package tier and any higher tier SHALL resolve to the higher tier, with
the package's copy recorded as shadowed exactly as SPEC-0006 already
requires for any other collision.

#### Scenario: A human's global skill wins over a package's

- **WHEN** a package bundles a skill named `playbook` and the operator's
  global `skill_paths` also supplies a skill named `playbook`
- **THEN** the global copy is projected, and the package's copy is recorded
  as shadowed

#### Scenario: An uninstalled package contributes nothing

- **WHEN** a harness has no `source`
- **THEN** the package tier contributes no roots for that harness, and the
  merge behaves exactly as SPEC-0006 already defines

### Requirement: REQ-11 — No Fleet-Wide Auto-Registration

Installing or upgrading a package SHALL NOT modify any `[mcp.*]` table,
including `[mcp.prompts]`. When a package bundles a `prompts/` directory,
`install` and `upgrade` output SHALL name that directory and state that it
is not registered automatically. No command defined by this spec SHALL
write to `[mcp.*]`, `[job.*]`, `[server]`, `[profile.*]`, `[adapter.*]`, or
`[skill_repo.*]`.

#### Scenario: A bundled prompts directory is not wired in

- **WHEN** a package bundling `prompts/release-notes.md` is installed
- **THEN** `[mcp.prompts]` is unchanged, and the install output names the
  bundled directory and says it must be added by hand

### Requirement: REQ-12 — CLI Visibility

`harness agent list` SHALL show every harness whose table carries a
`source`, its stable, package, pinned SHA (short form for display, full form
with `--json`), and whether the stable's local clone (as of its last
`stable update`) has a newer default-branch commit than the installed pin
(informational only; it SHALL NOT trigger a fetch or an upgrade).
`harness describe` on a package-sourced harness SHALL show its `source`
value and which of its effective keys came from the package versus a local
override. `harness doctor` SHALL add a warn row for any harness whose
`source` pin is missing from local disk, distinct from an ordinary load
error, when config as a whole otherwise loaded (i.e., when the missing pin
was caught before the rest of the file, `doctor` reports it as a load
failure instead, per existing behavior). Clarification (issue #815): the
row reads the running daemon's harness records — its last-good config
view — which is the only place a pin can be referenced while missing from
disk: one pruned or deleted after the daemon loaded its config.

Amendment (issue #933): `harness agent stable lint [PATH]` and `harness
agent stable check [PATH]` SHALL validate a stable checkout (a directory
holding `packages/`; `PATH` defaults to the working directory) offline: no
daemon socket, no git operation and no network access. `lint` SHALL use the
same manifest loader (REQ-3) and the same content scan (REQ-5) as install,
and SHALL report as an error, per package: every manifest violation the
loader returns; every `high` scan finding; a `[package].name` that does not
match its directory; a path key naming a file the package does not ship, or
an empty one; a symlink; and a missing `README.md` (`package.no-readme`) or
license (`package.no-license`). These two convention findings are `low` at
install and are promoted to errors by finding id. Every other `low` finding
SHALL be a warning. `check` SHALL run `lint`, then load every package whose
manifest parsed through config load's own `source` resolution (REQ-7) under
a synthetic `[harness.<package>]` table, adding a `schedule` when the
package supplies a prompt, and SHALL report any load error. Both verbs SHALL
exit non-zero on any error, and `--json` SHALL emit one `{package, errors,
warnings}` object per package. A directory with no packages SHALL fail
rather than report a clean run.

#### Scenario: stable lint fails a package install would refuse

- **WHEN** a stable checkout's package carries a `high` scan finding
- **THEN** `harness agent stable lint` exits non-zero and names the file,
  the line and the pattern id, without echoing the matched text

#### Scenario: stable check catches a load error lint cannot

- **WHEN** a package's manifest names an adapter Harness does not know
- **THEN** `harness agent stable lint` passes it, and `harness agent stable
  check` exits non-zero with the same error config load would report

#### Scenario: stable lint runs offline

- **WHEN** `harness agent stable lint` or `check` runs
- **THEN** it dials no daemon socket, runs no git command and writes nothing
  under the state directory

#### Scenario: list shows staleness without fetching

- **WHEN** a stable's local clone (from its last `stable update`) is three commits
  ahead of an installed pin
- **THEN** `harness agent list` marks that package as having a newer
  version available, without performing any network access

#### Scenario: describe attributes each field

- **WHEN** `harness describe pr-reviewer` is run for a package-sourced
  harness with one local override
- **THEN** the output marks the overridden key as `local` and every other
  key as coming from the package's pin

### Requirement: Error Handling Standards

All error-producing operations in this spec SHALL follow structured error
handling:

- Errors SHALL be wrapped with context at each layer boundary, naming the
  stable, the package, and the harness where applicable.
- Sentinel errors SHALL be defined for the failure modes callers
  distinguish: unknown stable, unknown package, manifest schema violation,
  blocked high-severity finding, missing local pin, and diverged stable clone.
- An error MUST NOT be swallowed silently. A blocked install or upgrade is
  always a non-zero exit with a message naming the reason, never a partial,
  silent success.
- Logging SHALL be structured key-value, and SHALL never include the
  content of a scanned file or a rendered confirmation prompt, only its
  path, finding category, and severity.

#### Scenario: A blocked install exits non-zero with a named reason

- **WHEN** an install is blocked by a `high`-severity finding
- **THEN** the process exits non-zero, and the error names the file and the
  finding category without echoing the file's content into an unrelated log
  stream
