package scheduler

// Scheduler Hardening Tests
//
// The failure modes that turn a scheduler into duplicate PR merges or silent
// sweeps: a brand-new job treated as having missed every window since the
// epoch, the deployed schedules (every one `CRON_TZ=UTC`, on America/Detroit
// daemons) drifting across a US DST transition, a config reload racing the tick
// into deciding a window twice, a stored time keeping the monotonic reading
// that stops during suspend, and a Close that leaks the tick goroutine.
//
// Two of these pin behavior the original suite did not: mutating Apply to trust
// a zero-time mark, or dropping the Round(0) monotonic strip, fails only here.
//
// Governing: ADR-0013; SPEC-0008 REQ "Suspend-Safe Schedule Evaluation", REQ
// "Missed Window Handling", REQ "Schedule Time Zone", REQ "Schedule
// Reconciliation On Reload"; issue #117.
//
// @joestump-agent 09/11/2026 - Added in adversarial review of PR #306.

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// TestBrandNewJobIsNotBehind: a job armed for the first time — no mark, or a
// mark whose decided-through is the zero time — decides nothing on its first
// evaluation and arms its next real window. It must never be treated as having
// missed every window since the epoch.
func TestBrandNewJobIsNotBehind(t *testing.T) {
	det := mustZone(t, "America/Detroit")
	const spec = "CRON_TZ=UTC 0 7 * * *"
	for _, catchUp := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			marks map[string]Mark
		}{
			{"no mark", nil},
			{"zero mark under the same spec", map[string]Mark{"sweep": {Spec: spec}}},
			{"zero mark with a last run", map[string]Mark{"sweep": {Spec: spec, LastRunAt: utc(2026, 9, 1, 7, 0, 0)}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				store := newMemStore()
				for k, v := range tc.marks {
					store.marks[k] = v
				}
				now := utc(2026, 9, 11, 16, 0, 0)
				r := newRigWithStore(t, now, det, store)
				r.s.Apply(cfgOf(job{"sweep", spec, catchUp}, job{"poll", "@every 1s", catchUp}))
				r.tick()

				if fires, misses := r.fired(), r.missed(); len(fires)+len(misses) != 0 {
					t.Fatalf("catch_up=%v: first evaluation decided windows: fires %+v misses %+v", catchUp, fires, misses)
				}
				if next, ok := r.s.NextFire("sweep"); !ok || !next.Equal(utc(2026, 9, 12, 7, 0, 0)) {
					t.Errorf("NextFire = %v (ok=%v), want tomorrow 07:00Z", next, ok)
				}
				if m, _ := store.mark("sweep"); !m.DecidedThrough.Equal(now) {
					t.Errorf("mark = %+v, want armed from now", m)
				}
			})
		}
	}
}

// TestAncientMarkDecidesOnceAndRearms: a mark from long ago (a clock that
// booted at the epoch, a state.json restored from an old backup) is bounded:
// one decision, then the schedule is back on its real windows.
func TestAncientMarkDecidesOnceAndRearms(t *testing.T) {
	const spec = "CRON_TZ=UTC 0 7 * * *"
	for _, catchUp := range []bool{false, true} {
		store := newMemStore()
		store.marks["sweep"] = Mark{Spec: spec, DecidedThrough: time.Unix(0, 0).UTC()}
		now := utc(2026, 9, 11, 16, 0, 0)
		r := newRigWithStore(t, now, time.UTC, store)
		r.s.Apply(cfgOf(job{"sweep", spec, catchUp}))
		r.tick()
		r.tick()

		if got := len(r.fired()) + len(r.missed()); got != 1 {
			t.Fatalf("catch_up=%v: decisions = %d (fires %+v misses %+v), want exactly one", catchUp, got, r.fired(), r.missed())
		}
		if next, _ := r.s.NextFire("sweep"); !next.After(now) || next.After(now.Add(24*time.Hour)) {
			t.Errorf("catch_up=%v: NextFire = %v, want the next real window", catchUp, next)
		}
	}
}

