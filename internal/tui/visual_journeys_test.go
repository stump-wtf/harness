package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// journey builds a Model already driven into one visual state, ready to render.
type journey struct {
	name  string
	build func(w, h int) *Model
}

// baseModel is a connected dashboard model with sample harnesses + profiles.
func baseModel(w, h int) *Model {
	fc := &fakeController{harnesses: sampleHarnesses(), profiles: sampleProfiles()}
	m := New(Options{})
	m.ctrl, m.attach = fc, &fakeAttach{}
	m.conn = startOK
	m.harnesses = fc.harnesses
	m.profiles = fc.profiles
	m.w, m.h = w, h
	m.help.Width = w
	return m
}

// attachedModel is a connected model attached to a running harness, with a
// full-width sentinel painted on the embedded terminal's top row.
func attachedModel(w, h int, ro bool) *Model {
	m := baseModel(w, h)
	mode := protocol.AttachRW
	if ro {
		m.opts.ReadOnly = true
		mode = protocol.AttachRO
	}
	m.mode = modeAttached
	cols, rows := m.attachViewport()
	m.att = newAttachState("crush-signal", mode, sessionBase, cols, rows)
	sentinel := "TOP-ROW-SENTINEL"
	m.att.view.write([]byte("\x1b[42m" + sentinel + strings.Repeat(" ", w-len(sentinel)) + "\x1b[0m"))
	return m
}

func journeys() []journey {
	return []journey{
		{name: "dashboard", build: func(w, h int) *Model { return baseModel(w, h) }},
		{name: "dashboard-empty", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.harnesses = nil
			m.profiles = nil
			return m
		}},
		{name: "dashboard-search-filter", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.openSearch()
			for _, r := range "crush" {
				m.onKey(runeKey(string(r)))
			}
			return m
		}},
		{name: "attached", build: func(w, h int) *Model { return attachedModel(w, h, false) }},
		{name: "attached-readonly", build: func(w, h int) *Model { return attachedModel(w, h, true) }},
		{name: "attached-prefix-armed", build: func(w, h int) *Model {
			m := attachedModel(w, h, false)
			m.att.prefixArmed = true
			return m
		}},
		{name: "scrollback", build: func(w, h int) *Model {
			m := attachedModel(w, h, false)
			var lines []string
			for i := 0; i < h*3; i++ {
				lines = append(lines, fmt.Sprintf("scrollback history line %d", i))
			}
			m.att.enterScrollback(lines, m.scrollbackHeight())
			return m
		}},
		{name: "palette", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.openPalette()
			return m
		}},
		{name: "palette-query", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.openPalette()
			for _, r := range "rest redu" {
				m.onKey(runeKey(string(r)))
			}
			return m
		}},
		{name: "help-overlay", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.overlay = overlayHelp
			return m
		}},
		{name: "profile-switcher", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.openProfileSwitcher()
			return m
		}},
		{name: "confirm", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.overlay = overlayConfirm
			m.confirm = confirmState{action: ActionDelete, target: "crush-signal", prompt: confirmPrompt(ActionDelete, "crush-signal")}
			return m
		}},
		{name: "new-form", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.openForm(false)
			return m
		}},
		{name: "no-daemon", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.conn = startNoDaemon
			return m
		}},
		{name: "reconnecting", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.reconn = true
			return m
		}},
		{name: "attaching", build: func(w, h int) *Model {
			m := baseModel(w, h)
			m.opts.AttachOnly = "crush-signal"
			m.att = nil
			return m
		}},
	}
}

var sizes = [][2]int{{200, 50}, {145, 42}, {120, 40}, {100, 30}, {80, 24}}

// TestVisualJourneysFit is the broad deterministic visual guard: every screen,
// at every size, must render within the viewport — no line wider than the
// terminal (which wraps + scrolls), and no more rows than the terminal (checked
// both as a line count and via x/vt: the cursor must stay on-grid). This is the
// same View()→x/vt approach as TestAttachedVisualNoScroll, applied across the
// whole UI so a regression in any screen's sizing is caught deterministically —
// no PTY, no daemon, no timing.
func TestVisualJourneysFit(t *testing.T) {
	for _, j := range journeys() {
		for _, s := range sizes {
			w, h := s[0], s[1]
			view := j.build(w, h).View()
			lines := strings.Split(view, "\n")

			// Width: no rendered line may exceed the terminal width.
			for i, ln := range lines {
				if lw := lipgloss.Width(ln); lw > w {
					t.Errorf("%s %dx%d: row %d width %d exceeds terminal width %d",
						j.name, w, h, i, lw, w)
				}
			}

			// Height: every screen must fit — including the new-harness form,
			// which scrolls within its overlay on short terminals (issue #25).
			if len(lines) > h {
				t.Errorf("%s %dx%d: %d rows exceed terminal height %d", j.name, w, h, len(lines), h)
			}
			if cy := emulate(view, w, h).CursorPosition().Y; cy >= h {
				t.Errorf("%s %dx%d: content scrolled below the grid (cursor row %d)", j.name, w, h, cy)
			}
		}
	}
}

// TestNewFormFitsShortTerminals is the direct guard for issue #25: the Huh
// new-harness form is intrinsically ~34 rows, but bounded to the overlay
// viewport it must fit (and scroll internally) at any terminal size — never
// overflow and scroll the whole screen. Covers editing (pre-filled) too.
func TestNewFormFitsShortTerminals(t *testing.T) {
	for _, editing := range []bool{false, true} {
		for _, s := range [][2]int{{120, 40}, {100, 30}, {80, 24}, {90, 20}, {70, 16}} {
			w, h := s[0], s[1]
			m := baseModel(w, h)
			m.openForm(editing)
			view := m.View()
			lines := strings.Split(view, "\n")
			if len(lines) > h {
				t.Errorf("editing=%v %dx%d: form overlay %d rows exceed height %d", editing, w, h, len(lines), h)
			}
			for i, ln := range lines {
				if lw := lipgloss.Width(ln); lw > w {
					t.Errorf("editing=%v %dx%d: row %d width %d exceeds width %d", editing, w, h, i, lw, w)
				}
			}
		}
	}
}
