// Package budget is the pure half of SPEC-0021's run budgets: the admission
// decision every process start passes before it spawns, and the budget-day
// arithmetic its counters are kept against.
//
// # Run Budgets
//
// The package has no clock, no I/O and no state. Decide turns an admission
// request and the state the caller measured into a verdict; Day turns an
// instant and a day_starts value into the budget day around it. Which instant
// is "now", where the counters live and what a refusal does are the
// supervisor Manager's (internal/supervisor, manager_admit.go), which owns the
// mutable state under its lock and calls Decide inside Manager.Admit, the one
// funnel every start path goes through. Keeping the decision pure is what lets
// it be table-tested exhaustively, the pattern internal/hours proved for the
// operating-hours gate.
//
// Admission evaluates, in this fixed order, and the first reason that applies
// decides (REQ-4):
//
//  1. operating hours (SPEC-0012, unchanged);
//  2. a quota park on the harness or its quota group (REQ-12);
//  3. a spent daily_cost_usd, on the harness or on [budget] (REQ-10);
//  4. a spent max_runs_per_day (REQ-5);
//  5. no free concurrency slot, for a one-shot only (REQ-6).
//
// A refused resident is held (Hold), a refused one-shot firing is skipped
// (Skip), and a one-shot with no free slot waits (Wait).
//
// Governing: ADR-0027 (run budgets and usage-limit backoff); SPEC-0021 REQ-3
// "The budget day", REQ-4 "Admission", REQ-5 "Run-count budget", REQ-21
// "Error handling and concurrency safety"; run-budgets design.md § "A new
// internal/budget package owns admission and counters", § "Admission is a
// funnel on the Manager".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.
package budget

