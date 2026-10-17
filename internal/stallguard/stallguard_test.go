package stallguard

// Governing: ADR-0045; issue #959.

import (
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// clock is every fixture's shared "now".
var clock = time.Unix(1_700_000_000, 0)

// hourAgo is the start time of the wedged worker most fixtures describe: an
// hour without a call, against a 45m timeout.
func hourAgo() time.Time { return clock.Add(-60 * time.Minute) }

type report struct {
	harness   string
	startedAt time.Time
}

type fakeSup struct {
	mu       sync.Mutex
	snaps    []supervisor.Snapshot
	harness  map[string]core.Harness
	reports  []report
	logs     []string
	refusing bool
}

func (f *fakeSup) Snapshots() []supervisor.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]supervisor.Snapshot(nil), f.snaps...)
}

func (f *fakeSup) HarnessRecord(name string) (core.Harness, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.harness[name]
	return h, "test.toml", ok
}

func (f *fakeSup) ReportStall(name string, startedAt time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refusing {
		return false
	}
	f.reports = append(f.reports, report{name, startedAt})
	return true
}

func (f *fakeSup) LogLifecycle(name, msg string, kv ...any) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, name+": "+msg)
	return true
}

func (f *fakeSup) reported() []report {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]report(nil), f.reports...)
}

func (f *fakeSup) logged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.logs...)
}

func (f *fakeSup) restart(name string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.snaps {
		if f.snaps[i].Name == name {
			f.snaps[i].LastStarted = at
		}
	}
}

type fakeSub struct{ ch chan observe.Event }

func (s *fakeSub) Subscribe(string, int) (<-chan observe.Event, func()) { return s.ch, func() {} }

// fix is a test's clock plus the guard and its fake Manager and observer.
type fix struct {
	g   *Guard
	sup *fakeSup
	sub *fakeSub
	now time.Time
}

func newFix(t *testing.T, snaps []supervisor.Snapshot, harness map[string]core.Harness, opts Options) *fix {
	t.Helper()
	f := &fix{
		sup: &fakeSup{snaps: snaps, harness: harness},
		sub: &fakeSub{ch: make(chan observe.Event, 32)},
		now: clock,
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard)
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return f.now }
	}
	f.g = New(f.sup, opts)
	f.g.Start(f.sub)
	t.Cleanup(f.g.Close)
	return f
}

// running is the one snapshot shape every fixture reaches for: a live worker
// whose process is this test binary, so the process tree the stall dumps is
// real.
func running(name string, started time.Time) supervisor.Snapshot {
	return supervisor.Snapshot{Name: name, State: core.StateRunning, LastStarted: started, PID: os.Getpid()}
}

