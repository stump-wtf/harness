package supervisor

// A Pipe Run's Live Stream, Handed To A Consumer In Process
//
// A structured one-shot's activity belongs to the run that printed it
// (ADR-0033 "One source per session"): its stream is the exact record of what
// the spawned process did, and a transcript store is at best a second copy of
// the same calls. Harness holds no stream parser of its own (SPEC-0006), so
// the supervisor cannot read the stream it is moving; it can only hand it, in
// process, to a consumer the daemon installs. That consumer is the agent event
// observer (internal/observe), which normalizes the lines through agent-trace,
// fans them out, and suppresses the transcript's copy of the session the
// stream announced (issue #995).
//
// The sink belongs to the run, not the Manager, because the bytes do: it is
// opened when a run's readers start and closed when its stream does, by
// whichever of the two reaches the end first — the same race the stream file
// already runs. A supervisor with no sink installed — a test's, or a daemon
// that built no observer — moves lines exactly as it did before.
//
// Governing: ADR-0033 "Structured one-shots run on pipes", "One activity
// stream per session"; SPEC-0006 REQ "Run Correlation" (the daemon SHALL NOT
// parse the stream); ADR-0007 (a consumer never slows a run); ADR-0008
// (nothing leaves the run unmasked).
//
// @joestump-agent 10/26/2026 - Added for stump.wtf/harness#995.

// StreamSink receives one live pipe run's stream lines, each already masked,
// in the order the agent printed them.
type StreamSink interface {
	// Line hands over one masked stream line. It must not block: a consumer
	// that cannot keep up drops, and counts the drop (ADR-0007).
	Line(line string)
	// Close says the run's stream is finished: every line it wrote has been
	// handed over. It is called at least once — by the stdout reader at EOF
	// and by the run's end — and a sink must treat the second call as the
	// no-op it is.
	Close()
}

// StreamIngestSink opens the sink for run runID of harness, or returns nil
// when nothing wants the run's stream. Only a run on pipes asks; a PTY run
// has no stream to hand.
type StreamIngestSink func(harness string, runID int) StreamSink

// SetStreamIngestSink installs the seam a pipe run hands its stream through
// (issue #995). nil removes it, and a Manager with none ingests nothing.
func (m *Manager) SetStreamIngestSink(fn StreamIngestSink) {
	m.mu.Lock()
	m.streamIngest = fn
	m.mu.Unlock()
}

// OpenRunStream asks the installed sink for run runID of harness's stream.
// The run's readers call it once, on the actor loop, before they start moving
// lines; nil is the answer when nothing is watching.
func (m *Manager) OpenRunStream(harness string, runID int) StreamSink {
	m.mu.Lock()
	sink := m.streamIngest
	m.mu.Unlock()
	if sink == nil {
		return nil
	}
	return sink(harness, runID)
}

// streamIngestJournal is a RunJournal that opens a run's stream for ingest.
// The Manager is one, through its sink; a journal with no seam — a test's
// stub, or a Manager the daemon never wired — hands out nothing.
type streamIngestJournal interface {
	OpenRunStream(harness string, runID int) StreamSink
}
