---
status: draft
date: 2026-09-22
implements: [ADR-0026]
extends: [SPEC-0006, SPEC-0013]
requires: [SPEC-0002, SPEC-0003, SPEC-0008]
---

# SPEC-0020: Fail-Closed Model Pinning

## Overview

A harness MAY declare a **model pin**: the model, the route, the upstream
providers allowed to serve it, and the data policy the request must carry, with
fallbacks disallowed. The daemon renders the pin into the harness's client
configuration at spawn (Crush, Pi/OMP, Claude Code, or a LiteLLM gateway
entry), scrubs every credential the route does not need from the child's
environment, and attests every model call from the client's own transcript.
The first call served by the wrong model or provider stops the run, which is
recorded `model_mismatch` and, by default, holds the harness. `harness doctor
--models` proves the pin end to end, including the negative case that the
route refuses rather than substitutes.

See ADR-0026 for the decision. This spec extends SPEC-0006 (adapters gain a pin
renderer and an attestation capability) and SPEC-0013 (attestation series). It
requires SPEC-0002 (protocol fields and events), SPEC-0003 (the state machine
for a held or failed harness), and SPEC-0008 (run records and skip reasons).

Requirements are numbered. Cite them as `SPEC-0020 REQ-n`.

## Requirements

### Requirement: REQ-1 — Model Pin Table

A `[harness.<name>.model_pin]` sub-table SHALL accept these keys:

| Key | Type | Required | Meaning |
| --- | --- | --- | --- |
| `route` | string | yes | `openrouter`, `anthropic` or `litellm` |
| `model` | string | yes | The upstream model ID the pin approves; one token, no whitespace |
| `providers` | array of strings | `openrouter`: yes; `anthropic`: rejected; `litellm`: optional | Upstream provider slugs allowed to serve, in preference order |
| `allow_fallbacks` | bool | no (default `false`) | MUST be `false` |
| `data_policy` | string | no (default `provider-default`) | `zdr`, `deny-collection` or `provider-default` |
| `attest` | string | no (default `full`) | `full` (model and provider) or `model` |
| `on_mismatch` | string | no (default `hold`) | `hold` or `fail-run` |
| `accept_served` | array of strings | no | Additional exact served-model IDs to accept (REQ-9) |
| `gateway_model` | string | `litellm`: yes; otherwise rejected | The gateway alias the client requests |
| `gateway_url` | string | `litellm`: yes; otherwise rejected | The gateway's base URL; `https` unless loopback |

A harness with `model_pin` SHALL NOT also set `model`. The error SHALL say that
the pin supplies the model. `model_pin` SHALL be valid on resident and one-shot
harnesses alike. `allow_fallbacks = true` SHALL fail validation, stating that a
pin that allows fallbacks is not a pin. For `route = "openrouter"`, `model`
SHALL NOT contain `:` (routing variants) and SHALL NOT be an auto-router
(`openrouter/auto`). Each provider slug SHALL match
`^[a-z0-9][a-z0-9-]*(/[a-z0-9-]+)?$`. For `route = "anthropic"`, `data_policy`
SHALL be `provider-default`, because zero data retention there is an account
agreement Harness cannot request.

A project `harness.toml` MAY declare `model_pin` on its own harnesses.

#### Scenario: A complete OpenRouter pin

- **WHEN** an `omp` harness declares `route = "openrouter"`,
  `model = "z-ai/glm-5.3-flash"`, `providers = ["deepinfra"]` and
  `data_policy = "zdr"`
- **THEN** the config loads with `attest = "full"` and `on_mismatch = "hold"`

#### Scenario: A pin and a model

- **WHEN** a harness declares both `model` and `model_pin`
- **THEN** config validation fails, naming both keys

#### Scenario: Fallbacks allowed

- **WHEN** a pin declares `allow_fallbacks = true`
- **THEN** config validation fails with a located error

#### Scenario: Auto-router

- **WHEN** an OpenRouter pin declares `model = "openrouter/auto"`
- **THEN** config validation fails, stating that the pin must name one model

#### Scenario: ZDR on the Anthropic route

- **WHEN** an `anthropic` pin declares `data_policy = "zdr"`
- **THEN** config validation fails, stating that the route cannot carry the
  policy

