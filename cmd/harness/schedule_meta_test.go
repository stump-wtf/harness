package main

// Governing: ADR-0013 (schedule surfaced on the listing surfaces); SPEC-0003
// (state legibility — scheduled harnesses swap their state glyph for a clock
// and carry the next-run time inline in DESCRIPTION, highlighted, rather
// than in dedicated columns).
//
// The cron-label and duration tables live in internal/schedfmt now that the
// TUI renders them too (#160); what stays here is the table's own rendering
// and the "-" empty cell that only a table has.

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
	for _, unwanted := range []string{"SCHEDULE", "NEXT", "PID", "0 */6 * * *"} {
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

// TestDescriptionCellStylesSurviveWrapping pins the rendering bug behind the
// inline next-run: DESCRIPTION is the wrapping column, and wrapWords breaks on
// spaces without any notion of style spans. Styling "in 1h30m" as one run put
// the opening escape on one line and its reset on the next whenever the break
// fell between the two words, so the attribute bled past the row on a color
// terminal (reproduced in the real CLI at 80 columns for 6 of 16 sampled
// description lengths).
//
// Every rendered line must close whatever it opens, at every description
// length that shifts the wrap boundary across the appended time.
func TestDescriptionCellStylesSurviveWrapping(t *testing.T) {
	const base = "sweeps the fleet and reports anything unhealthy every six hours to the operator"
	next := time.Now().Add(90 * time.Minute).Format(time.RFC3339)

	for n := 1; n <= len(base); n++ {
		var buf bytes.Buffer
		tbl := NewTable(&buf, "NAME", "STATE", "ENABLED", "RESTARTS", "DESCRIPTION")
		// Force the styled path: a real TTY colors, a *bytes.Buffer does not.
		tbl.colored = true
		tbl.Row("stumpcloud-sweep", tbl.stateCell("stopped", "0 */6 * * *"),
			tbl.enabledCell(false), "0",
			tbl.descriptionCell(base[:n], "0 */6 * * *", next))
		if err := tbl.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		for i, line := range strings.Split(buf.String(), "\n") {
			// lipgloss closes with a bare ESC[m; anything else is an opener.
			resets := strings.Count(line, "\x1b[m")
			opens := strings.Count(line, "\x1b[") - resets
			if opens != resets {
				t.Fatalf("description len %d, line %d leaves %d style(s) unclosed: %q",
					n, i, opens-resets, strings.ReplaceAll(line, "\x1b", "<ESC>"))
			}
		}
	}
}
