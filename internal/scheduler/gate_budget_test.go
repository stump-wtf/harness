package scheduler

// Gate Pass And The Budget Day
//
// The budget day rides the gate pass's tick (SPEC-0021 REQ-3): the pass asks
// the Gate's BudgetDue on every tick, on the tick's own clock, which is what
// rolls the day over, and carries out what it answers: a budget hold added,
// and a one-shot's budget skips settled. The fake gate answers
// level-triggered, as the Manager does, so these pin that the pass acts once
// per answer, defers a harness that already has an action this tick, and
// never reads a clock of its own.
//
// Governing: ADR-0027; SPEC-0021 REQ-3, REQ-5, REQ-20; design.md § "The
// budget day reuses the gate's clock".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"slices"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// BudgetDue is asked once per tick, with the tick's own clock, gated
// harnesses or not, and across a suspend-sized jump.
func TestBudgetDueRidesTheTick(t *testing.T) {
	r, g := gateRig(t, mon(23, 30))
	r.s.Apply(&core.Config{})
	r.tick()
	r.at(mon(23, 45))
	woke := mon(8, 0).AddDate(0, 0, 1)
	r.at(woke)
	g.mu.Lock()
	asked := append([]time.Time(nil), g.asked...)
	g.mu.Unlock()
	want := []time.Time{mon(23, 30), mon(23, 45), woke}
	if len(asked) != len(want) {
		t.Fatalf("BudgetDue asked %d times (%v), want once per tick %v", len(asked), asked, want)
	}
	for i := range want {
		if !asked[i].Equal(want[i]) {
			t.Errorf("tick %d asked at %v, want the tick's clock %v", i, asked[i], want[i])
		}
	}
	wantCalls(t, g, "nothing due")
}

// A budget hold and a settle-up are each carried out once; a harness the
// pass already decided something for this tick waits for the next.
func TestBudgetPassActsOnWhatIsDue(t *testing.T) {
	r, g := gateRig(t, mon(13, 30))
	r.s.Apply(gatedCfg(t, "gated", "TZ=UTC 09:00-14:00", core.HoursShutdownImmediate))
	g.set("gated", true, false)
	g.mu.Lock()
	g.due["resident"] = core.HoldSetOf(core.HoldBudget)
	g.settle["sweep"] = true
	g.mu.Unlock()

	r.tick()
	// Each action runs on its own goroutine, so their order is not fixed.
	if got := g.took(); !slices.Equal(slices.Sorted(slices.Values(got)), []string{"budget-hold resident budget", "settle sweep"}) {
		t.Fatalf("13:30: gate calls = %q, want one budget hold and one settle-up", got)
	}
	if _, holds, _, _ := g.Status("resident"); holds != core.HoldSetOf(core.HoldBudget) {
		t.Fatalf("resident holds = %s, want budget", holds)
	}
	r.tick()
	wantCalls(t, g, "13:30 again: nothing left due")

	// 14:00: the gated harness is held for its hours on this tick, so a
	// budget hold due on it the same tick waits for the next.
	g.mu.Lock()
	g.due["gated"] = core.HoldSetOf(core.HoldBudget)
	g.mu.Unlock()
	r.at(mon(14, 0))
	wantCalls(t, g, "14:00", "hold gated immediate")
	r.tick()
	wantCalls(t, g, "14:00 again", "budget-hold gated budget")
	if _, holds, _, _ := g.Status("gated"); holds != core.HoldSetOf(core.HoldHours, core.HoldBudget) {
		t.Fatalf("gated holds = %s, want hours and budget", holds)
	}
}
