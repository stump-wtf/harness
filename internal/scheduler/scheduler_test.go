package scheduler

// Scheduler Tests
//
// Every temporal scenario runs against fakeClock and drives evaluate()
// directly, so a five-hour suspend or a DST weekend takes microseconds and
// never depends on the host's zone, load, or timer behavior. Only the
// lifecycle tests run the real tick goroutine, and even they feed it ticks by
// hand.
//
// Governing: ADR-0013; SPEC-0008 REQ "Suspend-Safe Schedule Evaluation", REQ
// "Missed Window Handling", REQ "Schedule Time Zone", REQ "Schedule
// Reconciliation On Reload", REQ "Scheduler Fault Isolation"; issue #117.
//
// @joestump-agent 09/11/2026 - Rewritten for the wall-clock tick (issue #117).

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	_ "time/tzdata" // the DST cases must not depend on the runner's zoneinfo

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// fakeClock is a hand-driven Clock.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	ticks   chan time.Time
	periods []time.Duration
	stopped bool
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, ticks: make(chan time.Time)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.periods = append(c.periods, d)
	return c.ticks, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stopped = true
	}
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) tickerPeriods() ([]time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.periods), c.stopped
}

// memStore is an in-memory Store.
type memStore struct {
	mu     sync.Mutex
	marks  map[string]Mark
	writes int
	err    error
}

func newMemStore() *memStore { return &memStore{marks: make(map[string]Mark)} }

func (m *memStore) LoadMark(name string) (Mark, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mark, ok := m.marks[name]
	return mark, ok
}

func (m *memStore) UpdateMarks(put map[string]Mark, forget []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	for _, name := range forget {
		delete(m.marks, name)
	}
	for name, mark := range put {
		m.marks[name] = mark
	}
	return m.err
}

func (m *memStore) mark(name string) (Mark, bool) { return m.LoadMark(name) }

func (m *memStore) writeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writes
}

// rig wires a Scheduler to a fake clock, an in-memory store, and recorders
// for what it starts and what it records missed.
type rig struct {
	s     *Scheduler
	clock *fakeClock
	store *memStore

	mu     sync.Mutex
	hook   func(Firing)
	fires  []Firing
	misses []MissedWindow
}

func newRig(t *testing.T, now time.Time, loc *time.Location) *rig {
	t.Helper()
	return newRigWithStore(t, now, loc, newMemStore())
}

func newRigWithStore(t *testing.T, now time.Time, loc *time.Location, store *memStore) *rig {
	t.Helper()
	r := &rig{clock: newFakeClock(now), store: store}
	r.s = New(Options{Start: r.start, Clock: r.clock, Location: loc, Store: store, Recorder: r})
	t.Cleanup(r.s.Close)
	return r
}

func (r *rig) setHook(fn func(Firing)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hook = fn
}

func (r *rig) start(f Firing) {
	r.mu.Lock()
	hook := r.hook
	r.mu.Unlock()
	if hook != nil {
		hook(f)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fires = append(r.fires, f)
}

func (r *rig) RecordMissed(m MissedWindow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.misses = append(r.misses, m)
}

// tick evaluates once and waits for every firing it dispatched.
func (r *rig) tick() {
	r.s.evaluate()
	r.s.firings.Wait()
}

// at sets the clock to t and ticks.
func (r *rig) at(t time.Time) {
	r.clock.set(t)
	r.tick()
}

// runUntil steps the clock to end, ticking after every step.
func (r *rig) runUntil(end time.Time, step time.Duration) {
	for r.clock.Now().Before(end) {
		r.clock.advance(step)
		r.tick()
	}
}

func (r *rig) fired() []Firing {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.fires)
}

func (r *rig) missed() []MissedWindow {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.misses)
}

type job struct {
	name, spec string
	catchUp    bool
}

func cfgOf(jobs ...job) *core.Config {
	cfg := &core.Config{Harnesses: make(map[string]core.Harness)}
	for _, j := range jobs {
		cfg.Harnesses[j.name] = core.Harness{Name: j.name, Prompt: "test", Schedule: j.spec, CatchUp: j.catchUp}
		cfg.HarnessOrder = append(cfg.HarnessOrder, j.name)
	}
	return cfg
}

func utc(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load zone %s: %v", name, err)
	}
	return loc
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

func countFor(fires []Firing, name string) int {
	n := 0
	for _, f := range fires {
		if f.Name == name {
			n++
		}
	}
	return n
}

func windowsBetween(fires []Firing, from, to time.Time) []time.Time {
	var out []time.Time
	for _, f := range fires {
		if !f.Window.Before(from) && f.Window.Before(to) {
			out = append(out, f.Window)
		}
	}
	return out
}

func sameMiss(a, b MissedWindow) bool {
	return a.Name == b.Name && a.Spec == b.Spec && a.Count == b.Count &&
		a.First.Equal(b.First) && a.Last.Equal(b.Last) && a.DetectedAt.Equal(b.DetectedAt)
}

