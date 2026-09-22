---
status: draft
date: 2026-09-22
implements: [ADR-0024]
extends: [SPEC-0006]
requires: [SPEC-0008, SPEC-0010, SPEC-0014]
---

# SPEC-0018: Stack Installer and Centralized Stack Management

## Overview

Two commands install and wire the Harness, Switchboard and Cairn loop:

* **`harness init`** is the **user's** command. It is a converge-style config
  generator that connects agent clients and named personas to an instance of
  Switchboard and Cairn: it vends each persona's endpoint, obtains its Cairn
  token, and writes the Harness drop-ins, env files, client MCP configuration
  and skills. It runs interactively or entirely from flags and an answers file.
* **`harness stack`** is the **operator's** command. `stack init`, `up`, `down`,
  `status`, `upgrade` and `doctor` write, start, inspect, upgrade and check a
  Compose bundle on this host (one Postgres, Switchboard, Cairn, an object
  store, optional Caddy), pinned by tag and digest. `stack up` finishes by
  running `harness init` as the instance's first user.

Both end in an **end-to-end self-test** that proves an agent claimed a real
todo and wrote a real artifact, by comparing content.

This spec also extends SPEC-0006: a `claude-code` prompt harness gains
`system_prompt_file`, `mcp_config` and `allowed_tools`, and `env_file` accepts a
list. See ADR-0024 for the decision, the options, and what it changes in
ADR-0006, ADR-0011, ADR-0019 and ADR-0021.

