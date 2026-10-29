package supervisor

// A pipe run's own stream, folded into its ledger record at run end
// (issue #994).
//
// A structured one-shot's usage, when its transcript never flushed, comes
// from the run's own stream file: <id>.stream.jsonl, sealed by finishRunWith
// before the run's record is closed. The Manager asks its sink to read the
// file's final result and fold it into the ledger's usage accumulator, so
// the run's closed line carries the usage whatever the transcript did.
//
// The sink is the daemon's to install (SetStreamUsageSink): the Manager does
// not read the stream itself. Normalizing it is agent-trace's job (ADR-0033),
// and the glue between the parsed result and the ledger belongs to the
// package that owns both sides (internal/runusage), which neither the
// supervisor nor the observer may reach from here.

// StreamUsageSink folds the run named by name and runID, whose stream file
// is streamPath, into the ledger's record for it. kind is the run's
// trajectory kind, the one that decides whether the stream is parseable at
// all. It must not wait on a supervisor or the ledger's sync.
type StreamUsageSink func(name, kind string, runID int, streamPath string)

// SetStreamUsageSink installs the seam a run's end brings its own stream
// through (issue #994): the daemon passes a sink that reads the sealed
// stream's final result and folds it into the ledger's usage accumulator.
// nil removes it; a Manager with no sink folds nothing, as a test Manager
// does.
func (m *Manager) SetStreamUsageSink(fn StreamUsageSink) {
	m.mu.Lock()
	m.streamUsage = fn
	m.mu.Unlock()
}

// FoldRunStreamUsage asks the sink to fold runID's stream into the ledger.
// It is called from the run's end, once the stream is sealed and before the
// run's record is closed, so the folded usage rides the closed line.
func (m *Manager) FoldRunStreamUsage(name, kind string, runID int, streamPath string) {
	if streamPath == "" {
		return
	}
	m.mu.Lock()
	sink := m.streamUsage
	m.mu.Unlock()
	if sink != nil {
		sink(name, kind, runID, streamPath)
	}
}
