package supervisor

// Admission: The Manager's Funnel
//
// Manager.Admit is SPEC-0021 REQ-4's one admission point. Every process start,
// on whichever path, asks it before it spawns (admit.go is the supervisor's
// half, and TestEveryStartPathIsAdmitted holds every path to it). Admit takes
// budgetMu, rolls the budget day over if the clock has passed it, measures the
// harness's state, asks internal/budget's pure Decide, and on an admission
// counts the run and queues its `opened` ledger line before the lock drops,
// so two starts racing for the fortieth run cannot both take it. The sync is
// waited for outside the lock, the split appendNew already makes, so one
// harness's fsync never delays another's admission; a budgeted start whose
// line then fails is uncounted again and refused with ledger_unavailable,
// while an unbudgeted one starts anyway, logged and counted.
//
// The counters are running totals with exactly one store, the ledger (REQ-7):
// the first use of the budget day (at boot, bootBudget, after the ledger is
// reconciled and before Autostart) folds the day's admitted records out of
// it, every admission increments the total, and a rollover zeroes it. Nothing
// persists a counter anywhere else.
//
// The day rolls over on the scheduler's gate tick (BudgetDue, through the
// gate pass), on the tick's own clock (REQ-3), and admission rolls it as well
// if a start arrives first, so neither ever counts a run into a day that has
// ended. A day_starts change by reload takes effect at the next rollover and
// never starts a day early (REQ-20). On the same tick the pass adds the
// budget reason where a hold is due (a resident whose cap a reload lowered
// below today's count, REQ-20; one already held for another reason whose run
// budget is spent too, REQ-4 Scenario "Hours decide before budgets"), and
// settles each one-shot's budget skips from an earlier day, catching up once
// (REQ-5 Scenario "Catch-up after a budget skip"). A budget hold clears
// through the pass's clearing hook (budgetHoldCleared) once the budget is no
// longer spent: a new day, or a cap a reload raised (REQ-14, REQ-20).
//
// Three of REQ-4's inputs are stubs, each a named seam its own story fills:
// quotaPark (the park story, stump.wtf/harness#477), costToday (the caps
// story, #482) and concurrency (the concurrency story, #479). Decide already
// evaluates all five checks; until those stories land, their inputs read "not
// parked", "nothing spent" and "no cap".
//
// Lock order: budgetMu, then journalMu, then mu. Nothing that holds mu or
// journalMu takes budgetMu, and nothing under budgetMu waits on a supervisor's
// actor loop: Admit itself runs on one.
//
// Governing: ADR-0027; SPEC-0021 REQ-3 "The budget day", REQ-4 "Admission",
// REQ-5 "Run-count budget", REQ-7 "Durable counters", REQ-14, REQ-20 "Reload",
// REQ-21 "Error handling and concurrency safety"; design.md § "Admission is a
// funnel on the Manager", § "Counters are running totals rebuilt from the
// ledger", § "The budget day reuses the gate's clock"; SPEC-0022 REQ-6.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/budget"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/ledger"
)

// budgetBook is the Manager's budget-day bookkeeping, guarded by budgetMu.
type budgetBook struct {
	// start and next bound the current budget day; zero until its first use.
	start, next time.Time
	// runs is each harness's admissions since start.
	runs map[string]int
	// skips maps a one-shot to when a firing of it was last refused for
	// budget. One refused on a day that has since ended is owed its
	// settle-up (and, with catch_up = true, its one catch-up run).
	skips map[string]time.Time
	// ledger is what admission met of the run ledger, for doctor.
	ledger AdmissionLedger
	// gaps are the run records boot found missing from the ledger.
	gaps []LedgerGap
}

// AdmissionLedger is what admission met of the run ledger (SPEC-0021 REQ-4):
// budgeted starts it refused because their record could not be written,
// unbudgeted starts it let go ahead with their record still queued, and the
// newest of those failures. doctor reports it.
type AdmissionLedger struct {
	Refused, Unrecorded int
	LastError           string
	LastAt              time.Time
}

// LedgerGap is a harness whose run ids outran the ledger: state.json's id
// allocator last handed out Last, but the ledger's newest record for it is
// Max. The records in between are gone (a day file deleted by hand, a disk
// that lost them), and today's counters were rebuilt without them (SPEC-0021
// REQ-7 Scenario "A lost ledger file").
type LedgerGap struct {
	Harness   string
	Last, Max int
}

