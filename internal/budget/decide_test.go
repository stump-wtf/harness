package budget

// Governing: SPEC-0021 REQ-4 "Admission" (the fixed order, first reason
// decides), REQ-5 "Run-count budget", REQ-6 (the Wait verdict), REQ-10 (check
// 3), REQ-12 (check 2), REQ-15 (Override), REQ-21 (sentinels).
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

var (
	t0      = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	dayNext = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
)

func oneshot() AdmitRequest { return AdmitRequest{Harness: "pr-review", Trigger: "webhook", Now: t0} }
func resident() AdmitRequest {
	return AdmitRequest{Harness: "crush-sb", Resident: true, Trigger: "restart", Now: t0}
}

func TestDecideAdmitsTheZeroState(t *testing.T) {
	for _, req := range []AdmitRequest{oneshot(), resident()} {
		d := Decide(req, State{})
		if !d.Admitted() || d.Reason != ReasonNone || d.Err() != nil {
			t.Errorf("Decide(%+v, zero) = %+v, want an admission", req, d)
		}
	}
}

// TestDecideRunCount is REQ-5: with max_runs_per_day = N the (N+1)th
// admission of the day is the first refused, a resident is held and a
// one-shot skipped, and the refusal clears at the rollover.
func TestDecideRunCount(t *testing.T) {
	st := State{RunsToday: 39, MaxRunsPerDay: 40, DayNext: dayNext}
	if d := Decide(oneshot(), st); !d.Admitted() {
		t.Fatalf("39/40: %+v, want admitted", d)
	}
	st.RunsToday = 40
	d := Decide(oneshot(), st)
	if d.Verdict != Skip || d.Reason != ReasonBudget || d.Detail != "40/40 runs today" || !d.Next.Equal(dayNext) {
		t.Errorf("one-shot 40/40: %+v, want skip/budget \"40/40 runs today\" next %v", d, dayNext)
	}
	if !errors.Is(d.Err(), ErrOverBudget) || errors.Is(d.Err(), ErrParked) {
		t.Errorf("Err() = %v, want ErrOverBudget only", d.Err())
	}
	if !strings.Contains(d.Err().Error(), "40/40 runs today") {
		t.Errorf("Err() = %q, want the detail in it", d.Err())
	}
	if d := Decide(resident(), st); d.Verdict != Hold || d.Reason != ReasonBudget {
		t.Errorf("resident 40/40: %+v, want hold/budget", d)
	}
	if got := ReasonBudget.HoldReason(); got != core.HoldBudget {
		t.Errorf("ReasonBudget holds %v, want budget", got)
	}
}

// TestDecideOrder pins REQ-4's order: every refusal that applies is set, and
// peeling them off one at a time must surface them in the listed order.
func TestDecideOrder(t *testing.T) {
	park := Park{Until: t0.Add(3 * time.Hour), Rule: "crush/litellm.ratelimiterror", Group: "claude-max"}
	req := oneshot()
	req.Holds = core.HoldSetOf(core.HoldHours)
	st := State{
		HoursNext:          t0.Add(20 * time.Hour),
		Park:               park,
		CostTodayUSD:       15.02,
		DailyCostUSD:       15,
		DaemonCostTodayUSD: 50,
		DaemonDailyCostUSD: 50,
		RunsToday:          40,
		MaxRunsPerDay:      40,
		DayNext:            dayNext,
		InFlight:           4,
		MaxConcurrent:      4,
	}
	steps := []struct {
		want   Reason
		detail string
		peel   func()
	}{
		{ReasonHours, "out of operating hours until", func() { req.Holds = 0 }},
		{ReasonQuotaParked, "by crush/litellm.ratelimiterror (quota group claude-max)", func() { st.Park = Park{} }},
		{ReasonBudget, "15.02/15.00 USD today", func() { st.CostTodayUSD = 0 }},
		{ReasonBudget, "50.00/50.00 USD today across the daemon", func() { st.DaemonCostTodayUSD = 0 }},
		{ReasonBudget, "40/40 runs today", func() { st.RunsToday = 0 }},
		{ReasonConcurrency, "waiting 4/4 in flight", func() { st.InFlight = 0 }},
	}
	for i, s := range steps {
		d := Decide(req, st)
		if d.Reason != s.want || !strings.Contains(d.Detail, s.detail) {
			t.Fatalf("step %d: %+v, want reason %q with detail containing %q", i, d, s.want, s.detail)
		}
		s.peel()
	}
	if d := Decide(req, st); !d.Admitted() {
		t.Fatalf("every reason peeled: %+v, want admitted", d)
	}
}

