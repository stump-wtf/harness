package supervisor

// Governing: ADR-0007 (amended — the durable per-harness log stores sanitized
// output history, not raw PTY bytes). ptyHistory converts the raw PTY stream
// into the line-oriented history the log is made of: it runs the bytes through
// a dedicated x/vt emulator and appends, as plain text, exactly the rows that
// SCROLL off the top of the screen. A full-screen agent TUI that repaints
// itself in place (spinner glyph, elapsed-seconds counter, clear-and-home
// frames) never shifts the screen up, so a sixty-second tool run produces zero
// junk lines instead of sixty — what the log records is what the program
// actually wrote, one line per line.
//
// Scroll detection is a screen-shift diff, not the emulator's scrollback
// buffer: charmbracelet/x/vt pushes the whole screen into scrollback on
// ED-2 (clear screen), so a repaint-only frame would otherwise masquerade as
// two scrolled lines (that quirk is exactly the per-second junk this file
// exists to eliminate). A row is emitted only when the post-write screen is
// the pre-write screen shifted up by k rows — the signature of a printed
// newline at the bottom — and the vanished top k rows are written verbatim.
//
// The raw stream still reaches the attach mux untouched (ADR-0003 live attach
// needs the escape bytes); only the durable log side is sanitized. Structured
// lifecycle events (state changes, exits, flapping) are written to the same
// log by the Supervisor via charmbracelet/log — see logEvent in supervisor.go.

import (
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/charmbracelet/x/vt"

	clog "github.com/charmbracelet/log"
)

// ptyHistory is an io.Writer that extracts scrolled-off screen rows from a
// raw PTY stream and appends them, one "\n"-terminated plain-text line each,
// to out (the rotating log). It is safe for concurrent use; in practice the
// single PTY reader goroutine is the only writer.
type ptyHistory struct {
	mu   sync.Mutex
	term vt.Terminal
	out  io.Writer
	// prev is the pre-write screen text (blank-padded rows trimmed); the
	// post-write screen is compared against it to measure the scroll.
	prev []string
	// flushed guards the end-of-stream Flush against a second call (closeLog
	// also flushes, defensively, for runs that never got a reader EOF).
	flushed bool
}

// newPtyHistory builds the sanitizer for a PTY born at cols x rows.
func newPtyHistory(out io.Writer, cols, rows int) *ptyHistory {
	if cols < 1 {
		cols = defaultPTYCols
	}
	if rows < 1 {
		rows = defaultPTYRows
	}
	return &ptyHistory{term: vt.NewEmulator(cols, rows), out: out, prev: blankScreen(cols, rows)}
}

// Write feeds p through the emulator and appends any scrolled-off rows to the
// log. It implements io.Writer and never fails the PTY reader: write errors on
// the log are swallowed (the rotating log itself is best-effort, exactly as it
// was when it received raw bytes).
//
// p is fed to the emulator newline-chunk by newline-chunk, diffing the screen
// after each: a burst that arrives in one read (a fast `seq`) can scroll many
// rows at once, and only the intermediate screens reveal each scroll step.
func (h *ptyHistory) Write(p []byte) (int, error) {
	n := len(p)
	h.mu.Lock()
	defer h.mu.Unlock()
	for len(p) > 0 {
		// Hand the emulator up to and including the next LF (or the rest of p).
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i+1]
		}
		_, _ = h.term.Write(chunk)
		p = p[len(chunk):]
		h.diffLocked()
	}
	return n, nil
}

// diffLocked emits the rows that scrolled off since the last diff and caches
// the current screen. Caller holds h.mu.
func (h *ptyHistory) diffLocked() {
	cur := screenText(h.term)
	for _, ln := range h.prev[:scrollCount(h.prev, cur)] {
		if ln != "" {
			_, _ = io.WriteString(h.out, ln+"\n")
		}
	}
	h.prev = cur
}

// Flush appends the current on-screen content (minus trailing blank rows) to
// the log. The Supervisor calls it when the stream ends so the final
// screenful — a short run's entire output, which never scrolled — is not lost.
func (h *ptyHistory) Flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.flushed {
		return
	}
	h.flushed = true
	for _, ln := range screenText(h.term) {
		if ln != "" {
			_, _ = io.WriteString(h.out, ln+"\n")
		}
	}
}

// scrollCount returns how many rows scrolled off: the largest k such that
// cur[i] == prev[i+k] for every i < len(prev)-k (a pure upward shift). Both
// screens are first trimmed of trailing blank rows — after a scroll the bottom
// row(s) are blank until new content lands there, and unwritten padding must
// not break the match. A clear-and-repaint frame does not satisfy the shift
// for any k > 0 and correctly reports 0.
func scrollCount(prev, cur []string) int {
	pp := trimBlankTail(prev)
	cp := trimBlankTail(cur)
	for k := len(pp) - 1; k > 0; k-- {
		shifted := true
		for i := 0; i+k < len(pp); i++ {
			if i >= len(cp) || cp[i] != pp[i+k] {
				shifted = false
				break
			}
		}
		if shifted {
			return k
		}
	}
	return 0
}

// trimBlankTail drops trailing rows with no visible content. Unwritten screen
// padding renders as blank rows; they carry no history.
func trimBlankTail(rows []string) []string {
	n := len(rows)
	for n > 0 && rows[n-1] == "" {
		n--
	}
	return rows[:n]
}

// blankScreen is the all-blank starting screen (rows of empty strings).
func blankScreen(cols, rows int) []string {
	s := make([]string, rows)
	for i := range s {
		s[i] = ""
	}
	return s
}

// screenText renders the current screen rows as plain text: cell graphemes
// only, no styling, trailing padding stripped.
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

// newEventLogger builds the charmbracelet/log logger that writes structured
// lifecycle events into the durable log: timestamps on, plain text (the log
// may be tailed by `harness logs` from a non-terminal).
func newEventLogger(w io.Writer) *clog.Logger {
	l := clog.New(w)
	l.SetReportTimestamp(true)
	return l
}
