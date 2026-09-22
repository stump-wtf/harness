package scheduler

// harness_hours_changed Emission Tests
//
// SPEC-0012 REQ "Operating Hours Visibility": the event fires exactly on an
// in_hours flip, never on an unchanged tick — CLAUDE.md "A zero" cuts both
// ways here too, so this also proves the callback CAN fire before trusting
// its silence elsewhere.
//
// Governing: ADR-0019, SPEC-0012 REQ "Operating Hours Visibility".
//
// @joestump-agent 09/22/2026 - Added for stump.wtf/harness#385.

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// hoursChangeRecorder collects every HoursChanged callback, formatted as
// "name in next" ("next" is "-" when the callback passed a zero time).
type hoursChangeRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *hoursChangeRecorder) record(name string, in bool, next time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := "-"
	if !next.IsZero() {
		n = next.Format(time.RFC3339)
	}
	inStr := "out"
	if in {
		inStr = "in"
	}
	r.calls = append(r.calls, name+" "+inStr+" "+n)
}

func (r *hoursChangeRecorder) took() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.calls)
	r.calls = nil
	return out
}

// gateRigWithHours is gateRig plus a HoursChanged recorder wired in.
func gateRigWithHours(t *testing.T, now time.Time) (*rig, *fakeGate, *hoursChangeRecorder) {
	t.Helper()
	g := newFakeGate()
	rec := &hoursChangeRecorder{}
	r := &rig{clock: newFakeClock(now), store: newMemStore()}
	r.s = New(Options{
		Start: r.start, Clock: r.clock, Location: time.UTC, Store: r.store, Recorder: r,
		Gate: g, HoursChanged: rec.record,
	})
	t.Cleanup(r.s.Close)
	return r, g, rec
}

// TestHoursChangedFiresExactlyOnFlip drives a gated harness through a full
// close-then-open cycle and asserts the callback fires exactly twice — once
// per real flip — never on the many unchanged ticks either side.
func TestHoursChangedFiresExactlyOnFlip(t *testing.T) {
	r, g, rec := gateRigWithHours(t, mon(12, 58))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate))
	g.set("a", true, false)

	// The first observation seeds lastIn silently: applying the config and
	// running the first few ticks while still in hours must not fire.
	r.runUntil(mon(12, 59).Add(-time.Second), time.Second)
	if calls := rec.took(); len(calls) != 0 {
		t.Fatalf("seeding + steady in-hours ticks fired: %v", calls)
	}

	// Crossing the close: exactly one call, in=false, next=the next open.
	r.at(mon(13, 0))
	calls := rec.took()
	if len(calls) != 1 {
		t.Fatalf("at the close: calls = %v, want exactly 1", calls)
	}
	if calls[0] != "a out 2026-09-22T09:00:00Z" {
		t.Errorf("at the close: got %q, want in=out and next=the following day's 09:00 UTC", calls[0])
	}

	// Steady while held: no more calls, however many ticks pass.
	r.runUntil(mon(13, 5), time.Second)
	if calls := rec.took(); len(calls) != 0 {
		t.Errorf("steady while held fired: %v", calls)
	}

	// Crossing the open: exactly one call, in=true, next=the same day's close.
	r.at(mon(9, 0).AddDate(0, 0, 1))
	calls = rec.took()
	if len(calls) != 1 {
		t.Fatalf("at the open: calls = %v, want exactly 1", calls)
	}
	if calls[0] != "a in 2026-09-22T13:00:00Z" {
		t.Errorf("at the open: got %q, want in=in and next=that day's 13:00 UTC close", calls[0])
	}

	// Steady while running in hours again: no more calls.
	r.runUntil(mon(9, 5).AddDate(0, 0, 1), time.Second)
	if calls := rec.took(); len(calls) != 0 {
		t.Errorf("steady after reopening fired: %v", calls)
	}
}

// TestHoursChangedOmitsNextForWholeWeek pins the omission half of the
// contract on the event itself (mirrored on the wire by protocol's own
// omission test): an expression covering the entire week never has a next
// flip, so a real flip INTO one — a reload that widens a harness's window to
// the whole week while it happens to be out of hours — reports the change
// with next omitted (zero), not a stale or invented time.
func TestHoursChangedOmitsNextForWholeWeek(t *testing.T) {
	r, g, rec := gateRigWithHours(t, mon(8, 0)) // out of the 09:00-13:00 window
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate))
	g.set("a", false, true) // already held

	r.tick() // seeds lastIn["a"] = false; must not fire
	if calls := rec.took(); len(calls) != 0 {
		t.Fatalf("seeding tick fired: %v", calls)
	}

	// Reload to a whole-week window: still out-of-hours by lastIn's record,
	// but the new expression reads "in" everywhere — a real flip, reported
	// with next omitted since a whole-week expression has none.
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC Mon-Sun 00:00-24:00", core.HoursShutdownImmediate))
	r.tick()
	calls := rec.took()
	if len(calls) != 1 {
		t.Fatalf("tick after widening to a whole-week window: calls = %v, want exactly 1", calls)
	}
	if calls[0] != "a in -" {
		t.Errorf("whole-week flip: got %q, want in=in and next=- (no flip exists to report)", calls[0])
	}

	// Steady under the whole-week window: no further calls — it can never
	// flip again.
	r.runUntil(mon(8, 5), time.Second)
	if calls := rec.took(); len(calls) != 0 {
		t.Errorf("steady under a whole-week window fired: %v", calls)
	}
}
