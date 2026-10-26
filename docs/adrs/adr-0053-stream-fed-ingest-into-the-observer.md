---
status: proposed
date: 2026-10-26
decision-makers: []
extends: [ADR-0033]
related: [ADR-0007, ADR-0008, ADR-0021, ADR-0032]
---

# ADR-0053: A run's live stream is ingested where it is born — the supervisor hands it over, agent-trace normalizes it, and one session has one source

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#995 (slice 3 of
> the #915 split) by the l lane. ADR-0033 already decided that a session
> announced on a run's stdout is claimed by that run and that its normalized
> items reach the subscriber fan-out marked `source = stream`; nothing has
> decided *how* the bytes get from a run's stdout to that fan-out, or what
> happens to a claimed session when its stream ends.

## Context and Problem Statement

A structured one-shot prints its own record: `claude -p --output-format
stream-json` writes every tool call, every result and a final `result` line to
stdout, and the daemon already reads that stdout line by line, masks it, and
keeps it in `jobs/<name>/<run_id>.stream.jsonl` (SPEC-0017 REQ-18). ADR-0021 and
SPEC-0006 REQ "Structured Prompt Stream" put that format inside the adapter and
its normalizer: the daemon SHALL NOT parse the stream.

The subscriber fan-out, though, is fed only by the poller, which reads
agent-trace **stores** on a poll interval and attributes each session to a
harness by matching it against run windows. For a pipe one-shot that route is
weak exactly when it matters: Claude Code flushes `~/.claude/projects` per turn
at best, a run killed on a timeout or a stop may leave a transcript that never
mentions the calls it made, and attribution has to guess from timestamps and
candidate sets when the daemon is the one component that knows which run wrote
the bytes. So the daemon displays a run's activity late, or not at all, while
holding the exact record in a file it is forbidden to read.