It requires SPEC-0008 (the run machinery the self-test's one-shot uses),
SPEC-0010 (the command tree and environment layer the new commands join), and
SPEC-0014 (the channel source the self-test binds). Records written in
parallel are cited by number: SPEC-0017 (the command kind), SPEC-0021 (run
budgets), SPEC-0022 (run history), SPEC-0023 (dev-channels auto-confirm);
Switchboard SPEC-0025 (doorbell acknowledgement and `doctor`), SPEC-0027
(release and version contract) and SPEC-0033 (teams); Cairn SPEC-0023 (teams).

Terms: an **instance** is one deployment of Switchboard and Cairn. The
**operator** runs an instance; a **user** has an account on one. A **persona**
is a named agent: one Harness harness, one Switchboard endpoint, one Cairn
token, one client configuration. The **bundle** is the directory `harness stack`
manages. The **manifest** is the pinned component list embedded in the Harness
binary.

## Requirements

### Requirement: REQ-1 Profile Separation

`harness init` SHALL be usable by any user of any instance. `harness stack`
SHALL operate only the Compose project in a bundle on the local host, and SHALL
NOT accept a flag, answer or environment variable naming a remote Switchboard or
Cairn instance. A resource `harness init` creates on an instance SHALL be created
with the signed-in user's own credentials, never with an instance-level
credential.

#### Scenario: A user connects to a shared instance

- **GIVEN** a user with an account on `https://sb.example.com`
- **WHEN** they run `harness init --connect https://sb.example.com`
- **THEN** `init` logs them in, vends their personas' endpoints under their own
  account, and writes local files, and it neither needs nor requests any
  operator credential

#### Scenario: Stack commands cannot target a remote instance

- **WHEN** an operator runs `harness stack up --switchboard-url https://sb.example.com`
- **THEN** the command fails with a usage error (exit `2`) stating that
  `harness stack` manages only this host's bundle and that `harness init
  --connect` is the way to use a remote instance

#### Scenario: The operator is the first user

- **WHEN** `harness stack up` starts a fresh bundle
- **THEN** it runs `harness init --connect` against the local URLs, the
  operator signs in through the instance's identity provider like any user, and
  every endpoint and token created is owned by that account

### Requirement: REQ-2 Init Answers

`harness init` SHALL accept every answer from a flag or from `--answers FILE` (a
TOML file whose keys mirror the flags), and SHALL prompt interactively only for
answers not supplied, and only when stdin and stdout are both terminals. With
`--non-interactive`, or with no terminal, a missing required answer SHALL fail
the command with exit `2` and a message naming every missing answer. A secret
(a token or a key) SHALL be accepted only from a file (`--*-file PATH`), an
environment variable, or a hidden interactive prompt, and SHALL NOT be accepted
as a flag value.

#### Scenario: Fully non-interactive

- **WHEN** an agent runs `harness init --non-interactive --answers init.toml`
  and the file supplies every required answer
- **THEN** `init` completes without reading stdin

#### Scenario: Missing answers are listed together

- **WHEN** `harness init --non-interactive` runs with no Switchboard URL and no
  persona
- **THEN** it exits `2` and names both `switchboard_url` and `persona`, not only
  the first

#### Scenario: A secret on the command line

- **WHEN** a user runs `harness init --claude-oauth-token sk-ant-oat01-…`
- **THEN** the command fails with a usage error that names
  `--claude-oauth-token-file` and `CLAUDE_CODE_OAUTH_TOKEN`, and the value is not
  echoed

### Requirement: REQ-3 Client Selection

`harness init` SHALL offer the clients `claude-code`, `crush`, `codex` and
`pi-omp` per persona. A `pi-omp` persona SHALL be written as the command kind
SPEC-0017 defines. While the running binary has no command kind, `init` SHALL
refuse `pi-omp` with an error naming the missing capability. `init` SHALL NOT
write a `generic` harness carrying `prompt` or `prompt_file` for any client.

#### Scenario: Pi on a binary without the command kind

- **WHEN** a user selects `pi-omp` on a Harness build that predates SPEC-0017
- **THEN** `init` fails for that persona, names the command kind as the missing
  capability, and writes nothing for it

#### Scenario: No silent Crush

- **WHEN** any persona is written
- **THEN** its drop-in never pairs `harness = "generic"` with a prompt source

### Requirement: REQ-4 Persona Templates

Harness SHALL embed built-in persona templates named `coordinator`, `planner`,
`implementer`, `reviewer`, `verifier` and `drainer`. A directory
`$XDG_CONFIG_HOME/harness/templates/<name>/` SHALL add a template, or shadow the
built-in of the same name. A template SHALL consist of `persona.toml`,
`prompt.md` and `system.md`. `persona.toml` SHALL declare at least a
description, a mode (`resident` or `one-shot`), a default client, the queues the
persona drains, the MCP servers it needs, and its allowed tools. A `one-shot`
template SHALL declare a `max_runs_per_day` default, rendered only when the
running binary supports that key (SPEC-0021). Templates SHALL be rendered once,
at `init` time, from `init`'s answers only. A template SHALL NOT be rendered with
event or todo content.

`harness init --list-templates` SHALL print every available template, its
source (built-in or the shadowing path) and its description.

#### Scenario: Shadowing a built-in

- **GIVEN** `~/.config/harness/templates/reviewer/` exists
- **WHEN** a user runs `harness init --list-templates`
- **THEN** `reviewer` is listed with the user path as its source and the
  built-in noted as shadowed

#### Scenario: A malformed template

- **WHEN** a user template's `persona.toml` has no `mode`
- **THEN** `init` fails before any network call, naming the template file and
  the missing key

#### Scenario: A verifier on a different family

- **WHEN** a user adds an `implementer` on a Claude model and a `verifier`
  without choosing its model
- **THEN** `init` proposes a default model for the verifier from a different
  model family than the implementer's, and says why

### Requirement: REQ-5 Persona Output Files

For each persona `<p>`, `harness init` SHALL write:

* a `harness_d` drop-in `<harness_d>/<p>.toml` containing exactly the persona's
  `[harness.<p>]` table and any `[channel.*]` or `[webhook.*]` source it binds;
* the persona's prompt and system-prompt files under
  `$XDG_CONFIG_HOME/harness/personas/<p>/`;
* the persona's env file `$XDG_CONFIG_HOME/harness/env/<p>.env`, mode `0600`;
* the persona's client configuration (REQ-13).

When `harness.toml` does not exist, `init` SHALL create it with a `[server]`
table setting `harness_d`. When it exists and sets `harness_d`, `init` SHALL
write into that directory. When it exists without `harness_d`, `init` SHALL NOT
modify it: it SHALL print the exact line to add and exit `3` before any
provisioning call, unless `--harness-d DIR` names the directory to use and the
operator has added it. `init` SHALL NOT modify or replace a file it did not
create; it SHALL record every file it creates in
`$XDG_STATE_HOME/harness/init-manifest.json`.

#### Scenario: Fresh machine

- **GIVEN** no `harness.toml`
- **WHEN** `init` writes two personas
- **THEN** `harness.toml` exists with `[server] harness_d = "harness.d"`, two
  drop-ins exist, and `harness reload` loads both

#### Scenario: Config rendered by a dotfiles tool

- **GIVEN** a `harness.toml` without `harness_d`
- **WHEN** `init` runs
- **THEN** the file is byte-identical afterwards, `init` prints the line to
  add, exits `3`, and has vended nothing

#### Scenario: A hand-edited drop-in

- **GIVEN** the operator edited `harness.d/reviewer.toml` after `init` wrote it
- **WHEN** `init` runs again with an unchanged answer set
- **THEN** it reports the drop-in as locally modified and leaves it untouched

### Requirement: REQ-6 Converge, Diff, Rotate And Dry Run

A second `harness init` with the same answers SHALL make no network call that
mutates and write no file. A changed answer SHALL be shown as a unified diff of
each affected file before anything is written, and SHALL require confirmation
interactively or `--yes` non-interactively. A persona whose endpoint exists on
the instance but whose token is absent locally SHALL fail with an error naming
`--rotate <p>`. `--rotate <p>` SHALL revoke the persona's endpoint, vend a new
one and replace the token in its env file, after confirmation. `--dry-run`
SHALL print every file it would write and every API call it would make (method,
path, and the non-secret body fields), SHALL make no mutating call, and SHALL
write nothing.

#### Scenario: Idempotent re-run

- **WHEN** `init --answers init.toml` runs twice
- **THEN** the second run reports every persona `unchanged`, calls no vend, and
  every file's modification time is unchanged

#### Scenario: Lost token

- **GIVEN** `reviewer.env` was deleted
- **WHEN** `init` runs
- **THEN** it fails for `reviewer` and names `--rotate reviewer`, and does not
  vend a second endpoint with the same name

#### Scenario: Dry run makes no change

- **WHEN** `init --dry-run` runs against a fake instance that records calls
- **THEN** the fake records no `POST`, `PUT`, `PATCH` or `DELETE`, and no file is
  written

### Requirement: REQ-7 Switchboard Provisioning

`harness init` SHALL authenticate to Switchboard's operator API as the signed-in
human through Switchboard's OAuth authorization server, using dynamic client
registration and authorization code with PKCE (S256) over a loopback redirect,
with Harness registered as its own client. It SHALL store the resulting grant
at `$XDG_CONFIG_HOME/harness/credentials/switchboard-<host>.json`, mode `0600`,
refresh it before expiry, and re-run the login when refresh is refused. For
each persona it SHALL find an existing endpoint by agent name
(`GET /api/v1/endpoints`) and SHALL vend a missing one (`POST
/api/v1/endpoints`) with the persona's queue. It SHALL write the endpoint's MCP
URL and token into the persona's env file as `SWITCHBOARD_MCP_URL` and
`SWITCHBOARD_TOKEN`. It SHALL NOT write the token anywhere else, and SHALL NOT
use the `mcp_json` the vend response returns.

`--no-browser` SHALL print the authorization URL instead of opening a browser,
and state that the loopback redirect must reach this machine (for example through
an SSH port forward).

#### Scenario: Vend on first run

- **WHEN** `init` runs for persona `reviewer` on queue `reviews`
- **THEN** exactly one `POST /api/v1/endpoints` with `{"name":"reviewer","queue":"reviews"}`
  is made, and `reviewer.env` contains the returned token and no other file
  does

#### Scenario: Refused refresh

- **GIVEN** the stored grant was revoked in Switchboard
- **WHEN** `init` runs
- **THEN** it starts a new login rather than failing, and the old grant file is
  replaced

#### Scenario: Switchboard unreachable

- **WHEN** the Switchboard URL refuses connections
- **THEN** `init` fails before writing any persona file, names the URL, and exits
  `1`

#### Scenario: Another user's endpoint name

- **GIVEN** another user on the instance owns an endpoint named `reviewer`
- **WHEN** this user's `init` looks up `reviewer`
- **THEN** the lookup finds nothing of the other user's (the operator API lists
  only the caller's endpoints) and `init` vends this user's own `reviewer`

### Requirement: REQ-8 Cairn Provisioning

When a Cairn URL is configured, `harness init` SHALL obtain one agent token per
persona, owned by the signed-in user or the chosen team, and SHALL write it to
the persona's env file as `CAIRN_TOKEN` with the instance URL as `CAIRN_URL`.
Where the instance offers a consented CLI mint for agent tokens, `init` SHALL use
it. Otherwise `init` SHALL print the instance's token page URL, ask for an agent
token through a hidden prompt (or `--cairn-token-file` per persona in
non-interactive mode), and verify it by one authenticated read before writing
it. `init` SHALL NOT configure or use a static instance token
(`CAIRN_API_TOKENS`) for a user's persona. A Cairn error response SHALL be shown
to the user verbatim, with the field and reason the instance returned.

#### Scenario: A token that does not authenticate

- **WHEN** the pasted token is rejected by Cairn
- **THEN** `init` reports that the token did not authenticate, does not write it,
  and asks again (or exits `1` non-interactively)

#### Scenario: No Cairn

- **WHEN** no Cairn URL is given
- **THEN** personas are written without Cairn configuration, and templates that
  require Cairn are refused with a message naming the missing URL

### Requirement: REQ-9 Owner Selection

`harness init` SHALL create every resource owned by the signed-in user unless
`--owner team:<slug>` is given. With `--owner team:<slug>`, `init` SHALL pass the
team as the owner on every create call to an instance that advertises teams
(Switchboard SPEC-0033, Cairn SPEC-0023), and SHALL fail before any create call
when an instance does not advertise teams or the user is not permitted to
create resources for that team. `init` SHALL NOT create any resource without an
owner.

#### Scenario: Team on an instance without teams

- **WHEN** `init --owner team:platform` runs against an instance whose
  Switchboard does not advertise teams
- **THEN** it fails before vending anything, and says the instance does not
  support team ownership

#### Scenario: A team the user is not in

- **WHEN** the instance answers a team-owned create with a permission error
- **THEN** `init` stops, reports the team and the refusal, and leaves no
  partially written persona

### Requirement: REQ-10 Claude Code Auth

For personas using `claude-code`, `harness init` SHALL offer three modes:
`keychain`, `subscription-token` and `api-key`. `keychain` SHALL be offered only
on macOS and SHALL write no credential. `subscription-token` SHALL instruct the
user to run `claude setup-token`, read the resulting token from a hidden prompt,
`--claude-oauth-token-file`, or `CLAUDE_CODE_OAUTH_TOKEN`, and write it as
`CLAUDE_CODE_OAUTH_TOKEN` to `$XDG_CONFIG_HOME/harness/env/claude.env`, mode
`0600`, which every Claude Code persona's `env_file` list includes. `api-key`
SHALL state before writing that the key bills the API and not a subscription.
`init` SHALL validate a token's prefix and length only, and SHALL NOT print any
part of it beyond its prefix.

#### Scenario: Headless Linux

- **WHEN** a user on Linux chooses `subscription-token` and pastes a token
- **THEN** `claude.env` holds `CLAUDE_CODE_OAUTH_TOKEN`, mode `0600`, and each
  Claude Code drop-in's `env_file` lists `claude.env` before the persona's own
  env file

#### Scenario: A pasted API key in the token prompt

- **WHEN** the value pasted into the subscription-token prompt starts `sk-ant-api`
- **THEN** `init` rejects it, says it is an API key, and offers the `api-key`
  mode instead

#### Scenario: Keychain on macOS

- **WHEN** a macOS user chooses `keychain`
- **THEN** no Claude credential is written, and `init` notes that the daemon must
  run in the user's GUI session for the Keychain to be reachable

### Requirement: REQ-11 Claude Code One-Shot Keys

A harness with `harness = "claude-code"` and `prompt` or `prompt_file` SHALL
accept three optional keys. Each SHALL be folded into the synthesized argv at
spawn, before the prompt, and SHALL NOT be desugared into `args`:

| Key | Type | Synthesized |
| --- | --- | --- |
| `system_prompt_file` | path | `--append-system-prompt-file <resolved path>` |
| `mcp_config` | path | `--mcp-config <resolved path> --strict-mcp-config` |
| `allowed_tools` | list of strings | `--allowedTools <t1> <t2> …` |

Paths SHALL resolve against the declaring file, exactly as `prompt_file` does
(ADR-0018). `system_prompt_file` and `mcp_config` SHALL be checked for existence
at load, and a missing file SHALL be a located config error. Each key SHALL be a
config error on any other adapter, on a harness without a prompt source, when
blank, and, for `allowed_tools`, when empty, or when an entry is blank or
begins with `-` (which the CLI would parse as a flag). Each `allowed_tools`
entry SHALL be passed as its own argv element, so a pattern such as
`Bash(git log:*)` survives intact. The three keys SHALL be emitted after the
existing `--dangerously-skip-permissions`, `--model` and `--max-turns` flags and
before `--verbose`, so every variadic flag is followed by another flag. `args`
SHALL remain mutually exclusive with `prompt` and `prompt_file`.

#### Scenario: A reviewer one-shot

- **WHEN** a harness sets `prompt_file`, `system_prompt_file = "system.md"`,
  `mcp_config = "mcp.json"` and `allowed_tools = ["Read", "mcp__switchboard"]`
- **THEN** the spawned argv is `claude -p [auto_accept, model and max_turns
  flags] --append-system-prompt-file <dir>/system.md --mcp-config <dir>/mcp.json
  --strict-mcp-config --allowedTools Read mcp__switchboard --verbose
  --output-format stream-json <prompt>`

#### Scenario: A flag smuggled in as a tool

- **WHEN** `allowed_tools = ["--dangerously-skip-permissions"]`
- **THEN** config parsing fails, naming the harness, `allowed_tools` and the
  entry

#### Scenario: On a resident harness

- **WHEN** a harness sets `mcp_config` with `args` and no prompt
- **THEN** config parsing fails, naming the harness and `mcp_config`, and says
  a long-running harness passes `--mcp-config` through its own `args`

#### Scenario: On another adapter

- **WHEN** a `harness = "crush"` prompt harness sets `allowed_tools`
- **THEN** config parsing fails, naming the key and the `claude-code` adapter

#### Scenario: Missing file

- **WHEN** `system_prompt_file` names a file that does not exist
- **THEN** config parsing fails with the harness name, the key and the resolved
  path

### Requirement: REQ-12 Env File Lists

`env_file` SHALL accept a string or a non-empty list of strings. A list SHALL be
loaded in order, with a later file's value winning on a key collision, and the
merged result SHALL be appended to the daemon environment exactly as a single
file is today. A missing file in a list SHALL be tolerated as a missing string
`env_file` is today. A string SHALL keep its current meaning.

#### Scenario: Shared and per-persona files

- **GIVEN** `env_file = ["claude.env", "reviewer.env"]` and both define
  `CLAUDE_CODE_OAUTH_TOKEN`
- **WHEN** the harness spawns
- **THEN** the child sees `reviewer.env`'s value

#### Scenario: An empty list

- **WHEN** a harness sets `env_file = []`
- **THEN** config parsing fails, naming the harness and `env_file`

### Requirement: REQ-13 Client MCP Configuration

For each persona, `harness init` SHALL write the client's MCP configuration for
Switchboard, and Cairn when configured, with every credential expressed as a
reference that the client resolves from the environment the persona's `env_file`
supplies. No client configuration file `init` writes SHALL contain a token.

| Client | File | Reference form |
| --- | --- | --- |
| `claude-code` | `$XDG_CONFIG_HOME/harness/personas/<p>/mcp.json` | `${SWITCHBOARD_TOKEN}` in a header value |
| `crush` | `crush.json` in the persona's workdir | `$SWITCHBOARD_TOKEN` |
| `codex` | a managed block in the persona's Codex configuration | `bearer_token_env_var = "SWITCHBOARD_TOKEN"` |
| `pi-omp` | `$XDG_CONFIG_HOME/harness/personas/<p>/mcp.json`, exposed to the argv template | environment |

A `claude-code` one-shot persona SHALL reference its `mcp.json` through
`mcp_config`. A resident `claude-code` persona SHALL reference it through `args`
(`--mcp-config`, `--strict-mcp-config`). A resident persona that listens for
doorbells SHALL additionally carry the development-channels flag in `args`, and
`init` SHALL enable the auto-confirm SPEC-0023 defines for that server when the
running binary supports it, and say so. A persona's workdir `crush.json` that
`init` did not create SHALL NOT be modified (REQ-5); `init` SHALL print the
block to merge instead.

`init` SHALL modify the user's global client configuration only with
`--connect-interactive-session`, and then only through the client's own CLI (for
Claude Code, `claude mcp add --scope user`), still with references rather than
tokens.

#### Scenario: No literal token

- **WHEN** `init` has written three personas across two clients
- **THEN** a search of every file `init` wrote, except env files, for each
  token value finds nothing

#### Scenario: Connecting the interactive session

- **WHEN** a user passes `--connect-interactive-session` with `claude-code`
- **THEN** `init` runs `claude mcp add --scope user` for `switchboard` with a
  `${SWITCHBOARD_TOKEN}` header, and tells the user which env file to source

### Requirement: REQ-14 Skill Installation

`harness init` SHALL install the Switchboard, Cairn and Harness skills for each
client from refs pinned in the manifest. For `claude-code` it SHALL use `claude
plugin marketplace add` and `claude plugin install` from the public GitHub
repositories. For `crush` it SHALL fetch the pinned ref into
`$XDG_DATA_HOME/harness/skills/<plugin>@<ref>/` and add its `skills` directory to
the persona's `options.skills_paths`. A failed skill install SHALL be reported
as a warning naming the plugin, and SHALL NOT fail the persona.

#### Scenario: Pinned fetch for Crush

- **WHEN** `init` installs skills for a Crush persona
- **THEN** the persona's `crush.json` lists
  `.../skills/claude-plugin-switchboard@<ref>/skills`, and `<ref>` is the
  manifest's pin, not a branch name

### Requirement: REQ-15 Stack Bundle

`harness stack init` SHALL write, into the bundle directory (default
`$XDG_CONFIG_HOME/harness/stack/`, overridable with `--dir`), a `compose.yaml`,
a Postgres initialization script, an object-store configuration, an optional
`Caddyfile`, `stack.lock.json`, and `secrets/*.env`, and SHALL start nothing.
The Compose project SHALL contain: one Postgres; Switchboard; Cairn; an
S3-compatible object store unless `--object-store external` supplies one; and
Caddy only with `--caddy`. Postgres SHALL hold one database and one role per
service, and each service SHALL connect as its own role, which owns only its own
database. `compose.yaml` SHALL contain no secret value; secrets SHALL reach
containers only through `env_file` entries under `secrets/`. Without Caddy,
every published port SHALL bind `127.0.0.1`. Postgres and the object store SHALL
publish no host port.

#### Scenario: Committable compose

- **WHEN** `stack init` has run
- **THEN** a scan of `compose.yaml`, `Caddyfile` and `stack.lock.json` for every
  generated secret value finds none

#### Scenario: Role isolation

- **WHEN** the stack is up
- **THEN** Switchboard's role cannot connect to Cairn's database

#### Scenario: Re-running stack init

- **GIVEN** an existing bundle
- **WHEN** `stack init` runs again with a changed base URL
- **THEN** it shows a diff of the affected files, keeps every existing secret
  file byte-identical, and writes only after confirmation

### Requirement: REQ-16 Manifest And Pinning

Each Harness release SHALL embed a manifest listing, per component (Postgres,
Switchboard, Cairn, the object store, Caddy), an image repository, a tag, a
digest and a minimum version, plus the pinned refs of the skill plugins and
upgrade notes per component version. The build SHALL fail when an entry's tag
is `latest`, is absent, or has no digest. `compose.yaml` SHALL reference every
image by `repository:tag@digest`. `stack.lock.json` SHALL record the manifest
version and every digest in use. `harness stack` SHALL NOT accept an image
override that lacks a digest.

#### Scenario: A latest tag in the manifest

- **WHEN** a manifest change sets Switchboard's tag to `latest`
- **THEN** `make test` fails, naming the component

#### Scenario: An override without a digest

- **WHEN** an operator runs `stack init --image switchboard=ghcr.io/stump-wtf/switchboard:v0.3.1`
- **THEN** the command fails and asks for the `@sha256:` digest

### Requirement: REQ-17 Generated Secrets

`harness stack init` SHALL generate every infrastructure secret (Postgres role
passwords, Switchboard's secret-encryption key and metrics token, object-store
credentials, and any Caddy credential) from a cryptographically secure source,
write each to a file under `secrets/` with mode `0600` in a directory with mode
`0700`, and SHALL NOT overwrite an existing secret file. No `harness stack`
subcommand SHALL print a secret value to stdout, stderr, a log, or `--json`
output. Where a secret is shown, it SHALL be shown as the first 12 hexadecimal
characters of its SHA-256.

#### Scenario: Secret file already present

- **GIVEN** `secrets/switchboard.env` was written by a secrets manager
- **WHEN** `stack init` runs
- **THEN** the file is unchanged and `stack init` reports it as supplied

#### Scenario: Loose permissions

- **WHEN** a secret file is found group- or world-readable
- **THEN** `stack doctor` fails that check and names the file, without printing
  its contents

### Requirement: REQ-18 Identity Provider

`harness stack init` SHALL require an identity provider for human login: an OIDC
issuer with client ID and secret, or a GitHub OAuth application. It SHALL
configure both Switchboard and Cairn with it. It SHALL NOT enable either
product's development login under any flag.

#### Scenario: No identity provider

- **WHEN** `stack init --non-interactive` runs without identity-provider answers
- **THEN** it exits `2` and names the OIDC and GitHub answers

### Requirement: REQ-19 Optional Caddy

With `--caddy`, `harness stack init` SHALL generate a `Caddyfile` that serves
Switchboard and Cairn on the configured hostnames with automatic TLS, disables
response buffering on Switchboard's `/mcp/*` routes (`flush_interval -1`), and
passes the client address in `X-Forwarded-For` from a trusted proxy. The base
URLs written for Switchboard and Cairn SHALL be the `https://` hostnames Caddy
serves.

#### Scenario: Streaming preserved

- **WHEN** the stack is up behind the generated Caddy
- **THEN** a doorbell reaches a connected MCP session within one second of the
  todo being created, rather than when the connection closes

### Requirement: REQ-20 Stack Up

`harness stack up` SHALL pull the pinned images, start the services, and wait
for each service's health check up to `--timeout` (default 5 minutes). It SHALL
then verify that every running container's image digest equals the lock, and
that each service's reported version equals the manifest's tag where the service
reports one. On success it SHALL run `harness init --connect` against the
bundle's URLs (skippable with `--no-init`) and then the self-test (REQ-25,
skippable with `--no-selftest`). It SHALL exit non-zero if any step fails, and
SHALL name the failed step and service.

#### Scenario: Digest drift

- **GIVEN** a container running an image whose digest differs from the lock
- **WHEN** `stack up` verifies the stack
- **THEN** it fails, names the service, both digests, and suggests `stack
  upgrade` or re-pulling

#### Scenario: A service never becomes healthy

- **WHEN** Cairn's health check does not pass within the timeout
- **THEN** `stack up` exits `1`, names Cairn, prints the last 50 lines of its
  container log with any secret-looking value masked, and does not run `init`

### Requirement: REQ-21 Stack Down

`harness stack down` SHALL stop and remove the bundle's containers and SHALL
keep its volumes and secrets. `--volumes` SHALL also remove the volumes, only
after the operator types the bundle's project name to confirm (or passes
`--yes-destroy-data` non-interactively). `harness stack down` SHALL NOT delete
any file under `secrets/`.

#### Scenario: Data kept by default

- **WHEN** an operator runs `stack down` and then `stack up`
- **THEN** every endpoint, todo and artifact created before is still present

### Requirement: REQ-22 Stack Status

`harness stack status` SHALL report, per service: its state, the pinned digest,
the running digest, the reported version, and its URL; per generated secret: its
file and fingerprint; and the result and time of the last self-test. `--json`
SHALL emit the same fields as a stable JSON document.

#### Scenario: Status never prints a secret

- **WHEN** `stack status --json` runs
- **THEN** the output contains fingerprints and no secret value

### Requirement: REQ-23 Stack Upgrade

`harness stack upgrade` SHALL move the bundle to the manifest embedded in the
running binary. It SHALL print the plan (each component's current and target
version) and the upgrade notes between them, and ask for confirmation (or
`--yes`). Before starting any service at a new version it SHALL write a
`pg_dump` of each database to `$XDG_STATE_HOME/harness/stack/backups/<timestamp>/`
with mode `0600`. It SHALL upgrade one service at a time, waiting for its health
check, and SHALL then re-run the verification of REQ-20 and the self-test. A
step the manifest marks breaking SHALL require `--accept-breaking`. A target
older than the lock SHALL be refused. When a service fails to become healthy
after its upgrade, `upgrade` SHALL restore that service's previous image, stop,
and report the backup path.

#### Scenario: A breaking step

- **GIVEN** the target Switchboard version drops a configuration the bundle uses
- **WHEN** `stack upgrade` runs without `--accept-breaking`
- **THEN** it prints the note, changes nothing, and exits `3`

#### Scenario: A Postgres major version

- **WHEN** the target manifest moves Postgres to a new major version
- **THEN** `upgrade` treats it as breaking and prints the dump-and-restore
  procedure rather than starting the new major on the old data directory

#### Scenario: Failed service upgrade

- **WHEN** Switchboard fails its health check after the image change
- **THEN** the previous Switchboard image is running again, the backup path is
  printed, and later services were not upgraded

### Requirement: REQ-24 Stack Doctor

`harness stack doctor` SHALL check the host (Docker and Compose present and
supported, ports free, disk space), the bundle (secret file modes, lock versus
running digests, one database and role per service, Switchboard's encryption key
set, the metrics token length, reverse-proxy streaming when Caddy is enabled),
and the connected personas (each credential still authenticates). It SHALL print
each check as `ok`, `warn` or `fail` with a one-line next step, and SHALL exit
`0` when no check fails, `1` otherwise. `--e2e` SHALL add the self-test. When the
connected Switchboard offers the doorbell `doctor` of SPEC-0025, `stack doctor`
SHALL call it for each resident persona's endpoint and report its result.

#### Scenario: Encryption key unset

- **WHEN** Switchboard's encryption key is empty in its secret file
- **THEN** doctor fails that check and says self-managed webhook secrets would be
  stored in plaintext

### Requirement: REQ-25 End-To-End Self-Test

The self-test SHALL run as the last step of `stack up` and on `harness stack
doctor --e2e` and `harness init --verify`. It SHALL:

1. vend or reuse an endpoint named `harness-selftest` on queue
   `harness-selftest`, owned by the signed-in user;
2. write a one-shot `harness-selftest` harness with the persona client and model
   the user chose, bound to that endpoint as a SPEC-0014 channel source;
3. POST a JSON delivery carrying a fresh 128-bit random nonce to that endpoint's
   token-trust ingest URL;
4. wait up to `--selftest-timeout` (default 5 minutes) for the run to exit;
5. read the todo back as that endpoint with `list_todos`, require state `done`,
   and take the Cairn handle from its result;
6. read that artifact from Cairn, and require that its body contains the nonce
   and that it carries the tag `harness-selftest`;
7. require that the run's record shows exit 0 and trigger `channel`.

The prompt SHALL instruct the agent to claim the todo, create a Cairn artifact
containing the nonce with tag `harness-selftest` and a one-hour TTL, and complete
the todo with the artifact's handle. Each step SHALL report `pass`, `fail` or
`not verified`, with the first failure naming the component and the next check.
When the running binary cannot hold a channel source, step 2 SHALL start the
one-shot directly after step 3, and the push path SHALL be reported `not
verified` rather than `pass`. When no Cairn is configured, steps 5 and 6 SHALL
require the todo's result to contain the nonce instead. The self-test SHALL NOT
pass on health checks, delivery logs, or a zero exit alone.

#### Scenario: The whole loop

- **WHEN** the self-test runs on a working stack
- **THEN** every step reports `pass`, and the report names the todo ID, the run
  ID and the artifact handle

#### Scenario: The agent completes without writing the artifact

- **WHEN** the agent completes the todo with a result that names no artifact
- **THEN** step 5 fails and says the agent did not report a Cairn handle

#### Scenario: A forged artifact

- **WHEN** the handle in the result names an artifact whose body lacks the nonce
- **THEN** step 6 fails, even though the todo is `done` and the run exited 0

#### Scenario: Nobody hears the doorbell

- **WHEN** the channel source is not connected and no run starts within the
  timeout
- **THEN** step 4 fails, names the channel source's state, and the todo remains
  `pending` for inspection

### Requirement: REQ-26 Bundle Ownership

`harness stack` SHALL act only on a bundle whose `stack.lock.json` it wrote, and
only on the Compose project that lock names. Pointed at a directory without a
lock, every subcommand other than `init` SHALL fail with an error naming the
directory. It SHALL NOT act on Compose projects it did not create.

#### Scenario: A foreign directory

- **WHEN** an operator runs `stack status --dir ~/some-other-compose`
- **THEN** the command fails, says no Harness bundle lives there, and touches no
  container

### Requirement: REQ-27 No Instance-Global User Resources

`harness init` and `harness stack` SHALL NOT create any user-facing resource that
no user or team owns. In particular, `harness stack init` SHALL NOT set Cairn's
instance-wide outbound webhook list, SHALL NOT generate a static Cairn token for
any user, SHALL NOT generate any credential that can act for users in
Switchboard, and SHALL NOT write to either product's database directly.

#### Scenario: Handoff routing without a global webhook

- **WHEN** a user chooses handoff routing between Cairn and Switchboard during
  `init`
- **THEN** `init` configures it through a per-owner webhook where the instance
  supports one, and otherwise reports the routing as unavailable, and in neither
  case writes an instance-wide outbound webhook

### Requirement: REQ-28 Discovery Document

With `--publish-discovery`, `harness stack init` SHALL generate a JSON document
served at `/.well-known/harness-stack.json` on the Switchboard hostname, listing
the Switchboard and Cairn base URLs, each component's version, and whether teams
are supported. It SHALL contain no secret and no user data. `harness init
--connect <url>` SHALL fetch it when present and use its URLs as defaults.

#### Scenario: One URL to connect

- **WHEN** a user runs `harness init --connect https://sb.example.com` against a
  stack publishing the document
- **THEN** `init` proposes the Cairn URL from the document without asking for it

#### Scenario: No document

- **WHEN** the document is absent
- **THEN** `init` asks for the Cairn URL (or reads it from its answers) and
  continues

### Requirement: REQ-29 Error Handling Standards

All error-producing operations MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary
  (for example, "vend reviewer: POST /api/v1/endpoints: 401 unauthorized").
- Sentinel errors MUST be defined for failure modes callers distinguish:
  missing answers, a lost credential, an unsupported owner, digest drift, and a
  failed self-test step.
- Silent error swallowing MUST NOT occur. A skill-install failure is a reported
  warning, not a dropped error.
- Structured logging MUST be used, and no log entry may carry a secret value.

#### Scenario: A located failure

- **WHEN** a vend call returns `401`
- **THEN** the error names the persona, the method and path, and the status, and
  carries no token

## Security Requirements

This capability runs short-lived local HTTP listeners (the OAuth loopback
callback) and makes authenticated calls to two network services. It serves no
public route of its own; the optional discovery document is a static file served
by the bundle's Caddy.

### Authentication

Every call to Switchboard and Cairn SHALL carry the user's own credential:
Switchboard's OAuth access token for the operator API, an endpoint token for
`list_todos`, and a Cairn token for reads. No call SHALL use an instance-level
credential.

### Rate Limiting

`harness init` SHALL make calls serially per instance, and SHALL honor `429`
with `Retry-After` from either service by waiting, up to the command's
timeout, rather than retrying immediately.

### Security Headers

Not applicable to the loopback listener's single response page, which SHALL
nonetheless send `Content-Security-Policy: default-src 'none'` and
`Cache-Control: no-store`, because it is shown in a browser.

### Request Body Size Limits

The loopback listener SHALL read no request body. Responses from Switchboard and
Cairn SHALL be read with a 1 MiB cap.

### CSRF Protection

The loopback listener SHALL bind `127.0.0.1` on an ephemeral port, SHALL accept
only a `GET` to its callback path carrying the `state` value it generated, and
SHALL ignore any other request without aborting the login. It SHALL close after
the first valid callback or the login timeout.

### Redirect Validation

The listener SHALL NOT redirect. HTTP clients SHALL NOT follow a redirect to a
different scheme or host than the configured instance URL, so a redirect cannot
move a bearer token to another origin.
