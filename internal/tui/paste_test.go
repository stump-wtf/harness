package tui

// Governing: SPEC-0001 scenario "Driving a live agent" (attached input must
// forward faithfully), #272. Bubble Tea v2 delivers bracketed pastes as
// tea.PasteMsg, not KeyPressMsg, so the Update switch dropped them silently.
// onPaste must wrap the content in ESC[200~/ESC[201~ and forward it to the
// guest PTY in the attached interactive substate — and only there.

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

func TestPasteForwardsWrappedToPTY(t *testing.T) {
	m, fa := attachedFake(80, 24)
	sid := m.att.sessionID

	mm, cmd := m.Update(tea.PasteMsg{Content: "hello world"})
	m2, ok := mm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T, want *Model", mm)
	}
	drain(cmd)

	if len(fa.inputs) != 1 {
		t.Fatalf("expected one AttachInput, got %d", len(fa.inputs))
	}
	want := "\x1b[200~hello world\x1b[201~"
	if got := string(fa.inputs[0]); got != want {
		t.Fatalf("AttachInput = %q, want %q", got, want)
	}
	if m2.att.sessionID != sid {
		t.Fatalf("paste forwarded to session %d, want %d", m2.att.sessionID, sid)
	}
}

func TestPasteDisarmsPrefix(t *testing.T) {
	m, fa := attachedFake(80, 24)
	m.att.prefixArmed = true

	mm, cmd := m.Update(tea.PasteMsg{Content: "bulk"})
	drain(cmd)
	if mv, ok := mm.(*Model); !ok || mv.att.prefixArmed {
		t.Fatal("paste should disarm an armed Ctrl-b prefix")
	}
	if len(fa.inputs) != 1 {
		t.Fatalf("paste should still forward while disarming, got %d inputs", len(fa.inputs))
	}
}

func TestPasteDroppedWhenNotForwardable(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Model)
	}{
		{name: "read-only attach drops paste (ADR-0008)", setup: func(m *Model) { m.att.mode = protocol.AttachRO }},
		{name: "scrollback substate drops paste", setup: func(m *Model) { m.att.enterScrollback(nil, 4) }},
		{name: "dashboard mode drops paste", setup: func(m *Model) { m.mode = modeDashboard; m.att = nil }},
		{name: "nil attach state drops paste", setup: func(m *Model) { m.att = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fa := attachedFake(80, 24)
			tt.setup(m)
			_, cmd := m.Update(tea.PasteMsg{Content: "nope"})
			drain(cmd)
			if len(fa.inputs) != 0 {
				t.Fatalf("paste should be dropped, got inputs %v", fa.inputs)
			}
		})
	}
}