func sameFiring(a, b Firing) bool {
	return a.Name == b.Name && a.Trigger == b.Trigger && a.Window.Equal(b.Window) &&
		a.Late == b.Late && a.Missed == b.Missed
}

// TestEveryOneSecondFiresRepeatedly: a job on `@every 1s` fires once per
// elapsed window, every window, and a tick with no new window fires nothing —
// the next run comes from the schedule, never from the last one ending.
func TestEveryOneSecondFiresRepeatedly(t *testing.T) {
	t0 := utc(2026, 9, 11, 12, 0, 0)
	r := newRig(t, t0, time.UTC)
	r.s.Apply(cfgOf(job{"worker", "@every 1s", false}))

	for range 10 {
		r.clock.advance(time.Second)
		r.tick()
	}
	r.tick() // no window elapsed since the last tick

	fires := r.fired()
	if len(fires) != 10 {
		t.Fatalf("fired %d times over 10 windows, want 10", len(fires))
	}
	for i, f := range fires {
		want := t0.Add(time.Duration(i+1) * time.Second)
		if f.Trigger != TriggerSchedule || !f.Window.Equal(want) {
			t.Errorf("firing %d = %+v, want an on-time firing for %v", i, f, want)
		}
	}
	if m := r.missed(); len(m) != 0 {
		t.Errorf("recorded misses on an uninterrupted run: %+v", m)
	}
}

// TestSuspendAcrossWindowsCatchUpFiresOnce: a jump past five hourly windows
// with catch_up = true starts exactly one run, not five.
func TestSuspendAcrossWindowsCatchUpFiresOnce(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 1, 30, 0), time.UTC)
	r.s.Apply(cfgOf(job{"sweep", "0 * * * *", true}))
	r.tick()

	r.at(utc(2026, 9, 11, 6, 30, 0)) // woke 4h30m after the 02:00 window
	r.at(utc(2026, 9, 11, 6, 30, 30))

	fires := r.fired()
	want := Firing{Name: "sweep", Trigger: TriggerCatchUp, Window: utc(2026, 9, 11, 6, 0, 0), Late: 30 * time.Minute, Missed: 5}
	if len(fires) != 1 || !sameFiring(fires[0], want) {
		t.Fatalf("fires = %+v, want exactly %+v", fires, want)
	}
	if m := r.missed(); len(m) != 0 {
		t.Errorf("catch_up = true recorded misses: %+v", m)
	}

	r.at(utc(2026, 9, 11, 7, 0, 0))
	if fires := r.fired(); len(fires) != 2 || fires[1].Trigger != TriggerSchedule {
		t.Errorf("the next real window did not fire on time: %+v", fires)
	}
}

// TestSuspendAcrossWindowsWithoutCatchUpRecordsOneMiss: the same jump with
// catch_up = false spawns nothing and records exactly one miss covering all
// five windows.
func TestSuspendAcrossWindowsWithoutCatchUpRecordsOneMiss(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 1, 30, 0), time.UTC)
	r.s.Apply(cfgOf(job{"sweep", "0 * * * *", false}))
	r.tick()

	r.at(utc(2026, 9, 11, 6, 30, 0))
	r.at(utc(2026, 9, 11, 6, 30, 30))

	if fires := r.fired(); len(fires) != 0 {
		t.Fatalf("catch_up = false spawned a run for missed windows: %+v", fires)
	}
	misses := r.missed()
	want := MissedWindow{
		Name: "sweep", Spec: "0 * * * *",
		First: utc(2026, 9, 11, 2, 0, 0), Last: utc(2026, 9, 11, 6, 0, 0), Count: 5,
		DetectedAt: utc(2026, 9, 11, 6, 30, 0),
	}
	if len(misses) != 1 || !sameMiss(misses[0], want) {
		t.Fatalf("misses = %+v, want exactly %+v", misses, want)
	}

	r.at(utc(2026, 9, 11, 7, 0, 0))
	if fires := r.fired(); len(fires) != 1 || fires[0].Trigger != TriggerSchedule {
		t.Errorf("the next real window did not fire on time: %+v", fires)
	}
}

