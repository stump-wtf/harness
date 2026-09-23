// Package source is the trigger source manager: the piece between a source
// that heard something and the harnesses bound to it.
//
// One event fans out to every harness whose `triggers` lists the source, in
// config order, through the same `Manager.StartRun` a schedule firing uses. So
// a webhook run gets the same run record, per-run log, `timeout`, `on_overlap`
// and `keep_runs` a cron one-shot does, and the overlap decision stays atomic
// on each harness's actor loop rather than being re-decided here.
//
// It is a subpackage of internal/trigger rather than the package itself
// because of an import cycle, and the cycle is worth stating: internal/trigger
// holds the event envelope, which internal/supervisor writes to disk and so
// imports; the manager calls into internal/supervisor, so it cannot live
// beside the envelope. design.md's package table puts "the source manager" in
// internal/trigger; this is that manager, one directory down.
//
// Governing: ADR-0021; SPEC-0014 REQ "Firing", REQ "Overlap Skip Coalescing",
// REQ "Concurrency Safety".
//
// @joestump 09/22/2026 - Introduced with SPEC-0014 firing fan-out (#457).
package source

import (
	"context"
	"fmt"
	"sync"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
)

// Runner is the run entry point a firing reaches. *supervisor.Manager
// implements it; a test substitutes a fake to observe what was asked for
// without spawning anything.
type Runner interface {
	// StartRun asks for a run of name, returning the decision and false for
	// an unknown harness.
	StartRun(name string, req supervisor.RunRequest) (supervisor.RunDecision, bool)
}

// Options configure a Manager.
type Options struct {
	// Runner is where firings go. Required.
	Runner Runner
	// Config returns the CURRENT config. It is a function, not a value,
	// because the set of harnesses bound to a source changes on reload and
	// REQ "Source Reconciliation On Reload" says that change applies from the
	// next event — which is only true if each firing re-reads it.
	Config func() *core.Config
	// Log is where firings and failures are reported. Defaults to the
	// package logger.
	Log *log.Logger
}

// Decision is what happened to one harness in a fan-out. It is the shape a
// webhook's `202` reports, which is why it carries a per-harness error rather
// than failing the whole firing: a delivery that reached two harnesses and
// started one of them is not a failed delivery.
type Decision struct {
	// Harness is the harness this decision is about.
	Harness string
	// Kind is "started", "queued" or "skipped"; empty when Err is set.
	Kind supervisor.RunDecisionKind
	// RunID is the run's id, 0 for a queued firing (which has none until it
	// starts) and for an error.
	RunID int
	// Err says why this harness could not be fired at all — it is unknown to
	// the Manager, or handling it panicked. A run that started and failed is
	// NOT an error here: it has a record saying so.
	Err string
}

// Manager fans events out to the harnesses bound to their source.
type Manager struct {
	runner Runner
	config func() *core.Config
	log    *log.Logger

	mu       sync.Mutex
	closed   bool
	ctx      context.Context
	cancel   context.CancelFunc
	inFlight sync.WaitGroup
}

// New builds a Manager. It does nothing until Start.
func New(opts Options) *Manager {
	logger := opts.Log
	if logger == nil {
		logger = log.Default()
	}
	cfg := opts.Config
	if cfg == nil {
		cfg = func() *core.Config { return &core.Config{} }
	}
	return &Manager{runner: opts.Runner, config: cfg, log: logger}
}

// Start makes the Manager accept firings, and ties its lifetime to ctx.
// Cancelling ctx has the same effect as Close: firings that have not reached
// the run entry point are abandoned.
// Governing: SPEC-0014 REQ "Concurrency Safety".
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.closed = false
}

// Close stops accepting firings and waits for those in progress to reach the
// run entry point.
//
// Waiting rather than cancelling mid-flight is deliberate. A firing that has
// already called StartRun has a run record; abandoning it after that point
// would leave a record whose process never started and nothing to explain it.
// A firing that has not reached StartRun has no record at all, so dropping it
// is invisible and safe — which is exactly the line REQ "Concurrency Safety"
// draws.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	cancel := m.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.inFlight.Wait()
}

