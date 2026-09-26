package main

// Governing: ADR-0001 (Charmbracelet stack owns the visual language),
// SPEC-0003 REQ "State Model" (glyph + state text carry the meaning; colour
// decorates). Pins both halves of the one-shot verb contract: the plain
// non-TTY lines are byte-for-byte what they were, and the styled records carry
// glyph, name and state — in the state's own colour on a colour terminal, and
// still legible with every colour stripped.

import (
	"bytes"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/tui/theme"
)

// colorStyle pins a truecolor dark theme so the SGR assertions do not depend
// on the terminal running the tests.
func colorStyle() lifecycleStyle {
	return newLifecycleStyleFor(theme.New(colorprofile.TrueColor, true, theme.DefaultPalette()))
}

// monoStyle is a terminal with no colour at all (NO_COLOR, or a dumb term).
func monoStyle() lifecycleStyle {
	return newLifecycleStyleFor(theme.New(colorprofile.Ascii, true, theme.DefaultPalette()))
}

// TestPlainLifecycleLineContract pins the non-TTY line scripts parse. It is
// the exact format main.go printed before the styled path existed.
func TestPlainLifecycleLineContract(t *testing.T) {
	for _, tc := range []struct {
		info protocol.HarnessInfo
		want string
	}{
		{protocol.HarnessInfo{Name: "claude-rc", State: "running", PID: 42, Description: "x"}, "● claude-rc → running\n"},
		{protocol.HarnessInfo{Name: "a/b", State: "stopped"}, "○ a/b → stopped\n"},
		{protocol.HarnessInfo{Name: "w", State: "failed", RestartCount: 3}, "✖ w → failed\n"},
		{protocol.HarnessInfo{Name: "s", State: "starting"}, "◌ s → starting\n"},
	} {
		if got := plainLifecycleLine(tc.info); got != tc.want {
			t.Errorf("plainLifecycleLine(%+v) = %q, want %q", tc.info, got, tc.want)
		}
	}
}

// TestEmitNonTTYIsPlain: a writer that is not a terminal gets the plain
// string verbatim and the styled renderer is never consulted. A bytes.Buffer
// stands in for a pipe — cliui.WriterIsTTY is false for any non-*os.File.
func TestEmitNonTTYIsPlain(t *testing.T) {
	var buf bytes.Buffer
	called := false
	emit(&buf, "reloaded — 3 harnesses\n", func(lifecycleStyle) string {
		called = true
		return "STYLED"
	})
	if called {
		t.Error("styled renderer ran for a non-TTY writer")
	}
	if got := buf.String(); got != "reloaded — 3 harnesses\n" {
		t.Errorf("non-TTY output = %q, want the plain line byte for byte", got)
	}
}

func TestRenderOutcomeTransition(t *testing.T) {
	s := colorStyle()
	out := s.renderOutcome(lifecycleOutcome{
		action: "restarted",
		before: "failed",
		info: protocol.HarnessInfo{
			Name: "claude-rc", State: "running", PID: 48211, RestartCount: 3,
			Description: "remote-control claude",
		},
	})
	plain := ansi.Strip(out)
	for _, want := range []string{
		"● claude-rc  failed → running\n",
		"  restarted · pid 48211 · 3 restarts · remote-control claude\n",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("record missing %q:\n%s", want, plain)
		}
	}
	// Each state wears its own SPEC-0001 colour: the glyph and "running" in
	// mint, the "from" state in coral.
	th := s.th
	if !strings.Contains(out, th.StateStyle(core.StateRunning).Render("●")) {
		t.Errorf("glyph is not in the running colour:\n%q", out)
	}
	if !strings.Contains(out, th.StateStyle(core.StateFailed).Render("failed")) {
		t.Errorf("before-state is not in the failed colour:\n%q", out)
	}
	if strings.Contains(plain, "harness logs") {
		t.Errorf("a running result must not carry the logs hint:\n%s", plain)
	}
}