// TestDaemonStartAfterMissedWindowMatchesWake: a daemon that boots after
// windows it was down for reaches exactly the decision a wake does, through
// Start's first evaluation rather than a parallel boot path.
func TestDaemonStartAfterMissedWindowMatchesWake(t *testing.T) {
	const spec = "0 * * * *"
	for _, catchUp := range []bool{false, true} {
		name := "catch_up=false"
		if catchUp {
			name = "catch_up=true"
		}
		t.Run(name, func(t *testing.T) {
			wake := newRig(t, utc(2026, 9, 11, 1, 30, 0), time.UTC)
			wake.s.Apply(cfgOf(job{"sweep", spec, catchUp}))
			wake.tick()
			wake.at(utc(2026, 9, 11, 6, 30, 0))

			store := newMemStore()
			store.marks["sweep"] = Mark{Spec: spec, DecidedThrough: utc(2026, 9, 11, 1, 30, 0)}
			boot := newRigWithStore(t, utc(2026, 9, 11, 6, 30, 0), time.UTC, store)
			boot.s.Apply(cfgOf(job{"sweep", spec, catchUp}))
			boot.s.Start()
			eventually(t, func() bool { return len(boot.fired())+len(boot.missed()) > 0 })
			boot.s.Close()

			wf, bf := wake.fired(), boot.fired()
			if len(wf) != len(bf) || (len(wf) == 1 && !sameFiring(wf[0], bf[0])) {
				t.Errorf("fires differ: wake %+v, boot %+v", wf, bf)
			}
			wm, bm := wake.missed(), boot.missed()
			if len(wm) != len(bm) || (len(wm) == 1 && !sameMiss(wm[0], bm[0])) {
				t.Errorf("misses differ: wake %+v, boot %+v", wm, bm)
			}
			if len(wf)+len(wm) != 1 {
				t.Errorf("expected exactly one decision, got fires %+v misses %+v", wf, wm)
			}
		})
	}
}

// TestWakeJustAfterWindowRunsItOnTime: waking 20s after the 03:00 window
// runs 03:00 as an ordinary firing; the 02:00 window slept through is a miss
// without catch_up, and is covered by that same single run with it.
func TestWakeJustAfterWindowRunsItOnTime(t *testing.T) {
	for _, catchUp := range []bool{false, true} {
		r := newRig(t, utc(2026, 9, 11, 1, 59, 30), time.UTC)
		r.s.Apply(cfgOf(job{"sweep", "0 * * * *", catchUp}))
		r.tick()
		r.at(utc(2026, 9, 11, 3, 0, 20))

		fires := r.fired()
		want := Firing{Name: "sweep", Trigger: TriggerSchedule, Window: utc(2026, 9, 11, 3, 0, 0), Late: 20 * time.Second}
		if len(fires) != 1 || !sameFiring(fires[0], want) {
			t.Fatalf("catch_up=%v: fires = %+v, want exactly %+v", catchUp, fires, want)
		}
		misses := r.missed()
		switch {
		case catchUp && len(misses) != 0:
			t.Errorf("catch_up=true: the on-time run covers the earlier window, but misses = %+v", misses)
		case !catchUp:
			wantMiss := MissedWindow{
				Name: "sweep", Spec: "0 * * * *",
				First: utc(2026, 9, 11, 2, 0, 0), Last: utc(2026, 9, 11, 2, 0, 0), Count: 1,
				DetectedAt: utc(2026, 9, 11, 3, 0, 20),
			}
			if len(misses) != 1 || !sameMiss(misses[0], wantMiss) {
				t.Errorf("catch_up=false: misses = %+v, want exactly %+v", misses, wantMiss)
			}
		}
	}
}

// TestDelayedTickWithinGraceCoalesces: a tick that lands a few windows late
// under load is still on time: one run, nothing recorded missed.
func TestDelayedTickWithinGraceCoalesces(t *testing.T) {
	t0 := utc(2026, 9, 11, 12, 0, 0)
	r := newRig(t, t0, time.UTC)
	r.s.Apply(cfgOf(job{"worker", "@every 1s", false}))
	r.at(t0.Add(3 * time.Second))

	if fires := r.fired(); len(fires) != 1 || fires[0].Trigger != TriggerSchedule {
		t.Errorf("fires = %+v, want one on-time firing", fires)
	}
	if m := r.missed(); len(m) != 0 {
		t.Errorf("a 3s-late tick recorded misses: %+v", m)
	}
}

// TestMissedScanIsBounded: an `@every 1s` job asleep for five hours stops
// walking windows at maxWindowScan, records one miss, and re-arms from now.
func TestMissedScanIsBounded(t *testing.T) {
	t0 := utc(2026, 9, 11, 12, 0, 0)
	r := newRig(t, t0, time.UTC)
	r.s.Apply(cfgOf(job{"worker", "@every 1s", false}))
	r.tick()
	wake := t0.Add(5 * time.Hour)
	r.at(wake)

	misses := r.missed()
	if len(misses) != 1 || misses[0].Count != maxWindowScan {
		t.Fatalf("misses = %+v, want one miss counting %d windows", misses, maxWindowScan)
	}
	if next, ok := r.s.NextFire("worker"); !ok || !next.Equal(wake.Add(time.Second)) {
		t.Errorf("NextFire = %v, want re-armed from now (%v)", next, wake.Add(time.Second))
	}
	r.at(wake.Add(time.Second))
	if fires := r.fired(); len(fires) != 1 {
		t.Errorf("fires after re-arm = %+v, want one", fires)
	}
}

