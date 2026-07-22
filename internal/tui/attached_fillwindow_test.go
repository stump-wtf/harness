package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// TestAttachedFillsWindow verifies viewAttached emits exactly m.h lines, each
// exactly m.w columns wide, across a range of window widths and status-bar
// states. A line wider than m.w wraps in the alt-screen and scrolls the
// embedded terminal so it no longer fills the window (the "not 100%x100%" bug);
// a short final line leaves a gap. Both are regressions this guards.
func TestAttachedFillsWindow(t *testing.T) {
	cases := []struct {
		name        string
		w, h        int
		prefixArmed bool
		readOnly    bool
	}{
		{"wide", 200, 50, false, false},
		{"typical", 120, 40, false, false},
		{"joe-terminal", 145, 42, false, false},
		{"narrow", 80, 24, false, false},
		{"very-narrow", 40, 20, false, false},
		{"tiny", 20, 10, false, false},
		{"prefix-armed", 120, 40, true, false},
		{"read-only", 120, 40, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode := protocol.AttachRW
			if tc.readOnly {
				mode = protocol.AttachRO
			}
			m := New(Options{AttachOnly: "crush", ReadOnly: tc.readOnly})
			m.w, m.h = tc.w, tc.h
			m.help.Width = tc.w
			m.mode = modeAttached
			cols, rows := m.attachViewport()
			m.att = newAttachState("crush", mode, sessionBase, cols, rows)
			m.att.prefixArmed = tc.prefixArmed
			m.harnesses = []protocol.HarnessInfo{{Name: "crush", State: "running"}}

			// Simulate the daemon streaming a full-width repaint plus content.
			m.att.view.write([]byte("\x1b[42m" + strings.Repeat(" ", tc.w) + "\x1b[0m"))
			m.att.view.write([]byte("\r\nwelcome, top-left content\r\n"))

			lines := strings.Split(m.viewAttached(), "\n")
			if len(lines) != tc.h {
				t.Fatalf("line count = %d, want %d (%dx%d)", len(lines), tc.h, tc.w, tc.h)
			}
			for i, ln := range lines {
				if w := lipgloss.Width(ln); w != tc.w {
					t.Errorf("line %d width = %d, want %d (%dx%d)", i, w, tc.w, tc.w, tc.h)
				}
			}
		})
	}
}

// TestScrollbackDoesNotOverflow guards the scrollback substate against the same
// overflow-and-scroll bug: viewScrollback renders its own status footer and
// viewAttached appends the global status bar, so the frozen buffer must reserve
// a row for each and never exceed m.h total lines.
func TestScrollbackDoesNotOverflow(t *testing.T) {
	for _, dim := range [][2]int{{200, 50}, {120, 40}, {80, 24}, {40, 12}} {
		w, h := dim[0], dim[1]
		m := New(Options{AttachOnly: "crush"})
		m.w, m.h = w, h
		m.help.Width = w
		m.mode = modeAttached
		cols, rows := m.attachViewport()
		m.att = newAttachState("crush", protocol.AttachRW, sessionBase, cols, rows)
		m.harnesses = []protocol.HarnessInfo{{Name: "crush", State: "running"}}
		// More history than fits, so the viewport is fully packed.
		var lines []string
		for i := 0; i < h*3; i++ {
			lines = append(lines, "scrollback line content")
		}
		m.att.enterScrollback(lines, m.scrollbackHeight())
		got := strings.Split(m.viewAttached(), "\n")
		if len(got) > h {
			t.Errorf("%dx%d scrollback: %d lines overflow window height %d", w, h, len(got), h)
		}
	}
}
