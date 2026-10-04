package supervisor

// Admission Tests
//
// SPEC-0021's admission funnel through a real Manager, its real run ledger on
// disk and real `sh` processes, so every count is one the funnel made and
// every record one the ledger holds. The budget day runs on an injected clock
// (ManagerOptions.Now) where a test crosses one; a test on the wall clock
// moves day_starts twelve hours away (farDayStarts), so no run can straddle a
// rollover.
//
// Governing: ADR-0027; SPEC-0021 REQ-3 "The budget day", REQ-4 "Admission",
// REQ-5 "Run-count budget", REQ-7 "Durable counters", REQ-20, REQ-21.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/budget"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/ledger"
)

// testClock is a settable clock for ManagerOptions.Now.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock(t time.Time) *testClock { return &testClock{t: t} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func mustDaily(t *testing.T, s string) hours.DailyInstant {
	t.Helper()
	d, err := hours.ParseDailyInstant(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// farDayStarts is a day_starts twelve hours from now, for a test on the wall
// clock: its whole run is far from either end of the budget day.
func farDayStarts(t *testing.T) hours.DailyInstant {
	at := time.Now().UTC().Add(12 * time.Hour)
	return mustDaily(t, fmt.Sprintf("TZ=UTC %02d:%02d", at.Hour(), at.Minute()))
}

// budgetSweep is a scheduled one-shot whose every spawn appends a line to
// marker, with max_runs_per_day = max.
func budgetSweep(name, marker string, max int) core.Harness {
	h := sweep(name, "echo ran >> '"+marker+"'")
	h.Budget.MaxRunsPerDay = max
	return h
}

// budgetManager boots a restored Manager on e, deciding admission on now (the
// wall clock when nil), and returns it with an idempotent close.
func budgetManager(t *testing.T, e runsEnv, cfg *core.Config, now func() time.Time) (*Manager, func()) {
	t.Helper()
	m := NewManager(cfg, ManagerOptions{Policy: fastPolicy(), StatePath: e.state, LogDir: e.logs, Now: now})
	closeOnce := sync.OnceFunc(m.Close)
	t.Cleanup(closeOnce)
	if err := m.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return m, closeOnce
}

// fire is one schedule firing, waited on until its run has finished.
func fire(t *testing.T, m *Manager, name string, wantRecords int) RunDecision {
	t.Helper()
	d, ok := m.StartRun(name, RunRequest{Trigger: TriggerSchedule})
	if !ok {
		t.Fatalf("StartRun(%q): unknown harness", name)
	}
	waitRuns(t, m, name, fmt.Sprintf("%d records, none running", wantRecords), func(rs []RunRecord) bool {
		if len(rs) != wantRecords {
			return false
		}
		for _, r := range rs {
			if r.Outcome == OutcomeRunning {
				return false
			}
		}
		return true
	})
	return d
}

// admittedToday counts the ledger's admitted records of name since start.
func admittedToday(t *testing.T, m *Manager, name string, start time.Time) int {
	t.Helper()
	recs, _, err := m.Ledger().Query(ledger.Query{Names: []string{name}, Since: start})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range recs {
		if admittedRecord(f) {
			n++
		}
	}
	return n
}

// TestFortiethRunRaces is SPEC-0021 REQ-4 Scenario "The fortieth run races":
// at 39 of 40, two firings released together on real goroutines are admitted
// exactly once, and the other is refused for budget. Then, under more
// pressure, sixteen racing for ten units never over-admit. Run under -race
// (make race) it is also the check that the counters are guarded.
func TestFortiethRunRaces(t *testing.T) {
	e := newRunsEnv(t)
	cfg := sweepCfg(budgetSweep("pr-review", filepath.Join(e.dir, "marker"), 40))
	cfg.Budget.DayStarts = farDayStarts(t)
	m, _ := budgetManager(t, e, cfg, nil)
	admit := func() AdmitResult {
		return m.Admit("pr-review", budget.AdmitRequest{Trigger: string(TriggerWebhook)}, RunRecord{Trigger: TriggerWebhook, Outcome: OutcomeRunning})
	}
	race := func(n int) []AdmitResult {
		out := make([]AdmitResult, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range out {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				out[i] = admit()
			}()
		}
		close(start)
		wg.Wait()
		return out
	}
	count := func(rs []AdmitResult) (admitted int, refused []budget.Decision) {
		for _, r := range rs {
			if r.Decision.Admitted() {
				admitted++
			} else {
				refused = append(refused, r.Decision)
			}
		}
		return admitted, refused
	}

	for i := range 39 {
		if r := admit(); !r.Decision.Admitted() {
			t.Fatalf("admission %d of 39: %+v", i+1, r.Decision)
		}
	}
	admitted, refused := count(race(2))
	if admitted != 1 || len(refused) != 1 {
		t.Fatalf("two firings at 39/40: %d admitted, %d refused, want exactly one each", admitted, len(refused))
	}
	if d := refused[0]; d.Verdict != budget.Skip || d.Reason != budget.ReasonBudget || d.Detail != "40/40 runs today" {
		t.Errorf("the loser: %+v, want skipped for budget at 40/40", d)
	}
	start, _ := m.BudgetDay()
	if got, disk := m.RunsToday("pr-review"), admittedToday(t, m, "pr-review", start); got != 40 || disk != 40 {
		t.Fatalf("runs today: counter %d, ledger %d, want 40 and 40", got, disk)
	}

	// Raised to 50 by a reload: sixteen racing for the last ten.
	raised := sweepCfg(budgetSweep("pr-review", filepath.Join(e.dir, "marker"), 50))
	raised.Budget.DayStarts = cfg.Budget.DayStarts
	m.Reload(raised)
	if admitted, refused := count(race(16)); admitted != 10 || len(refused) != 6 {
		t.Fatalf("sixteen racing for ten: %d admitted, %d refused, want 10 and 6", admitted, len(refused))
	}
	if got, disk := m.RunsToday("pr-review"), admittedToday(t, m, "pr-review", start); got != 50 || disk != 50 {
		t.Fatalf("runs today: counter %d, ledger %d, want 50 and 50", got, disk)
	}
}

// TestFortiethRunRacesThroughTheLoop is the same scenario end to end: two
// firings for a harness at its last run, sent together through the
// Manager's StartRun on real goroutines. Exactly one process runs; the other
// firing is recorded skipped with reason budget (it waited in the overlap
// queue, and admission refused it when its turn came).
func TestFortiethRunRacesThroughTheLoop(t *testing.T) {
	e := newRunsEnv(t)
	marker := filepath.Join(e.dir, "marker")
	h := shHarness("pr-review", "echo ran >> '"+marker+"'; sleep 0.3", 0)
	h.Restart, h.Triggers, h.OnOverlap = core.RestartNo, []string{"webhook.gh"}, core.OverlapQueue
	h.Budget.MaxRunsPerDay = 2
	cfg := sweepCfg(h)
	cfg.Budget.DayStarts = farDayStarts(t)
	m, _ := budgetManager(t, e, cfg, nil)

	if d, _ := m.StartRun("pr-review", RunRequest{Trigger: TriggerWebhook}); d.Kind != DecisionStarted {
		t.Fatalf("first run: %+v", d)
	}
	waitRuns(t, m, "pr-review", "the first run", outcomesAre(OutcomeSuccess))

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			m.StartRun("pr-review", RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"})
		}()
	}
	close(start)
	wg.Wait()
	rs := waitRuns(t, m, "pr-review", "one more run and one budget skip", outcomesAre(OutcomeSuccess, OutcomeSuccess, OutcomeSkipped))
	if rs[2].Reason != ReasonBudget {
		t.Errorf("the refused firing's reason = %q, want budget", rs[2].Reason)
	}
	if n := spawns(t, marker); n != 2 {
		t.Errorf("%d processes ran, want 2", n)
	}
}