// call delivers one observed tool call and waits until the guard has taken it
// in, so a test never races its own progress signal.
func (f *fix) call(t *testing.T, harness string, at time.Time) {
	t.Helper()
	f.sub.ch <- observe.Event{Harness: harness, Kind: observe.KindTool, Time: at}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.g.mu.Lock()
		w := f.g.watches[harness]
		seen := w != nil && !w.lastCall.Before(at)
		f.g.mu.Unlock()
		if seen {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the guard never took in the observed call")
}

// TestStallGuardWedgesAndRecovers: a worker with no observed call past its
// timeout is acted on once per episode; progress closes the episode, and a
// later wedge inside the same process is acted on again.
func TestStallGuardWedgesAndRecovers(t *testing.T) {
	start := hourAgo()
	f := newFix(t,
		[]supervisor.Snapshot{running("worker", start)},
		map[string]core.Harness{"worker": {Name: "worker", Adapter: "crush", StallTimeout: 45 * time.Minute, StallAction: core.StallRestart}},
		Options{})

	f.g.sample()
	if got := f.sup.reported(); len(got) != 1 || got[0].harness != "worker" || !got[0].startedAt.Equal(start) {
		t.Fatalf("reports = %+v, want one for the sampled process start", got)
	}

	f.g.sample()
	if got := len(f.sup.reported()); got != 1 {
		t.Fatalf("reports after a second sample = %d, want the episode acted on once", got)
	}

	f.call(t, "worker", f.now.Add(-time.Minute))
	if f.g.Restarts("worker") != 0 {
		t.Fatalf("restarts after progress = %d, want the latch released", f.g.Restarts("worker"))
	}
	f.g.sample()
	if got := len(f.sup.reported()); got != 1 {
		t.Fatalf("a progressing harness produced %d reports", got)
	}

	f.now = f.now.Add(50 * time.Minute)
	f.g.sample()
	if got := len(f.sup.reported()); got != 2 {
		t.Fatalf("reports after wedging again = %d, want 2", got)
	}
	// The call between the two episodes released the latch, so this stall
	// counts as the first one in a row again.
	if f.g.Restarts("worker") != 1 {
		t.Fatalf("restarts = %d, want 1 after progress reset the count", f.g.Restarts("worker"))
	}
}

// TestStallGuardNeverTripsWhileProgressing: nine hours of work is not a stall
// when the calls keep landing — the whole reason this measures progress and not
// wall time.
func TestStallGuardNeverTripsWhileProgressing(t *testing.T) {
	f := newFix(t,
		[]supervisor.Snapshot{running("worker", clock)},
		map[string]core.Harness{"worker": {Name: "worker", Adapter: "crush", StallTimeout: 45 * time.Minute, StallAction: core.StallRestart}},
		Options{})
	for i := 0; i < 18; i++ {
		f.now = f.now.Add(30 * time.Minute)
		f.call(t, "worker", f.now)
		f.g.sample()
		if got := f.sup.reported(); len(got) != 0 {
			t.Fatalf("a progressing worker was reported at step %d: %+v", i, got)
		}
	}
}

// TestStallNotifyActionLeavesItRunning: the default finds, logs and reports the
// stall, and never touches the process.
func TestStallNotifyActionLeavesItRunning(t *testing.T) {
	var got []Stall
	f := newFix(t,
		[]supervisor.Snapshot{running("worker", hourAgo())},
		map[string]core.Harness{"worker": {Name: "worker", Adapter: "crush", StallTimeout: 45 * time.Minute, StallAction: core.StallNotify}},
		Options{OnStall: func(s Stall) { got = append(got, s) }})

	f.g.sample()
	if len(f.sup.reported()) != 0 {
		t.Fatalf("notify restarted the harness: %+v", f.sup.reported())
	}
	if len(got) != 1 || got[0].Action != core.StallNotify {
		t.Fatalf("stalls = %+v, want one notify episode", got)
	}
	if got[0].Idle < 45*time.Minute {
		t.Errorf("idle = %v, want past the timeout", got[0].Idle)
	}
	if len(got[0].Tree) == 0 {
		t.Errorf("no process tree in the stall report")
	}
	if len(f.sup.logged()) != 1 {
		t.Errorf("harness log lines = %v, want the stall line", f.sup.logged())
	}
}

// TestStallNoneStaysSilent: `none` is what an operator sets when they want the
// finding in the log and nothing else.
func TestStallNoneStaysSilent(t *testing.T) {
	fired := 0
	f := newFix(t,
		[]supervisor.Snapshot{running("worker", hourAgo())},
		map[string]core.Harness{"worker": {Name: "worker", Adapter: "crush", StallTimeout: 30 * time.Minute, StallAction: core.StallNone}},
		Options{OnStall: func(Stall) { fired++ }})
	f.g.sample()
	if fired != 0 || len(f.sup.reported()) != 0 {
		t.Fatalf("stall_action=none notified %d times and reported %d times", fired, len(f.sup.reported()))
	}
	if len(f.sup.logged()) != 1 {
		t.Fatalf("harness log lines = %v, want the stall line", f.sup.logged())
	}
}

// TestStallRestartLimitStopsRevivingAWorkerThatNeverStarts: a harness the
// observer never reports on is restarted a bounded number of times and then
// only reported, because another restart cannot fix a worker that wedges before
// its first call.
func TestStallRestartLimitStopsRevivingAWorkerThatNeverStarts(t *testing.T) {
	var got []Stall
	f := newFix(t,
		[]supervisor.Snapshot{running("worker", clock)},
		map[string]core.Harness{"worker": {Name: "worker", Adapter: "crush", StallTimeout: time.Minute, StallAction: core.StallRestart}},
		Options{OnStall: func(s Stall) { got = append(got, s) }})

	for i := 0; i < DefaultRestartLimit+2; i++ {
		f.now = f.now.Add(2 * time.Minute)
		f.sup.restart("worker", f.now.Add(-2*time.Minute))
		f.g.sample()
	}
	if got := f.sup.reported(); len(got) != DefaultRestartLimit {
		t.Fatalf("reports = %d, want the limit %d", len(got), DefaultRestartLimit)
	}
	if len(got) != DefaultRestartLimit+2 {
		t.Fatalf("episodes reported to the hook = %d, want every stall still announced", len(got))
	}
	if !got[len(got)-1].GaveUp {
		t.Errorf("last episode = %+v, want GaveUp set", got[len(got)-1])
	}
	if got[0].GaveUp {
		t.Errorf("first episode = %+v, want the restarts counted up to the limit", got[0])
	}
}

// TestStallGuardIgnoresWhatCannotBeWedged: only a RUNNING, non-triggered
// harness with a watchdog configured is measured.
func TestStallGuardIgnoresWhatCannotBeWedged(t *testing.T) {
	sweep := running("sweep", hourAgo())
	sweep.Triggered = true
	f := newFix(t, []supervisor.Snapshot{
		sweep,
		running("unwatched", hourAgo()),
		running("unwatched", hourAgo()),
		{Name: "down", State: core.StateStopped, LastStarted: hourAgo(), PID: os.Getpid()},
	}, map[string]core.Harness{
		"sweep":     {Name: "sweep", StallTimeout: time.Minute, StallAction: core.StallRestart},
		"unwatched": {Name: "unwatched"},
		"down":      {Name: "down", StallTimeout: time.Minute, StallAction: core.StallRestart},
	}, Options{})
	f.g.sample()
	if got := f.sup.reported(); len(got) != 0 {
		t.Fatalf("reports = %+v, want none", got)
	}
}

// TestStallReportRefusedIsNotRetried: a Manager that says the process it
// measured is already gone is recorded on the episode, which stays acted on
// instead of being re-reported every interval.
func TestStallReportRefusedIsNotRetried(t *testing.T) {
	f := newFix(t,
		[]supervisor.Snapshot{running("worker", hourAgo())},
		map[string]core.Harness{"worker": {Name: "worker", StallTimeout: time.Minute, StallAction: core.StallRestart}},
		Options{})
	f.sup.refusing = true
	f.g.sample()
	f.g.sample()
	if len(f.sup.reported()) != 0 {
		t.Fatalf("a refusing Manager should not have been asked again: %+v", f.sup.reported())
	}
	if logged := f.sup.logged(); len(logged) != 1 {
		t.Fatalf("harness log lines = %v, want one", logged)
	}
}
