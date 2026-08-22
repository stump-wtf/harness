package main

// Governing: ADR-0001 (Charmbracelet stack owns the visual language) and
// SPEC-0003 (glyph + adaptive color — legible even when the test terminal
// strips every color). Exercises the animated --all model headlessly: the
// op-result sequence a real run produces, the failure path, and the
// ctrl-c interrupt.

import (
	"bytes"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

func TestHarnessNames(t *testing.T) {
	hs := []protocol.HarnessInfo{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got := harnessNames(hs)
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("harnessNames = %v", got)
	}
}

func TestLifecycleModelCompletes(t *testing.T) {
	m := newLifecycleModel("start", nil, []string{"alpha", "beta"})
	_ = m.Init()

	var cmd tea.Cmd
	mi, cmd := m.Update(opDoneMsg{idx: 0, info: protocol.HarnessInfo{State: "running"}})
	m = mi.(*lifecycleModel)
	if cmd == nil {
		t.Fatal("completion of a row must schedule the next op + progress")
	}
	if m.idx != 1 || !m.rows[1].running {
		t.Fatalf("expected row 1 active after first op, idx=%d", m.idx)
	}
	mi, _ = m.Update(opDoneMsg{idx: 1, info: protocol.HarnessInfo{State: "running"}})
	m = mi.(*lifecycleModel)

	if !m.quitting {
		t.Fatal("last op must quit the program")
	}
	if len(m.errs) != 0 {
		t.Fatalf("unexpected errors: %v", m.errs)
	}
	out := m.finalView()
	for _, want := range []string{"✓ alpha", "✓ beta", "2 harnesses started"} {
		if !strings.Contains(out, want) {
			t.Errorf("final view missing %q:\n%s", want, out)
		}
	}
}

func TestLifecycleModelFailureCollected(t *testing.T) {
	m := newLifecycleModel("stop", nil, []string{"alpha", "beta"})
	mi, _ := m.Update(opDoneMsg{idx: 0, err: errBoom})
	m = mi.(*lifecycleModel)
	mi, _ = m.Update(opDoneMsg{idx: 1, info: protocol.HarnessInfo{State: "stopped"}})
	m = mi.(*lifecycleModel)

	if len(m.errs) != 1 || !strings.Contains(m.errs[0], "alpha") {
		t.Fatalf("errs = %v, want one alpha failure", m.errs)
	}
	out := m.finalView()
	if !strings.Contains(out, "✗ alpha") || !strings.Contains(out, "boom") {
		t.Errorf("final view must show the failed row:\n%s", out)
	}
	if !strings.Contains(out, "1 failed") {
		t.Errorf("final view must tally failures:\n%s", out)
	}
}

func TestLifecycleModelInterrupt(t *testing.T) {
	m := newLifecycleModel("start", nil, []string{"alpha", "beta"})
	m2, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("ctrl-c must quit")
	}
	if !m2.(*lifecycleModel).quitting {
		t.Fatal("quit key must set quitting")
	}
	if !strings.Contains(m2.(*lifecycleModel).finalView(), "interrupted") {
		t.Errorf("interrupted run must say so:\n%s", m2.(*lifecycleModel).finalView())
	}
}

func TestVerbForms(t *testing.T) {
	cases := map[string][2]string{
		"start":   {"Starting", "started"},
		"stop":    {"Stopping", "stopped"},
		"restart": {"Restarting", "restarted"},
	}
	for verb, want := range cases {
		if got := verbGerund(verb); got != want[0] {
			t.Errorf("verbGerund(%q) = %q", verb, got)
		}
		if got := verbPast(verb); got != want[1] {
			t.Errorf("verbPast(%q) = %q", verb, got)
		}
	}
	if got := firstLine("a\nb\nc"); got != "a" {
		t.Errorf("firstLine = %q", got)
	}
}

// errBoom is a sentinel for the failure-path tests.
var errBoom = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }

// TestLifecycleResultExitStatus pins the verdict the caller turns into an exit
// code. The interrupt case is the one that was wrong: aborting collects no
// per-harness error, so reporting only errs exited 0 and told a script the
// whole fleet had been acted on.
func TestLifecycleResultExitStatus(t *testing.T) {
	complete := newLifecycleModel("start", nil, []string{"alpha", "beta"})
	mi, _ := complete.Update(opDoneMsg{idx: 0, info: protocol.HarnessInfo{State: "running"}})
	mi, _ = mi.(*lifecycleModel).Update(opDoneMsg{idx: 1, info: protocol.HarnessInfo{State: "running"}})
	if err := mi.(*lifecycleModel).result(); err != nil {
		t.Errorf("a clean run must succeed, got %v", err)
	}

	interrupted := newLifecycleModel("stop", nil, []string{"alpha", "beta", "gamma"})
	mi, _ = interrupted.Update(opDoneMsg{idx: 0, info: protocol.HarnessInfo{State: "stopped"}})
	mi, _ = mi.(*lifecycleModel).Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	err := mi.(*lifecycleModel).result()
	if err == nil {
		t.Fatal("an interrupted run must not report success")
	}
	if !strings.Contains(err.Error(), "interrupted after 1 of 3") {
		t.Errorf("interrupt error = %q, want the count it got through", err)
	}

	// A failure underneath an interrupt survives it.
	both := newLifecycleModel("stop", nil, []string{"alpha", "beta"})
	mi, _ = both.Update(opDoneMsg{idx: 0, err: errBoom})
	mi, _ = mi.(*lifecycleModel).Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	err = mi.(*lifecycleModel).result()
	if err == nil || !strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("result = %v, want both the interrupt and the failure", err)
	}
}