// TestDisarmedJobIsNeverDue: a job whose schedule is removed after its window
// came due, but before the tick that would have evaluated it, neither fires
// nor records a miss, and its mark is forgotten.
func TestDisarmedJobIsNeverDue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after *core.Config
	}{
		{"schedule removed", &core.Config{
			Harnesses:    map[string]core.Harness{"sweep": {Name: "sweep", Prompt: "test"}},
			HarnessOrder: []string{"sweep"},
		}},
		{"harness removed", &core.Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, utc(2026, 9, 11, 1, 30, 0), time.UTC)
			r.s.Apply(cfgOf(job{"sweep", "0 * * * *", true}))
			r.clock.set(utc(2026, 9, 11, 6, 30, 0)) // several windows due…
			r.s.Apply(tc.after)                     // …then disarmed before any tick
			r.tick()

			if fires, misses := r.fired(), r.missed(); len(fires)+len(misses) != 0 {
				t.Errorf("disarmed job was evaluated: fires %+v misses %+v", fires, misses)
			}
			if _, ok := r.s.NextFire("sweep"); ok {
				t.Error("disarmed job still reports a next fire")
			}
			if _, ok := r.store.mark("sweep"); ok {
				t.Error("disarmed job's mark was not forgotten")
			}
		})
	}
}

// TestDSTSpringForwardFiresOnce: on the day America/New_York skips 02:00-03:00,
// a job pinned to a time of day runs exactly once — a window inside the gap
// runs at the instant the pre-jump offset names instead of being dropped — and
// the next day resumes at its wall-clock time.
func TestDSTSpringForwardFiresOnce(t *testing.T) {
	ny := mustZone(t, "America/New_York")
	cases := []struct {
		name string
		spec string
		loc  *time.Location
		want time.Time // the one run on 2026-03-08
		next time.Time // the run on 2026-03-09
	}{
		{"window inside the gap", "30 2 * * *", ny, utc(2026, 3, 8, 7, 30, 0), utc(2026, 3, 9, 6, 30, 0)},
		{"window at the gap start", "0 2 * * *", ny, utc(2026, 3, 8, 7, 0, 0), utc(2026, 3, 9, 6, 0, 0)},
		{"window at the gap end", "0 3 * * *", ny, utc(2026, 3, 8, 7, 0, 0), utc(2026, 3, 9, 7, 0, 0)},
		{"window before the gap", "30 1 * * *", ny, utc(2026, 3, 8, 6, 30, 0), utc(2026, 3, 9, 5, 30, 0)},
		{"CRON_TZ pins the zone on a UTC daemon", "CRON_TZ=America/New_York 30 2 * * *", time.UTC, utc(2026, 3, 8, 7, 30, 0), utc(2026, 3, 9, 6, 30, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, utc(2026, 3, 7, 17, 0, 0), tc.loc) // Mar 7 12:00 EST
			r.s.Apply(cfgOf(job{"sweep", tc.spec, false}))
			r.runUntil(utc(2026, 3, 9, 16, 0, 0), 30*time.Second) // Mar 9 12:00 EDT

			day := windowsBetween(r.fired(), time.Date(2026, 3, 8, 0, 0, 0, 0, ny), time.Date(2026, 3, 9, 0, 0, 0, 0, ny))
			if len(day) != 1 || !day[0].Equal(tc.want) {
				t.Errorf("runs on spring-forward day = %v, want exactly [%v]", day, tc.want)
			}
			after := windowsBetween(r.fired(), time.Date(2026, 3, 9, 0, 0, 0, 0, ny), time.Date(2026, 3, 10, 0, 0, 0, 0, ny))
			if len(after) != 1 || !after[0].Equal(tc.next) {
				t.Errorf("runs the day after = %v, want exactly [%v]", after, tc.next)
			}
			if m := r.missed(); len(m) != 0 {
				t.Errorf("recorded misses across DST: %+v", m)
			}
		})
	}
}

// TestDSTFallBackFiresOnce: on the day 01:00-02:00 happens twice, a job pinned
// inside that hour runs only at its first occurrence.
func TestDSTFallBackFiresOnce(t *testing.T) {
	ny := mustZone(t, "America/New_York")
	cases := []struct {
		name string
		spec string
		loc  *time.Location
		want time.Time
	}{
		{"window in the repeated hour", "30 1 * * *", ny, utc(2026, 11, 1, 5, 30, 0)}, // 01:30 EDT
		{"window after the repeated hour", "30 2 * * *", ny, utc(2026, 11, 1, 7, 30, 0)},
		{"CRON_TZ pins the zone on a UTC daemon", "CRON_TZ=America/New_York 30 1 * * *", time.UTC, utc(2026, 11, 1, 5, 30, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, utc(2026, 10, 31, 16, 0, 0), tc.loc) // Oct 31 12:00 EDT
			r.s.Apply(cfgOf(job{"sweep", tc.spec, false}))
			r.runUntil(utc(2026, 11, 2, 17, 0, 0), 30*time.Second)

			day := windowsBetween(r.fired(), time.Date(2026, 11, 1, 0, 0, 0, 0, ny), time.Date(2026, 11, 2, 0, 0, 0, 0, ny))
			if len(day) != 1 || !day[0].Equal(tc.want) {
				t.Errorf("runs on fall-back day = %v, want exactly [%v]", day, tc.want)
			}
		})
	}
}

