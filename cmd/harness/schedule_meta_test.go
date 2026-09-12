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
		if got := nextRunSuffix(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestPrintHarnessTableShowsScheduleAsAField: the cadence and the countdown
// are their own columns, derived from config and the scheduler — NOT spliced
// into DESCRIPTION, and not dependent on the operator having written the
// cadence into their description text (#331).
func TestPrintHarnessTableShowsScheduleAsAField(t *testing.T) {
	var buf bytes.Buffer
	hs := []protocol.HarnessInfo{
		{
			Name: "sweep", State: "stopped",
			Schedule:    "CRON_TZ=UTC 0 10 * * *",
			NextRun:     time.Now().Add(2 * time.Hour).Format(time.RFC3339),
			Description: "checks the fleet",
		},
		{Name: "always-on", State: "running", Description: "the api"},
	}
	if err := printHarnessTable(&buf, hs); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// Columns exist, the cadence is rendered from the expression, the
	// countdown from the scheduler's stamp, and the state reads "armed".
	for _, want := range []string{"SCHEDULE", "NEXT", "daily 10:00 UTC", "in 2h", "⏱ armed", "checks the fleet"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in table:\n%s", want, out)
		}
	}
	// The unscheduled row keeps its plain state glyph and says it has no
	// schedule rather than leaving the cells ambiguous.
	if !strings.Contains(out, "● running") {
		t.Errorf("unscheduled row lost its state glyph:\n%s", out)
	}
	if !strings.Contains(out, "—") {
		t.Errorf("unscheduled row should show an em dash for schedule/next:\n%s", out)
	}
}

// TestPrintHarnessTableDescriptionIsOnlyTheDescription: the description cell
// carries the operator's words and nothing else. Before #331 the cadence and
// the countdown were appended to it, so a harness whose description happened
// not to mention its schedule showed an unhighlighted one — and dotfiles had
// to write the cadence into prose to get it on screen at all.
func TestPrintHarnessTableDescriptionIsOnlyTheDescription(t *testing.T) {
	var buf bytes.Buffer
	hs := []protocol.HarnessInfo{{
		Name: "sweep", State: "stopped",
		Schedule:    "0 */6 * * *",
		NextRun:     time.Now().Add(90 * time.Minute).Format(time.RFC3339),
		Description: "checks every StumpCloud service",
	}}
	if err := printHarnessTable(&buf, hs); err != nil {
		t.Fatal(err)
	}
	// Find the description text and confirm no schedule text trails it on the
	// same line: the columns hold that now.
	out := buf.String()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "checks every") {
			continue
		}
		if strings.Contains(line, "·") {
			t.Errorf("description cell still carries appended schedule text:\n%s", line)
		}
	}
	if !strings.Contains(out, "every 6h") || !strings.Contains(out, "in 1h30m") {
		t.Errorf("cadence and countdown missing from their columns:\n%s", out)
	}
}

// TestScheduleColumnStylesSurviveWrapping pins the style-span hazard the old
// inline next-run had, now against the styled SCHEDULE and NEXT columns:
// wrapWords breaks on spaces with no notion of style spans, so a multi-word
// cadence ("daily 10:00 UTC") styled as ONE run would put the opening escape
// on one line and its reset on the next, bleeding the attribute past the row.
// Both cells style per word for that reason.
//
// Every rendered line must close whatever it opens, at every description
// length that shifts the wrap boundaries.
func TestScheduleColumnStylesSurviveWrapping(t *testing.T) {
	const base = "sweeps the fleet and reports anything unhealthy every six hours to the operator"
	next := time.Now().Add(90 * time.Minute).Format(time.RFC3339)

	for n := 1; n <= len(base); n++ {
		var buf bytes.Buffer
		tbl := NewTable(&buf, "NAME", "STATE", "SCHEDULE", "NEXT", "RESTARTS", "DESCRIPTION")
		// Force the styled path: a real TTY colors, a *bytes.Buffer does not.
		tbl.colored = true
		tbl.Row("stumpcloud-sweep", tbl.stateCell("stopped", "CRON_TZ=UTC 0 10 * * *"),
			tbl.scheduleCell("CRON_TZ=UTC 0 10 * * *"), tbl.nextRunCell("CRON_TZ=UTC 0 10 * * *", next),
			"0", tbl.dimPlain(base[:n]))
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

// TestStateCellArmedForScheduledStopped pins what #268 asked for and #331
// reworded: a scheduled harness that is stopped reads "armed", not "stopped",
// and it is amber rather than the pink SPEC-0001 gives stopped — which sat
// next to failed's coral and read as trouble on a job that was simply waiting
// for its next firing.
func TestStateCellArmedForScheduledStopped(t *testing.T) {
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
			"scheduled + stopped is amber armed",
			"stopped", "0 */6 * * *",
			amber.Render(schedfmt.ScheduleGlyph + " armed"),
		},
		{
			// An operator really did turn this one off. Saying "armed" would
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

// TestStateCellUncoloredArmedLabel covers the mono path: color is decorative,
// so the word itself must carry the change (SPEC-0001 REQ "State
// Presentation" — legible from glyphs and text alone).
func TestStateCellUncoloredArmedLabel(t *testing.T) {
	tbl := NewTable(&bytes.Buffer{}, "NAME", "STATE")
	tbl.colored = false
	if got, want := tbl.stateCell("stopped", "0 */6 * * *"), schedfmt.ScheduleGlyph+" armed"; got != want {
		t.Errorf("stateCell = %q, want %q", got, want)
	}
	if got, want := tbl.stateCell("stopped"), core.StateStopped.Glyph()+" stopped"; got != want {
		t.Errorf("stateCell = %q, want %q", got, want)
	}
}

// describe is the third surface that renders a state, and it has the schedule
// on hand (it prints it a few rows down). Left off, a resting sweep read
// "stopped" under `harness describe` and "armed" in `harness list` — the
// divergence schedfmt was created to prevent, and the one its package doc
// calls out by name.
//
// Driven through the real verb against a live daemon, because the bug was in
// the call site, not in stateCell: passing the wrong arguments is exactly what
// a test of stateCell alone cannot see.
//
// @joestump 08/26/2026 - Found in review of #269.
func TestDescribeRendersScheduledHarnessAsArmed(t *testing.T) {
	socket := bootScheduledDaemon(t)

	out, err := captureStdout(t, func() error {
		return withClient(verbOpts{socket: socket, name: "sweeper"}, nil, cmdDescribe)
	})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !strings.Contains(out, schedfmt.ArmedLabel) {
		t.Errorf("describe does not render a resting scheduled harness as %q "+
			"— `harness list` calls it that, so the two surfaces disagree "+
			"about one harness:\n%s", schedfmt.ArmedLabel, out)
	}
	// And it reports whether the schedule is armed rather than an `enabled`
	// that is false for every cron job by construction (#331).
	if strings.Contains(out, "enabled") {
		t.Errorf("describe still reports enabled for a scheduled harness:\n%s", out)
	}
	if strings.Contains(out, "state") && strings.Contains(out, " stopped") {
		t.Errorf("describe still says \"stopped\" for a scheduled harness:\n%s", out)
	}
}

// bootScheduledDaemon is bootTestDaemon with a cron one-shot in the config,
// which is the shape TestDescribeRendersScheduledHarnessAsArmed needs and
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
