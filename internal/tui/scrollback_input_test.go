package tui

// Governing: SPEC-0001 REQ "Scrollback Substate" — regression coverage for the
// input-lifecycle journey reported in stump-wtf/harness#7: enter scrollback by
// BOTH entry paths (wheel-up and the documented Ctrl-b [ prefix chord), make
// sure the frozen substate owns navigation keys without leaking them to the
// PTY, exit back to live, and prove the first keystroke after exit reaches the
// PTY with the mouse grab re-asserted. The report's failure signature was
// "input permanently dead after scrolling until the session is killed" — every
// property asserted here is one link in the chain that prevents that: no stuck
// prefix after either entry path, no swallowed key after exit, no lost mouse
// grab in or out of the substate.

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stump-wtf/harness/internal/protocol"
)

// prefixScrollback sends the documented Ctrl-b [ chord as two keystrokes.
func prefixScrollback(m *Model) {
	_, _ = m.onKey(tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	_, _ = m.onKey(runeKey("["))
}

// enterScrollbackWheel enters the substate with a plain wheel-up.
func enterScrollbackWheel(m *Model) {
	_, _ = m.onMouse(wheelUp())
}

// exitScrollbackLive presses q (the Live binding) to return to the live view.
func exitScrollbackLive(m *Model) {
	_, _ = m.onKey(runeKey("q"))
}

// TestScrollbackEntryPathsKeepInputAlive verifies both documented entry paths
// land in the substate with the prefix disarmed and the mouse grab intact, so
// the state after entry cannot silently eat the next chord.
func TestScrollbackEntryPathsKeepInputAlive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry func(*Model)
	}{
		{"wheel-up", enterScrollbackWheel},
		{"ctrl-b prefix chord", prefixScrollback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRW)
			m := fx.m

			tc.entry(m)
			if m.att.substate != substateScrollback {
				t.Fatalf("%s should enter scrollback", tc.name)
			}
			if m.att.prefixArmed {
				t.Fatalf("%s must disarm the Ctrl-b prefix so the first post-exit key is not eaten as a chord", tc.name)
			}
			if !mouseGrabbed(m) {
				t.Fatalf("%s must not release the mouse grab", tc.name)
			}
		})
	}
}

// TestScrollbackNavigationKeysDoNotReachPTY verifies the frozen substate owns
// navigation keys: arrows and page keys move the view and none of them leak to
// the guest PTY while frozen.
func TestScrollbackNavigationKeysDoNotReachPTY(t *testing.T) {
	fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRW)
	m, fa := fx.m, fx.fa

	enterScrollbackWheel(m)
	for _, k := range []tea.KeyPressMsg{
		{Code: tea.KeyUp}, {Code: tea.KeyDown},
		{Code: tea.KeyPgUp}, {Code: tea.KeyPgDown},
	} {
		_, _ = m.onKey(k)
	}
	if got := len(fa.inputs); got != 0 {
		t.Fatalf("scrollback navigation keys must not reach the PTY, got %d forwarded", got)
	}
}

// TestInputAliveAfterScrollbackExit is the regression for the #7 signature:
// after entering scrollback (both paths) and exiting, the first keystroke must
// be forwarded to the PTY and the mouse grab must be re-asserted — nothing in
// the cycle may leave the terminal deaf.
func TestInputAliveAfterScrollbackExit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry func(*Model)
	}{
		{"wheel-up then q", enterScrollbackWheel},
		{"ctrl-b [ then q", prefixScrollback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRW)
			m, fa := fx.m, fx.fa

			tc.entry(m)
			exitScrollbackLive(m)
			if m.att.substate != substateInteractive {
				t.Fatalf("%s: q must return to the live view", tc.name)
			}

			// The first keystroke after the round trip must reach the agent.
			_, cmd := m.onKey(runeKey("a"))
			drain(cmd)
			if len(fa.inputs) != 1 || string(fa.inputs[0]) != "a" {
				t.Fatalf("%s: the first key after scrollback exit must reach the PTY, inputs=%v", tc.name, fa.inputs)
			}
			if !mouseGrabbed(m) {
				t.Fatalf("%s: the mouse grab must be held after the round trip", tc.name)
			}
		})
	}
}

// TestInputAliveAfterNativeScrollbackDetour covers the client-terminal-steals-
// the-wheel variant of #7: with the grab released for native selection, a wheel
// event still enters the harness scrollback, and the full exit cycle restores
// input — key forwarded, grab re-taken.
func TestInputAliveAfterNativeScrollbackDetour(t *testing.T) {
	fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRW)
	m, fa := fx.m, fx.fa

	// Native selection detour: shift+click released the grab, the queued
	// wheel-up arrives while released.
	_, _ = m.onMouse(shiftClick())
	enterScrollbackWheel(m)
	if m.att.substate != substateScrollback {
		t.Fatal("wheel-up after a shift release must still enter scrollback")
	}

	// A key inside scrollback restores the grab (router-level re-enable)…
	_, _ = m.onKey(runeKey("j"))
	if m.mouseReleased {
		t.Fatal("a key inside scrollback must re-enable the mouse grab")
	}

	// …and the exit cycle leaves the terminal fully live again.
	exitScrollbackLive(m)
	_, cmd := m.onKey(runeKey("a"))
	drain(cmd)
	if len(fa.inputs) != 1 || string(fa.inputs[0]) != "a" {
		t.Fatalf("input must be alive after the native-scrollback detour, inputs=%v", fa.inputs)
	}
	if !mouseGrabbed(m) {
		t.Fatal("the mouse grab must be restored after the native-scrollback detour")
	}
}