// TestDSTEveryHourCadenceRunsInRealTime: an expression that fires every hour
// is a cadence, not a time of day, so across both transitions it runs once per
// real hour — no hour dropped at spring-forward, the repeated hour run twice
// at fall-back.
func TestDSTEveryHourCadenceRunsInRealTime(t *testing.T) {
	ny := mustZone(t, "America/New_York")
	for _, tc := range []struct {
		name       string
		start, end time.Time
	}{
		{"spring forward", utc(2026, 3, 8, 4, 30, 0), utc(2026, 3, 8, 9, 30, 0)},
		{"fall back", utc(2026, 11, 1, 3, 30, 0), utc(2026, 11, 1, 8, 30, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.start, ny)
			r.s.Apply(cfgOf(job{"poll", "0 * * * *", false}))
			r.runUntil(tc.end, 30*time.Second)

			got := windowsBetween(r.fired(), tc.start, tc.end)
			if len(got) != 5 {
				t.Fatalf("runs = %v, want 5 (one per real hour)", got)
			}
			for i := 1; i < len(got); i++ {
				if gap := got[i].Sub(got[i-1]); gap != time.Hour {
					t.Errorf("gap %d = %v, want 1h (runs %v)", i, gap, got)
				}
			}
		})
	}
}

// TestClockSteppingBackwardsDoesNotRefire: after a run, the wall clock steps
// back an hour; passing 03:00 again does not run the job a second time, and a
// daemon restarted into the stepped-back clock agrees.
func TestClockSteppingBackwardsDoesNotRefire(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 2, 59, 0), time.UTC)
	r.s.Apply(cfgOf(job{"sweep", "0 3 * * *", false}))
	r.at(utc(2026, 9, 11, 3, 0, 0))
	if len(r.fired()) != 1 {
		t.Fatalf("setup: expected the 03:00 run, got %+v", r.fired())
	}

	r.at(utc(2026, 9, 11, 2, 0, 0))
	r.runUntil(utc(2026, 9, 11, 4, 0, 0), 30*time.Second)
	if fires := r.fired(); len(fires) != 1 {
		t.Errorf("passing 03:00 again after a backwards step refired: %+v", fires)
	}
	if next, _ := r.s.NextFire("sweep"); !next.Equal(utc(2026, 9, 12, 3, 0, 0)) {
		t.Errorf("NextFire = %v, want tomorrow 03:00", next)
	}

	boot := newRigWithStore(t, utc(2026, 9, 11, 2, 30, 0), time.UTC, r.store)
	boot.s.Apply(cfgOf(job{"sweep", "0 3 * * *", false}))
	boot.runUntil(utc(2026, 9, 11, 4, 0, 0), 30*time.Second)
	if fires, misses := boot.fired(), boot.missed(); len(fires)+len(misses) != 0 {
		t.Errorf("restart into a stepped-back clock re-decided a window: fires %+v misses %+v", fires, misses)
	}
}

// TestZonePrefixOverridesDaemonZone: CRON_TZ= and TZ= evaluate the expression
// in the named zone whatever the daemon's own zone is; an unprefixed
// expression stays in the daemon's zone.
func TestZonePrefixOverridesDaemonZone(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	r := newRig(t, utc(2026, 9, 11, 8, 0, 0), la)
	r.s.Apply(cfgOf(
		job{"cron-tz", "CRON_TZ=UTC 0 9 * * *", false},
		job{"tz", "TZ=UTC 0 9 * * *", false},
		job{"half-hour-zone", "CRON_TZ=Asia/Kolkata 0 15 * * *", false},
		job{"every-hour-zone", "CRON_TZ=Asia/Kolkata 0 * * * *", false},
		job{"local", "0 9 * * *", false},
	))
	want := map[string]time.Time{
		"cron-tz":         utc(2026, 9, 11, 9, 0, 0),
		"tz":              utc(2026, 9, 11, 9, 0, 0),
		"half-hour-zone":  utc(2026, 9, 11, 9, 30, 0),
		"every-hour-zone": utc(2026, 9, 11, 8, 30, 0),
		"local":           utc(2026, 9, 11, 16, 0, 0), // 09:00 PDT
	}
	for name, w := range want {
		if next, ok := r.s.NextFire(name); !ok || !next.Equal(w) {
			t.Errorf("%s: NextFire = %v (ok=%v), want %v", name, next, ok, w)
		}
	}

	r.runUntil(utc(2026, 9, 11, 16, 15, 0), 30*time.Second)
	for name, w := range want {
		var windows []time.Time
		for _, f := range r.fired() {
			if f.Name == name {
				windows = append(windows, f.Window)
			}
		}
		if len(windows) == 0 || !windows[0].Equal(w) {
			t.Errorf("%s: first run at %v, want %v", name, windows, w)
		}
	}
}

