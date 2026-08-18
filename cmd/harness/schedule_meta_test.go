package main

// Governing: ADR-0013 (schedule surfaced on the listing surfaces); SPEC-0003
// (state legibility — the SCHEDULE/NEXT columns are metadata, so they render
// plain and degrade to "-" in the non-scheduled case rather than shouting).

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

func TestScheduleCell(t *testing.T) {
	if got := scheduleCell(""); got != "-" {
		t.Errorf("empty schedule: got %q, want %q", got, "-")
	}
	if got := scheduleCell("0 */6 * * *"); got != "0 */6 * * *" {
		t.Errorf("cron spec must render verbatim, got %q", got)
	}
}

func TestNextRunCell(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "-"},
		{"malformed", "not-a-time", "-"},
		{"past", time.Now().Add(-time.Minute).Format(time.RFC3339), "due"},
		{"future", time.Now().Add(90 * time.Minute).Format(time.RFC3339), "in 1h30m"},
	}
	for _, tc := range cases {
		if got := nextRunCell(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestShortDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h30m"},
		{2 * time.Hour, "2h"},
		// Rounding carries into the next unit rather than reporting "60m".
		{59*time.Minute + 45*time.Second, "1h"},
		{52 * time.Hour, "2d4h"},
		{48 * time.Hour, "2d"},
	}
	for _, tc := range cases {
		if got := shortDuration(tc.in); got != tc.want {
			t.Errorf("%v: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPrintHarnessTableIncludesScheduleColumns(t *testing.T) {
	var buf bytes.Buffer
	hs := []protocol.HarnessInfo{
		{Name: "sweep", State: "stopped", Schedule: "0 */6 * * *", NextRun: time.Now().Add(2 * time.Hour).Format(time.RFC3339)},
		{Name: "always-on", State: "running"},
	}
	if err := printHarnessTable(&buf, hs); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"SCHEDULE", "NEXT", "0 */6 * * *", "in 2h", "-"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in table:\n%s", want, out)
		}
	}
}

// TestPrintHarnessTableOmitsScheduleColumnsWhenUnused is the other half: the
// columns cost 26 of the table's 80-cell budget, all of it taken from
// DESCRIPTION (the only flex column). Rendered for a fleet with no schedules
// they buy two columns of "-" and leave descriptions shredded into
// two-letter fragments.
func TestPrintHarnessTableOmitsScheduleColumnsWhenUnused(t *testing.T) {
	var buf bytes.Buffer
	hs := []protocol.HarnessInfo{
		{Name: "web", State: "running", Description: "the reduit web frontend dev server"},
		{Name: "api", State: "running", Description: "the reduit api"},
	}
	if err := printHarnessTable(&buf, hs); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, unwanted := range []string{"SCHEDULE", "NEXT"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unscheduled fleet still paid for the %s column:\n%s", unwanted, out)
		}
	}
	// The description must survive on one line rather than wrapping into a
	// stack of fragments — that is the budget the columns were eating.
	if !strings.Contains(out, "the reduit web frontend dev server") {
		t.Errorf("description wrapped away:\n%s", out)
	}
}