// Admit is the admission funnel (SPEC-0021 REQ-4): it decides req for name
// and, when it admits, counts the run and opens rec in the run ledger under
// the same lock. rec.StartedAt becomes the decision's instant, so the record
// counts toward the day it was admitted in (REQ-7).
func (m *Manager) Admit(name string, req budget.AdmitRequest, rec RunRecord) AdmitResult {
	req.Harness = name
	if req.Now.IsZero() {
		req.Now = m.now()
	}
	h, db := m.budgetDef(name)
	m.budgetMu.Lock()
	m.rolloverLocked(req.Now, db.DayStarts)
	d := budget.Decide(req, m.admitStateLocked(name, h, db, req))
	if !d.Admitted() {
		if d.Reason == budget.ReasonBudget && !req.Resident {
			m.bud.skips[name] = req.Now
		}
		m.budgetMu.Unlock()
		// A refused firing is a coalesced skip record and, the first time, a
		// line on its durable log; a burst of them is not worth a daemon log
		// line each. A refused resident is rare and is held: say so.
		logf := log.Debug
		if req.Resident {
			logf = log.Info
		}
		logf("start refused", "harness", name, "trigger", req.Trigger, "reason", string(d.Reason), "detail", d.Detail)
		return AdmitResult{Decision: d}
	}
	budgeted := subjectToBudget(h, db, req.Resident)
	if budgeted {
		if st := m.ledger.Stats(); st.Degraded {
			// The ledger is already failing: refuse before allocating an id
			// or queuing a line, so a burst of firings through an outage is
			// one coalesced skip rather than a record each.
			m.noteLedgerLocked(true, errors.New(st.LastError))
			m.budgetMu.Unlock()
			detail := "the run ledger cannot be written"
			if st.LastError != "" {
				detail += ": " + st.LastError
			}
			log.Error("start refused: the run ledger cannot record it", "harness", name, "trigger", req.Trigger, "err", st.LastError)
			return AdmitResult{Decision: ledgerRefusal(req, detail)}
		}
	}
	day := m.bud.start
	m.bud.runs[name]++
	rec.StartedAt = req.Now
	opened, wait, err := m.enqueueNew(name, ledger.TypeOpened, rec)
	m.budgetMu.Unlock()
	if err == nil {
		err = wait()
	}
	if err != nil && budgeted {
		m.budgetMu.Lock()
		if m.bud.start.Equal(day) && m.bud.runs[name] > 0 {
			m.bud.runs[name]-- // not admitted after all; a rollover since already zeroed it
		}
		m.noteLedgerLocked(true, err)
		m.budgetMu.Unlock()
		return m.refuseOpened(name, req, opened, err)
	}
	if err != nil {
		// SPEC-0021 REQ-4: a harness subject to no budget starts anyway, and
		// the failure is logged (here, and on its durable log by the
		// supervisor) and counted.
		m.budgetMu.Lock()
		m.noteLedgerLocked(false, err)
		m.budgetMu.Unlock()
		log.Warn("run ledger did not record a start; starting anyway (no budget applies)", "harness", name, "run_id", opened.RunID, "err", err)
	}
	var path string
	if rec.Kind != KindResident {
		m.pruneRunLogs(name, opened.RunID)
		path = opened.Log
	}
	return AdmitResult{Decision: d, Run: opened, Log: path, Err: err}
}

// refuseOpened refuses a budgeted start whose `opened` line the ledger could
// not write: it stays queued and lands, in order, when the disk recovers, so
// a `closed` line follows it saying the start was refused (skipped, reason
// ledger_unavailable), and the record never reads as a run. A record so
// closed is not counted as admitted by a later rebuild (admittedRecord).
func (m *Manager) refuseOpened(name string, req budget.AdmitRequest, rec RunRecord, cause error) AdmitResult {
	now := m.now()
	rec.Outcome, rec.Reason, rec.EndedAt = OutcomeSkipped, ReasonLedgerUnavailable, &now
	if _, err := m.ledger.Append(ledger.Line{
		Type: ledger.TypeClosed, At: now, Harness: name, RunID: rec.RunID,
		Record: ledger.Record{Outcome: string(OutcomeSkipped), Reason: string(ReasonLedgerUnavailable), EndedAt: &now},
	}, false); err != nil {
		log.Error("could not queue the refusal of a run the ledger did not record", "harness", name, "run_id", rec.RunID, "err", err)
	}
	log.Error("start refused: the run ledger could not record it", "harness", name, "run_id", rec.RunID, "trigger", req.Trigger, "err", cause)
	return AdmitResult{Decision: ledgerRefusal(req, cause.Error()), Run: rec}
}