// TestSlowStartDoesNotBlockTheTick: a start that never returns cannot delay
// evaluating, or firing, any other entry.
func TestSlowStartDoesNotBlockTheTick(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 12, 0, 0), time.UTC)
	release := make(chan struct{})
	r.setHook(func(f Firing) {
		if f.Name == "slow" {
			<-release
		}
	})
	r.s.Apply(cfgOf(job{"slow", "@every 1s", false}, job{"fast", "@every 1s", false}))

	for i := 1; i <= 3; i++ {
		r.clock.advance(time.Second)
		done := make(chan struct{})
		go func() {
			r.s.evaluate()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("tick %d blocked behind a slow start", i)
		}
		eventually(t, func() bool { return countFor(r.fired(), "fast") == i })
	}
	if n := countFor(r.fired(), "slow"); n != 0 {
		t.Fatalf("slow start returned early (%d)", n)
	}
	close(release)
	r.s.firings.Wait()
	if n := countFor(r.fired(), "slow"); n != 3 {
		t.Errorf("slow firings = %d, want 3", n)
	}
}

// TestFiringPanicIsRecovered: a panicking start is contained and later
// firings still happen.
func TestFiringPanicIsRecovered(t *testing.T) {
	t0 := utc(2026, 9, 11, 12, 0, 0)
	r := newRig(t, t0, time.UTC)
	var calls atomic.Int32
	r.setHook(func(Firing) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
	})
	r.s.Apply(cfgOf(job{"worker", "@every 1s", false}))
	r.at(t0.Add(time.Second))
	r.at(t0.Add(2 * time.Second))

	if fires := r.fired(); len(fires) != 1 || !fires[0].Window.Equal(t0.Add(2*time.Second)) {
		t.Errorf("fires after a panic = %+v, want the second window", fires)
	}
}

// TestCloseWaitsForInFlightFiring: shutdown waits for a firing being handled.
func TestCloseWaitsForInFlightFiring(t *testing.T) {
	t0 := utc(2026, 9, 11, 12, 0, 0)
	r := newRig(t, t0, time.UTC)
	entered, release := make(chan struct{}), make(chan struct{})
	r.setHook(func(Firing) {
		close(entered)
		<-release
	})
	r.s.Apply(cfgOf(job{"worker", "@every 1s", false}))
	r.clock.advance(time.Second)
	r.s.evaluate()
	<-entered

	closed := make(chan struct{})
	go func() {
		r.s.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a firing was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the firing finished")
	}
}

// TestMarkIsDurableBeforeRunStarts: by the time a run starts, the store
// already holds the window it accounts for.
func TestMarkIsDurableBeforeRunStarts(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 2, 59, 30), time.UTC)
	r.s.Apply(cfgOf(job{"sweep", "0 3 * * *", false}))
	firedAt := utc(2026, 9, 11, 3, 0, 10)
	r.setHook(func(f Firing) {
		m, ok := r.store.mark("sweep")
		if !ok || !m.DecidedThrough.Equal(f.Window) || !m.LastRunAt.Equal(firedAt) {
			t.Errorf("mark at run start = %+v (ok=%v), want decided through %v and last run %v", m, ok, f.Window, firedAt)
		}
	})
	r.at(firedAt)
	if len(r.fired()) != 1 {
		t.Fatalf("expected one run, got %+v", r.fired())
	}
}

// TestRestartRightAfterFiringDoesNotRefire: a daemon that crashes just after
// firing and restarts inside the grace window does not run it again.
func TestRestartRightAfterFiringDoesNotRefire(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 2, 59, 30), time.UTC)
	r.s.Apply(cfgOf(job{"sweep", "0 3 * * *", false}))
	r.at(utc(2026, 9, 11, 3, 0, 0))
	if len(r.fired()) != 1 {
		t.Fatalf("setup: expected the 03:00 run, got %+v", r.fired())
	}

	boot := newRigWithStore(t, utc(2026, 9, 11, 3, 0, 5), time.UTC, r.store)
	boot.s.Apply(cfgOf(job{"sweep", "0 3 * * *", false}))
	boot.tick()
	if fires, misses := boot.fired(), boot.missed(); len(fires)+len(misses) != 0 {
		t.Errorf("restart refired or re-recorded the 03:00 window: fires %+v misses %+v", fires, misses)
	}
	if next, _ := boot.s.NextFire("sweep"); !next.Equal(utc(2026, 9, 12, 3, 0, 0)) {
		t.Errorf("NextFire after restart = %v, want tomorrow 03:00", next)
	}
}

