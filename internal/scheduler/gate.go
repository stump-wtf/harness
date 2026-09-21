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
	// restarting), whether it is held, and whether a graceful close is in
	// flight. ok is false for an unknown harness.
	Status(name string) (up, held, closing, ok bool)
	// Lease reports name's after-hours lease end, ok=false when it has none.
	// A harness covered by a lease that has not ended is never held.
	Lease(name string) (until time.Time, ok bool)
	// CloseAt reports the instant name went out of hours — the anchor a
	// graceful close's deadline is measured from. ok is false when the
	// boundary cannot be determined and the close anchors to now instead.
	CloseAt(name string, now time.Time) (time.Time, bool)
	// Hold shuts name down for its hours without touching enabled intent.
	// Under a graceful mode it marks the close and returns; closeAt is the
	// instant the harness went out of hours.
	Hold(name string, mode core.HoursShutdownMode, closeAt time.Time)
	// CloseStep advances a graceful close by one observation of the
	// turn-state watch.
	CloseStep(name string, now time.Time)
	// Arm warms the turn-state watch for a close within armLead.
	Arm(name string, closeAt time.Time)
	// Release starts a held harness without touching enabled intent.
	Release(name string)
}

// gateEntry is one gated harness.
type gateEntry struct {
	raw  string
	expr hours.Expr
	mode core.HoursShutdownMode
}

// armLead is how close a close has to be before the pass warms the
// turn-state watch for it — the only trace I/O the daemon does outside a
// close itself (SPEC-0012 REQ "Turn State Signal": none the rest of the day).
const armLead = time.Minute

// gateAction is one decision the pass made, carried out after the lock drops.
type gateAction struct {
	name      string
	hold      bool // false: release
	mode      core.HoursShutdownMode
	closeAt   time.Time // the instant the harness went out of hours
	anchored  bool      // closeAt came from a real boundary, not a fallback
	stepAt    time.Time // the tick's clock, for a close step (no wall-clock read)
	closeStep bool      // advance a graceful close in flight
	arm       bool      // warm the turn-state watch for a close within armLead
	ungated   bool      // a release for a harness whose hours a reload removed
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
		in, next, _ := g.expr.In(now)
		up, held, closing, ok := s.gate.Status(name)
		if !ok {
			continue
		}
		switch {
		case !in && s.leased(name, now):
			// Covered by an after-hours lease: nothing to enforce — but if
			// its end is close, warm the watch a graceful close will need.
			if until, had := s.gate.Lease(name); had && until.Sub(now) <= armLead {
				if s.armOnce(name, until) {
					acts = append(acts, gateAction{name: name, arm: true, closeAt: until})
				}
			}
		case !in && closing:
			// A graceful close in flight: step it from this tick's clock.
			acts = append(acts, gateAction{name: name, closeStep: true, stepAt: now})
		case !in && up:
			closeAt, anchored := s.gate.CloseAt(name, now)
			delete(s.armed, name) // the close takes over from the warm-up
			acts = append(acts, gateAction{name: name, hold: true, mode: g.mode, closeAt: closeAt, anchored: anchored})
		case in && held:
			acts = append(acts, gateAction{name: name})
		case in && next.Sub(now) <= armLead:
			// The window ends within the minute: warm the watch a graceful
			// close will need, so its first step already has a sample.
			if s.armOnce(name, next) {
				acts = append(acts, gateAction{name: name, arm: true, closeAt: next})
			}
		}
	}
	for name := range s.ungated {
		delete(s.ungated, name)
		if _, held, _, ok := s.gate.Status(name); ok && held {
			delete(s.armed, name)
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

// armOnce reports whether name's watch still needs warming for closeAt: the
// pass re-evaluates every tick inside the arm lead, and one arm per close is
// the point. Caller holds s.mu.
func (s *Scheduler) armOnce(name string, closeAt time.Time) bool {
	if s.armed[name].Equal(closeAt) {
		return false
	}
	if s.armed == nil {
		s.armed = make(map[string]time.Time)
	}
	s.armed[name] = closeAt
	return true
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
		switch {
		case a.closeStep:
			s.safely("operating-hours close step", a.name, func() {
				s.gate.CloseStep(a.name, a.stepAt)
			})
		case a.arm:
			s.safely("operating-hours close arm", a.name, func() {
				s.gate.Arm(a.name, a.closeAt)
			})
		case a.hold:
			s.safely("operating-hours hold", a.name, func() {
				if a.mode == core.HoursShutdownGraceful {
					if a.anchored {
						log.Info("operating hours closed; closing gracefully",
							"harness", a.name,
							"went_out_of_hours", a.closeAt.Format(time.RFC3339))
					} else {
						log.Info("operating hours closed; closing gracefully",
							"harness", a.name,
							"went_out_of_hours", "unknown, anchoring the deadline to now")
					}
				} else {
					log.Info("operating hours closed; stopping", "harness", a.name)
				}
				s.gate.Hold(a.name, a.mode, a.closeAt)
			})
		default:
			s.safely("operating-hours release", a.name, func() {
				if a.ungated {
					log.Info("operating hours removed from a held harness; starting", "harness", a.name)
				} else {
					log.Info("operating hours opened; starting", "harness", a.name)
				}
				s.gate.Release(a.name)
			})
		}
	}()
}
