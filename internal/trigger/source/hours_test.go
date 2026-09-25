package source

// Operating-hours gate tests for the fan-out.
//
// The property is about what the manager ASKED the runner for: an out-of-hours
// harness must reach SkipRun with reason outside_hours and must never reach
// StartRun, while every other harness bound to the same source fires as
// before. The fake runner records both kinds of call, so "skipped" and "never
// asked to run" are checked separately rather than inferred from each other.
//
// Governing: SPEC-0014 REQ "Operating Hours On Triggered Harnesses"; SPEC-0012
// REQ "Gate Evaluation".
//
// @joestump 09/23/2026 - Added for stump.wtf/harness#484.

import (
	"context"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/supervisor"
)

const weekdays = "TZ=UTC Mon-Fri 09:00-18:00"

// gate gives name operating_hours, parsed as config load would.
func gate(t *testing.T, cfg *core.Config, name, raw string) {
	t.Helper()
	expr, err := hours.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Harnesses[name]
	h.OperatingHours, h.HoursExpr = raw, expr
	cfg.Harnesses[name] = h
}

// Mon 2026-09-21 is a Monday; Sat 2026-09-26 a Saturday.
var (
	saturday = time.Date(2026, 9, 26, 14, 0, 0, 0, time.UTC)
	mondayAt = func(h, m, s, ns int) time.Time { return time.Date(2026, 9, 21, h, m, s, ns, time.UTC) }
)

// TestOutOfHoursFiringIsSkipped is the "Doorbell at night" scenario at the
// fan-out: the gated harness is recorded skipped with outside_hours, the
// ungated one bound to the same source still fires, and the decision the
// webhook's 202 reports says skipped.
func TestOutOfHoursFiringIsSkipped(t *testing.T) {
	r := &fakeRunner{}
	cfg := cfgWith(
		[2]string{"gated", "webhook.gh"},
		[2]string{"always", "webhook.gh"},
	)
	gate(t, cfg, "gated", weekdays)
	m := newManager(t, r, cfg)

	ev := webhookEvent("webhook.gh", "d-1")
	ev.ReceivedAt = saturday
	got := m.Fire(ev)

	if len(got) != 2 {
		t.Fatalf("decisions = %+v", got)
	}
	if got[0].Kind != supervisor.DecisionSkipped || got[0].Reason != supervisor.ReasonOutsideHours {
		t.Errorf("gated decision = %+v, want skipped / outside_hours", got[0])
	}
	if got[1].Kind != supervisor.DecisionStarted {
		t.Errorf("ungated decision = %+v, want started: one harness's hours must not gate another", got[1])
	}
	for _, c := range r.calls {
		switch c.name {
		case "gated":
			if c.skip != supervisor.ReasonOutsideHours {
				t.Errorf("gated harness was asked to run (%+v); it must only be recorded skipped", c)
			}
			if c.req.Trigger != supervisor.TriggerWebhook || c.req.Source != "webhook.gh" || c.req.Event == nil || c.req.Event.EventID != "d-1" {
				t.Errorf("skip request = %+v, want the firing's trigger, source and event id", c.req)
			}
		case "always":
			if c.skip != "" {
				t.Errorf("ungated harness was skipped: %+v", c)
			}
		}
	}
}

// TestHoursBoundaryIsEndExclusive drives the gate across both edges of the
// window. Windows are end-exclusive: 18:00:00 exactly is closed, a nanosecond
// before it is open, and 09:00:00 exactly is open.
func TestHoursBoundaryIsEndExclusive(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		skip bool
	}{
		{"opening instant", mondayAt(9, 0, 0, 0), false},
		{"just before opening", mondayAt(8, 59, 59, 999999999), true},
		{"just before close", mondayAt(17, 59, 59, 999999999), false},
		{"closing instant", mondayAt(18, 0, 0, 0), true},
		{"saturday", saturday, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{}
			cfg := cfgWith([2]string{"gated", "webhook.gh"})
			gate(t, cfg, "gated", weekdays)
			m := newManager(t, r, cfg)

			ev := webhookEvent("webhook.gh", "d-1")
			ev.ReceivedAt = tc.at
			got := m.Fire(ev)

			skipped := got[0].Kind == supervisor.DecisionSkipped && got[0].Reason == supervisor.ReasonOutsideHours
			if skipped != tc.skip {
				t.Errorf("at %s: decision = %+v, want skipped=%v", tc.at.Format(time.RFC3339Nano), got[0], tc.skip)
			}
			if asked := r.calls[0].skip == ""; asked == tc.skip {
				t.Errorf("at %s: StartRun asked = %v, want %v", tc.at.Format(time.RFC3339Nano), asked, !tc.skip)
			}
		})
	}
}

// TestGateJudgesTheReceiveTime: the firing is judged at ReceivedAt, not at
// whatever the manager's clock says when it gets round to it — an event
// received at 17:59 and fanned out at 18:01 was received in hours.
func TestGateJudgesTheReceiveTime(t *testing.T) {
	r := &fakeRunner{}
	cfg := cfgWith([2]string{"gated", "webhook.gh"})
	gate(t, cfg, "gated", weekdays)
	m := New(Options{
		Runner: r, Config: func() *core.Config { return cfg }, Log: log.New(discard{}),
		Now: func() time.Time { return mondayAt(18, 1, 0, 0) },
	})
	m.Start(context.Background())
	t.Cleanup(m.Close)

	ev := webhookEvent("webhook.gh", "d-1")
	ev.ReceivedAt = mondayAt(17, 59, 0, 0)
	if got := m.Fire(ev); got[0].Kind != supervisor.DecisionStarted {
		t.Errorf("decision = %+v, want started: the event arrived in hours", got[0])
	}
}

// TestReconnectCatchUpOutOfHoursIsSkipped: a channel reconnect's catch-up is
// a firing too, so a daemon booted on a Saturday records it skipped rather
// than running a gated harness at once — and that skip is what owes the
// harness its catch-up when hours open.
func TestReconnectCatchUpOutOfHoursIsSkipped(t *testing.T) {
	r := &fakeRunner{}
	cfg := cfgWith([2]string{"gated", "channel.sb"}, [2]string{"always", "channel.sb"})
	gate(t, cfg, "gated", weekdays)
	for _, n := range []string{"gated", "always"} {
		h := cfg.Harnesses[n]
		h.CatchUp = true
		cfg.Harnesses[n] = h
	}
	m := New(Options{
		Runner: r, Config: func() *core.Config { return cfg }, Log: log.New(discard{}),
		Now: func() time.Time { return saturday },
	})
	m.Start(context.Background())
	t.Cleanup(m.Close)

	m.catchUp("channel.sb")

	if len(r.calls) != 2 {
		t.Fatalf("calls = %+v, want one per harness", r.calls)
	}
	for _, c := range r.calls {
		if c.req.Trigger != supervisor.TriggerCatchUp {
			t.Errorf("%s: trigger = %q, want catch_up", c.name, c.req.Trigger)
		}
		want := supervisor.RunReason("")
		if c.name == "gated" {
			want = supervisor.ReasonOutsideHours
		}
		if c.skip != want {
			t.Errorf("%s: skip reason = %q, want %q", c.name, c.skip, want)
		}
	}
}
