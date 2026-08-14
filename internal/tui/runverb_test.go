package tui

// Governing: SPEC-0001 REQ "Command Palette" — the palette mirrors the CLI 1:1.
//
// TestPaletteMirrorsCLI already asserts the palette *offers* the same verbs the
// CLI does. What it cannot see is whether each verb actually *does* the right
// thing: runVerb is the single dispatch point behind every palette entry, and
// only `restart` had a test. A verb wired to the wrong action — or silently
// falling through the switch — would keep the palette looking correct while
// doing nothing, or worse, doing something else to the named harness.
//
// These tests drive each branch and assert the observable effect on the fake
// controller, so "the palette lists it" and "the palette performs it" are both
// pinned.

import (
	"testing"
)

// verbFixture returns a dashboard model plus the fake controller behind it, with
// profile filtering off so every sample harness is addressable by name.
func verbFixture() (*Model, *fakeController) {
	fc := &fakeController{harnesses: sampleHarnesses(), profiles: sampleProfiles()}
	m := New(Options{})
	m.ctrl, m.attach = fc, &fakeAttach{}
	m.conn = startOK
	m.harnesses = fc.harnesses
	m.profiles = fc.profiles
	m.w, m.h = 120, 40
	m.help.SetWidth(120)
	m.showAll = true
	return m, fc
}

// TestRunVerbLifecycleActions covers start/stop/restart in one table. Each must
// reach the daemon with the *named* target — a branch that dropped the target
// and acted on the current selection would be invisible to a palette-listing
// test and catastrophic in use.
func TestRunVerbLifecycleActions(t *testing.T) {
	tests := []struct {
		verb   string
		target string
		got    func(*fakeController) []string
	}{
		{"start", "backup-watch", func(fc *fakeController) []string { return fc.startCalls }},
		{"stop", "crush-signal", func(fc *fakeController) []string { return fc.stopCalls }},
		{"restart", "claude-src", func(fc *fakeController) []string { return fc.rstCalls }},
	}
	for _, tt := range tests {
		t.Run(tt.verb, func(t *testing.T) {
			m, fc := verbFixture()
			_, cmd := m.runVerb(tt.verb, tt.target)
			if cmd == nil {
				t.Fatalf("%s produced no command", tt.verb)
			}
			drain(cmd)
			calls := tt.got(fc)
			if len(calls) != 1 {
				t.Fatalf("%s made %d calls, want exactly 1 (%v)", tt.verb, len(calls), calls)
			}
			if calls[0] != tt.target {
				t.Errorf("%s acted on %q, want the named target %q", tt.verb, calls[0], tt.target)
			}
		})
	}
}

// TestRunVerbBypassesConfirmGuard pins a deliberate asymmetry. Pressing `x` on
// the dashboard opens a confirm overlay (destructive-action guard), but running
// `stop` from the palette is already an explicit, typed-out instruction — so it
// executes immediately. If the guard leaked into the palette, every palette
// stop would need a second keystroke nobody expects.
func TestRunVerbBypassesConfirmGuard(t *testing.T) {
	m, fc := verbFixture()
	_, cmd := m.runVerb("stop", "crush-signal")
	if m.overlay == overlayConfirm {
		t.Error("palette stop opened the confirm overlay; palette execution is explicit and should not re-ask")
	}
	drain(cmd)
	if len(fc.stopCalls) != 1 {
		t.Errorf("palette stop did not reach the daemon: %v", fc.stopCalls)
	}
}

// TestRunVerbAttachOpensSession pins that `attach <name>` actually opens an
// attach for that harness rather than merely moving the selection.
func TestRunVerbAttachOpensSession(t *testing.T) {
	m, _ := verbFixture()
	fa, ok := m.attach.(*fakeAttach)
	if !ok {
		t.Fatal("fixture attach is not a fakeAttach")
	}
	_, cmd := m.runVerb("attach", "claude-src")
	drain(cmd)
	if len(fa.opens) == 0 {
		t.Fatal("attach verb opened no session")
	}
	if m.att == nil || m.att.name != "claude-src" {
		t.Errorf("attached to %v, want claude-src", m.att)
	}
}

// TestRunVerbAttachUnknownTargetIsSafe pins the nil guard. A palette entry can
// name a harness that vanished between listing and Enter; that must be a no-op,
// not a panic.
func TestRunVerbAttachUnknownTargetIsSafe(t *testing.T) {
	m, _ := verbFixture()
	_, _ = m.runVerb("attach", "no-such-harness")
	if m.att != nil {
		t.Error("attached to a harness that does not exist")
	}
}