// TestDetroitDaemonUTCSchedulesAcrossDST runs the schedules deployed on tars
// and kitt — `CRON_TZ=UTC` specs on an America/Detroit daemon — plus unprefixed
// and `@every` controls, through a week spanning each US DST transition. Every
// UTC spec fires exactly on its UTC windows (the oracle is robfig evaluating in
// UTC, which has no DST), the local specs fire at their Detroit wall-clock
// times, and nothing is late or missed.
func TestDetroitDaemonUTCSchedulesAcrossDST(t *testing.T) {
	det := mustZone(t, "America/Detroit")
	jobs := []job{
		{"stumpcloud-sweep", "CRON_TZ=UTC 30 9 * * *", false},
		{"pr-sweep", "CRON_TZ=UTC 0 9 * * *", false},
		{"weekly-mon", "CRON_TZ=UTC 0 7 * * 1", false},
		{"weekly-fri", "CRON_TZ=UTC 0 16 * * 5", false},
		{"weekly-sun", "CRON_TZ=UTC 0 6 * * 0", false},
		{"utc-6h", "CRON_TZ=UTC 0 */6 * * *", false},
		{"utc-daily", "CRON_TZ=UTC @daily", false},
		{"tz-prefix", "TZ=UTC 20 7 * * *", false},
		{"local-daily", "@daily", false},
		{"local-0900", "0 9 * * *", false},
		{"every-6h", "@every 6h", false},
	}
	utcOracle := func(spec string, from, to time.Time) []time.Time {
		sched, err := cron.ParseStandard(spec)
		if err != nil {
			t.Fatalf("parse %q: %v", spec, err)
		}
		var out []time.Time
		for w := sched.Next(from.UTC()); w.Before(to); w = sched.Next(w) {
			out = append(out, w)
		}
		return out
	}
	localDaily := func(hour int, from, to time.Time) []time.Time {
		var out []time.Time
		d := from.In(det)
		for day := time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, det); day.Before(to); day = time.Date(day.Year(), day.Month(), day.Day()+1, hour, 0, 0, 0, det) {
			if day.After(from) {
				out = append(out, day)
			}
		}
		return out
	}

	for _, tr := range []struct {
		name       string
		start, end time.Time
	}{
		{"spring forward 2026-03-08", utc(2026, 3, 5, 0, 0, 10), utc(2026, 3, 12, 0, 0, 10)},
		{"fall back 2026-11-01", utc(2026, 10, 29, 0, 0, 10), utc(2026, 11, 5, 0, 0, 10)},
	} {
		t.Run(tr.name, func(t *testing.T) {
			r := newRig(t, tr.start, det)
			r.s.Apply(cfgOf(jobs...))
			r.runUntil(tr.end, 30*time.Second)

			want := map[string][]time.Time{
				"local-daily": localDaily(0, tr.start, tr.end),
				"local-0900":  localDaily(9, tr.start, tr.end),
			}
			for w := tr.start.Add(6 * time.Hour); w.Before(tr.end); w = w.Add(6 * time.Hour) {
				want["every-6h"] = append(want["every-6h"], w)
			}
			for _, j := range jobs {
				if strings.Contains(j.spec, "TZ=UTC") {
					want[j.name] = utcOracle(strings.SplitN(j.spec, " ", 2)[1], tr.start, tr.end)
				}
			}

			got := map[string][]time.Time{}
			for _, f := range r.fired() {
				if !f.Window.Before(tr.end) {
					continue // runUntil's last tick lands exactly on end
				}
				if f.Trigger != TriggerSchedule || f.Late > 30*time.Second {
					t.Errorf("%s: firing %+v is not on time", f.Name, f)
				}
				got[f.Name] = append(got[f.Name], f.Window)
			}
			for _, j := range jobs {
				g, w := got[j.name], want[j.name]
				if len(w) == 0 {
					t.Fatalf("%s: oracle produced no windows", j.name)
				}
				if len(g) != len(w) {
					t.Errorf("%s (%s): fired %d windows, want %d\n got  %v\n want %v", j.name, j.spec, len(g), len(w), g, w)
					continue
				}
				for i := range w {
					if !g[i].Equal(w[i]) {
						t.Errorf("%s (%s): window %d = %v, want %v", j.name, j.spec, i, g[i], w[i])
					}
				}
			}
			if m := r.missed(); len(m) != 0 {
				t.Errorf("recorded misses: %+v", m)
			}
		})
	}
}

