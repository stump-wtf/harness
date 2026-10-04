package budget

// The Admission Decision
//
// Decide is SPEC-0021 REQ-4's ordered evaluation as a pure function of the
// request and the measured state. Order is the point: the first reason that
// applies decides, so a harness out of hours whose run budget is also spent
// reports hours (the reason a window opening can clear), and a parked harness
// is never admitted past its park because its budget happens to have room.
//
// Governing: ADR-0027; SPEC-0021 REQ-4 "Admission", REQ-5 "Run-count budget",
// REQ-6 "Concurrency admission" (the Wait verdict), REQ-10 "Daily cost caps"
// (check 3), REQ-12 "Parking" (check 2), REQ-15 (Override); design.md §
// "Admission is a funnel on the Manager".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"fmt"
	"time"
)

// Decide evaluates req against st in REQ-4's order and returns the first
// refusal that applies, or an admission. It reads no clock: req.Now is the
// only "now" it knows.
func Decide(req AdmitRequest, st State) Decision {
	// 1. Operating hours: SPEC-0012's verdict, as the gate keeps it.
	if req.Holds.Has(hoursReason) {
		detail := "out of operating hours"
		if !st.HoursNext.IsZero() {
			detail += " until " + stamp(st.HoursNext)
		}
		return refuse(req, ReasonHours, detail, st.HoursNext)
	}
	// 2. A quota park on the harness or its quota group. A hold another spec
	// defines (SPEC-0020's model hold) slots in between 1 and 2 when it
	// lands; nothing here needs to change for it.
	if st.Park.Active(req.Now) {
		detail := "parked until " + stamp(st.Park.Until)
		if st.Park.Rule != "" {
			detail += " by " + st.Park.Rule
		}
		if st.Park.Group != "" {
			detail += " (quota group " + st.Park.Group + ")"
		}
		return refuse(req, ReasonQuotaParked, detail, st.Park.Until)
	}
	if !req.Override {
		// 3. A spent daily cost cap: the harness's own, then [budget]'s.
		if st.DailyCostUSD > 0 && st.CostTodayUSD >= st.DailyCostUSD {
			return refuse(req, ReasonBudget, fmt.Sprintf("%.2f/%.2f USD today", st.CostTodayUSD, st.DailyCostUSD), st.DayNext)
		}
		if st.DaemonDailyCostUSD > 0 && st.DaemonCostTodayUSD >= st.DaemonDailyCostUSD {
			return refuse(req, ReasonBudget, fmt.Sprintf("%.2f/%.2f USD today across the daemon", st.DaemonCostTodayUSD, st.DaemonDailyCostUSD), st.DayNext)
		}
		// 4. A spent run count. The (N+1)th admission of the day is the
		// first refused one.
		if st.MaxRunsPerDay > 0 && st.RunsToday >= st.MaxRunsPerDay {
			return refuse(req, ReasonBudget, fmt.Sprintf("%d/%d runs today", st.RunsToday, st.MaxRunsPerDay), st.DayNext)
		}
	}
	// 5. No free concurrency slot. Residents neither count toward nor are
	// limited by it (REQ-6), and a one-shot that meets it waits for a slot
	// rather than being dropped.
	if !req.Resident && st.MaxConcurrent > 0 && st.InFlight >= st.MaxConcurrent {
		return Decision{Verdict: Wait, Reason: ReasonConcurrency, Detail: fmt.Sprintf("waiting %d/%d in flight", st.InFlight, st.MaxConcurrent)}
	}
	return Decision{Verdict: Admit}
}

// hoursReason is core.HoldHours, named here so Decide reads as the REQ-4 list.
var hoursReason = ReasonHours.HoldReason()

// refuse is a refusal for reason: a held resident, or a skipped firing.
func refuse(req AdmitRequest, reason Reason, detail string, next time.Time) Decision {
	v := Skip
	if req.Resident {
		v = Hold
	}
	return Decision{Verdict: v, Reason: reason, Detail: detail, Next: next}
}

// stamp renders an instant for a Detail sentence, in its own zone: the
// operator reads "15:00" in the zone the reset was computed in.
func stamp(t time.Time) string { return t.Format("2006-01-02 15:04 MST") }