### Requirement: REQ-2 — Client And Route Support

Each adapter SHALL declare which routes its renderer offers. A pin on a client
and route pair the adapter does not offer SHALL fail config validation, naming
the client, the route and the offered alternatives. In v1:

| Adapter | `openrouter` | `anthropic` | `litellm` |
| --- | --- | --- | --- |
| `crush` | offered | not offered | offered |
| `pi`, `omp` | offered if the client's models configuration carries provider preferences (see design); otherwise not offered | not offered | offered |
| `claude-code` | not offered | offered | offered |
| `codex` | not offered | not offered | not offered |
| `generic` | not offered | not offered | not offered |
| `command` | attestation only | attestation only | attestation only |

A pin on a `command` harness SHALL require `transcripts` (SPEC-0017 REQ-4). It
SHALL render nothing, and SHALL supply `model_pin.model` as the `{{model}}`
template value. The operator's argv owns routing. The daemon only attests.

#### Scenario: Codex pin

- **WHEN** a `codex` harness declares a model pin
- **THEN** config validation fails, naming `codex` and stating that no route is
  offered in this version

#### Scenario: Claude Code on OpenRouter

- **WHEN** a `claude-code` harness declares `route = "openrouter"`
- **THEN** config validation fails, listing `anthropic` and `litellm`

#### Scenario: Command pin without transcripts

- **WHEN** a `command` harness declares a model pin and no `transcripts`
- **THEN** config validation fails, stating that nothing could be attested

### Requirement: REQ-3 — Rendering

At every spawn of a pinned harness, the adapter's renderer SHALL write the
client's native configuration for the pin under
`$XDG_STATE_HOME/harness/pins/<harness>/`. Directories SHALL be `0700` and files
`0600`. The renderer SHALL point the client at that configuration using the
highest-precedence mechanism the client offers: an adapter-owned flag, an
environment variable the daemon sets, or a generated directory. The rendering
SHALL:

* select exactly `model` on the route;
* for `openrouter`, carry the provider preferences `{only: providers, order:
  providers, allow_fallbacks: false}`, plus `data_collection: "deny"` for `zdr`
  and `deny-collection`, and `zdr: true` for `zdr`;
* never carry a model-fallback list, an auto-router, or a variant suffix;
* reference credentials by environment variable name only, never by value.

The renderer SHALL NOT modify any file outside the harness's pins directory.
When it layers over a user-owned client file that the client would otherwise
read at the same precedence, it SHALL read that file and write a merged copy
into the pins directory. The source file SHALL be left untouched. Removing
`model_pin` SHALL stop the rendering, and the pins directory SHALL be removed at
the next spawn.

`harness pin render <name>` SHALL print what the next spawn would write and
set, with credential values replaced by their variable names, and SHALL exit
non-zero on any REQ-2, REQ-4 or REQ-5 error.

#### Scenario: Crush on OpenRouter

- **WHEN** a pinned `crush` harness spawns against a stand-in route that records
  request bodies
- **THEN** every chat request carries `model = z-ai/glm-5.3-flash` and
  `provider.only = ["deepinfra"]`, `provider.allow_fallbacks = false`,
  `provider.data_collection = "deny"` and `provider.zdr = true`

#### Scenario: No secrets in rendered files

- **WHEN** a pinned harness whose `env_file` holds `OPENROUTER_API_KEY` spawns
- **THEN** no file under its pins directory contains the key's value

#### Scenario: The user's file is untouched

- **WHEN** a pinned harness spawns and the user has a global client config
- **THEN** that file's bytes and mtime are unchanged afterwards

#### Scenario: Preview

- **WHEN** an operator runs `harness pin render implementer`
- **THEN** the output shows the rendered files and variables, with
  `OPENROUTER_API_KEY` shown by name only

### Requirement: REQ-4 — Overrides The Renderer Cannot Beat

Each renderer SHALL declare the client configuration sources that take
precedence over its own mechanism: for example, a project-level config file in
the workdir or its parents. At config load, and again at each spawn, the
daemon SHALL check those sources for any key that sets the model, the provider
preferences, the route's base URL, or a fallback. It SHALL fail when it finds
one. At load that is a located config error. At spawn the start fails, and a
one-shot's run records `failed`. The error SHALL name the overriding file and
key.