// TestOneShotRunsOutOfRuns is REQ-5 Scenario "A one-shot runs out of runs":
// with max_runs_per_day = 2 and two runs today, a third firing starts no
// process and gains a skipped record with reason budget, and after the next
// rollover a firing runs normally.
func TestOneShotRunsOutOfRuns(t *testing.T) {
	e := newRunsEnv(t)
	marker := filepath.Join(e.dir, "marker")
	clock := newTestClock(time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC))
	cfg := sweepCfg(budgetSweep("sweep", marker, 2))
	cfg.Budget.DayStarts = mustDaily(t, "TZ=UTC 00:00")
	m, _ := budgetManager(t, e, cfg, clock.Now)

	fire(t, m, "sweep", 1)
	fire(t, m, "sweep", 2)
	d := fire(t, m, "sweep", 3)
	if d.Kind != DecisionSkipped || d.Run.Reason != ReasonBudget || !errors.Is(d.Refused, budget.ErrOverBudget) {
		t.Fatalf("third firing: kind=%s reason=%s refused=%v, want skipped for budget (ErrOverBudget)", d.Kind, d.Run.Reason, d.Refused)
	}
	if n := spawns(t, marker); n != 2 {
		t.Fatalf("%d processes ran, want 2: the third firing must start none", n)
	}
	if log := readText(t, filepath.Join(e.logs, "sweep.log")); !strings.Contains(log, "2/2 runs today") {
		t.Errorf("the durable log does not say why:\n%s", tailOf(log, 600))
	}

	// The next day: the tick rolls it over, and a firing runs normally.
	clock.Set(time.Date(2026, 10, 5, 0, 0, 30, 0, time.UTC))
	m.BudgetDue(clock.Now())
	if d := fire(t, m, "sweep", 4); d.Kind != DecisionStarted {
		t.Fatalf("after the rollover: %+v, want started", d)
	}
	if n, today := spawns(t, marker), m.RunsToday("sweep"); n != 3 || today != 1 {
		t.Fatalf("after the rollover: %d processes, %d runs today, want 3 and 1", n, today)
	}
}