#915 split the work in two. The sealed stream's `result` folded into the run's
ledger line is shipped (#994: a sink the Manager calls at run end). This ADR
covers the other half: the live activity of the same bytes, delivered once.

## Decision

* **The seam belongs to the supervisor, and knows nothing about formats.**
  `supervisor.Manager` takes a `StreamIngestSink`: given `(harness, runID)`, it
  returns a `StreamSink` with `Line(string)` and `Close()`. At the start of a
  pipe run's readers the Supervisor asks for that run's sink and hands it every
  line it masks, in order, before writing it to the stream file — the same
  bytes, the same masking, no second read. `Close` comes twice by design (EOF,
  and the run's end), and a sink must treat the second as a no-op. A PTY run
  asks for nothing: it has no stream. A daemon that installs no seam behaves
  exactly as it does today.
* **The observer consumes it, agent-trace normalizes it.** `cmd/harness` wires
  the seam to `observe.Observer.OpenStream` before `Autostart`, so the first run
  of a boot has somewhere to put its stream. The observer resolves the harness's
  adapter, asks it for its declared prompt stream, and uses that adapter's own
  `StreamParser` — the same normalizer that reads the format from a store, fed an
  `io.Reader` over the run's lines. No Harness type names a stream field
  (ADR-0021, SPEC-0006). An adapter that declares none gets its lines drained and
  counted, never parsed by hand.
* **Attribution is a fact, not a guess.** A stream arrives from a run the
  supervisor is watching, so that run is its author: items carry the run's
  harness and adapter directly, with no candidate sets and no slack window. Each
  carries `Event.Origin = OriginStream` — the `source = stream` mark ADR-0033
  asked for — and store-delivered items carry `OriginTranscript`, so a consumer
  can see that two records of one session are one session. (`Origin`, not
  `source`: `Source` in this package is the supervisor read the poller performs.)
* **One source per session, enforced.** The first `session_id` a run's stream
  announces is **claimed** by that run. While a session is claimed the poller
  leaves it alone: it does not begin tracking it, and it delivers nothing it
  reads from it, counting what it skipped in `Stats.Suppressed`. A Claude
  one-shot that streams its calls *and* writes the transcript of the same
  session is therefore counted once, from the stream — the rule ADR-0033 states,
  with the counter that shows it holding. Two live runs claiming one session is
  contested: the first keeps it (it is the one the poller was told to suppress)
  and the second is counted ambiguous, as a peer always is.
* **Release floors, so no backlog escapes.** The store's watermark advances while
  a session is suppressed, so a claim that ends cannot release a backlog. The
  scan that finds an ended claim hands the session back with a floor at that
  instant: nothing the stream already delivered is delivered again, and a later
  run that resumes the session is read from the store from there forward. A
  daemon that stops mid-stream keeps no claim across the restart: the new daemon
  treats the session as it would any session it has not read.
* **A run is never paced by its consumer.** Lines move through a bounded queue
  (512 lines). A full queue drops, and the drop is counted where it happens
  (`Stats.StreamDropped`); the pipe reader never blocks on a parser, a scan or a
  slow subscriber (ADR-0007). The parser runs on its own goroutine per run, so
  the fan-out reaches subscribers while the run is still writing.
* **Redaction still applies.** A line is masked once, on its way to the file, and
  every string that reaches the fan-out passes through `redact` again, because
  this fan-out reaches consumers that ship events off the host (ADR-0008). A
  stream is not a trusted channel.

## Explicit non-goal

A stream's `result` record is not turned into an event here. A run's cost and
usage belong to its ledger line, folded from the sealed stream when the run
closes — #994's path — and putting the same numbers on the bus as well would
give consumers two answers for one run.

## Consequences

* A structured one-shot's calls reach subscribers while it runs, within the line
  that printed them, instead of at the next poll of a store that may never hold
  them.
* A pipe run's session is never counted twice, and the counters say which route
  carried it: `StreamItems` next to a nonzero `Suppressed` is the claim working,
  not a stuck backlog.
* `Event` gained a field and `Stats` gained stream counters. Consumers that
  ignore unknown fields are unaffected.
* A session is read from one place at a time. While a run streams it, the store
  route is silent for it — including for items only the store holds (a mark the
  CLI never echoed), which the handback floor then drops. That is the accepted
  cost of one source per session; ADR-0033 chose the property.
* `internal/supervisor` gained one seam and no knowledge of any agent's format;
  `internal/daemon` gained one call and still parses nothing; `cmd/harness`
  decides that a run's stream is the observer's to consume.
* A harness whose adapter declares no stream parser but that reaches the seam has
  its lines drained and counted (`StreamParseErrors`), so a config that changed
  under a running harness is visible rather than silent.

## Considered options

* **Re-parse `.stream.jsonl` from a byte cursor per poll.** Rejected:
  `ParseStream` hands a still-open tool call once, with an empty result, at the
  end of whatever it was given, so chunk boundaries duplicate a call or lose its
  `IsError`; and a record-aware rewind is the daemon interpreting the format,
  which SPEC-0006 forbids.
* **Re-parse the whole stream file per poll.** Rejected: O(file) every interval
  for an append-only record, with the same open-call ambiguity at the tail.
* **Run-end-only ingest.** Rejected as the whole answer: it is #994's job for
  cost, and for activity it abandons the live reporting #915 exists to provide.
* **A parser in `internal/daemon`.** Rejected: SPEC-0006 and ADR-0021 put a
  backend's format inside the adapter, and a second copy would drift from
  agent-trace's.
* **Spawn `agent-trace normalize` per run.** Rejected by ADR-0033 already: it
  needs supervising, and the binary on `PATH` can drift from the library Harness
  links against.
* **Let the poller keep attributing stream items** (treat a run's stream as
  another store). Rejected: the author is already known, and the slack window
  that makes the guess possible is what makes double counting possible.

## Open questions for the maintainer

* Should a claim be keyed by `(adapter, session id)` as implemented, or by
  session id alone? Two adapters that mint identical ids would today both be
  believed.
* Should a run that announces a session already claimed by *another live* run be
  suppressed as well as counted ambiguous — currently its items are published,
  because only the transcript route is suppressed.
* Should a stream-only session's items also be written to the trace recorder as
  they arrive, or keep the current trace-at-run-end shape?

## Links

* Issue: https://gitea.stump.rocks/stump.wtf/harness/issues/995 — slice 3 of
  https://gitea.stump.rocks/stump.wtf/harness/issues/915
* Sibling slice: https://gitea.stump.rocks/stump.wtf/harness/pulls/998 (a sealed
  stream's result folded into ledger usage)
* Governing: ADR-0033 "One activity stream per session", "Structured one-shots
  run on pipes", "Normalization belongs to agent-trace"; ADR-0021; ADR-0007;
  ADR-0008; SPEC-0006 REQ "Structured Prompt Stream"; SPEC-0017 REQ-18;
  SPEC-0021 REQ-8 (cost, owned by the #994 path)
