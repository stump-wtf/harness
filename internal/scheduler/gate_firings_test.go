package scheduler

// Gate-pass tests for a harness whose operating_hours gate its FIRINGS
// (SPEC-0014): one that sets `triggers`. The pass must never hold or release
// it — a run in flight at the close continues, bounded by its timeout — and
// its one job is the catch-up: OpenFirings on the first in-hours evaluation
// after an outside_hours skip, and never otherwise. The fake clock drives
// every edge, including the end-exclusive close.
//
// Governing: SPEC-0014 REQ "Operating Hours On Triggered Harnesses"; SPEC-0012
// REQ "Gate Evaluation".
//
// @joestump 09/23/2026 - Added for stump.wtf/harness#484.

import (
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// firingGatedCfg is gatedCfg for a harness with a webhook trigger.
func firingGatedCfg(t *testing.T, name, expr string) *core.Config {
	t.Helper()
	cfg := gatedCfg(t, name, expr, core.HoursShutdownGraceful)
	h := cfg.Harnesses[name]
	h.Triggers = []string{"webhook.gh"}
	cfg.Harnesses[name] = h
	return cfg
}

func (g *fakeGate) skip(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.skipped[name] = true
}

// TestFiringGateNeverHoldsARun: a triggered harness with a run in flight
// (up) is left alone across the close — no arm, no hold, no close step — and
// is not "released" at the open. A resident harness beside it, same hours,
// is held as before, so the pass still enforces the hours it should.
func TestFiringGateNeverHoldsARun(t *testing.T) {
	r, g := gateRig(t, mon(17, 58))
	cfg := firingGatedCfg(t, "hook", "TZ=UTC Mon-Fri 09:00-18:00")
	res := gatedCfg(t, "res", "TZ=UTC Mon-Fri 09:00-18:00", core.HoursShutdownImmediate)
	cfg.Harnesses["res"] = res.Harnesses["res"]
	cfg.HarnessOrder = append(cfg.HarnessOrder, "res")
	r.s.Apply(cfg)
	g.set("hook", true, false)
	g.set("res", true, false)

	r.tick()
	r.at(mon(17, 59))
	wantCalls(t, g, "inside the arm lead", "arm res")
	r.at(mon(18, 0))
	wantCalls(t, g, "at the close", "hold res immediate")
	r.runUntil(mon(18, 5), time.Second)
	wantCalls(t, g, "after the close")

	g.set("hook", false, true) // even a held-looking triggered harness is not the pass's to start
	r.at(mon(9, 0).AddDate(0, 0, 1))
	wantCalls(t, g, "at the open", "release res")
}

// TestFiringGateCatchesUpOnFirstInHoursTick is the "Catching up at opening"
// scenario at the pass: skips over the weekend, nothing until Monday 09:00,
// then exactly one OpenFirings — and none on the ticks after it.
func TestFiringGateCatchesUpOnFirstInHoursTick(t *testing.T) {
	sat := time.Date(2026, 9, 26, 14, 0, 0, 0, time.UTC)
	nextMon := func(h, m, s int) time.Time { return time.Date(2026, 9, 28, h, m, s, 0, time.UTC) }
	r, g := gateRig(t, sat)
	r.s.Apply(firingGatedCfg(t, "hook", "TZ=UTC Mon-Fri 09:00-18:00"))

	g.skip("hook")
	r.tick()
	wantCalls(t, g, "saturday, skipped")

	r.at(nextMon(8, 59, 59))
	wantCalls(t, g, "a second before the open")
	r.at(nextMon(9, 0, 0))
	wantCalls(t, g, "at the open", "open hook")
	r.runUntil(nextMon(9, 0, 30), time.Second)
	wantCalls(t, g, "ticks after the catch-up")
}

// TestFiringGateNothingSkippedNothingOwed: in hours with no skip there is no
// catch-up, however many ticks pass.
func TestFiringGateNothingSkippedNothingOwed(t *testing.T) {
	r, g := gateRig(t, mon(8, 59))
	r.s.Apply(firingGatedCfg(t, "hook", "TZ=UTC Mon-Fri 09:00-18:00"))
	r.runUntil(mon(9, 1), time.Second)
	wantCalls(t, g, "an opening with nothing skipped")
}

// TestFiringGateCloseIsEndExclusive: a skip recorded exactly at the close is
// not caught up on that same tick — 18:00:00 is out of hours — but on the
// next opening.
func TestFiringGateCloseIsEndExclusive(t *testing.T) {
	r, g := gateRig(t, mon(17, 59))
	r.s.Apply(firingGatedCfg(t, "hook", "TZ=UTC Mon-Fri 09:00-18:00"))
	r.tick()
	wantCalls(t, g, "17:59")

	g.skip("hook")
	r.at(mon(18, 0))
	wantCalls(t, g, "the closing instant")
	r.at(mon(9, 0).AddDate(0, 0, 1))
	wantCalls(t, g, "the next opening", "open hook")
}
