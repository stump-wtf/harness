// Package scheduler fires harnesses on a cron schedule.
//
// Governing: issue #66; ADR-0006 (harness.toml is the source of truth the
// entries reconcile against); ADR-0011 (the scheduled unit is a prompt
// one-shot). A scheduled harness is a one-shot agent run that the daemon
// owns: at each cron firing the daemon starts the harness if it is not
// already running (an overlapping firing is skipped, not stacked). The run
// exiting is terminal for that firing; the restart policy applies only to
// abnormal exit if configured.
package scheduler

import (
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/robfig/cron/v3"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// StartFunc is the callback the scheduler invokes when a harness fires. It
// starts the harness by name (if it is not already running) and returns.
type StartFunc func(name string)

// entry tracks one scheduled harness: the spec it was registered with (so a
// reload can detect changes) and the cron entry backing it.
type entry struct {
	spec string
	id   cron.EntryID
}

// Scheduler owns a single long-lived cron.Cron that fires scheduled
// harnesses. Apply reconciles entries incrementally against a config, so an
// unchanged entry keeps its phase across reloads (an "@every 6h" countdown is
// not reset by a config rewrite that didn't touch it). It is safe for
// concurrent use.
type Scheduler struct {
	cron  *cron.Cron
	start StartFunc

	mu      sync.Mutex
	entries map[string]entry
}

// New creates a Scheduler. Call Apply to register entries and Start to
// activate it.
func New(start StartFunc) *Scheduler {
	logger := cron.PrintfLogger(log.Default().With("component", "scheduler"))
	return &Scheduler{
		// Recover is load-bearing: cron v3 runs each firing in a bare
		// goroutine with an empty default chain, so without it a panic in
		// the StartFunc would crash the whole daemon.
		cron: cron.New(
			cron.WithLogger(logger),
			cron.WithChain(cron.Recover(logger)),
			cron.WithLocation(time.Local)),
		start:   start,
		entries: make(map[string]entry),
	}
}

// Apply reconciles the scheduler's entries against the given config: it adds
// entries for harnesses with a non-empty Schedule, removes entries for
// harnesses that lost their schedule (or disappeared), and re-registers
// entries whose spec changed. Unchanged entries are left untouched so their
// next-fire time is preserved across reloads. Harnesses without a schedule
// are ignored.
func (s *Scheduler) Apply(cfg *core.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove entries whose harness is gone or whose spec changed (the latter
	// are re-added below).
	for name, e := range s.entries {
		h, ok := cfg.Harnesses[name]
		if ok && h.Schedule == e.spec {
			continue
		}
		s.cron.Remove(e.id)
		delete(s.entries, name)
		if !ok || h.Schedule == "" {
			log.Info("unscheduled harness", "harness", name)
		}
	}

	// Add new or changed entries. AddFunc is safe on a running cron.
	for _, name := range cfg.HarnessOrder {
		h := cfg.Harnesses[name]
		if h.Schedule == "" {
			continue
		}
		if _, ok := s.entries[name]; ok {
			continue // unchanged: keep its phase
		}
		id, err := s.cron.AddFunc(h.Schedule, func() {
			s.start(name)
		})
		if err != nil {
			// Defense in depth: config validation already rejects invalid
			// specs at parse time (registerHarness).
			log.Error("invalid schedule, skipping harness", "harness", name, "schedule", h.Schedule, "err", err)
			continue
		}
		s.entries[name] = entry{spec: h.Schedule, id: id}
		log.Info("scheduled harness", "harness", name, "schedule", h.Schedule)
	}
}

// Start activates the cron scheduler. Entries applied while running take
// effect immediately.
func (s *Scheduler) Start() {
	s.cron.Start()
}

// Close stops the scheduler and waits for in-progress firings to finish.
func (s *Scheduler) Close() {
	stopCtx := s.cron.Stop()
	<-stopCtx.Done()
}
