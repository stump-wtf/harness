package observe

// Attribution Clock Tests
//
// A scan takes its `now` once, when it begins, and reads every session after
// that, so a row written while the scan runs is dated after the scan's clock.
// Attribution asks which harness's run covers the row's instant, and an open
// run used to end at the scan's `now` plus runtrace.Slack: a row newer than
// that matched no harness, was counted Unattributed, and the session's cursor
// moved past it, so no later scan delivered it either. The tests run the real
// crush adapter over a real store, as the daemon does.
//
// @joestump-agent 09/25/2026 - Added for #710.

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
)

// TestActivityNewerThanTheScanClockIsDelivered writes a tool call dated 10s
// past the clock the next scan begins at (far beyond runtrace.Slack) on a
// harness that is still running. It must be delivered once, not dropped.
func TestActivityNewerThanTheScanClockIsDelivered(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start, Updated: start.Add(time.Second),
		Messages: []rt.CrushMessage{userSays("go", start.Add(time.Second))}})
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	f.tick(start.Add(5 * time.Second)) // baseline read of the session
	drain(ch)

	scanAt := start.Add(10 * time.Second)
	rt.AppendCrushMessages(t, f.crushDB(), "s", crushGrep("g1", "x", scanAt.Add(10*time.Second))...)
	f.tick(scanAt)
	// A later scan cannot rescue a dropped row (its cursor has moved past it),
	// so asserting after both scans pins exactly-once delivery.
	f.tick(scanAt.Add(30 * time.Second))

	var tools []string
	for _, d := range describe(drain(ch)) {
		if strings.Contains(d, ":tool:") {
			tools = append(tools, d)
		}
	}
	if len(tools) != 1 || !strings.HasPrefix(tools[0], "worker:tool:grep@") {
		t.Fatalf("delivered tool events %v, want exactly one grep for worker (stats %+v)", tools, f.obs.Stats())
	}
	if s := f.obs.Stats(); s.Unattributed != 0 {
		t.Fatalf("Unattributed = %d, want 0 (stats %+v)", s.Unattributed, s)
	}
}

// TestActivityAfterAStoppedRunIsStillUnattributed is the control: a run that
// has ended does not stretch to cover a row newer than the scan's clock, so a
// harness that stopped is never credited with what came after it. It also
// proves the Unattributed count can fire in this rig.
func TestActivityAfterAStoppedRunIsStillUnattributed(t *testing.T) {
	f := newFixture(t, nil)
	stopped := running(start.Add(-time.Hour))
	stopped.State = core.StateStopped
	stopped.PID = 0
	stopped.LastExitAt = start.Add(2 * time.Second)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, stopped)
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start, Updated: start.Add(time.Second),
		Messages: []rt.CrushMessage{userSays("go", start.Add(time.Second))}})
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	f.tick(start.Add(5 * time.Second))
	drain(ch)

	scanAt := start.Add(10 * time.Second)
	rt.AppendCrushMessages(t, f.crushDB(), "s", crushGrep("g1", "x", scanAt.Add(10*time.Second))...)
	f.tick(scanAt)

	for _, d := range describe(drain(ch)) {
		if strings.Contains(d, ":tool:") {
			t.Fatalf("a stopped harness was credited with later activity: %s", d)
		}
	}
	if s := f.obs.Stats(); s.Unattributed == 0 {
		t.Fatalf("Unattributed = 0 for activity after the only run ended (stats %+v)", s)
	}
}