// ledgerRefusal is a ledger_unavailable refusal of req.
func ledgerRefusal(req budget.AdmitRequest, detail string) budget.Decision {
	v := budget.Skip
	if req.Resident {
		v = budget.Hold
	}
	return budget.Decision{Verdict: v, Reason: budget.ReasonLedgerUnavailable, Detail: detail}
}

// noteLedgerLocked counts one ledger failure admission met. Caller holds
// budgetMu.
func (m *Manager) noteLedgerLocked(refused bool, err error) {
	if refused {
		m.bud.ledger.Refused++
	} else {
		m.bud.ledger.Unrecorded++
	}
	if err != nil {
		m.bud.ledger.LastError = err.Error()
	}
	m.bud.ledger.LastAt = m.now()
}

// subjectToBudget reports whether a start of h is subject to a budget, which
// is what makes a ledger failure refuse it (SPEC-0021 REQ-4: "a harness that
// has any REQ-1 budget key, or is subject to a [budget] cap"). [budget]'s
// daily cost applies to every harness; its concurrency caps to one-shots
// only (REQ-6).
func subjectToBudget(h core.Harness, db core.DaemonBudget, resident bool) bool {
	if h.Budget != (core.Budget{}) || db.DailyCostUSD > 0 {
		return true
	}
	if resident {
		return false
	}
	return db.MaxConcurrent > 0 || (h.Budget.QuotaGroup != "" && db.Groups[h.Budget.QuotaGroup].MaxConcurrent > 0)
}

// budgetDef is name's definition and the [budget] table as admission reads
// them: the global config as of the last reload, so a reload applies at the
// next admission and the next tick, never by restarting anything (REQ-20). A
// project or scratch harness has no budget: budget keys are global-config
// only (REQ-1), and budgets on project harnesses are out of scope.
func (m *Manager) budgetDef(name string) (core.Harness, core.DaemonBudget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.provenance[name] != "" {
		return core.Harness{Name: name}, core.DaemonBudget{DayStarts: m.cfg.Budget.DayStarts}
	}
	return m.cfg.Harnesses[name], m.cfg.Budget
}

// admitStateLocked measures what Decide reads for name at req.Now. Caller
// holds budgetMu.
func (m *Manager) admitStateLocked(name string, h core.Harness, db core.DaemonBudget, req budget.AdmitRequest) budget.State {
	st := budget.State{
		Park:               m.quotaPark(name, h, req.Now),
		DailyCostUSD:       h.Budget.DailyCostUSD,
		DaemonDailyCostUSD: db.DailyCostUSD,
		RunsToday:          m.bud.runs[name],
		MaxRunsPerDay:      h.Budget.MaxRunsPerDay,
		DayNext:            m.bud.next,
	}
	st.CostTodayUSD, st.DaemonCostTodayUSD = m.costToday(name)
	st.InFlight, st.MaxConcurrent = m.concurrency(name, h, db, req)
	if req.Holds.Has(core.HoldHours) {
		st.HoursNext = hoursOpen(h, req.Now)
	}
	return st
}

// quotaPark is admission's check 2: the quota park in force on name, or on
// its quota group, at now (SPEC-0021 REQ-12, REQ-13).
//
// THE SEAM THE PARK STORY FILLS (stump.wtf/harness#477): it adds the park
// store (state.json's `parks`, design.md § "What is persisted") and answers
// from it here; a park on the harness or on h.Budget.QuotaGroup refuses a
// one-shot with quota_parked and holds a resident for quota. Until then no
// harness is parked. Called with budgetMu held, so it must not take a lock a
// park path holds while it waits on admission, or wait on any supervisor.
func (m *Manager) quotaPark(name string, h core.Harness, now time.Time) budget.Park {
	return budget.Park{}
}

// costToday is admission's check 3 input: name's spend today and the whole
// daemon's, which the meter-driven caps story (stump.wtf/harness#482) folds
// in from the usage accumulator. Until then nothing is spent, so a
// daily_cost_usd never refuses. Called with budgetMu held.
func (m *Manager) costToday(name string) (harness, daemon float64) {
	return 0, 0
}