import (
	"errors"
	"fmt"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// Sentinel errors for a refused start (SPEC-0021 REQ-21). The control op maps
// each to its own protocol error code, so a script and the CLI can tell an
// over-budget harness from a parked one and from a ledger that cannot be
// written. Decision.Err wraps them with the decision's Detail.
var (
	// ErrOverBudget: a daily cap the harness is subject to is spent (REQ-5,
	// REQ-10).
	ErrOverBudget = errors.New("over budget")
	// ErrParked: the harness, or its quota group, is parked on an exhausted
	// provider quota (REQ-12).
	ErrParked = errors.New("parked")
	// ErrLedgerUnavailable: the run's ledger record could not be written, and
	// the harness is subject to a budget, so it was refused rather than
	// started uncounted (REQ-4).
	ErrLedgerUnavailable = errors.New("ledger unavailable")
)

// Verdict is what admission decided. The zero value is no decision at all, so
// a Decision nobody filled in can never read as an admission.
type Verdict uint8

const (
	// Admit: the start may proceed.
	Admit Verdict = iota + 1
	// Hold: a resident harness is held down for the decision's Reason.
	Hold
	// Skip: a one-shot firing is recorded skipped for the decision's Reason.
	Skip
	// Wait: a one-shot firing waits for a concurrency slot (REQ-6).
	Wait
)

// String is the verdict's name, for log lines.
func (v Verdict) String() string {
	switch v {
	case Admit:
		return "admit"
	case Hold:
		return "hold"
	case Skip:
		return "skip"
	case Wait:
		return "wait"
	}
	return "undecided"
}

// Reason is why admission refused a start; "" for an admission. The values
// are the skip reasons SPEC-0014's run records carry and the reason label
// SPEC-0021 REQ-18's admission metric uses.
type Reason string

const (
	// ReasonNone: admitted.
	ReasonNone Reason = ""
	// ReasonHours: out of operating hours (check 1).
	ReasonHours Reason = "hours"
	// ReasonQuotaParked: parked on an exhausted quota (check 2).
	ReasonQuotaParked Reason = "quota_parked"
	// ReasonBudget: a daily cost or run-count cap is spent (checks 3 and 4).
	ReasonBudget Reason = "budget"
	// ReasonConcurrency: no free concurrency slot (check 5).
	ReasonConcurrency Reason = "concurrency"
	// ReasonLedgerUnavailable: the run's ledger record could not be written
	// for a harness subject to a budget (REQ-4). Decide never returns it; the
	// Manager does, after Decide admitted and the append failed.
	ReasonLedgerUnavailable Reason = "ledger_unavailable"
)

// HoldReason is the hold reason a resident refused for r is held for: hours,
// quota or budget. A refusal that holds nothing (concurrency, which only a
// one-shot meets, and a ledger failure, which is not a condition a hold can
// wait out) is the zero HoldReason.
func (r Reason) HoldReason() core.HoldReason {
	switch r {
	case ReasonHours:
		return core.HoldHours
	case ReasonQuotaParked:
		return core.HoldQuota
	case ReasonBudget:
		return core.HoldBudget
	}
	return 0
}

// AdmitRequest is one start asking to be admitted.
type AdmitRequest struct {
	// Harness names the harness.
	Harness string
	// Resident is a harness with no prompt: its process lifetime is the run,
	// and a refusal holds it rather than skipping a firing.
	Resident bool
	// Trigger is the start path: schedule, catch_up, channel, webhook,
	// manual, autostart, restart, release or lease (SPEC-0022 REQ-3).
	Trigger string
	// Holds is the harness's hold set at the moment of the request, on its
	// own actor loop. Check 1 reads operating hours from it: the gate pass
	// (SPEC-0012) is what judges hours, on the scheduler's tick, and keeps
	// the hours reason current; admission does not judge them a second time
	// against a different clock.
	Holds core.HoldSet
	// Override is `--over-budget`, accepted only on the control socket
	// (SPEC-0021 REQ-15): it skips the budget checks (3 and 4) for this one
	// start. Nothing sets it until the override story (#481).
	Override bool
	// Now is the instant the decision is made at: the Manager's clock seam,
	// never a wall-clock read in this package.
	Now time.Time
}

// Park is a quota park in force on a harness or its quota group (SPEC-0021
// REQ-12). The zero Park is no park.
type Park struct {
	// Until is the park's reset instant.
	Until time.Time
	// Rule names the classifier rule that matched (never error text,
	// ADR-0008).
	Rule string
	// Group is the quota group the park is on, "" for the harness's own.
	Group string
}

// Active reports whether the park still holds at now: it ends AT its reset
// instant.
func (p Park) Active(now time.Time) bool { return !p.Until.IsZero() && now.Before(p.Until) }

// State is what the caller measured for one harness at the request's instant:
// every input Decide reads. A zero field is "not configured" or "nothing
// spent", so the zero State admits everything.
type State struct {
	// HoursNext is when a harness held for its hours next opens; zero when
	// unknown. Check 1's "out of hours" is AdmitRequest.Holds.
	HoursNext time.Time
	// Park is the quota park in force, if any (check 2).
	Park Park
	// CostTodayUSD and DailyCostUSD are the harness's spend today and its
	// daily_cost_usd (check 3); DaemonCostTodayUSD and DaemonDailyCostUSD
	// are the same for [budget]. A zero cap is unset.
	CostTodayUSD, DailyCostUSD             float64
	DaemonCostTodayUSD, DaemonDailyCostUSD float64
	// RunsToday is how many starts of the harness were admitted since the
	// budget day began, and MaxRunsPerDay its cap, 0 when unset (check 4).
	RunsToday, MaxRunsPerDay int
	// DayNext is when the budget day rolls over: when a budget refusal
	// clears.
	DayNext time.Time
	// InFlight is how many one-shot runs are in flight under the cap
	// MaxConcurrent applies to, 0 when unset (check 5).
	InFlight, MaxConcurrent int
}

// Decision is admission's answer.
type Decision struct {
	Verdict Verdict
	// Reason is why a start was refused; "" when admitted.
	Reason Reason
	// Detail is the human sentence the CLI prints verbatim: "40/40 runs
	// today", "parked until 15:00 by crush/litellm.ratelimiterror".
	Detail string
	// Next is when the reason is expected to clear; zero when unknown.
	Next time.Time
}

// Admitted reports whether the start may proceed.
func (d Decision) Admitted() bool { return d.Verdict == Admit }

// Err is the refusal as an error wrapping its sentinel (ErrOverBudget,
// ErrParked, ErrLedgerUnavailable), so errors.Is distinguishes them; nil for
// an admission. A refusal with no sentinel of its own (hours, concurrency)
// is still an error, naming its reason.
func (d Decision) Err() error {
	if d.Verdict == Admit {
		return nil
	}
	var sentinel error
	switch d.Reason {
	case ReasonBudget:
		sentinel = ErrOverBudget
	case ReasonQuotaParked:
		sentinel = ErrParked
	case ReasonLedgerUnavailable:
		sentinel = ErrLedgerUnavailable
	default:
		if d.Detail == "" {
			return fmt.Errorf("refused: %s", d.Reason)
		}
		return fmt.Errorf("refused: %s: %s", d.Reason, d.Detail)
	}
	if d.Detail == "" {
		return sentinel
	}
	return fmt.Errorf("%w: %s", sentinel, d.Detail)
}
