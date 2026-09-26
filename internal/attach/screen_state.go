package attach

// Screen observability: the plain-text state of the emulator every harness
// already feeds (ADR-0003), served without a TTY.
//
// Governing: issue #735; ADR-0040. The durable log deliberately records only
// scrolled-off rows (ADR-0007, as amended for #279) — a full-screen prompt that
// repaints in place never scrolls, so it never lands in `harness logs`, and
// "attach and look" needs an interactive TTY. The emulator is the missing
// truthful source: it has every byte the guest ever wrote, projected to the
// visible screen, so `capture` and stuck-prompt detection read it directly.
// Like ansifold, these views keep the daemon agnostic — they describe WHAT is
// on the glass, never what kind of program drew it.

import (
	"strings"
	"time"

	"github.com/charmbracelet/x/vt"
)

// ScreenState is a point-in-time reading of one harness's visible screen: the
// plain-text rows, and how long the screen has been unchanged as of the caller
//'s now (so a caller with a clock of its own — a test, a sweep replaying a
// capture — gets verdicts consistent with its own timeline).
//
// The Manager wires a tee target for every configured harness at registration
// (ManagerOptions.ExtraOutFor), so a Mux can exist long before the harness has
// ever produced output. Fed distinguishes the two: an unfed mux's screen is an
// unstarted emulator (blank rows, meaningless idle), and every consumer below
// treats it as "no screen to read" — the honest answer, not an empty one.
type ScreenState struct {
	// Fed reports whether the emulator has ever received PTY bytes. False
	// means the harness has produced no output yet — there is no screen to
	// observe, and Idle carries no information.
	Fed bool
	// Rows are the visible screen rows as plain text, trailing padding
	// trimmed per row; trailing blank rows are dropped entirely (an idle
	// screen should not append twenty empty lines to a sweep's output),
	// interior blank rows kept: screen geometry is part of the answer for a
	// rendered dialog.
	Rows []string
	// Idle is how long the emulator has received no PTY bytes, measured to the
	// caller's now. Meaningful only when Fed.
	Idle time.Duration
	// Cols/RowsAt are the viewport the screen was rendered at, so a consumer
	// re-flowing Rows does not have to guess geometry.
	Cols   int
	RowsAt int
}

// screenStateLocked reads the screen and the idle age under m.mu. The screen
// is read through the same trimming the sanitizer applies to its own screen
// text, so a capture row of the same content agrees with a history row
// byte for byte (ADR-0007).
func (m *Mux) screenStateLocked(now time.Time) ScreenState {
	st := ScreenState{
		Fed:    !m.lastWrite.IsZero(),
		Idle:   now.Sub(m.lastWrite),
		Cols:   m.term.Width(),
		RowsAt: m.term.Height(),
	}
	if !st.Fed {
		st.Idle = 0
	}
	rows := screenText(m.term)
	st.Rows = trimBlankTail(rows)
	return st
}

// ScreenState reads the harness's visible screen and its idle age as of now.
func (m *Mux) ScreenState(now time.Time) ScreenState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.screenStateLocked(now)
}

// AnsiScreen serializes the current screen as an ANSI repaint (the same bytes
// an attach client's first frame carries), for a consumer that wants the
// styled view — writing them to a terminal reproduces the guest's screen
// pixel-faithfully, cursor position included (SPEC-0002 REQ "Attach Session").
func (m *Mux) AnsiScreen() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return renderScreen(m.term)
}

// screenText renders the current screen rows as plain text: cell graphemes
// only, no styling, trailing padding stripped per row. Mirrors the sanitizer's
// own screen text so a capture row and a history row of the same content agree
// byte for byte (ADR-0007).
func screenText(t vt.Terminal) []string {
	rows := make([]string, t.Height())
	for y := 0; y < t.Height(); y++ {
		var b strings.Builder
		for x := 0; x < t.Width(); x++ {
			if c := t.CellAt(x, y); c != nil {
				b.WriteString(c.String())
			}
		}
		rows[y] = strings.TrimRight(b.String(), " ")
	}
	return rows
}

// trimBlankTail drops trailing rows with no visible content.
func trimBlankTail(rows []string) []string {
	n := len(rows)
	for n > 0 && rows[n-1] == "" {
		n--
	}
	return rows[:n]
}