// TestCatchUpAfterBudgetSkip is REQ-5 Scenario "Catch-up after a budget
// skip": firings refused for budget during the day earn, with catch_up =
// true, exactly one catch_up run when the day rolls over, and it counts
// toward the new day. The refusals coalesce into one record meanwhile
// (SPEC-0014 REQ "Overlap Skip Coalescing").
func TestCatchUpAfterBudgetSkip(t *testing.T) {
	e := newRunsEnv(t)
	marker := filepath.Join(e.dir, "marker")
	clock := newTestClock(time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC))
	h := budgetSweep("sweep", marker, 1)
	h.CatchUp = true
	cfg := sweepCfg(h)
	cfg.Budget.DayStarts = mustDaily(t, "TZ=UTC 00:00")
	m, _ := budgetManager(t, e, cfg, clock.Now)

	fire(t, m, "sweep", 1)
	fire(t, m, "sweep", 2)
	clock.Set(time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC))
	fire(t, m, "sweep", 2) // coalesced into the same skip record
	// The count is a buffered checkpoint (CoalesceRun), so wait for it.
	waitRuns(t, m, "sweep", "one budget skip covering 2 firings", func(rs []RunRecord) bool {
		return len(rs) == 2 && rs[1].Outcome == OutcomeSkipped && rs[1].Reason == ReasonBudget && rs[1].Coalesced == 2
	})
	if _, settle := m.BudgetDue(clock.Now()); len(settle) != 0 {
		t.Fatalf("the same day settles nothing, got %q", settle)
	}

	clock.Set(time.Date(2026, 10, 5, 0, 1, 0, 0, time.UTC))
	_, settle := m.BudgetDue(clock.Now())
	if len(settle) != 1 || settle[0] != "sweep" {
		t.Fatalf("after the rollover settle = %q, want [sweep]", settle)
	}
	d, ok := m.SettleBudget("sweep")
	if !ok || d.Kind != DecisionStarted || d.Run.Trigger != TriggerCatchUp {
		t.Fatalf("settle-up: ok=%v %+v, want one catch_up run started", ok, d)
	}
	waitRuns(t, m, "sweep", "the catch-up run", outcomesAre(OutcomeSuccess, OutcomeSkipped, OutcomeSuccess))
	if _, again := m.SettleBudget("sweep"); again {
		t.Error("a second settle-up for the same day's skips ran")
	}
	if _, settle := m.BudgetDue(clock.Now()); len(settle) != 0 {
		t.Errorf("still owed after the settle-up: %q", settle)
	}
	if n, today := spawns(t, marker), m.RunsToday("sweep"); n != 2 || today != 1 {
		t.Fatalf("%d processes, %d runs today, want 2 and the catch-up counted toward the new day", n, today)
	}
}

