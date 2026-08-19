package main

// Governing: ADR-0013 (schedule surfaced on the listing surfaces); SPEC-0003
// (state legibility — scheduled harnesses swap their state glyph for a clock
// and carry the next-run time inline in DESCRIPTION, highlighted, rather
// than in dedicated columns).

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

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

func TestPrintHarnessTableMarksScheduledInline(t *testing.T) {
	var buf bytes.Buffer
	hs := []protocol.HarnessInfo{
		{Name: "sweep", State: "stopped", Schedule: "0 */6 * * *", NextRun: time.Now().Add(2 * time.Hour).Format(time.RFC3339)},
		{Name: "always-on", State: "running"},
	}
	if err := printHarnessTable(&buf, hs); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// The scheduled row carries the clock glyph and the highlighted next-run
	// time in DESCRIPTION; no SCHEDULE/NEXT columns exist anymore.
	for _, want := range []string{"⏱ stopped", "in 2h"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in table:\n%s", want, out)
		}
	}
	// The unscheduled row keeps its plain state glyph.
	if !strings.Contains(out, "● running") {
		t.Errorf("unscheduled row lost its state glyph:\n%s", out)
	}
	for _, unwanted := range []string{"SCHEDULE", "NEXT", "0 */6 * * *"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("schedule column/data leaked into table (%q):\n%s", unwanted, out)
		}
	}
}

// TestPrintHarnessTableOmitsScheduleColumnsWhenUnused is the other half:
// with no scheduled harness there is nothing schedule-shaped on screen at
// all — no clock glyph, no next-run badge — and DESCRIPTION keeps its full
// budget.
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