// TestRenderOutcomeTroubleHint: a failed or degraded result points at the
// logs, in the failure (or warning) colour.
func TestRenderOutcomeTroubleHint(t *testing.T) {
	s := colorStyle()
	for _, tc := range []struct {
		state string
		style func() string
	}{
		{"failed", func() string { return s.failed.Render("→ see why: harness logs w") }},
		{"degraded", func() string { return s.warn.Render("→ see why: harness logs w") }},
	} {
		out := s.renderOutcome(lifecycleOutcome{action: "started", info: protocol.HarnessInfo{Name: "w", State: tc.state}})
		if !strings.Contains(out, tc.style()) {
			t.Errorf("%s: hint missing or wrong colour:\n%q", tc.state, out)
		}
		glyph := core.State(tc.state).Glyph()
		if !strings.HasPrefix(ansi.Strip(out), glyph+" w  "+tc.state+"\n") {
			t.Errorf("%s: headline wrong:\n%s", tc.state, ansi.Strip(out))
		}
	}
}

// TestRenderOutcomeFlappingAndLease covers the optional context facts, and
// that a result with no before-state renders a bare state, not "→ running".
func TestRenderOutcomeFlappingAndLease(t *testing.T) {
	out := ansi.Strip(colorStyle().renderOutcome(lifecycleOutcome{
		action: "started",
		info:   protocol.HarnessInfo{Name: "n", State: "running", Flapping: true, LeaseUntil: "2026-09-26T18:00:00Z", RestartCount: 1},
	}))
	if !strings.HasPrefix(out, "● n  running\n") {
		t.Errorf("headline without a before-state = %q", out)
	}
	if !strings.Contains(out, "started · 1 restart · lease until 2026-09-26T18:00:00Z · flapping") {
		t.Errorf("context line wrong:\n%s", out)
	}
}

// TestRenderOutcomeMonoLegible: with every colour stripped (NO_COLOR, a
// monochrome terminal) the record still says everything — glyph, name,
// transition — and carries no colour SGR at all (SPEC-0001 REQ "State
// Presentation").
func TestRenderOutcomeMonoLegible(t *testing.T) {
	out := monoStyle().renderOutcome(lifecycleOutcome{
		action: "stopped", before: "running",
		info: protocol.HarnessInfo{Name: "api", State: "stopped"},
	})
	if strings.Contains(out, "38;") {
		t.Errorf("mono profile emitted a foreground colour:\n%q", out)
	}
	if !strings.Contains(ansi.Strip(out), "○ api  running → stopped") {
		t.Errorf("mono record not legible:\n%s", ansi.Strip(out))
	}
}

// TestSingleLifecycleModel walks the one-row program: a spinner row naming
// the harness and the verb while the op is out, an empty final frame once it
// answers (the record is written after exit — a non-empty frame would print
// twice), and the result carried out of the model.
func TestSingleLifecycleModel(t *testing.T) {
	calls := 0
	op := func() (protocol.HarnessInfo, error) {
		calls++
		return protocol.HarnessInfo{Name: "claude-rc", State: "running"}, nil
	}
	m := newSingleLifecycleModel("restart", "claude-rc", "failed", op, colorStyle())
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init must start the spinner and the op")
	}
	live := ansi.Strip(m.View().Content)
	for _, want := range []string{"claude-rc", "restarting…", "was failed"} {
		if !strings.Contains(live, want) {
			t.Errorf("live row missing %q: %q", want, live)
		}
	}

	info, err := op()
	mi, cmd := m.Update(singleDoneMsg{info: info, err: err})
	m = mi.(*singleLifecycleModel)
	if cmd == nil || !m.done {
		t.Fatal("the op's answer must end the program")
	}
	if m.View().Content != "" {
		t.Errorf("final frame must be empty, got %q", m.View().Content)
	}
	if m.info.State != "running" || calls != 1 {
		t.Errorf("result not carried: %+v (calls=%d)", m.info, calls)
	}
}

func TestSingleLifecycleModelInterrupt(t *testing.T) {
	m := newSingleLifecycleModel("stop", "api", "", func() (protocol.HarnessInfo, error) {
		return protocol.HarnessInfo{}, nil
	}, colorStyle())
	mi, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m = mi.(*singleLifecycleModel)
	if cmd == nil || !m.interrupted || m.done {
		t.Fatalf("ctrl-c must quit as interrupted, not done: %+v", m)
	}
	if strings.Contains(ansi.Strip(m.content()), "was") {
		t.Errorf("no before-state must mean no 'was' clause: %q", ansi.Strip(m.content()))
	}
}