// TestArmingPersistsMark: a schedule armed with no prior mark records "decided
// through now", so an outage before its first window is still detectable.
func TestArmingPersistsMark(t *testing.T) {
	now := utc(2026, 9, 11, 12, 0, 0)
	r := newRig(t, now, time.UTC)
	r.s.Apply(cfgOf(job{"sweep", "0 3 * * *", false}))
	m, ok := r.store.mark("sweep")
	if !ok || m.Spec != "0 3 * * *" || !m.DecidedThrough.Equal(now) {
		t.Errorf("mark = %+v (ok=%v), want spec and decided-through-now", m, ok)
	}
}

// TestChangedSpecDiscardsMark: windows of an old expression say nothing about
// a new one, so a mark taken under a different spec is ignored — no miss is
// invented for the new schedule's windows while the daemon was down.
func TestChangedSpecDiscardsMark(t *testing.T) {
	store := newMemStore()
	store.marks["sweep"] = Mark{Spec: "0 3 * * *", DecidedThrough: utc(2026, 9, 9, 3, 0, 0)}
	now := utc(2026, 9, 11, 12, 0, 0)
	r := newRigWithStore(t, now, time.UTC, store)
	r.s.Apply(cfgOf(job{"sweep", "0 4 * * *", false}))
	r.tick()

	if fires, misses := r.fired(), r.missed(); len(fires)+len(misses) != 0 {
		t.Errorf("stale mark produced decisions: fires %+v misses %+v", fires, misses)
	}
	if m, _ := store.mark("sweep"); m.Spec != "0 4 * * *" || !m.DecidedThrough.Equal(now) {
		t.Errorf("mark = %+v, want re-armed under the new spec from now", m)
	}
}

// TestPersistFailureStillFires: a store that cannot write does not stop the
// job from running.
func TestPersistFailureStillFires(t *testing.T) {
	store := newMemStore()
	store.err = errors.New("disk full")
	t0 := utc(2026, 9, 11, 12, 0, 0)
	r := newRigWithStore(t, t0, time.UTC, store)
	r.s.Apply(cfgOf(job{"worker", "@every 1s", false}))
	r.at(t0.Add(time.Second))
	if len(r.fired()) != 1 {
		t.Errorf("fires with a failing store = %+v, want one", r.fired())
	}
}

// TestApplyNoSchedules verifies Apply with no scheduled harnesses registers
// nothing.
func TestApplyNoSchedules(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 12, 0, 0), time.UTC)
	r.s.Apply(&core.Config{
		Harnesses:    map[string]core.Harness{"a": {Name: "a", Prompt: "test"}},
		HarnessOrder: []string{"a"},
	})
	if len(r.s.entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(r.s.entries))
	}
	if r.store.writeCount() != 0 {
		t.Errorf("nothing to persist, but the store was written %d times", r.store.writeCount())
	}
}

// TestInvalidScheduleSkipped verifies an invalid cron expression is skipped
// without error (defense in depth: config validation rejects these at parse
// time, so the scheduler only logs).
func TestInvalidScheduleSkipped(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 12, 0, 0), time.UTC)
	r.s.Apply(cfgOf(job{"a", "not-a-cron", false}, job{"b", "0 3 * * *", false}))
	if _, ok := r.s.entries["a"]; ok {
		t.Error("invalid schedule was registered")
	}
	if _, ok := r.s.entries["b"]; !ok {
		t.Error("one invalid schedule stopped the rest from reconciling")
	}
}

// TestReapplyKeepsUnchangedEntry verifies an unchanged schedule survives a
// re-Apply as the SAME entry with the same next window — the reconcile must not
// rebuild it, or interval schedules (@every N) would restart their countdown
// on every config reload and could starve forever under periodic rewrites.
func TestReapplyKeepsUnchangedEntry(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 12, 0, 0), time.UTC)
	cfg := cfgOf(job{"a", "@every 6h", false})
	r.s.Apply(cfg)
	first := r.s.entries["a"]
	next := first.next
	writes := r.store.writeCount()

	r.clock.advance(time.Hour)
	r.s.Apply(cfg)
	second := r.s.entries["a"]
	if second != first {
		t.Error("unchanged entry was rebuilt")
	}
	if !second.next.Equal(next) {
		t.Errorf("unchanged entry lost its phase: next %v -> %v", next, second.next)
	}
	if r.store.writeCount() != writes {
		t.Error("a no-change reload wrote marks")
	}
}