#### Scenario: A workdir config overrides the provider

- **WHEN** a pinned `crush` harness's workdir contains a `crush.json` that sets
  the OpenRouter provider's `extra_body`
- **THEN** the load fails, naming that file and key, and nothing spawns

#### Scenario: Override added after load

- **WHEN** such a file appears after the config loaded
- **THEN** the next spawn fails with the same error, and the run is recorded
  `failed`

### Requirement: REQ-5 — Claude Code Argument Guard

For a pinned `claude-code` harness, the adapter SHALL place `--model <model>`
(or `--model <gateway_model>` for `litellm`) in the argv it owns, for resident
and one-shot harnesses alike. `args` containing `--model` or `--fallback-model`,
in either the separate or the `=` form, SHALL fail config validation. For
`route = "anthropic"`, a spawn whose composed environment (the daemon's
environment plus `env_file`) sets `ANTHROPIC_BASE_URL` SHALL fail, because the
provider could no longer be attested as first party. For `route = "litellm"`,
the daemon SHALL set `ANTHROPIC_BASE_URL` to `gateway_url`.

#### Scenario: Fallback model in args

- **WHEN** a pinned claude-code harness declares
  `args = ["--fallback-model", "claude-haiku-4-5"]`
- **THEN** config validation fails, naming `--fallback-model`

#### Scenario: A stray base URL

- **WHEN** a claude-code harness pinned to `anthropic` has
  `ANTHROPIC_BASE_URL` in its `env_file`
- **THEN** the start fails with an error naming the variable, and nothing is
  exec'd

### Requirement: REQ-6 — Credential Scrubbing

The child environment of a pinned harness SHALL exclude every variable in the
scrub list (see design) except the variables the route needs. This SHALL apply
whether the variable came from the daemon's environment or from `env_file`.
The scrub list SHALL cover at least the Anthropic, OpenAI, OpenRouter, Google,
Groq, Mistral, xAI, DeepSeek, Together, Fireworks, AWS Bedrock and Azure OpenAI
credential and base-URL variables. A scrubbed variable SHALL be logged by name
once per spawn at debug level, and SHALL never be logged by value. There SHALL
be no key that disables scrubbing.

#### Scenario: A second key is removed

- **WHEN** a harness pinned to `openrouter` has both `OPENROUTER_API_KEY` and
  `ANTHROPIC_API_KEY` in its environment
- **THEN** the child sees `OPENROUTER_API_KEY` and not `ANTHROPIC_API_KEY`

#### Scenario: The route key is missing

- **WHEN** the route's credential variable is absent
- **THEN** the client runs with no provider credential at all, fails with its
  own error, and the run records `failed`. No other credential is available to
  it

### Requirement: REQ-7 — Gateway Entry

For `route = "litellm"`, `harness pin render <name> --target litellm` SHALL
print a LiteLLM `model_list` entry that implements the pin. Its `model_name`
SHALL be `gateway_model`. Its upstream `model` and credential reference SHALL be
taken from the pin, and `extra_body.provider` SHALL carry REQ-3's preferences
when `providers` is set. The output SHALL state, as a comment, that no
`fallbacks`, `context_window_fallbacks` or load-balancing group may name
`gateway_model`. The daemon SHALL NOT read, write or manage the gateway's
configuration. REQ-16's gateway case is the verification.

#### Scenario: Rendered gateway entry

- **WHEN** an operator renders a `litellm` pin with `providers = ["deepinfra"]`
- **THEN** the entry carries `extra_body.provider.only = ["deepinfra"]` and
  `allow_fallbacks: false`, and the output names `gateway_model` as
  fallback-free

### Requirement: REQ-8 — Attestation Evidence And Capability

