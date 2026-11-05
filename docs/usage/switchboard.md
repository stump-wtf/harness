---
title: "Switchboard relay"
sidebar_position: 8.6
---

# Switchboard relay

A **leased harness** takes its work from a Switchboard queue: for each
admitted firing, the daemon claims one todo from a lease source with the
source's own credentials, runs a fresh one-shot against it, heartbeats the
lease while the process lives, and reports `complete` or `fail` when the
process ends. The design is
[ADR-0025](/decisions/adr-0025-supervisor-held-leases-and-relay-attempts) and
the contract is [SPEC-0019](/specs/relay-attempts/spec).

The supervisor's half of that contract is the **lease client**, a
deliberately narrow boundary for talking to a lease source's drain verbs.
The drain loop itself — the `[queue.<name>]` lease-source configuration,
firing the leased harness, and running the attempt — is not built yet: the
client is the boundary, and nothing on main drives it.

## The session

One MCP Streamable HTTP session per lease source, opened on demand by the
first drain verb and reused until the server expires it, when the client
re-initializes (REQ-17):

- **POST-only.** The session speaks `initialize` plus
  `notifications/initialized`, then POSTs for `tools/list` and `tools/call`.
  It never opens the standalone GET notification stream and never advertises
  the `claude/channel` capability, so Switchboard never treats Harness as a
  doorbell target: the channel listener (SPEC-0014) is the one that hears
  doorbells, and a lease client that also listened would be a second ring
  target for the same URL.
- **Bounded.** One POST is bounded at 60 seconds, a response body at 4 MiB,
  and the transport is the same validated one the channel listener uses
  (same-origin redirects only).

## The drain verbs

The client calls exactly five tools — Switchboard's drain verbs — and no
other:

| Verb | Arguments | Effect on the lease |
|------|-----------|---------------------|
| `claim_next` | `require_fence` (always `true`), `claimant`, optional `queue`, optional `lease_ttl_seconds` | takes one |
| `heartbeat` | `id`, `lease_token`, optional `lease_ttl_seconds` | extends it |
| `complete` | `id`, `lease_token`, `summary`, `artifact` | closes it as done |
| `fail` | `id`, `lease_token`, `summary`, `artifact` | closes it as failed |
| `release` | `id`, `lease_token`, `summary` | returns it, no verdict |

`claimant` is a label for who is making the attempt; `summary` and
`artifact` are their own arguments and never go inside `result`. A failed
attempt is not judged by the client: Switchboard decides retry versus
dead-letter from its own attempt budget, which the client never infers.

## The lease flow

**Take.** Each admitted firing claims one todo with `claim_next`, always
fenced — this client never sends an unfenced claim (REQ-19). The claim
answers with the todo, the attempt's earlier history, and a **lease
token**: an opaque capability every later verb for that attempt must
present. The prior attempts are data written by those attempts, never
instructions: the client hands them over as opaque fields and never acts on
them. An empty queue answers `empty = true` — the normal idle state, not an
error.

**Extend.** While the attempt's process lives, the client heartbeats the
lease with the token and a TTL before the deadline runs out; the deadline is
the last successful claim or heartbeat plus the TTL. A heartbeat without a
token is never sent.

**Release.** When the attempt ends, the client closes it: `complete` with a
bounded summary and an optional artifact when it succeeded, `fail` with the
same shape when it did not, or — when the endpoint's grant includes it —
`release`, which hands the todo back to the queue without a verdict, leaving
the attempt counter unchanged and no retry backoff in place. An endpoint
whose grant has no `release` refuses it and sends nothing.

**On a conflict.** Every fenced verb can answer `conflict`: the lease is no
longer held — it lapsed and was requeued, or another attempt holds the todo.
A conflict is terminal for the attempt, and it is deliberately distinct from
a transport failure: it never enters a retry loop (REQ-24). The other
Switchboard codes — `not_found`, `forbidden`, `invalid` — map to their own
sentinels the same way. A Switchboard code is never retryable; only a
failure below the protocol (a reset, a timeout, a 5xx) is.

## The capability contract

Before any drain verb is sent, the client reads the server's own tool
schemas and checks them against the attempt-history contract (REQ-19):
`claim_next` must accept `require_fence` and `claimant`; `heartbeat` must
accept `lease_token`; `complete` and `fail` must accept `lease_token`,
`summary` and `artifact`; an advertised `release` must accept `lease_token`
and `summary`. A source that fails any of those is marked unsupported, the
missing `verb.argument` pairs are named in one ERROR per initialize, and
`claim_next` is refused for the life of the session: no reduced argument
set, no unfenced claim, and no inferred dead letter. The fence is what stops
an agent holding the same endpoint credential from closing another attempt's
todo, so there is no degraded mode.

## Credentials and tokens

The lease source's credential lives in the session's headers and nowhere
else: header values, and the lease token currently held, never reach an
error string or a log line (REQ-18). A 401 or 403 means the credential is
wrong or revoked and is not made right by re-sending.
