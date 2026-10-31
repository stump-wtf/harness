---
title: "Model pins"
sidebar_position: 3.5
---

# Model pins

A **model pin** is a harness's policy for its model calls, declared as a
`[harness.<name>.model_pin]` sub-table: the one model the harness may use,
the route that may serve it, the upstream providers allowed to serve it, and
the data policy the request must carry. A pin that allows fallbacks is not a
pin, and the pin is fail-closed by design: a call the pin does not approve —
a substitute model, a non-approved provider, a missing piece of evidence —
stops the harness rather than proceeding. In this release the table loads,
validates and crosses the wire; the runtime enforcement (rendering,
credential scrubbing, attestation, the mismatch stop and hold) ships as the
adapter renderer work lands. The decision is
[ADR-0026](/decisions/adr-0026-fail-closed-model-pinning); the full behaviour
is [SPEC-0020](/specs/model-pinning/spec), and the design notes are
[SPEC-0020 design](/specs/model-pinning/design).

Pins are valid on resident and one-shot harnesses alike.

## Configuration

The pin is its own sub-table under the harness:

```toml
[harness.implementer]
harness = "crush"
prompt = "implement the issue"

[harness.implementer.model_pin]
route       = "openrouter"
model       = "z-ai/glm-5.3-flash"
providers   = ["deepinfra"]          # who may serve it, in preference order
data_policy = "zdr"                  # zdr | deny-collection | provider-default
attest      = "full"                 # full (model and provider) | model
on_mismatch = "hold"                 # hold (default) | fail-run
```

| Key | Type | Required | Meaning |
| --- | --- | --- | --- |
| `route` | string | yes | `openrouter`, `anthropic` or `litellm` |
| `model` | string | yes | The upstream model ID the pin approves; one token, no whitespace |
| `providers` | array of strings | `openrouter`: yes; `anthropic`: rejected; `litellm`: optional | Upstream provider slugs allowed to serve, in preference order |
| `allow_fallbacks` | bool | no (default `false`) | Must be `false`; `true` fails validation |
| `data_policy` | string | no (default `provider-default`) | `zdr`, `deny-collection` or `provider-default` |
| `attest` | string | no (default `full`) | `full` (model and provider) or `model` |
| `on_mismatch` | string | no (default `hold`) | `hold` or `fail-run` |
| `accept_served` | array of strings | no | Additional exact served-model IDs to accept |
| `gateway_model` | string | `litellm`: yes; otherwise rejected | The gateway alias the client requests |
| `gateway_url` | string | `litellm`: yes; otherwise rejected | The gateway's base URL; `https` unless loopback |