// TestRestartDoesNotRefund is REQ-7 Scenario "A restart does not refund",
// round-tripping the real ledger files: with max_runs_per_day = 3 and three
// runs today, a daemon restart rebuilds the count from the ledger, and the
// next firing is skipped for budget.
func TestRestartDoesNotRefund(t *testing.T) {
	e := newRunsEnv(t)
	marker := filepath.Join(e.dir, "marker")
	cfg := sweepCfg(budgetSweep("sweep", marker, 3))
	cfg.Budget.DayStarts = farDayStarts(t)
	m, closeM := budgetManager(t, e, cfg, nil)
	for i := range 3 {
		fire(t, m, "sweep", i+1)
	}
	closeM()
	files, _ := filepath.Glob(filepath.Join(e.ledgerDir(), "*.jsonl"))
	if len(files) == 0 {
		t.Fatal("no ledger day file on disk after the first daemon")
	}

	m2, _ := budgetManager(t, e, cfg, nil)
	if got := m2.RunsToday("sweep"); got != 3 {
		t.Fatalf("after the restart: %d runs today, want 3 rebuilt from the ledger", got)
	}
	d := fire(t, m2, "sweep", 4)
	if d.Kind != DecisionSkipped || d.Run.Reason != ReasonBudget {
		t.Fatalf("firing after the restart: %+v, want skipped for budget", d)
	}
	if n := spawns(t, marker); n != 3 {
		t.Fatalf("%d processes ran, want 3", n)
	}
}

// captureLog sends the default logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *safeBuffer {
	t.Helper()
	buf := &safeBuffer{}
	prev := log.Default()
	log.SetDefault(log.New(buf))
	t.Cleanup(func() { log.SetDefault(prev) })
	return buf
}

// TestALostLedgerFile is REQ-7 Scenario "A lost ledger file": today's ledger
// file deleted while the daemon was stopped, the next boot rebuilds today's
// counters from what remains, logs that the ledger has a gap, and reports it
// for doctor.
func TestALostLedgerFile(t *testing.T) {
	e := newRunsEnv(t)
	marker := filepath.Join(e.dir, "marker")
	cfg := sweepCfg(budgetSweep("sweep", marker, 3))
	cfg.Budget.DayStarts = farDayStarts(t)
	m, closeM := budgetManager(t, e, cfg, nil)
	fire(t, m, "sweep", 1)
	fire(t, m, "sweep", 2)
	closeM()
	files, _ := filepath.Glob(filepath.Join(e.ledgerDir(), "*.jsonl"))
	if len(files) == 0 {
		t.Fatal("no ledger day file to lose")
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}

	logged := captureLog(t)
	m2, _ := budgetManager(t, e, cfg, nil)
	if got := m2.RunsToday("sweep"); got != 0 {
		t.Errorf("rebuilt from what remains (nothing): %d runs today, want 0", got)
	}
	gaps := m2.LedgerGaps()
	if len(gaps) != 1 || gaps[0] != (LedgerGap{Harness: "sweep", Last: 2, Max: 0}) {
		t.Fatalf("gaps = %+v, want sweep's runs 1-2 missing", gaps)
	}
	if out := logged.String(); !strings.Contains(out, "run ledger has a gap") || !strings.Contains(out, "missing_run_ids=1-2") {
		t.Errorf("boot did not log the gap:\n%s", out)
	}
	// The day goes on from the rebuilt count.
	if d := fire(t, m2, "sweep", 1); d.Kind != DecisionStarted || d.Run.RunID != 3 {
		t.Fatalf("firing after the boot: %+v, want started as run 3 (ids are never reissued)", d)
	}
}

// TestLedgerCannotBeWritten is REQ-4 Scenario "The ledger cannot be written":
// with the ledger directory unwritable, a budgeted firing is refused with
// reason ledger_unavailable, said on its durable log, and an unbudgeted one
// starts anyway; both are counted for doctor. Admission does not hang on the
// outage: the refusal is reported before the next firing.
func TestLedgerCannotBeWritten(t *testing.T) {
	e := newRunsEnv(t)
	budgetMarker, freeMarker := filepath.Join(e.dir, "budgeted"), filepath.Join(e.dir, "free")
	cfg := sweepCfg(budgetSweep("budgeted", budgetMarker, 10), sweep("free", "echo ran >> '"+freeMarker+"'"))
	cfg.Budget.DayStarts = farDayStarts(t)
	m, closeM := budgetManager(t, e, cfg, nil)

	// A ledger directory that cannot hold a file, root or not: a file where
	// the directory was.
	dir := e.ledgerDir()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	d, _ := m.StartRun("budgeted", RunRequest{Trigger: TriggerSchedule})
	if d.Kind != DecisionSkipped || d.Run.Reason != ReasonLedgerUnavailable || !errors.Is(d.Refused, budget.ErrLedgerUnavailable) {
		t.Fatalf("budgeted firing: kind=%s reason=%s refused=%v, want skipped with ledger_unavailable", d.Kind, d.Run.Reason, d.Refused)
	}
	// A second refusal while the ledger is still failing is decided at once,
	// without queuing another record behind the failure.
	d2, _ := m.StartRun("budgeted", RunRequest{Trigger: TriggerSchedule})
	if !errors.Is(d2.Refused, budget.ErrLedgerUnavailable) {
		t.Fatalf("second budgeted firing: %+v, want refused too", d2)
	}
	if d, _ := m.StartRun("free", RunRequest{Trigger: TriggerSchedule}); d.Kind != DecisionStarted {
		t.Fatalf("unbudgeted firing: %+v, want started anyway", d)
	}
	waitFor(t, 5*time.Second, "the unbudgeted run's process ran", func() bool { return spawns(t, freeMarker) == 1 })
	if n := spawns(t, budgetMarker); n != 0 {
		t.Fatalf("the budgeted harness ran %d processes, want none", n)
	}
	if log := readText(t, filepath.Join(e.logs, "budgeted.log")); !strings.Contains(log, "ledger_unavailable") {
		t.Errorf("the budgeted harness's durable log does not record the refusal:\n%s", tailOf(log, 600))
	}
	st := m.AdmissionLedgerStats()
	if st.Refused < 2 || st.Unrecorded < 1 || st.LastError == "" {
		t.Errorf("admission ledger stats = %+v, want 2+ refused, 1+ unrecorded and the error", st)
	}
	if got := m.RunsToday("budgeted"); got != 0 {
		t.Errorf("refused starts counted: %d runs today, want 0", got)
	}

	// The disk recovers before shutdown, so the queue drains.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	closeM()
}

