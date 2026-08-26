package attach

// Governing: SPEC-0002 REQ "Attach Session"; #272 (bracketed pastes must reach
// the guest). A client wraps a paste in ESC[200~/ESC[201~ so the daemon can
// tell a paste from keystrokes, but only the emulator here knows whether the
// GUEST asked for bracketed paste (DECSET ?2004) — a client attaches to a
// screen snapshot, which carries no modes. Handing the brackets to a guest that
// never enabled the mode does not degrade gracefully: they land as literal
// input, so `ls -la` arrives as "^[[200~ls -la^[[201~".
//
// @joestump 08/26/2026 - Added in review of #274.

import (
	"sync"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// recordingMux returns a Mux whose PTY writes are captured.
func recordingMux(t *testing.T) (*Mux, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	m := newMux("h", 100, nil, func(p []byte) {
		mu.Lock()
		got = append(got, string(p))
		mu.Unlock()
	}, nil)
	return m, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestPasteBracketsFollowGuestMode(t *testing.T) {
	const paste = "\x1b[200~ls -la\x1b[201~"

	tests := []struct {
		name      string
		guestOut  string
		wantInput string
	}{
		{
			// A plain shell, `cat`, or any program that never sets ?2004.
			// Unwrapped, or the guest's command line is corrupted.
			name:      "guest never enabled ?2004 gets bare text",
			guestOut:  "$ ",
			wantInput: "ls -la",
		},
		{
			// readline at a prompt, or an agent TUI: it asked for brackets.
			name:      "guest with ?2004 on gets the brackets",
			guestOut:  "\x1b[?2004h$ ",
			wantInput: paste,
		},
		{
			// readline turns ?2004 off while a command runs. A paste landing
			// in that window is keystrokes to the child, not a paste.
			name:      "guest that turned ?2004 back off gets bare text",
			guestOut:  "\x1b[?2004h\x1b[?2004l$ ",
			wantInput: "ls -la",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, inputs := recordingMux(t)
			if _, err := m.Write([]byte(tt.guestOut)); err != nil {
				t.Fatalf("guest write: %v", err)
			}

			s := m.Attach(1, protocol.AttachRW, 80, 24, func([]byte) error { return nil })
			s.Input([]byte(paste))

			got := inputs()
			if len(got) != 1 {
				t.Fatalf("expected one PTY write, got %d: %q", len(got), got)
			}
			if got[0] != tt.wantInput {
				t.Errorf("guest received %q, want %q", got[0], tt.wantInput)
			}
		})
	}
}

// Ordinary keystrokes must pass through byte-for-byte whatever the mode is —
// the unwrap only ever fires on a payload that is exactly a bracketed paste.
func TestNonPasteInputUntouched(t *testing.T) {
	for _, guestOut := range []string{"$ ", "\x1b[?2004h$ "} {
		m, inputs := recordingMux(t)
		if _, err := m.Write([]byte(guestOut)); err != nil {
			t.Fatalf("guest write: %v", err)
		}
		s := m.Attach(1, protocol.AttachRW, 80, 24, func([]byte) error { return nil })

		// Includes a payload that merely CONTAINS the start marker without
		// being a well-formed paste: it must not be trimmed.
		for _, in := range []string{"ls -la\r", "\x1b[A", "\x1b[200~unterminated"} {
			s.Input([]byte(in))
		}
		got := inputs()
		want := []string{"ls -la\r", "\x1b[A", "\x1b[200~unterminated"}
		if len(got) != len(want) {
			t.Fatalf("got %d writes, want %d: %q", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("guest received %q, want %q", got[i], want[i])
			}
		}
	}
}
