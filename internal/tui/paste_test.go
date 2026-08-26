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
		{name: "dashboard mode drops paste", setup: func(m *Model) { m.mode = modeDashboard }},
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

func TestPasteInsertsIntoScrollbackSearch(t *testing.T) {
	m, fa := attachedFake(80, 24)
	m.att.enterScrollback(nil, 4)
	m.att.searchOn = true
	m.att.search.SetValue("er")
	m.att.search.Focus()
	m.att.scroll.search(m.att.search.Value())

	mm, _ := m.Update(tea.PasteMsg{Content: "ror"})
	mv, ok := mm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T, want *Model", mm)
	}
	if got := mv.att.search.Value(); got != "error" {
		t.Fatalf("search value = %q, want %q", got, "error")
	}
	if len(fa.inputs) != 0 {
		t.Fatalf("scrollback search paste must not reach the PTY, got inputs %v", fa.inputs)
	}
	if mv.att.scroll.term != "error" {
		t.Fatalf("scrollback search did not rescan, term = %q", mv.att.scroll.term)
	}
}

func TestPasteInsertsIntoScrollbackSearchFlattened(t *testing.T) {
	m, _ := attachedFake(80, 24)
	m.att.enterScrollback(nil, 4)
	m.att.searchOn = true
	m.att.search.Focus()

	mm, _ := m.Update(tea.PasteMsg{Content: "foo\r\nbar\nbaz\r"})
	mv := mm.(*Model)
	if got := mv.att.search.Value(); got != "foo bar baz" {
		t.Fatalf("search value = %q, want %q (newlines flattened)", got, "foo bar baz")
	}
}

func TestPasteInsertsIntoDashboardSearch(t *testing.T) {
	m, fa := attachedFake(80, 24)
	m.mode = modeDashboard
	mm, _ := m.openSearch()
	m = mm.(*Model)

	mm, _ = m.Update(tea.PasteMsg{Content: "reduit"})
	mv, ok := mm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T, want *Model", mm)
	}
	if got := mv.search.Value(); got != "reduit" {
		t.Fatalf("search value = %q, want %q", got, "reduit")
	}
	if mv.searchQuery != "reduit" {
		t.Fatalf("searchQuery = %q, want it live-updated", mv.searchQuery)
	}
	if len(fa.inputs) != 0 {
		t.Fatalf("dashboard search paste must not reach the PTY, got inputs %v", fa.inputs)
	}
}

func TestPasteInsertsIntoPalette(t *testing.T) {
	m, fa := attachedFake(80, 24)
	m.mode = modeDashboard
	mm, _ := m.openPalette()
	m = mm.(*Model)
	m.pal.all = []Command{{Verb: "restart", Target: "reduit-agent", Display: "restart reduit-agent"}, {Verb: "start", Target: "mixtape", Display: "start mixtape"}}
	m.pal.filtered = m.pal.all

	mm, _ = m.Update(tea.PasteMsg{Content: "reduit"})
	mv, ok := mm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T, want *Model", mm)
	}
	if got := mv.pal.input.Value(); got != "reduit" {
		t.Fatalf("palette value = %q, want %q", got, "reduit")
	}
	if len(mv.pal.filtered) != 1 || mv.pal.filtered[0].Target != "reduit-agent" {
		t.Fatalf("palette did not refilter after paste, filtered = %+v", mv.pal.filtered)
	}
	if len(fa.inputs) != 0 {
		t.Fatalf("palette paste must not reach the PTY, got inputs %v", fa.inputs)
	}
}

func TestFlattenPaste(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"foo\nbar", "foo bar"},
		{"foo\r\nbar", "foo bar"},
		{"foo\rbar", "foo bar"},
		{"foo\n", "foo"},
		{"  foo  ", "foo"},
	}
	for _, tt := range tests {
		if got := flattenPaste(tt.in); got != tt.want {
			t.Errorf("flattenPaste(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A paste must obey the same overlay-before-mode precedence routeKey uses.
// `Ctrl-b ?` opens the help overlay while attached, so an overlay without a
// paste target is reachable over a live agent — the paste has to stop there
// rather than land in the PTY behind the overlay.
func TestPasteDroppedByOverlayWithoutInput(t *testing.T) {
	overlays := []struct {
		name string
		ov   overlay
	}{
		{name: "help overlay swallows paste", ov: overlayHelp},
		{name: "confirm overlay swallows paste", ov: overlayConfirm},
		{name: "profile overlay swallows paste", ov: overlayProfile},
		{name: "form overlay swallows paste", ov: overlayForm},
	}
	for _, tt := range overlays {
		t.Run(tt.name, func(t *testing.T) {
			m, fa := attachedFake(80, 24)
			m.overlay = tt.ov

			mm, cmd := m.Update(tea.PasteMsg{Content: "rm -rf /"})
			drain(cmd)
			if _, ok := mm.(*Model); !ok {
				t.Fatalf("Update returned %T, want *Model", mm)
			}
			if len(fa.inputs) != 0 {
				t.Fatalf("paste under %v reached the PTY: %q", tt.ov, fa.inputs)
			}
		})
	}
}