// concurrency is admission's check 5 input: the one-shot runs in flight under
// the cap that applies to name, and that cap (0 when none), which the
// concurrency story (stump.wtf/harness#479) answers from its admission queue.
// Until then no cap is in force, so no firing ever waits. Called with
// budgetMu held.
func (m *Manager) concurrency(name string, h core.Harness, db core.DaemonBudget, req budget.AdmitRequest) (inFlight, limit int) {
	return 0, 0
}

// hoursOpen is when a gated harness's hours next open after now, zero when
// unknown: the Next of an hours refusal.
func hoursOpen(h core.Harness, now time.Time) time.Time {
	if h.HoursExpr.String() == "" {
		return time.Time{}
	}
	in, next, ok := h.HoursExpr.In(now)
	if in || !ok {
		return time.Time{}
	}
	return next
}

// rolloverLocked brings the budget day up to now. Its first use computes the
// day and rebuilds the day's counters from the ledger (REQ-7); after that,
// once now reaches the day's end it starts the day now falls in, however many
// rollovers a suspend or an outage slept through, with every counter at zero
// (REQ-3: no spend carries over). A day_starts a reload changed takes effect
// here and not before (REQ-20): the new day starts no earlier than the old one
// ended. Caller holds budgetMu.
func (m *Manager) rolloverLocked(now time.Time, spec hours.DailyInstant) {
	if m.bud.start.IsZero() {
		m.bud.start, m.bud.next = budget.Day(now, spec)
		m.rebuildLocked()
		return
	}
	if now.Before(m.bud.next) {
		return
	}
	start, next := budget.Day(now, spec)
	if start.Before(m.bud.next) {
		start = m.bud.next
	}
	log.Info("budget day rolled over", "day_start", start.Format(time.RFC3339), "next", next.Format(time.RFC3339), "previous_day_start", m.bud.start.Format(time.RFC3339))
	m.bud.start, m.bud.next = start, next
	clear(m.bud.runs)
}

// rebuildLocked folds the current day's admitted records out of the ledger
// into the running totals (REQ-7: rebuilt from the ledger at boot, before any
// start is admitted). Caller holds budgetMu.
func (m *Manager) rebuildLocked() {
	clear(m.bud.runs)
	recs, _, err := m.ledger.Query(ledger.Query{Since: m.bud.start})
	if err != nil {
		// Never swallowed: today's counts start from what the index holds,
		// and the failure is on the daemon log for doctor's ledger checks.
		log.Error("could not read today's runs from the run ledger; budget counters may undercount", "day_start", m.bud.start.Format(time.RFC3339), "err", err)
	}
	for _, f := range recs {
		if admittedRecord(f) {
			m.bud.runs[f.Harness]++
		}
	}
	if len(m.bud.runs) > 0 {
		log.Info("budget counters rebuilt from the run ledger", "day_start", m.bud.start.Format(time.RFC3339), "harnesses", len(m.bud.runs))
	}
}

// admittedRecord reports a record of a start admission let through: one that
// opened (it ran, or was admitted and then failed to spawn or to render its
// argv, which still counts, REQ-5) and is not a start a ledger failure refused
// after opening it (refuseOpened).
func admittedRecord(f ledger.Folded) bool {
	return f.HasOpened && (f.Outcome != string(OutcomeSkipped) || f.Reason != string(ReasonLedgerUnavailable))
}

// bootBudget readies admission at boot, after the ledger is reconciled and
// before Autostart admits anything (REQ-7): the budget day and its counters,
// a check for records the ledger lost, and the budget skips a one-shot is
// still owed its settle-up for.
func (m *Manager) bootBudget() {
	_, db := m.budgetDef("")
	m.budgetMu.Lock()
	m.rolloverLocked(m.now(), db.DayStarts)
	m.budgetMu.Unlock()
	m.findLedgerGaps()
	m.seedBudgetSkips()
}