The table is legal wherever the harness table is legal: the global
`harness.toml`, a drop-in under `harness_d` ([Configuration → Drop-in
harness files](./configuration#drop-in-harness-files-harness_d)), and a
project-scoped `harness.toml` carried by `harness up`. The table and its
defaults are the same in all three places.

A pinned harness must not also set `model`: the pin supplies the model, and
the loader names both keys when both are present.

## What validation enforces

Validation runs when the config loads (and again when a project re-up
registers a definition over the socket), so a pin that would not load is
rejected with a located error — file and line — before anything spawns:

- `route` must be one of `openrouter`, `anthropic`, `litellm`.
- `model` must name exactly one model: one token (no whitespace), and on
  `openrouter` no `:` routing variant and no auto-router — `openrouter/auto`
  fails, because a pin selects one model.
- `allow_fallbacks = true` fails: "a pin that allows fallbacks is not a pin".
- Every entry of `providers` must be a provider slug matching
  `^[a-z0-9][a-z0-9-]*(/[a-z0-9-]+)?$`.
- On `openrouter`, `providers` is required: a pin names who may serve the
  model.
- On `anthropic`, `providers` is rejected (first-party is the only provider),
  and `data_policy` must be `provider-default`: zero data retention there is
  an account agreement the daemon cannot request.
- `gateway_model` and `gateway_url` are litellm keys. On other routes they
  fail, and on `litellm` both are required — `gateway_url` must be `https`
  unless the gateway is loopback.
- The adapter must offer the route for the pin's client (below). A pin on a
  client and route pair the adapter does not offer fails, naming the client,
  the route and the offered alternatives.
- A pin on a `command` harness requires its `transcripts` binding: the daemon
  attests only what the bound adapter's transcript records, and without a
  binding nothing could be attested.
- `attest = "full"` must be evidenceable: on a client and route whose
  transcripts evidence only the model, it fails and offers
  `attest = "model"`.

## Which routes each client offers

In this version, route support and evidence capability are the v1 tables
below; an adapter that offers no route fails any pin at load.

| Client | `openrouter` | `anthropic` | `litellm` |
| --- | --- | --- | --- |
| `crush` | offered | not offered | offered |
| `pi`, `omp` | offered | not offered | offered |
| `opencode` | offered | not offered | offered |
| `claude-code` | not offered | offered | offered |
| `codex` | not offered | not offered | not offered |
| `generic` | not offered | not offered | not offered |
| `command` | attestation only | attestation only | attestation only |

A `command` harness renders nothing: its argv owns routing, and the daemon
only attests the calls the bound adapter's transcript records.

## Attestation capability

Each client/route pair declares what its transcripts can evidence for a
served call: `model`, or `model+provider`.

| Client/route | Evidence |
| --- | --- |
| `crush` on `openrouter` / `litellm` | `model` |
| `pi`, `omp` on `openrouter` | `model+provider` |
| `pi`, `omp` on `litellm` | `model` |
| `opencode` on `openrouter` / `litellm` | `model` (the session row records the model id, not the upstream provider) |
| `claude-code` on `anthropic` | `model+provider` (first party, by construction, when `ANTHROPIC_BASE_URL` is unset) |
| `claude-code` on `litellm` | `model` |

The capability is what the `attest = "full"` check above consults at load:
a pin that demands provider evidence from a `model`-only client is a load
error, not a silent downgrade.

## How the pin surfaces in the daemon protocol

The pin crosses the wire as data on the usual control plane, additive only —
a daemon or client without model pins simply omits or ignores the field:

- `list` and `describe` (the `HarnessInfo` projection) carry the pin as
  `model_pin`, mirroring the `[harness.<name>.model_pin]` table; `project up`
  and the TUI save path round-trip it unchanged, and a pin that would not
  load from a file cannot be registered over the socket.
- Run records (the `runs` op and the `job_run_finished` event) can carry the
  outcomes `model_mismatch` and `model_unattested`, and a skipped firing
  records the reason `model_hold`. A `model_mismatch` record names the first
  mismatching call: `{kind: model|provider, served_model, served_provider, at}`.
- State and hold changes ride the existing events: a pinned harness that
  fails on a mismatch arrives as `harness_state_changed` to `failed`, and a
  scheduled or triggered harness held after a mismatch arrives as
  `harness_hold_changed` with `model_hold` in `hold_reasons` — the same
  event `harness list` reads for its hold display. The mismatch itself — the
  run's outcome and the `mismatch` detail — is what `runs` reports for it.

## How the pin surfaces in the TUI

The TUI edit form treats the pin as read-only (SPEC-0020 REQ-18):

- On a pinned harness, the form's `model` row shows
  `pinned by model_pin: route=… model=…` with a note that the pin is
  read-only in the form and that `model` beside it is refused.
- Saving the form round-trips the pin unchanged into
  `[harness.<name>.model_pin]`; editing an unrelated field does not touch it.
- Setting `model` next to an existing pin is rejected by the form with the
  same message the parser gives, rather than silently dropped.

Change the pin itself by editing `harness.toml` (or the project file) and
`harness reload`.

## What ships next

This page documents the pin as it loads today. The remainder of SPEC-0020 —
the per-client renderers that write the pinned client configuration under
`$XDG_STATE_HOME/harness/pins/`, the credential scrubbing of the child
environment, the runtime attestation that stops a harness on the first
unapproved call, the mismatch hold and its release, `harness pin render`,
and the `harness doctor --models` canary that proves the route end to end —
is specified in the spec and its design notes and ships as the adapter
renderer work lands.