// TestStyledOneLiners covers every static styled record: each keeps the
// facts its plain line states, so the two renderings never disagree.
func TestStyledOneLiners(t *testing.T) {
	s := colorStyle()
	run7 := &protocol.RunInfo{RunID: 7}
	exit3 := 3
	for _, tc := range []struct {
		name string
		got  string
		want []string
	}{
		{"use-profile", s.renderUseProfile("work", []protocol.ProfileInfo{
			{Name: "home", Harnesses: []string{"x"}},
			{Name: "work", Harnesses: []string{"a", "b"}, Autostart: true, Description: "weekday agents", Active: true},
		}), []string{"✓ profile work active  2 harnesses · autostart · weekday agents"}},
		{"reload", s.renderReload([]protocol.HarnessInfo{
			{State: "running"}, {State: "running"}, {State: "stopped"}, {State: "failed"},
		}), []string{"✓ reloaded  4 harnesses", "○ 1 stopped  ● 2 running  ✖ 1 failed"}},
		{"down", s.renderDown(protocol.ProjectDownData{Project: "api", Removed: []string{"api/web", "api/worker"}}),
			[]string{"✓ project api down  2 harnesses stopped and deregistered", "○ api/web", "○ api/worker"}},
		{"rm", s.renderRemove(protocol.RemoveData{Name: "api/web", Project: "api"}),
			[]string{"✓ removed api/web  project api"}},
		{"run", s.renderOutcome(lifecycleOutcome{action: "scratchpad started", info: protocol.HarnessInfo{Name: "claude fix-tests", State: "running", PID: 9}}),
			[]string{"● claude fix-tests  running", "scratchpad started · pid 9"}},
		{"trigger started", s.renderTrigger(protocol.TriggerData{Name: "sweep", Decision: protocol.TriggerStarted, Run: run7}),
			[]string{"✓ sweep  started run #7"}},
		{"trigger queued", s.renderTrigger(protocol.TriggerData{Name: "sweep", Decision: protocol.TriggerQueued}),
			[]string{"◌ sweep  queued  a run is in flight"}},
		{"trigger skipped", s.renderTrigger(protocol.TriggerData{Name: "sweep", Decision: protocol.TriggerSkipped, Run: run7}),
			[]string{"⚠ sweep  skipped  a run is in flight · recorded as run #7"}},
		{"finish ok", s.renderFinish("sweep", protocol.RunInfo{RunID: 7, Outcome: "success", DurationMs: 3000}),
			[]string{"✓ sweep  run #7 success after 3s"}},
		{"finish failed", s.renderFinish("sweep", protocol.RunInfo{RunID: 7, Outcome: "failed", ExitCode: &exit3}),
			[]string{"✗ sweep  run #7 failed (exit 3)"}},
		{"daemon stop", s.renderDaemonStopping(4242), []string{"◌ daemon stopping  pid 4242"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.got, "\x1b[") {
				t.Errorf("styled record carries no styling at all: %q", tc.got)
			}
			plain := ansi.Strip(tc.got)
			for _, w := range tc.want {
				if !strings.Contains(plain, w) {
					t.Errorf("missing %q in:\n%s", w, plain)
				}
			}
			if !strings.HasSuffix(plain, "\n") {
				t.Errorf("record must end in a newline: %q", plain)
			}
		})
	}
}

// TestRenderFinishFailureColour: a run that did not succeed is marked in the
// failure colour, not the success one.
func TestRenderFinishFailureColour(t *testing.T) {
	s := colorStyle()
	out := s.renderFinish("sweep", protocol.RunInfo{RunID: 1, Outcome: "timed_out"})
	if !strings.Contains(out, s.failed.Render("timed_out")) {
		t.Errorf("non-success outcome not in the failure colour: %q", out)
	}
	if strings.Contains(out, s.done.Render("✓")) {
		t.Errorf("non-success outcome carries the success mark: %q", out)
	}
}
