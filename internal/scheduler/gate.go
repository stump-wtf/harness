package scheduler

// Operating Hours Gate
//
// Each tick, after the schedule entries are decided, the scheduler runs a
// gate pass over every gated harness (one with operating_hours): out of hours
// and not leased, a harness that is up is held; in hours, a held harness is
// released. The pass reuses the tick's own wall-clock reading (monotonic
// component stripped) — there is no second ticker and no timer armed to a
// window boundary. Evaluation is level-triggered: every tick decides from the
// current time alone, so a suspend across a close, a daemon outage, a clock
// step and both DST transitions need no special case; the first tick after
// any of them simply sees where the clock now is.
//
// The pass only decides. Doing is Gate's job, and the daemon's Gate sends the
// hold to the harness's supervisor actor loop, which re-checks and acts
// atomically with the process's own exits. A snapshot that has gone stale by
// then is harmless: a hold of a harness already down, or a release of one no
// longer held, is a no-op on the loop.
//
// Governing: ADR-0019 (operating hours), SPEC-0012 REQ "Gate Evaluation",
// REQ "Gate Enforcement", REQ "Operating Hours Reload"; design.md § "Evaluate
// on the existing scheduler tick"; SPEC-0008 REQ "Suspend-Safe Schedule
// Evaluation".
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#382.

import (
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
)

// Gate is the seam the operating-hours pass drives. The daemon passes an
// adapter over the supervisor Manager.
type Gate interface {
	// Status reports whether name is up (starting, running, degraded or
	// restarting) and whether it is held. ok is false for an unknown harness.
	Status(name string) (up, held, ok bool)
	// Lease reports name's after-hours lease end, ok=false when it has none.
	// A harness covered by a lease that has not ended is never held.
	Lease(name string) (until time.Time, ok bool)
	// Hold shuts name down for its hours without touching enabled intent.
	// It may block for the length of a graceful stop.
	Hold(name string, mode core.HoursShutdownMode)
	// Release starts a held harness without touching enabled intent.
	Release(name string)
}

// gateEntry is one gated harness.
type gateEntry struct {
	raw  string
	expr hours.Expr
	mode core.HoursShutdownMode
}

// gateAction is one decision the pass made, carried out after the lock drops.
type gateAction struct {
	name    string
	hold    bool // false: release
	mode    core.HoursShutdownMode
	ungated bool // a release for a harness whose hours a reload removed
}

// applyGates reconciles the gate set against cfg. Caller holds s.mu.
//
// Only what the pass needs is kept, so an unchanged value keeps its state for
// free: the pass is level-triggered, a harness that stays in hours is never
// touched, and there is no per-harness phase to lose. A harness that loses its
// operating_hours (or is removed) is queued for one release on the next tick,
// so a held harness whose gate went away starts rather than staying down with
// nothing left to open it (SPEC-0012 REQ "Operating Hours Reload").
func (s *Scheduler) applyGates(cfg *core.Config) {
	next := make(map[string]gateEntry)
	order := make([]string, 0)
	for _, name := range cfg.HarnessOrder {
		h := cfg.Harnesses[name]
		if h.OperatingHours == "" {
			continue
		}
		expr := h.HoursExpr
		if expr.String() == "" {
			// Defense in depth: config parsing always fills HoursExpr, but a
			// hand-built config may carry only the raw string.
			parsed, err := hours.Parse(h.OperatingHours)
			if err != nil {
				log.Error("invalid operating_hours, not gating harness", "harness", name, "operating_hours", h.OperatingHours, "err", err)
				continue
			}
			expr = parsed
		}
		if old, ok := s.gates[name]; !ok || old.raw != h.OperatingHours {
			log.Info("operating hours", "harness", name, "operating_hours", h.OperatingHours)
		}
		next[name] = gateEntry{raw: h.OperatingHours, expr: expr, mode: h.HoursShutdown}
		order = append(order, name)
	}
	for name := range s.gates {
		if _, ok := next[name]; !ok {
			log.Info("operating hours removed", "harness", name)
			if s.ungated == nil {
				s.ungated = make(map[string]bool)
			}
			s.ungated[name] = true
		}
	}
	for name := range next {
		delete(s.ungated, name) // gated again before the release ran
	}
	s.gates = next
	s.gateOrder = order
}

// gatePass decides every gated harness at now. Caller holds s.mu; the Gate is
// consulted only for its read-only status and lease answers, which never call
// back into the scheduler.
func (s *Scheduler) gatePass(now time.Time) []gateAction {
	if s.gate == nil {
		return nil
	}
	var acts []gateAction
	for _, name := range s.gateOrder {
		g := s.gates[name]
		if s.gating[name] {
			continue // the last decision is still being carried out
		}
		in, _, _ := g.expr.In(now)
		up, held, ok := s.gate.Status(name)
		if !ok {
			continue
		}
		switch {
		case !in && s.leased(name, now):
			// Covered by an after-hours lease: nothing to do.
		case !in && up:
			acts = append(acts, gateAction{name: name, hold: true, mode: g.mode})
		case in && held:
			acts = append(acts, gateAction{name: name})
		}
	}
	for name := range s.ungated {
		delete(s.ungated, name)
		if _, held, ok := s.gate.Status(name); ok && held {
			acts = append(acts, gateAction{name: name, ungated: true})
		}
	}
	for _, a := range acts {
		if s.gating == nil {
			s.gating = make(map[string]bool)
		}
		s.gating[a.name] = true
	}
	return acts
}

// leased reports whether name holds a lease that has not ended at now. The
// lease itself is the Gate's (it lives in state.json); this is only the seam
// the pass consults.
func (s *Scheduler) leased(name string, now time.Time) bool {
	until, ok := s.gate.Lease(name)
	return ok && now.Before(until)
}

// dispatchGate carries out one decision on its own goroutine, so a hold
// waiting out a stop grace cannot delay the tick. Close waits for it.
func (s *Scheduler) dispatchGate(a gateAction) {
	s.firings.Add(1)
	go func() {
		defer s.firings.Done()
		defer func() {
			s.mu.Lock()
			delete(s.gating, a.name)
			s.mu.Unlock()
		}()
		if a.hold {
			s.safely("operating-hours hold", a.name, func() {
				if a.mode == core.HoursShutdownGraceful {
					log.Info("operating hours closed; graceful shutdown is not yet available, stopping now",
						"harness", a.name, "hours_shutdown", string(a.mode))
				} else {
					log.Info("operating hours closed; stopping", "harness", a.name)
				}
				s.gate.Hold(a.name, a.mode)
			})
			return
		}
		s.safely("operating-hours release", a.name, func() {
			if a.ungated {
				log.Info("operating hours removed from a held harness; starting", "harness", a.name)
			} else {
				log.Info("operating hours opened; starting", "harness", a.name)
			}
			s.gate.Release(a.name)
		})
	}()
}