// TestLifecycleViewEmptyWhenQuitting pins the fix for the duplicated summary
// block. Bubble Tea leaves its last rendered frame in the terminal, and the
// caller prints finalView underneath it — so the moment View renders the
// summary while quitting, the user sees it twice. The earlier fix for this
// cleared the whole screen instead, which erased the user's prior output.
func TestLifecycleViewEmptyWhenQuitting(t *testing.T) {
	m := newLifecycleModel("start", nil, []string{"alpha"})
	if got := m.View().Content; got == "" {
		t.Fatal("a live run must render a frame")
	}
	mi, _ := m.Update(opDoneMsg{idx: 0, info: protocol.HarnessInfo{State: "running"}})
	m = mi.(*lifecycleModel)
	if !m.quitting {
		t.Fatal("last op must set quitting")
	}
	if got := m.View().Content; got != "" {
		t.Fatalf("final frame must be empty or it duplicates finalView, got %q", got)
	}
	if out := m.finalView(); !strings.Contains(out, "1 harnesses started") {
		t.Errorf("finalView is the permanent record:\n%s", out)
	}
}

// TestLifecycleAnimatedNeverClearsScreen guards the regression the screen-clear
// fix introduced: no code path in the animated run may emit a full-screen erase,
// which would take the user's earlier terminal output with it.
func TestLifecycleAnimatedNeverClearsScreen(t *testing.T) {
	src, err := os.ReadFile("lifecycle_tui.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`\033[2J`, `\x1b[2J`, "ansi.EraseEntireScreen"} {
		if bytes.Contains(src, []byte(bad)) {
			t.Errorf("animated lifecycle must not emit a full-screen erase (%s)", bad)
		}
	}
}

// TestClearStaleFrameEmitsCleanup verifies that clearStaleFrame writes the
// exact sequence that wipes leaked escape characters from Bubble Tea's close()
// without clearing the user's screen. The sequence must contain:
//   - SGR reset (ESC[0m) to clear any active styling
//   - carriage return (\r) to move to column 0
//   - erase to end of line (ESC[K) to remove leaked literal characters
//
// @joestump-agent 08/22/2026 - Regression test for the "weird characters with
// semicolons" bug. Bubble Tea v2's close() writes KittyKeyboard(0,1) →
// ESC[=0;1u which leaks as literal text on terminals without Kitty protocol
// support. clearStaleFrame cleans it up.
func TestClearStaleFrameEmitsCleanup(t *testing.T) {
	// Capture stdout to verify the exact bytes written.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	clearStaleFrame()
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	got := buf.String()

	// The cleanup must reset SGR, return to column 0, and erase the line.
	want := "\x1b[0m\r\x1b[K"
	if got != want {
		t.Errorf("clearStaleFrame = %q, want %q", got, want)
	}
}

// TestFinalViewNoRawSemicolonEscapes verifies that finalView() output does not
// contain raw CSI sequences with semicolons that would appear as literal
// "weird characters" on a terminal that doesn't support them. The Kitty
// keyboard reset (ESC[=0;1u) is the specific sequence that triggered this bug.
//
// finalView uses lipgloss for styling, which produces SGR sequences like
// ESC[0;39m — those are fine because they're complete and the terminal
// handles them. The bug is about incomplete or unrecognized sequences from
// Bubble Tea's close(), not from our own styled output.
func TestFinalViewNoRawSemicolonEscapes(t *testing.T) {
	m := newLifecycleModel("start", nil, []string{"alpha", "beta"})
	mi, _ := m.Update(opDoneMsg{idx: 0, info: protocol.HarnessInfo{State: "running"}})
	mi, _ = mi.(*lifecycleModel).Update(opDoneMsg{idx: 1, info: protocol.HarnessInfo{State: "running"}})
	m = mi.(*lifecycleModel)

	out := m.finalView()

	// The Kitty keyboard reset sequence that leaks as literal text.
	kittyReset := "\x1b[=0;1u"
	if strings.Contains(out, kittyReset) {
		t.Errorf("finalView must not contain Kitty keyboard reset %q", kittyReset)
	}

	// No raw ESC[= sequences (Kitty protocol) should appear in our output.
	if strings.Contains(out, "\x1b[=") {
		t.Errorf("finalView must not contain raw Kitty protocol sequences")
	}
}

// TestClearStaleFrameIsCalledBeforeFinalView verifies that the source code
// calls clearStaleFrame before printing finalView, so leaked characters are
// wiped before the permanent record is written.
func TestClearStaleFrameIsCalledBeforeFinalView(t *testing.T) {
	src, err := os.ReadFile("lifecycle_tui.go")
	if err != nil {
		t.Fatal(err)
	}
	clearIdx := bytes.Index(src, []byte("clearStaleFrame()"))
	finalIdx := bytes.Index(src, []byte("m.finalView()"))
	if clearIdx < 0 {
		t.Fatal("clearStaleFrame() must be called in runLifecycleAnimated")
	}
	if finalIdx < 0 {
		t.Fatal("m.finalView() must be called in runLifecycleAnimated")
	}
	if clearIdx >= finalIdx {
		t.Error("clearStaleFrame() must appear before m.finalView() in the source")
	}
}