// TestFallBackDayIsOneDay is REQ-3 Scenario "Fall-back day" at the counters:
// with day_starts = "00:00" in a zone that falls back at 02:00, the day lasts
// 25 hours, and runs admitted across it, the repeated hour included, all
// count toward the one day.
func TestFallBackDayIsOneDay(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	e := newRunsEnv(t)
	clock := newTestClock(time.Date(2026, 11, 1, 0, 30, 0, 0, la))
	cfg := sweepCfg(budgetSweep("sweep", filepath.Join(e.dir, "marker"), 100))
	cfg.Budget.DayStarts = mustDaily(t, "TZ=America/Los_Angeles 00:00")
	m, _ := budgetManager(t, e, cfg, clock.Now)

	firstOneThirty := time.Date(2026, 11, 1, 1, 30, 0, 0, la) // PDT
	for i, at := range []time.Time{
		clock.Now(),
		firstOneThirty,
		firstOneThirty.Add(time.Hour), // 01:30 again, PST
		time.Date(2026, 11, 1, 23, 30, 0, 0, la),
	} {
		r := m.Admit("sweep", budget.AdmitRequest{Trigger: string(TriggerManual), Now: at}, RunRecord{Trigger: TriggerManual, Outcome: OutcomeRunning})
		if !r.Decision.Admitted() {
			t.Fatalf("admission at %v: %+v", at, r.Decision)
		}
		if got := m.RunsToday("sweep"); got != i+1 {
			t.Fatalf("after %v: %d runs today, want %d (one day)", at, got, i+1)
		}
	}
	start, next := m.BudgetDay()
	if next.Sub(start) != 25*time.Hour {
		t.Fatalf("budget day [%v, %v) is %v, want 25h", start, next, next.Sub(start))
	}
	m.Admit("sweep", budget.AdmitRequest{Trigger: string(TriggerManual), Now: next}, RunRecord{Trigger: TriggerManual, Outcome: OutcomeRunning})
	if got := m.RunsToday("sweep"); got != 1 {
		t.Fatalf("at the next day's start: %d runs today, want 1", got)
	}
}