// findLedgerGaps compares each harness's restored run id allocator with the
// ledger's newest record for it (REQ-7 Scenario "A lost ledger file"). Ids are
// handed out in order and every one gets an `opened` or `decided` line, so an
// allocator ahead of the ledger means records were lost, and today's counters,
// just rebuilt, are missing them. Logged, and kept for doctor.
func (m *Manager) findLedgerGaps() {
	m.mu.Lock()
	var gaps []LedgerGap
	for name, h := range m.runs {
		if !h.floored && h.LastRunID > 0 {
			gaps = append(gaps, LedgerGap{Harness: name, Last: h.LastRunID})
		}
	}
	m.mu.Unlock()
	slices.SortFunc(gaps, func(a, b LedgerGap) int {
		switch {
		case a.Harness < b.Harness:
			return -1
		case a.Harness > b.Harness:
			return 1
		}
		return 0
	})
	var found []LedgerGap
	for _, g := range gaps {
		g.Max = m.ledger.MaxRunID(g.Harness)
		if g.Max >= g.Last {
			continue
		}
		found = append(found, g)
		log.Warn("run ledger has a gap: records were lost (a day file deleted?); today's budget counters were rebuilt without them",
			"harness", g.Harness, "missing_run_ids", fmt.Sprintf("%d-%d", g.Max+1, g.Last), "dir", m.ledger.Dir())
	}
	m.budgetMu.Lock()
	m.bud.gaps = found
	m.budgetMu.Unlock()
}

// seedBudgetSkips re-derives, at boot, the budget skips a one-shot is owed its
// settle-up for: one whose newest record is a budget skip has had nothing run
// since, the same rule seedHoursSkipped applies to outside_hours skips.
func (m *Manager) seedBudgetSkips() {
	for _, s := range m.snapshotSupervisors() {
		snap := s.Snapshot()
		if !snap.Triggered {
			continue
		}
		recs := m.Runs(snap.Name)
		if n := len(recs); n > 0 && recs[n-1].Outcome == OutcomeSkipped && recs[n-1].Reason == ReasonBudget {
			m.budgetMu.Lock()
			m.bud.skips[snap.Name] = recs[n-1].StartedAt
			m.budgetMu.Unlock()
		}
	}
}

// admitRelease is the admission a harness passes through when its last hold
// reason clears (SPEC-0021 REQ-14: it "SHALL go back through admission"),
// handed to every supervisor as Options.Admit and asked on its actor loop. It
// answers the reasons that would still hold the harness, empty to admit,
// without counting or recording anything: the start that follows is admitted
// for real by the funnel, so a race between the two can only hold it, never
// let it past.
func (m *Manager) admitRelease(name string) core.HoldSet {
	h, db := m.budgetDef(name)
	req := budget.AdmitRequest{Harness: name, Resident: !h.Triggered(), Trigger: string(TriggerRelease), Now: m.now()}
	m.budgetMu.Lock()
	m.rolloverLocked(req.Now, db.DayStarts)
	d := budget.Decide(req, m.admitStateLocked(name, h, db, req))
	m.budgetMu.Unlock()
	if d.Admitted() {
		return 0
	}
	return core.HoldSetOf(d.Reason.HoldReason())
}

// budgetHoldCleared is the clearing hook for the budget hold reason
// (HoldClearer; SPEC-0021 REQ-14): name's budget is no longer spent at now,
// the gate tick's clock, because the day rolled over or a reload raised or
// removed the cap (REQ-20 Scenario "Raising the cap mid-day").
func (m *Manager) budgetHoldCleared(name string, now time.Time) bool {
	h, db := m.budgetDef(name)
	m.budgetMu.Lock()
	defer m.budgetMu.Unlock()
	m.rolloverLocked(now, db.DayStarts)
	return !budgetSpent(m.admitStateLocked(name, h, db, budget.AdmitRequest{Resident: true, Now: now}))
}

// budgetSpent reports whether a budget check (3 or 4) refuses at st: Decide
// asked about the budget inputs alone, so "spent" has one definition.
func budgetSpent(st budget.State) bool {
	d := budget.Decide(budget.AdmitRequest{Resident: true}, budget.State{
		CostTodayUSD:       st.CostTodayUSD,
		DailyCostUSD:       st.DailyCostUSD,
		DaemonCostTodayUSD: st.DaemonCostTodayUSD,
		DaemonDailyCostUSD: st.DaemonDailyCostUSD,
		RunsToday:          st.RunsToday,
		MaxRunsPerDay:      st.MaxRunsPerDay,
	})
	return d.Reason == budget.ReasonBudget
}