// TestReapplyCatchUpChangeKeepsPhase: flipping catch_up is a policy change,
// not a new schedule, so the entry and its next window survive.
func TestReapplyCatchUpChangeKeepsPhase(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 12, 0, 0), time.UTC)
	r.s.Apply(cfgOf(job{"a", "@every 6h", false}))
	first := r.s.entries["a"]
	next := first.next

	r.s.Apply(cfgOf(job{"a", "@every 6h", true}))
	second := r.s.entries["a"]
	if second != first || !second.next.Equal(next) {
		t.Error("a catch_up change rebuilt the entry")
	}
	if !second.catchUp {
		t.Error("catch_up change was not applied")
	}
}

// TestReapplyReplacesChangedEntry verifies a changed spec re-registers the
// entry under the new schedule.
func TestReapplyReplacesChangedEntry(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 1, 0, 0), time.UTC)
	r.s.Apply(cfgOf(job{"a", "0 */6 * * *", false}))
	first := r.s.entries["a"]

	r.s.Apply(cfgOf(job{"a", "0 */3 * * *", false}))
	second, ok := r.s.entries["a"]
	if !ok {
		t.Fatal("entry lost after spec change")
	}
	if second == first {
		t.Error("changed entry kept its old registration")
	}
	if second.spec != "0 */3 * * *" || !second.next.Equal(utc(2026, 9, 11, 3, 0, 0)) {
		t.Errorf("entry = %q next %v, want the new spec's next window", second.spec, second.next)
	}
}

// TestNextFire verifies NextFire reports the resolved next window, and false
// for unscheduled, unknown, and never-firing harnesses.
func TestNextFire(t *testing.T) {
	r := newRig(t, utc(2026, 9, 11, 1, 30, 0), time.UTC)
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"a":     {Name: "a", Prompt: "test", Schedule: "0 */6 * * *"},
			"b":     {Name: "b", Prompt: "test"},
			"never": {Name: "never", Prompt: "test", Schedule: "0 0 30 2 *"},
		},
		HarnessOrder: []string{"a", "b", "never"},
	}
	r.s.Apply(cfg)

	if next, ok := r.s.NextFire("a"); !ok || !next.Equal(utc(2026, 9, 11, 6, 0, 0)) {
		t.Errorf("NextFire(a) = %v (ok=%v), want 06:00", next, ok)
	}
	if _, ok := r.s.NextFire("b"); ok {
		t.Error("unscheduled harness must report no next fire")
	}
	if _, ok := r.s.NextFire("missing"); ok {
		t.Error("unknown harness must report no next fire")
	}
	if _, ok := r.s.NextFire("never"); ok {
		t.Error("a schedule that never fires must report no next fire")
	}

	before, _ := r.s.NextFire("a")
	r.s.Apply(cfg)
	after, _ := r.s.NextFire("a")
	if !before.Equal(after) {
		t.Errorf("unchanged schedule moved next fire: %v -> %v", before, after)
	}
}

// TestStartTicksOnTheClock: Start evaluates on the Clock's ticker, holds no
// timer longer than TickInterval, and stops it on Close. A second Start, or a
// Start after Close, is a no-op.
func TestStartTicksOnTheClock(t *testing.T) {
	t0 := utc(2026, 9, 11, 12, 0, 0)
	r := newRig(t, t0, time.UTC)
	r.s.Apply(cfgOf(job{"worker", "@every 1s", false}))
	r.s.Start()
	r.s.Start()

	r.clock.advance(time.Second)
	r.clock.ticks <- t0 // unbuffered: returns once the loop has taken the tick
	eventually(t, func() bool { return len(r.fired()) == 1 })

	periods, _ := r.clock.tickerPeriods()
	if len(periods) != 1 {
		t.Fatalf("tickers created = %v, want exactly one", periods)
	}
	for _, d := range periods {
		if d > TickInterval {
			t.Errorf("armed a %v timer, longer than the %v tick", d, TickInterval)
		}
	}

	r.s.Close()
	if _, stopped := r.clock.tickerPeriods(); !stopped {
		t.Error("Close did not stop the ticker")
	}
	r.s.Start()
	if periods, _ := r.clock.tickerPeriods(); len(periods) != 1 {
		t.Error("Start after Close created a ticker")
	}
}

// TestNoWallClockReadsOutsideClock pins the injectable-clock contract: outside
// clock.go the package never reads the time or arms a timer itself, so every
// temporal behavior above is the behavior production gets.
func TestNoWallClockReadsOutsideClock(t *testing.T) {
	forbidden := []string{
		"time.Now(", "time.Since(", "time.Until(", "time.Sleep(",
		"time.NewTimer(", "time.NewTicker(", "time.AfterFunc(", "time.After(", "time.Tick(",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "clock.go" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") {
				continue
			}
			for _, bad := range forbidden {
				if strings.Contains(code, bad) {
					t.Errorf("%s:%d calls %s outside clock.go", file, i+1, bad)
				}
			}
		}
	}
}