The daemon SHALL attest each model call of a pinned harness from **evidence**:
the served model and, where recorded, the served upstream provider and
generation ID, taken from agent-trace per-message usage items
(stump.wtf/agent-trace#105) for the harness's transcript source. Each adapter
SHALL declare its evidence capability per route: `model`, or `model+provider`.
For `openrouter`, an adapter whose transcript records a generation ID but no
provider MAY declare `model+provider` through a **generation lookup**. That is
a request to the route's generation-metadata endpoint using the harness's own
route credential, made by the daemon after the call is observed.

A pin with `attest = "full"` on an adapter and route whose capability is only
`model` SHALL fail config validation. The error SHALL name the gap and offer
`attest = "model"`. For `route = "anthropic"` with `ANTHROPIC_BASE_URL` unset,
the provider SHALL be attested as first party by construction.

#### Scenario: Full attestation the client cannot give

- **WHEN** a pin with `attest = "full"` is declared on a client whose transcripts
  record no provider and no generation ID
- **THEN** config validation fails, naming the missing evidence and suggesting
  `attest = "model"`

#### Scenario: Generation lookup

- **WHEN** a pinned harness's transcript records generation ID `gen-1` for a
  call and no provider
- **THEN** the daemon resolves the provider for `gen-1` through the route and
  attests it. A failed lookup makes that call unattested (REQ-11), never
  attested

### Requirement: REQ-9 — Served Model And Provider Matching

A call's served model SHALL match when, after removing a leading `<route>/`
prefix, it equals `model` exactly, equals `model` followed by `-` and an
eight-digit date, or equals an entry of `accept_served` exactly. A served
provider SHALL match when its normalized form equals the normalized base of an
entry in `providers`. Normalized means lowercased with every character outside
`[a-z0-9]` removed, and base means the slug before any `/`. No other matching,
including substring, prefix beyond the route prefix, or case-folded model IDs,
SHALL be used.

#### Scenario: Dated model ID

- **WHEN** a pin approves `claude-opus-5` and a call reports
  `claude-opus-5-20260915`
- **THEN** the model matches

#### Scenario: Look-alike model

- **WHEN** a pin approves `z-ai/glm-5.3-flash` and a call reports
  `z-ai/glm-5.3-flash-lite`
- **THEN** the model does not match

#### Scenario: Provider display name

- **WHEN** a pin allows `deepinfra/fp8` and a call reports provider `DeepInfra`
- **THEN** the provider matches

### Requirement: REQ-10 — Continuous Attestation

The daemon SHALL feed each observed call of a pinned harness to that harness's
pin checker as the observer delivers it. On the first call whose model or
provider does not match (REQ-9), the daemon SHALL stop the harness's process
group by the SPEC-0003 graceful stop (signal, grace, kill). It SHALL NOT wait
for the current turn to finish. For a run with a record, the outcome SHALL be
`model_mismatch`. For a resident harness, the state SHALL be `failed` with
reason `model_mismatch`, and the restart policy SHALL NOT restart it. A
mismatch SHALL never be retried automatically.

#### Scenario: Substitution mid-run

- **WHEN** a pinned one-shot's second call is served by provider `together`,
  which is not in `providers`
- **THEN** the process group is stopped, and the run records `model_mismatch`
  with kind `provider`, served provider `together`, and the served model

#### Scenario: Resident harness substituted

- **WHEN** a pinned resident crush harness observes a call served by another
  model
- **THEN** the harness is stopped, reads `failed` with reason `model_mismatch`,
  and is not restarted despite `restart = "always"`

### Requirement: REQ-11 — Final Sweep At Exit

When a pinned harness's process exits, the daemon SHALL wait until the observer
has read the harness's sessions up to the exit, or until 15 seconds pass,
whichever comes first. It SHALL then check every call from the run that was not
yet checked. A mismatch found in the sweep SHALL set the outcome to
`model_mismatch`, even after exit 0. With `attest = "full"`, a call lacking
provider evidence, or any call lacking model evidence, SHALL set the outcome to
`model_unattested` unless a mismatch was found. A sweep that times out with
calls unread SHALL set `model_unattested`. A run that made no model calls SHALL
keep the outcome its exit code gives.

#### Scenario: Mismatch caught after a clean exit

- **WHEN** a pinned one-shot exits 0, and its last call, not yet observed at
  exit, was served by the wrong model
- **THEN** the run records `model_mismatch`

#### Scenario: Evidence missing

- **WHEN** a pinned run with `attest = "full"` makes a call whose usage item
  carries no provider, and no generation lookup is possible
- **THEN** the run records `model_unattested`

#### Scenario: A run that never called a model

- **WHEN** a pinned one-shot exits 2 before making any model call
- **THEN** the run records `failed` with exit code 2

### Requirement: REQ-12 — Mismatch Hold

With `on_mismatch = "hold"`, a `model_mismatch` or `model_unattested` outcome on
a scheduled or triggered harness SHALL **hold** it. Every subsequent firing
SHALL be recorded `skipped` with reason `model_hold`, and no process SHALL
spawn. The hold SHALL persist in the state file across daemon restarts. An
explicit operator `harness start <name>` or `harness trigger <name>` SHALL
release it and SHALL log the release at WARN. `harness stop <name>` SHALL NOT
release it. With `on_mismatch = "fail-run"`, the run fails and later firings
proceed. A resident harness SHALL behave as REQ-10 describes under either value.

#### Scenario: Firings after a mismatch

- **WHEN** a pinned triggered harness's run records `model_mismatch`, and three
  more events fire it
- **THEN** each firing is recorded `skipped` with reason `model_hold` (coalesced
  as SPEC-0014 describes), and nothing spawns

#### Scenario: Hold survives a restart

- **WHEN** the daemon restarts while a harness is held
- **THEN** the harness is still held

#### Scenario: Release

- **WHEN** the operator runs `harness trigger implementer`
- **THEN** the hold is released, a WARN names the operator action, and the run
  starts

### Requirement: REQ-13 — Run Record Fields

The run record (SPEC-0008 REQ "Run History", and ADR-0028's run ledger once it
lands) SHALL gain the outcomes `model_mismatch` and `model_unattested`, and the
skip reason `model_hold`. A `model_mismatch` record SHALL carry `mismatch`:
`{kind: model|provider, served_model, served_provider, at}` for the first
mismatching call. Records SHALL NOT carry prompts, output, credentials or
generation-lookup responses (ADR-0008).

#### Scenario: A mismatch record

- **WHEN** run 9 records `model_mismatch`
- **THEN** `harness runs implementer` shows outcome `model_mismatch`, and
  `--json` carries `mismatch.kind`, `served_model` and `served_provider`

### Requirement: REQ-14 — Visibility

* `harness list` SHALL show a held harness, and a resident harness failed with
  `model_mismatch`, distinctly in its STATE column. It SHALL NOT add a column.
* `harness describe` SHALL show the pin, the rendered target paths, the
  capability (`model` or `model+provider`), the last attestation time, and the
  first mismatch.
* `harness doctor` SHALL show one row for model pins. The row is `fail` while
  any harness is held or failed with `model_mismatch`, `warn` while any pin has
  `attest = "model"` or a run recorded `model_unattested` since daemon start,
  and `pass` otherwise. The detail names the harnesses.
* The daemon SHALL emit a `model_mismatch` event carrying the harness, run ID,
  kind, served model and served provider. It SHALL emit `model_hold` and
  `model_hold_released` events. These additions SHALL bump the protocol minor
  version.

#### Scenario: doctor fails on a hold

- **WHEN** one harness is held
- **THEN** `harness doctor` shows the model-pins row as `fail`, naming it, and
  exits non-zero

#### Scenario: list stays within its columns

- **WHEN** a harness is held
- **THEN** `harness list` still has six columns, and the STATE cell identifies
  the hold

### Requirement: REQ-15 — Metrics

The daemon SHALL expose, under SPEC-0013's registry, listener and cardinality
cap:

```
harness_model_calls_attested_total{harness,outcome}   counter  outcome: attested|mismatch|unattested
harness_model_mismatch_total{harness,kind}            counter  kind: model|provider
harness_model_pin_held{harness}                       gauge    0|1
```

Only pinned harnesses SHALL have these series. `harness_model_pin_held` SHALL be
present, as `0`, for every pinned harness that is not held.

#### Scenario: Alerting on substitution

- **WHEN** a pinned harness records a provider mismatch
- **THEN** `harness_model_mismatch_total{kind="provider"}` increments and
  `harness_model_pin_held` reads `1`

### Requirement: REQ-16 — Canary

`harness doctor --models [NAME...]` SHALL probe every pinned harness, or the
named ones, and report one row per case:

| Case | Pass condition |
| --- | --- |
| `approved` | A direct request to the route, carrying the rendered preferences with `max_tokens = 1`, returns a completion whose model matches (REQ-9) and, where the route reports it, a provider that matches |
| `unavailable` | The same request with `providers` replaced by a provider that cannot serve the model returns an error and no completion |
| `data-policy` | For `zdr` or `deny-collection`, the route accepts the rendered policy fields in the `approved` request |
| `gateway` | For `litellm`, `approved` and `unavailable` succeed through `gateway_url` and `gateway_model` |

With `--through-client`, it SHALL also run the harness's real client, with the
rendered configuration and scrubbed environment, in a temporary workdir:

| Case | Pass condition |
| --- | --- |
| `client-approved` | The client answers a fixed trivial prompt, and its transcript attests (REQ-8, REQ-9) |
| `client-unavailable` | With `providers` replaced as above, the client exits non-zero, or its transcript holds no completion |
| `client-no-credential` | With the route credential removed, the client exits non-zero, and its transcript holds no completion |

A case that the route or client cannot express SHALL be reported `skip` with a
reason, never `pass`. For example, a direct probe for an `anthropic` route
whose credential is not an API key. The command SHALL exit non-zero when any
case fails.

#### Scenario: A route that substitutes

- **WHEN** the `unavailable` probe returns a completion
- **THEN** the case is `fail` with detail "route substituted", and the command
  exits non-zero

#### Scenario: Through the client

- **WHEN** `--through-client` runs for a pinned `omp` harness
- **THEN** OMP is run once per client case, and the approved run's transcript is
  attested by the same checker the daemon uses

### Requirement: REQ-17 — Canary Safety

The canary SHALL run only when `--models` is passed. Plain `harness doctor`
SHALL NOT make model requests. The canary SHALL read credentials from each
harness's `env_file`. It SHALL display each credential only as a variable name
and a 12-hex-character SHA-256 fingerprint, and it SHALL never print or log a
credential value, a request header, or a response body beyond the fields a case
checks. Each case SHALL time out (default 60 s, `--timeout`), and a timeout
SHALL be `fail`. Direct cases SHALL request at most one output token.
Through-client cases SHALL use a temporary workdir that is removed afterwards.
`--json` SHALL emit every case's result.

#### Scenario: No key in the output

- **WHEN** the canary runs with `--json`
- **THEN** neither stdout nor stderr contains the credential's value, and the
  credential appears only as a name and a fingerprint

#### Scenario: A hanging route

- **WHEN** the `unavailable` probe gets no response within the timeout
- **THEN** the case is `fail` with detail "timed out"

### Requirement: REQ-18 — Config Writers Round-Trip

Every config writer, including the TUI edit form and the project-up wire, SHALL
round-trip `model_pin` and all its keys unchanged. The TUI SHALL show a pinned
harness's pin read-only, unless it offers a form for the whole table, and SHALL
NOT let `model` be set beside it.

#### Scenario: Editing an unrelated field

- **WHEN** a pinned harness's description is edited in the TUI
- **THEN** the written file carries the same `model_pin` table

### Requirement: Error Handling Standards

All error-producing operations in this spec SHALL follow structured error
handling:

- Errors SHALL be wrapped with the harness name and, for config errors, the file
  and line. Override errors SHALL name the overriding file and key.
- Sentinel errors SHALL be defined for: an unoffered route, a capability gap, an
  unbeatable override, a mismatch, and unattested evidence.
- A render, scrub, lookup or attestation error MUST NOT be swallowed. Each one
  either fails the load, fails the start, or sets an outcome.
- Logging SHALL be structured key-value, and SHALL name credentials only by
  variable name.

#### Scenario: A lookup failure is not success

- **WHEN** a generation lookup returns an error
- **THEN** the call is unattested, the error is logged with the harness and
  generation ID, and the call is never counted as attested

### Requirement: Concurrency Safety

The per-harness pin checker SHALL receive observer items without blocking the
observer. A full buffer SHALL be counted as a collection error, and the final
sweep SHALL re-read the session so that nothing dropped goes unchecked. The
stop on a mismatch SHALL go through the supervisor's actor loop, as every other
lifecycle command does. Generation lookups SHALL run under a context the
harness's stop cancels. Tests SHALL run with the race detector.

#### Scenario: A slow lookup does not delay a stop

- **WHEN** a generation lookup is in flight and the operator stops the harness
- **THEN** the lookup is cancelled, and the stop completes without waiting for
  it
