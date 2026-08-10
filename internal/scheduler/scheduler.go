// Package scheduler fires harnesses on a cron schedule.
//
// Governing: issue #66. A scheduled harness is a one-shot agent run that the
// daemon owns: at each cron firing the daemon starts the harness if it is not
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

// Scheduler owns a cron.Cron that fires scheduled harnesses. It is safe for
// concurrent use.
type Scheduler struct {
	cron  *cron.Cron
	start StartFunc

	mu      sync.Mutex
	entries map[string]cron.EntryID
}

// New creates a Scheduler. Call Start to activate it.
func New(start StartFunc) *Scheduler {
	return &Scheduler{
		cron: cron.New(cron.WithLogger(cron.PrintfLogger(
			log.Default().With("component", "scheduler"))),
			cron.WithLocation(time.Local)),
		start:   start,
		entries: make(map[string]cron.EntryID),
	}
}

// Apply reconciles the scheduler's entries against the given config. It adds
// entries for harnesses with a non-empty Schedule, removes entries for
// harnesses that lost their schedule, and updates entries whose schedule
// changed. Harnesses without a schedule are ignored.
func (s *Scheduler) Apply(cfg *core.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Stop the cron, rebuild entries, and restart — robfig/cron has no
	// spec-comparison accessor on Entry, so a full rebuild is the reliable
	// path. This runs only on config reload (not per firing), so the cost is
	// negligible.
	if s.cron != nil {
		s.cron.Stop()
	}
	s.cron = cron.New(cron.WithLogger(cron.PrintfLogger(
		log.Default().With("component", "scheduler"))),
		cron.WithLocation(time.Local))
	s.entries = make(map[string]cron.EntryID)

	for _, name := range cfg.HarnessOrder {
		h := cfg.Harnesses[name]
		if h.Schedule == "" {
			continue
		}
		harnessName := name
		id, err := s.cron.AddFunc(h.Schedule, func() {
			s.start(harnessName)
		})
		if err != nil {
			log.Error("invalid schedule, skipping harness", "harness", name, "schedule", h.Schedule, "err", err)
			continue
		}
		s.entries[name] = id
		log.Info("scheduled harness", "harness", name, "schedule", h.Schedule)
	}
	s.cron.Start()
}

// Start activates the cron scheduler. It is safe to call once, before Serve.
func (s *Scheduler) Start() {
	s.cron.Start()
}

// Close stops the scheduler and waits for in-progress firings to finish.
func (s *Scheduler) Close() {
	stopCtx := s.cron.Stop()
	<-stopCtx.Done()
}