// TestBudgetDueHoldsALoweredCap is the tick half of REQ-20 for a resident:
// lowering max_runs_per_day below today's count holds a running resident at
// the next tick, while one at exactly its cap keeps running; raising the cap
// back clears the hold at the next tick.
func TestBudgetDueHoldsALoweredCap(t *testing.T) {
	e := newRunsEnv(t)
	h := shHarness("crush-sb", "while true; do sleep 0.02; done", 0)
	h.Budget.MaxRunsPerDay = 2
	cfg := managerCfg(h)
	cfg.Budget.DayStarts = farDayStarts(t)
	m, _ := budgetManager(t, e, cfg, nil)
	for range 2 {
		m.Restart("crush-sb")
		waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("crush-sb"); return s.State == core.StateRunning })
	}
	if m.RunsToday("crush-sb") != 2 {
		t.Fatalf("runs today = %d, want 2", m.RunsToday("crush-sb"))
	}
	if holds, _ := m.BudgetDue(time.Now()); len(holds) != 0 {
		t.Fatalf("at exactly its cap: holds = %v, want none (the running process was admitted)", holds)
	}
	lowered := managerCfg(h)
	lowered.Budget.DayStarts = cfg.Budget.DayStarts
	h.Budget.MaxRunsPerDay = 1
	lowered.Harnesses[h.Name] = h
	m.Reload(lowered)
	holds, _ := m.BudgetDue(time.Now())
	if holds["crush-sb"] != core.HoldSetOf(core.HoldBudget) {
		t.Fatalf("after lowering the cap to 1: holds = %v, want crush-sb held for budget", holds)
	}
	m.AddHolds("crush-sb", holds["crush-sb"])
	snap := waitSnapshot(t, m, "crush-sb", "held for budget", func(s Snapshot) bool {
		return s.State == core.StateStopped && s.Holds == core.HoldSetOf(core.HoldBudget)
	})
	if !snap.Enabled {
		t.Error("the budget hold touched enabled")
	}
	if cleared := m.HoldsCleared(time.Now()); cleared["crush-sb"].Has(core.HoldBudget) {
		t.Fatalf("still spent, but the hold reports cleared: %v", cleared)
	}
	m.Reload(cfg) // back to 2: still spent (2/2)
	if cleared := m.HoldsCleared(time.Now()); cleared["crush-sb"].Has(core.HoldBudget) {
		t.Fatalf("2/2 is still spent, but the hold reports cleared: %v", cleared)
	}
	raised := managerCfg(h)
	raised.Budget.DayStarts = cfg.Budget.DayStarts
	h.Budget.MaxRunsPerDay = 3
	raised.Harnesses[h.Name] = h
	m.Reload(raised)
	if cleared := m.HoldsCleared(time.Now()); !cleared["crush-sb"].Has(core.HoldBudget) {
		t.Fatalf("raised to 3: cleared = %v, want the budget hold cleared", cleared)
	}
	m.Release("crush-sb", core.HoldBudget)
	waitSnapshot(t, m, "crush-sb", "running again through admission", func(s Snapshot) bool {
		return s.State == core.StateRunning && s.Holds.Empty()
	})
	if got := m.RunsToday("crush-sb"); got != 3 {
		t.Errorf("runs today = %d, want 3: the release went through admission", got)
	}
}

// waitSnapshot polls name's snapshot until pred holds.
func waitSnapshot(t *testing.T, m *Manager, name, desc string, pred func(Snapshot) bool) Snapshot {
	t.Helper()
	var snap Snapshot
	waitFor(t, 5*time.Second, desc, func() bool {
		snap, _ = m.Snapshot(name)
		return pred(snap)
	})
	return snap
}

// TestStartReportsTheRefusal is REQ-21 at the Manager: an operator start
// admission refuses comes back as an error wrapping ErrOverBudget (what the
// control op maps to over_budget), and leaves a resident held for budget with
// its intent recorded, not failed.
func TestStartReportsTheRefusal(t *testing.T) {
	e := newRunsEnv(t)
	h := shHarness("crush-sb", "exit 3", 0)
	h.Restart = core.RestartNo
	h.Budget.MaxRunsPerDay = 1
	cfg := managerCfg(h)
	cfg.Budget.DayStarts = farDayStarts(t)
	m, _ := budgetManager(t, e, cfg, nil)
	if ok, err := m.StartChecked("crush-sb", ""); !ok || err != nil {
		t.Fatalf("first start: ok=%v err=%v", ok, err)
	}
	waitSnapshot(t, m, "crush-sb", "exited", func(s Snapshot) bool { return s.State == core.StateFailed || s.State == core.StateStopped })
	ok, err := m.StartChecked("crush-sb", "")
	if !ok || !errors.Is(err, budget.ErrOverBudget) || !strings.Contains(err.Error(), "1/1 runs today") {
		t.Fatalf("second start: ok=%v err=%v, want ErrOverBudget naming 1/1 runs today", ok, err)
	}
	snap := waitSnapshot(t, m, "crush-sb", "held for budget", func(s Snapshot) bool { return s.Holds.Has(core.HoldBudget) })
	if snap.State != core.StateStopped || !snap.Enabled {
		t.Errorf("refused start: state=%s enabled=%v, want stopped and enabled", snap.State, snap.Enabled)
	}
	if ok, err := m.StartChecked("nope", ""); ok || err != nil {
		t.Errorf("unknown harness: ok=%v err=%v", ok, err)
	}
}