// BudgetDue is the budget day's tick, on the gate pass's clock (SPEC-0021
// REQ-3): asking it is what rolls the day over. It answers what the pass must
// do: the hold reasons to add (a resident running past a cap a reload lowered
// below today's count, REQ-20; a resident already held for another reason
// whose run budget is spent too, REQ-4 "Hours decide before budgets"), and
// the one-shots whose budget skips from an earlier day are owed their
// settle-up. Both are level-triggered: a harness the pass leaves for the next
// tick is reported again.
func (m *Manager) BudgetDue(now time.Time) (holds map[string]core.HoldSet, settle []string) {
	cfg := m.Config()
	type spent struct {
		name      string
		runs, max int
	}
	var due []spent
	m.budgetMu.Lock()
	m.rolloverLocked(now, cfg.Budget.DayStarts)
	for name, at := range m.bud.skips {
		if at.Before(m.bud.start) {
			settle = append(settle, name)
		}
	}
	for _, name := range cfg.HarnessOrder {
		h := cfg.Harnesses[name]
		if h.Triggered() || h.Budget.MaxRunsPerDay <= 0 {
			continue
		}
		if runs := m.bud.runs[name]; runs >= h.Budget.MaxRunsPerDay {
			due = append(due, spent{name, runs, h.Budget.MaxRunsPerDay})
		}
	}
	m.budgetMu.Unlock()
	slices.Sort(settle)
	for _, c := range due {
		s := m.get(c.name)
		if s == nil {
			continue
		}
		snap := s.Snapshot()
		if snap.Holds.Has(core.HoldBudget) {
			continue
		}
		switch up := snapUp(snap.State); {
		case up && c.runs > c.max:
			// Running on an admission a lower cap would have refused.
		case !up && !snap.Holds.Empty():
			// Down and held for another reason: held for its budget too, so
			// that reason clearing first leaves it held (REQ-14).
		default:
			continue
		}
		if holds == nil {
			holds = make(map[string]core.HoldSet)
		}
		holds[c.name] = core.HoldSetOf(core.HoldBudget)
	}
	return holds, settle
}

// AddHolds adds reasons other than hours to name's hold: the gate pass's
// dispatch of BudgetDue's holds. A harness that is up stops at once. A
// graceful budget close (REQ-10's resident crossing a daily cost cap) is the
// caps story's (stump.wtf/harness#482), which also teaches the pass to step a
// close that is not for hours; today the pass steps only hours closes.
func (m *Manager) AddHolds(name string, reasons core.HoldSet) {
	for _, r := range reasons.Without(core.HoldHours).Reasons() {
		m.Hold(name, r, core.HoursShutdownImmediate, time.Time{})
	}
}

// SettleBudget settles name's budget skips once the day they were refused in
// is over: their records close, and under catch_up = true one catch_up run
// starts, through admission, counting toward the new day (REQ-5 Scenario
// "Catch-up after a budget skip"). ok is false when nothing was owed, so the
// pass's level-triggered ask settles each day's skips exactly once.
func (m *Manager) SettleBudget(name string) (RunDecision, bool) {
	m.budgetMu.Lock()
	at, ok := m.bud.skips[name]
	owed := ok && at.Before(m.bud.start)
	if owed {
		delete(m.bud.skips, name)
	}
	m.budgetMu.Unlock()
	s := m.get(name)
	if !owed || s == nil {
		return RunDecision{}, false
	}
	return s.SettleBudget(), true
}

// RunsToday is how many starts of name admission has let through since the
// budget day began.
func (m *Manager) RunsToday(name string) int {
	m.budgetMu.Lock()
	defer m.budgetMu.Unlock()
	return m.bud.runs[name]
}

// BudgetDay is the current budget day's bounds; both zero before its first
// use.
func (m *Manager) BudgetDay() (start, next time.Time) {
	m.budgetMu.Lock()
	defer m.budgetMu.Unlock()
	return m.bud.start, m.bud.next
}

// AdmissionLedgerStats is what admission met of the run ledger, for doctor.
func (m *Manager) AdmissionLedgerStats() AdmissionLedger {
	m.budgetMu.Lock()
	defer m.budgetMu.Unlock()
	return m.bud.ledger
}

// LedgerGaps are the run records boot found missing from the ledger.
func (m *Manager) LedgerGaps() []LedgerGap {
	m.budgetMu.Lock()
	defer m.budgetMu.Unlock()
	return slices.Clone(m.bud.gaps)
}