// Fire fans one event out to every harness bound to its source, in config
// order, and returns one decision per harness.
//
// Sequential, not concurrent, and that is a choice rather than an oversight.
// REQ "Firing" asks for config order in the response and for one harness's
// failure not to prevent the others; StartRun returns as soon as the target's
// actor loop has DECIDED, so the cost of going in order is a few loop hops,
// while going concurrently would make the response order depend on scheduling.
// A harness whose loop is genuinely wedged is a problem the run machinery
// already has to answer for, and hiding it behind a goroutine per firing would
// only move it.
//
// A nil or invalid event fires nothing: the source is what decides which
// harnesses are bound, and an envelope with no valid source names none.
// Governing: SPEC-0014 REQ "Firing", REQ "Concurrency Safety".
func (m *Manager) Fire(ev *trigger.Envelope) []Decision {
	if ev == nil {
		return nil
	}
	if err := ev.Validate(); err != nil {
		m.log.Warn("trigger event dropped: invalid envelope", "source", ev.Source, "err", err.Error())
		return nil
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.log.Debug("trigger event dropped: the source manager is closed", "source", ev.Source)
		return nil
	}
	m.inFlight.Add(1)
	ctx := m.ctx
	m.mu.Unlock()
	defer m.inFlight.Done()

	if ctx != nil && ctx.Err() != nil {
		m.log.Debug("trigger event dropped: shutting down", "source", ev.Source)
		return nil
	}

	bound := m.config().BoundHarnesses(ev.Source)
	if len(bound) == 0 {
		// Not an error: a source may be declared, connected and simply not
		// bound yet. Counting it is #480's job; saying so is this one's.
		m.log.Debug("trigger event fired nothing: no harness binds the source", "source", ev.Source)
		return nil
	}

	runTrigger := supervisor.TriggerWebhook
	if ev.Kind == trigger.KindChannel {
		runTrigger = supervisor.TriggerChannel
	}

	out := make([]Decision, 0, len(bound))
	for _, name := range bound {
		// Checked per harness, not once per event: a Close that lands while
		// an earlier harness is inside StartRun must abandon the ones after
		// it, which have not reached the run entry point and so have no
		// record to leave dangling (REQ "Concurrency Safety").
		if ctx != nil && ctx.Err() != nil {
			m.log.Debug("trigger firing abandoned: shutting down", "harness", name, "source", ev.Source)
			out = append(out, Decision{Harness: name, Err: "abandoned: the daemon is shutting down"})
			continue
		}
		out = append(out, m.fireOne(name, runTrigger, ev))
	}
	return out
}

// fireOne fires a single harness, recovering a panic so one harness cannot
// take the daemon — or the rest of the fan-out — down with it.
//
// The recover is here rather than around the whole fan-out on purpose: at this
// scope a panic costs one harness's firing, and the loop carries on to the
// next. Around the fan-out it would cost every harness after the one that
// panicked, which is the failure REQ "Firing" names ("a failure to start one
// harness SHALL NOT prevent the others").
// Governing: SPEC-0014 REQ "Firing", REQ "Concurrency Safety".
func (m *Manager) fireOne(name string, runTrigger supervisor.RunTrigger, ev *trigger.Envelope) (d Decision) {
	d = Decision{Harness: name}
	defer func() {
		if r := recover(); r != nil {
			d = Decision{Harness: name, Err: fmt.Sprintf("panic while firing: %v", r)}
			// The panic value can carry anything, including something derived
			// from the event. It is logged as a formatted value, never the
			// event itself, and the event's payload never reaches a log line.
			m.log.Error("trigger firing panicked", "harness", name, "source", ev.Source, "panic", fmt.Sprint(r))
		}
	}()

	decision, ok := m.runner.StartRun(name, supervisor.RunRequest{
		Trigger: runTrigger,
		Source:  ev.Source,
		Event:   ev,
	})
	if !ok {
		m.log.Warn("trigger fired for a harness the daemon does not know", "harness", name, "source", ev.Source)
		return Decision{Harness: name, Err: "unknown harness"}
	}
	m.log.Info("trigger fired",
		"harness", name, "source", ev.Source, "event_id", ev.EventID,
		"decision", string(decision.Kind), "run_id", decision.Run.RunID)
	return Decision{Harness: name, Kind: decision.Kind, RunID: decision.Run.RunID}
}