// TestDecideParkSentinel: a park refuses with ErrParked until its reset
// instant, and not at it (a park ends AT its reset).
func TestDecideParkSentinel(t *testing.T) {
	st := State{Park: Park{Until: t0.Add(time.Minute)}}
	d := Decide(resident(), st)
	if d.Verdict != Hold || d.Reason != ReasonQuotaParked || !errors.Is(d.Err(), ErrParked) || !d.Next.Equal(st.Park.Until) {
		t.Fatalf("parked: %+v (%v), want hold/quota_parked/ErrParked next %v", d, d.Err(), st.Park.Until)
	}
	req := resident()
	req.Now = st.Park.Until
	if d := Decide(req, st); !d.Admitted() {
		t.Errorf("at the reset instant: %+v, want admitted", d)
	}
}

// TestDecideOverrideSkipsOnlyBudgets is REQ-15's --over-budget at the
// decision level: it lifts checks 3 and 4 and nothing else.
func TestDecideOverrideSkipsOnlyBudgets(t *testing.T) {
	req := oneshot()
	req.Override = true
	if d := Decide(req, State{RunsToday: 40, MaxRunsPerDay: 40, CostTodayUSD: 20, DailyCostUSD: 15}); !d.Admitted() {
		t.Errorf("override over budget: %+v, want admitted", d)
	}
	if d := Decide(req, State{Park: Park{Until: t0.Add(time.Hour)}}); d.Reason != ReasonQuotaParked {
		t.Errorf("override while parked: %+v, want quota_parked", d)
	}
}

// TestDecideConcurrencyIsOneShotOnly is REQ-6: residents neither count toward
// nor are limited by the cap; a one-shot waits rather than being dropped.
func TestDecideConcurrencyIsOneShotOnly(t *testing.T) {
	st := State{InFlight: 2, MaxConcurrent: 2}
	if d := Decide(resident(), st); !d.Admitted() {
		t.Errorf("resident at the cap: %+v, want admitted", d)
	}
	d := Decide(oneshot(), st)
	if d.Verdict != Wait || d.Reason != ReasonConcurrency {
		t.Errorf("one-shot at the cap: %+v, want wait/concurrency", d)
	}
	if d.Err() == nil || errors.Is(d.Err(), ErrOverBudget) {
		t.Errorf("Err() = %v, want a non-budget error", d.Err())
	}
}

func TestDecisionErrSentinels(t *testing.T) {
	for _, c := range []struct {
		d    Decision
		want error
	}{
		{Decision{Verdict: Skip, Reason: ReasonBudget}, ErrOverBudget},
		{Decision{Verdict: Hold, Reason: ReasonQuotaParked, Detail: "x"}, ErrParked},
		{Decision{Verdict: Skip, Reason: ReasonLedgerUnavailable, Detail: "disk full"}, ErrLedgerUnavailable},
	} {
		if err := c.d.Err(); !errors.Is(err, c.want) {
			t.Errorf("%+v.Err() = %v, want %v", c.d, err, c.want)
		}
	}
	if (Decision{}).Admitted() {
		t.Error("the zero Decision reads as an admission")
	}
	if (Decision{}).Err() == nil {
		t.Error("the zero Decision has no error")
	}
}
