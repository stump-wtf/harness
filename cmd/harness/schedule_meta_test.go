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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/daemon"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/schedfmt"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
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
	// time in DESCRIPTION; no SCHEDULE/NEXT columns exist anymore. It reads
	// "idle" rather than "stopped" — a cron job between firings is armed, not
	// switched off (#268).
	for _, want := range []string{"⏱ idle", "in 2h"} {
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

// TestStateCellIdleForScheduledStopped pins the two things #268 asked for: a
// scheduled harness that is stopped reads "idle", not "stopped", and it is
// amber rather than the pink SPEC-0001 gives stopped — which sat next to
// failed's coral and read as trouble on a job that was simply waiting for its
// next firing.
func TestStateCellIdleForScheduledStopped(t *testing.T) {
	tbl := NewTable(&bytes.Buffer{}, "NAME", "STATE")
	tbl.colored = true

	amber := lipgloss.NewStyle().Foreground(tbl.pal.Amber).Bold(true)
	pink := lipgloss.NewStyle().Foreground(tbl.pal.Pink).Bold(true)
	coral := lipgloss.NewStyle().Foreground(tbl.pal.Coral).Bold(true)

	tests := []struct {
		name     string
		state    string
		schedule string
		want     string
	}{
		{
			"scheduled + stopped is amber idle",
			"stopped", "0 */6 * * *",
			amber.Render(schedfmt.ScheduleGlyph + " idle"),
		},
		{
			// An operator really did turn this one off. Saying "idle" would
			// hide that, so an unscheduled harness is untouched.
			"unscheduled + stopped keeps pink stopped",
			"stopped", "",
			pink.Render(core.StateStopped.Glyph() + " stopped"),
		},
		{
			// A scheduled run that failed genuinely failed — it keeps coral.
			"scheduled + failed keeps coral failed",
			"failed", "0 */6 * * *",
			coral.Render(schedfmt.ScheduleGlyph + " failed"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tbl.stateCell(tc.state, tc.schedule); got != tc.want {
				t.Errorf("stateCell(%q, %q)\n got %q\nwant %q",
					tc.state, tc.schedule,
					strings.ReplaceAll(got, "\x1b", "<ESC>"),
					strings.ReplaceAll(tc.want, "\x1b", "<ESC>"))
			}
		})
	}
}

// TestStateCellUncoloredIdleLabel covers the mono path: color is decorative,
// so the word itself must carry the change (SPEC-0001 REQ "State
// Presentation" — legible from glyphs and text alone).
func TestStateCellUncoloredIdleLabel(t *testing.T) {
	tbl := NewTable(&bytes.Buffer{}, "NAME", "STATE")
	tbl.colored = false
	if got, want := tbl.stateCell("stopped", "0 */6 * * *"), schedfmt.ScheduleGlyph+" idle"; got != want {
		t.Errorf("stateCell = %q, want %q", got, want)
	}
	if got, want := tbl.stateCell("stopped"), core.StateStopped.Glyph()+" stopped"; got != want {
		t.Errorf("stateCell = %q, want %q", got, want)
	}
}

// describe is the third surface that renders a state, and it has the schedule
// on hand (it prints it a few rows down). Left off, a resting sweep read
// "stopped" under `harness describe` and "idle" in `harness list` — the
// divergence schedfmt was created to prevent, and the one its package doc
// calls out by name.
//
// Driven through the real verb against a live daemon, because the bug was in
// the call site, not in stateCell: passing the wrong arguments is exactly what
// a test of stateCell alone cannot see.
//
// @joestump 08/26/2026 - Found in review of #269.
func TestDescribeRendersScheduledHarnessAsIdle(t *testing.T) {
	socket := bootScheduledDaemon(t)

	out, err := captureStdout(t, func() error {
		return withClient(verbOpts{socket: socket, name: "sweeper"}, nil, cmdDescribe)
	})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !strings.Contains(out, schedfmt.IdleLabel) {
		t.Errorf("describe does not render a resting scheduled harness as %q "+
			"— `harness list` calls it that, so the two surfaces disagree "+
			"about one harness:\n%s", schedfmt.IdleLabel, out)
	}
	if strings.Contains(out, "state") && strings.Contains(out, " stopped") {
		t.Errorf("describe still says \"stopped\" for a scheduled harness:\n%s", out)
	}
}

// bootScheduledDaemon is bootTestDaemon with a cron one-shot in the config,
// which is the shape TestDescribeRendersScheduledHarnessAsIdle needs and
// writeMinimalConfig does not provide.
func bootScheduledDaemon(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "harness.toml")
	if err := os.WriteFile(configPath, []byte(
		"[harness.sweeper]\nharness = \"claude-code\"\nprompt = \"sweep the queue\"\nschedule = \"0 */6 * * *\"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shortSockDir(t), "d.sock")

	reg := attach.NewRegistry(1000)
	mgr := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		StatePath:   filepath.Join(tmp, "state.json"),
		LogDir:      filepath.Join(tmp, "logs"),
		ExtraOutFor: reg.WriterFor,
	})
	reg.SetController(mgr)

	srv := daemon.NewServer(daemon.Options{
		Manager:    mgr,
		Registry:   reg,
		SocketPath: socket,
		ConfigPath: configPath,
		Version:    buildinfo.Version,
	})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		srv.Close()
		mgr.Close()
	})
	return socket
}