// TestRunVerbSelectionVerbsMoveCursor covers describe/logs/edit: they retarget
// the dashboard selection onto the named harness. `edit` additionally opens the
// form — editing the wrong harness's config is a data-loss-shaped mistake, so
// the selection move is the part that matters.
func TestRunVerbSelectionVerbsMoveCursor(t *testing.T) {
	for _, verb := range []string{"describe", "logs", "edit"} {
		t.Run(verb, func(t *testing.T) {
			m, _ := verbFixture()
			target := "backup-watch"
			m.sel = 0
			if got, _ := m.selectedHarness(); got.Name == target {
				t.Fatal("fixture already had the target selected; the test would prove nothing")
			}
			_, _ = m.runVerb(verb, target)
			got, ok := m.selectedHarness()
			if !ok {
				t.Fatal("no selection after the verb")
			}
			if got.Name != target {
				t.Errorf("%s left the selection on %q, want %q", verb, got.Name, target)
			}
		})
	}
}

// TestRunVerbEditOpensForm pins the overlay half of `edit`.
func TestRunVerbEditOpensForm(t *testing.T) {
	m, _ := verbFixture()
	_, _ = m.runVerb("edit", "backup-watch")
	if m.overlay != overlayForm {
		t.Errorf("edit left overlay = %v, want the form overlay", m.overlay)
	}
	if !m.editing {
		t.Error("edit did not put the form in editing mode; it would create a duplicate harness instead of editing")
	}
}

// TestRunVerbNewOpensBlankForm pins that `new` opens the form WITHOUT editing
// mode — the distinction decides whether saving appends a harness or rewrites
// an existing one.
func TestRunVerbNewOpensBlankForm(t *testing.T) {
	m, _ := verbFixture()
	_, _ = m.runVerb("new", "")
	if m.overlay != overlayForm {
		t.Errorf("new left overlay = %v, want the form overlay", m.overlay)
	}
	if m.editing {
		t.Error("new opened the form in editing mode; saving would overwrite an existing harness")
	}
}

// TestRunVerbProfileSwitches pins `profile <name>` reaching UseProfile.
func TestRunVerbProfileSwitches(t *testing.T) {
	m, fc := verbFixture()
	_, cmd := m.runVerb("profile", "reduit")
	if cmd == nil {
		t.Fatal("profile verb produced no command")
	}
	drain(cmd)
	if fc.useProfile != "reduit" {
		t.Errorf("UseProfile called with %q, want %q", fc.useProfile, "reduit")
	}
}

// TestRunVerbProfileWithoutTargetIsNoop pins the empty-target guard: switching
// to "" would be a meaningless daemon call.
func TestRunVerbProfileWithoutTargetIsNoop(t *testing.T) {
	m, fc := verbFixture()
	_, cmd := m.runVerb("profile", "")
	if cmd != nil {
		drain(cmd)
	}
	if fc.useProfile != "" {
		t.Errorf("empty profile target still called UseProfile(%q)", fc.useProfile)
	}
}

// TestRunVerbReadOnlyVerbsRefresh covers list/profiles/daemon-info, which all
// collapse to "go re-read state". They must still issue that fetch — a palette
// entry that does nothing at all is worse than one that is missing.
func TestRunVerbReadOnlyVerbsRefresh(t *testing.T) {
	for _, verb := range []string{"list", "profiles", "daemon-info"} {
		t.Run(verb, func(t *testing.T) {
			m, _ := verbFixture()
			_, cmd := m.runVerb(verb, "")
			if cmd == nil {
				t.Fatalf("%s produced no command; the palette entry is inert", verb)
			}
			if msg := drain(cmd); msg == nil {
				t.Errorf("%s produced no message", verb)
			}
		})
	}
}

// TestRunVerbReloadIssuesReload pins `reload`.
func TestRunVerbReloadIssuesReload(t *testing.T) {
	m, _ := verbFixture()
	_, cmd := m.runVerb("reload", "")
	if cmd == nil {
		t.Fatal("reload produced no command")
	}
	if _, ok := drain(cmd).(reloadResultMsg); !ok {
		t.Error("reload did not produce a reloadResultMsg")
	}
}

// TestRunVerbWithoutControllerIsSafe pins every controller-dependent branch
// against a nil ctrl. The model runs with ctrl == nil whenever it is
// disconnected (onDisconnect clears it), and the palette is still reachable —
// so each of these must no-op rather than panic.
func TestRunVerbWithoutControllerIsSafe(t *testing.T) {
	for _, verb := range []string{"profile", "reload", "list", "profiles", "daemon-info"} {
		t.Run(verb, func(t *testing.T) {
			m, _ := verbFixture()
			m.ctrl = nil
			// Must not panic.
			_, _ = m.runVerb(verb, "reduit")
		})
	}
}

// TestRunVerbUnknownIsNoop pins the switch's default: an unrecognized verb
// changes nothing and issues nothing.
func TestRunVerbUnknownIsNoop(t *testing.T) {
	m, fc := verbFixture()
	before := m.overlay
	_, cmd := m.runVerb("frobnicate", "crush-signal")
	if cmd != nil {
		drain(cmd)
	}
	if m.overlay != before {
		t.Errorf("unknown verb changed the overlay to %v", m.overlay)
	}
	if len(fc.startCalls)+len(fc.stopCalls)+len(fc.rstCalls) != 0 {
		t.Error("unknown verb reached the daemon")
	}
}