// TestReloadRacingTickNeverDecidesAWindowTwice: config reloads that flip only
// catch_up (the czu/chezmoi rewrite loop) race the tick for thousands of
// windows. Every window fires exactly once, and no reload resets a phase.
func TestReloadRacingTickNeverDecidesAWindowTwice(t *testing.T) {
	t0 := utc(2026, 9, 11, 0, 0, 0)
	r := newRig(t, t0, time.UTC)
	a := cfgOf(job{"poll", "@every 1s", false}, job{"hourly", "0 * * * *", false})
	b := cfgOf(job{"poll", "@every 1s", true}, job{"hourly", "0 * * * *", true})
	r.s.Apply(a)

	const ticks = 5000
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				r.s.Apply(a)
			} else {
				r.s.Apply(b)
			}
			_, _ = r.s.NextFire("poll")
		}
	}()
	for range ticks {
		r.clock.advance(time.Second)
		r.s.evaluate()
	}
	close(stop)
	wg.Wait()
	r.s.firings.Wait()

	seen := map[string]map[time.Time]int{}
	for _, f := range r.fired() {
		if seen[f.Name] == nil {
			seen[f.Name] = map[time.Time]int{}
		}
		seen[f.Name][f.Window]++
	}
	for name, windows := range seen {
		for w, n := range windows {
			if n != 1 {
				t.Errorf("%s: window %v fired %d times", name, w, n)
			}
		}
	}
	if n := len(seen["poll"]); n != ticks {
		t.Errorf("poll fired %d distinct windows over %d one-second ticks, want %d", n, ticks, ticks)
	}
	if n := len(seen["hourly"]); n != 1 {
		t.Errorf("hourly fired %d windows over %s, want 1", n, time.Duration(ticks)*time.Second)
	}
	if m := r.missed(); len(m) != 0 {
		t.Errorf("recorded misses: %+v", m)
	}
}

// TestStoredTimesCarryNoMonotonicReading: every time the scheduler keeps or
// hands out is wall-clock only. A monotonic reading does not advance during
// suspend, so a window that kept one would come due late after a wake.
func TestStoredTimesCarryNoMonotonicReading(t *testing.T) {
	start := time.Now() // carries a monotonic reading, as production's clock does
	if !strings.Contains(start.String(), "m=") {
		t.Skip("platform time.Now carries no monotonic reading")
	}
	r := newRig(t, start, time.UTC)
	r.s.Apply(cfgOf(job{"poll", "@every 1s", false}, job{"daily", "0 3 * * *", false}))
	check := func(what string, ts ...time.Time) {
		t.Helper()
		for _, ts := range ts {
			if strings.Contains(ts.String(), "m=") {
				t.Errorf("%s carries a monotonic reading: %s", what, ts)
			}
		}
	}
	for name, e := range r.s.entries {
		check(name+" at arm", e.next, e.decided)
	}
	r.clock.advance(1500 * time.Millisecond)
	r.tick()
	for name, e := range r.s.entries {
		check(name+" after tick", e.next, e.decided, e.lastRunAt)
	}
	for _, f := range r.fired() {
		check("firing window", f.Window)
	}
	m, _ := r.store.mark("poll")
	check("persisted mark", m.DecidedThrough, m.LastRunAt)
}

// TestCloseLeavesNoGoroutines: Start/Close on the real clock releases the tick
// goroutine and its ticker every time.
func TestCloseLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	for range 50 {
		s := New(Options{})
		s.Apply(cfgOf(job{"poll", "@every 1h", false}))
		s.Start()
		s.Close()
	}
	eventually(t, func() bool { return runtime.NumGoroutine() <= before })
}

// BenchmarkEvaluateIdle measures one tick with many armed entries, none due:
// the steady-state cost the one-second tick pays forever.
func BenchmarkEvaluateIdle(b *testing.B) {
	now := utc(2026, 9, 11, 12, 0, 0)
	s := New(Options{Clock: newFakeClock(now), Location: time.UTC, Recorder: LogRecorder{}})
	var jobs []job
	for i := range 1000 {
		jobs = append(jobs, job{name: fmt.Sprintf("job-%d", i), spec: "CRON_TZ=UTC 30 9 * * *"})
	}
	s.Apply(cfgOf(jobs...))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s.evaluate()
	}
}
