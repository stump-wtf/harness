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
	"fmt"
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
	term := vt.NewEmulator(cols, rows)
	h := &ptyHistory{term: term, out: out, prev: blankScreen(cols, rows)}
	go h.drainReplies(term)
	return h
}

// drainReplies discards the bytes the sanitizer's emulator synthesizes in
// answer to the guest's terminal queries — Primary Device Attributes, DECRQM
// mode reports, cursor position reports, and the like. x/vt writes those
// replies into an unbuffered internal pipe that only Emulator.Read drains,
// and an undrained write blocks forever on the first query the guest sends.
// Since Write runs on the PTY reader goroutine, that block froze the entire
// guest→mux feed on the guest's opening handshake: every agent TUI that
// probes the terminal at startup (crush and claude both send DA1 before
// their first frame) looked headless — alive in `harness list`, painting
// nothing, attach and preview permanently empty (stump.wtf/harness#299).
//
// The replies themselves are DISCARDED, not forwarded: answering the guest
// is the attach mux's job (its own pumpReplies forwards through onInput),
// and a second answer to the same query would land in the guest's input as
// spurious keystrokes.
//
// The pump runs for its emulator's lifetime and is never stopped — the
// same never-Close trade the mux's pumpReplies makes, for the same reason:
// Emulator.Close races a parked Read (see vtview.pumpReplies). One parked
// goroutine per emulator, not a frozen production agent.
//
// The emulator is a parameter, not h.term: feedLocked replaces that field
// when it recovers from a panic, and reading it here would race that write
// (h.mu guards the field, and a pump that took the lock would hold it for
// the whole harness's lifetime). Binding each pump to the emulator it was
// started for keeps the field's only reads under the lock.
func (h *ptyHistory) drainReplies(term *vt.Emulator) {
	buf := make([]byte, 1024)
	for {
		if _, err := term.Read(buf); err != nil {
			return
		}
	}
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
		h.feedLocked(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// feedLocked hands one chunk to the emulator and diffs the screen, surviving
// a panic inside the emulator. x/vt's ScrollUp → ultraviolet DeleteLineArea
// indexes past the buffer when a guest sets a scroll region taller than the
// PTY (crush does, rendering a channel doorbell on an 80×24 PTY):
// "index out of range [24] with length 24". Unrecovered, that panic is on
// the PTY reader goroutine and takes the whole daemon — and every harness
// it supervises — down with it (2026-09-12, stump-wtf/harness#1's cousin).
//
// Recovery is reset-first, rebuild-fallback: ESC[r resets DECSTBM to the
// default scroll region (1;height), which heals the poisoned margins. A
// rebuild leaks one goroutine per panic (the old emulator's drainReplies
// pump parks forever), so we try reset first and only rebuild when reset
// cannot heal the state. The frame is dropped in either case; the durable
// log loses at most one screen of context.
func (h *ptyHistory) feedLocked(chunk []byte) {
	defer func() {
		if r := recover(); r != nil {
			// Try a reset-first recovery: ESC[r restores the default scroll region
			// (1;height) without creating a new emulator or leaking a goroutine.
			// The screen content is unchanged; only the margin state resets.
			healed := false
			func() {
				defer func() {
					_ = recover()
				}()
				_, _ = h.term.Write([]byte("\x1b[r"))
				healed = true
			}()
			if healed {
				// The screen shifted during the panicked write: diffLocked compares
				// the post-panic screen against a prev that reflected the half-scroll
				// state from before the panic. Resync prev to the current screen so
				// the next write diffs cleanly.
				h.prev = screenText(h.term)
				_, _ = io.WriteString(h.out, fmt.Sprintf("[harness] recovered from a terminal rendering panic (%v) by resetting scroll region\n", r))
				return
			}
			// Reset itself panicked or did not heal the state. Fall back to a
			// full rebuild. This leaks one drainReplies goroutine (the old pump
			// parks forever), but it keeps the guest running when reset cannot
			// fix whatever caused the original panic.
			cols, rows := h.term.Width(), h.term.Height()
			if cols < 1 || rows < 1 {
				cols, rows = defaultPTYCols, defaultPTYRows
			}
			term := vt.NewEmulator(cols, rows)
			h.term = term
			h.prev = blankScreen(cols, rows)
			go h.drainReplies(term)
			_, _ = io.WriteString(h.out, fmt.Sprintf("[harness] dropped a frame the terminal emulator could not render (%v); emulator reset\n", r))
		}
	}()
	_, _ = h.term.Write(chunk)
	h.diffLocked()
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
