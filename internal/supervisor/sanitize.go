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
// Screen rows are not the whole story, because a row can also end in the
// terminal rather than in the program: text that reaches the last column is
// folded onto the next row without the program writing anything, and a row
// that scrolls off after such a fold is half a line. Splitting there is what
// makes a long path or a long sentence land in the log in pieces (#279). So
// each piece of the stream is classified before it is diffed — see feedLocked
// for the two signals and ADR-0057 for the decision — and a row that ended in
// a fold is held and glued to the row after it, which restores the line the
// program actually wrote. Rows that ended in a newline are written alone,
// exactly as before, so a full-screen TUI's full-width frames and rule lines
// are never glued to their neighbours.
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

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	clog "github.com/charmbracelet/log"

	"github.com/stump-wtf/agent-trace/redact"
)

// historyEndsCap bounds how many finished-but-unscrolled rows the sanitizer
// remembers its fold records for (pushEnd): a screenful is enough for the
// normal case, and a stream that never scrolls is not one joining can help.
const historyEndsCap = 4096

// logLine masks credentials in one history line on its way to disk.
//
// The durable log records what the program printed, so a token-bearing git
// remote or an `Authorization:` header lands in it verbatim. Masking here —
// rather than only when a log is read — is what keeps the secret out of the
// artifact: the file is readable directly at $XDG_STATE_HOME/harness/logs,
// whatever the protocol chooses to serve. Readers mask too, because that is
// the only thing covering logs written before this existed; redact.String is
// idempotent, so a line masked here passes through a reader unchanged.
//
// This is deliberately broader than ADR-0008 as originally written, which
// banned persisting secrets *we* control and disclaimed the ones a harnessed
// program prints. Those are precisely the ones that reach this line.
//
// Cost, measured: ~7.2µs for an ordinary line, ~15.1µs when a rule matches.
// Negligible in steady state; ~0.7s of CPU on this goroutine for a 100k-line
// burst, which backpressures the PTY. That is the trade this makes knowingly.
//
// It is stateful because the log is written a row at a time: a PEM private
// key spans many rows, and only redact.Lines remembers that the row after a
// BEGIN line is key material. Caller holds h.mu.
//
// Governing: ADR-0008 (secrets, as amended for #312), ADR-0007 (the durable
// log is sanitized); issue #312.
func (h *ptyHistory) logLine(ln string) string { return h.mask.String(ln) }

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
	// ends queues, in output order, how each row the cursor has left ended:
	// true when the terminal folded it onto the next row. Rows are written a
	// screenful after the cursor leaves them, so the queue holds roughly one
	// screen of records; head is the next record to consume (popEnd).
	ends []bool
	head int
	// pending holds the text of a folded row whose tail has not been emitted
	// yet; hasPending says whether it holds anything (an empty row is folded
	// too, and folding it away would drop a line break).
	pending    string
	hasPending bool
	// noJoin freezes joining for the rest of the run once the records in ends
	// can no longer be trusted to name the right row (see pushEnd).
	noJoin bool
	// mask carries credential masking across rows (logLine).
	mask redact.Lines
	// released records that the owner is done with this sanitizer and its
	// emulator's reply pump has been ended (release).
	released bool
}

// newPtyHistory builds the sanitizer for a PTY born at cols x rows.
func newPtyHistory(out io.Writer, cols, rows int) *ptyHistory {
	if cols < 1 {
		cols = defaultPTYCols
	}
	if rows < 1 {
		rows = defaultPTYRows
	}
	h := &ptyHistory{out: out, prev: blankScreen(cols, rows)}
	h.startEmulator(cols, rows)
	return h
}

// startEmulator installs a fresh emulator as h.term and starts its reply pump.
// Called from the constructor and, under h.mu, from feedLocked's rebuild.
//
// Scrollback is capped at one row: the sanitizer diffs screens and never
// reads the emulator's scrollback, which by default keeps 10,000 rows of
// 112-byte cells — ~85 MiB at 80 columns, filled by nothing but a long run
// (SPEC-0002 REQ "Emulator Memory"). x/vt ignores sizes below 1, and caps only
// the main screen; the alternate screen's scrollback has no public knob.
//
// A sanitizer that is already released gets its new emulator's pipe closed
// instead of a pump, so a late rebuild neither parks a goroutine nor lets a
// reply block the writer.
func (h *ptyHistory) startEmulator(cols, rows int) {
	term := newEmulator(cols, rows)
	term.SetScrollbackSize(1)
	h.term = term
	if h.released {
		releaseEmulator(term)
		return
	}
	go h.drainReplies(term)
}

// releaseEmulator ends t's reply pump by closing the emulator's input pipe:
// the pump's parked Read returns EOF, and any later reply write fails at once
// instead of blocking. It never touches Emulator.Close, whose unsynchronised
// `closed` flag races a parked Read (stump.wtf/harness#142); io.Pipe is safe
// to close from any goroutine. Closing twice is harmless.
//
// Governing: SPEC-0002 REQ "Emulator Memory"; ADR-0003.
func releaseEmulator(t vt.Terminal) {
	if c, ok := t.InputPipe().(io.Closer); ok {
		_ = c.Close()
	}
}

// release is called by the owner once it is done with the sanitizer — after
// the final Flush — and ends the emulator's reply pump. Until it runs, that
// pump's parked Read keeps the whole emulator reachable after the Supervisor
// has dropped it: its 4 MiB parser buffer and every scrolled row. Unreleased,
// that was ~98 MiB and one goroutine per spawn after a 5 MiB stream-json run
// (https://github.com/stump-wtf/harness/issues/18). Idempotent.
//
// A reader that outlived closeLog's drain bound may still Write afterwards.
// That is safe: the screen still updates, and a query's reply fails against
// the closed pipe rather than blocking the PTY reader.
func (h *ptyHistory) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	releaseEmulator(h.term)
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
// The pump runs until its owner calls release, which closes the emulator's
// input pipe so this Read returns EOF. It must be released, not abandoned: a
// parked pump keeps the whole emulator reachable, and one was parked for
// every spawn the daemon ever made (https://github.com/stump-wtf/harness/issues/18).
// Emulator.Close would also unpark it, but races the parked Read on the
// emulator's unsynchronised `closed` flag (see releaseEmulator).
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
// p is fed to the emulator piece by piece, cutting each line's text apart from
// the newline that ends it and diffing after each piece. Two reasons, both
// about what a screen shift cannot tell you on its own: a burst that arrives in
// one read (a fast `seq`) can scroll many rows at once, and only the
// intermediate screens reveal each scroll step; and a piece whose text has no
// newline in it cannot scroll the screen by printing at all, so any shift it
// does cause is the terminal folding a row — the difference feedLocked records
// to decide whether the rows that vanish are whole lines or halves of one.
// Feeding "text\n" as one piece would let a fold and a newline cancel into a
// single shift that says nothing about either.
func (h *ptyHistory) Write(p []byte) (int, error) {
	n := len(p)
	h.mu.Lock()
	defer h.mu.Unlock()
	for len(p) > 0 {
		// Hand the emulator the text up to the next LF, then the LF itself, so
		// each is diffed as its own kind of step; the rest of p (no LF at all)
		// is one piece.
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			h.feedLocked(p, false)
			break
		}
		if text := p[:i]; len(text) > 0 {
			h.feedLocked(text, false)
		}
		h.feedLocked(p[i:i+1], true)
		p = p[i+1:]
	}
	return n, nil
}

// feedLocked hands one piece to the emulator, records how that piece ended the
// rows it touched, and emits whatever scrolled off, surviving a panic inside
// the emulator. lf says whether the piece is a newline the program wrote (the
// pieces are cut at each LF by Write).
//
// The record for a row that leaves the screen is pushed while the piece is
// processed and consumed when the row is emitted, because those are different
// moments: a scroll finishes the row under the cursor and evicts the row at the
// top of the screen, which the cursor left about a screenful of rows earlier.
// In output order the two line up one per scroll, which is what makes the
// queue (pushEnd/popEnd) the whole bookkeeping: no per-row flags to shift
// around.
//
// A piece of printable bytes that is shorter than the room left on the current
// row cannot reach the last column — each printable byte advances the cursor at
// most one column — so it can neither scroll the screen nor move the cursor
// down a row: it is fed without a diff, leaving the screen to the next piece,
// which keeps an ordinary short line at one screen extraction. A piece with a
// control byte in it does not get the skip: a tab advances several columns per
// byte and CR moves the cursor, so the byte count no longer bounds the columns
// and such a piece can fold while reading as "too short". Any scroll a
// piece does cause is attributed to a fold unless the piece held a newline,
// since the only other way to scroll without writing a newline is an escape
// sequence, and emitLocked rejects a fold whose row does not fill the width.
//
// x/vt's ScrollUp → ultraviolet DeleteLineArea
// indexes past the buffer when a guest sets a scroll region taller than the
// PTY (crush does, rendering a channel doorbell on an 80×24 PTY):
// "index out of range [24] with length 24". Unrecovered, that panic is on
// the PTY reader goroutine and takes the whole daemon — and every harness
// it supervises — down with it (2026-09-12, stump-wtf/harness#1's cousin).
//
// Recovery is reset-first, rebuild-fallback: ESC[r resets DECSTBM to the
// default scroll region (1;height), which heals the poisoned margins. A
// rebuild costs a whole new emulator (a 4 MiB parser buffer before any
// screen), and releases the old one first so its pump does not park, so we
// try reset first and only rebuild when reset cannot heal the state. The
// frame is dropped in either case; the durable log loses at most one screen
// of context.
func (h *ptyHistory) feedLocked(chunk []byte, lf bool) {
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
				// The screen shifted during the panicked write: the diff compares
				// the post-panic screen against a prev that reflected the half-scroll
				// state from before the panic. Resync prev to the current screen so
				// the next write diffs cleanly, and drop the fold records with it:
				// which row each one describes is no longer knowable.
				h.prev = screenText(h.term)
				h.dropEnds()
				_, _ = io.WriteString(h.out, fmt.Sprintf("[harness] recovered from a terminal rendering panic (%v) by resetting scroll region\n", r))
				return
			}
			// Reset itself panicked or did not heal the state. Fall back to a
			// full rebuild, which keeps the guest running when reset cannot
			// fix whatever caused the original panic. Release the old
			// emulator first: its pump would otherwise park forever and keep
			// it reachable alongside its replacement.
			cols, rows := h.term.Width(), h.term.Height()
			if cols < 1 || rows < 1 {
				cols, rows = defaultPTYCols, defaultPTYRows
			}
			releaseEmulator(h.term)
			h.startEmulator(cols, rows)
			h.prev = blankScreen(cols, rows)
			h.dropEnds()
			_, _ = io.WriteString(h.out, fmt.Sprintf("[harness] dropped a frame the terminal emulator could not render (%v); emulator reset\n", r))
		}
	}()

	pos := h.term.CursorPosition()
	esc := bytes.IndexByte(chunk, 0x1b) >= 0
	_, _ = h.term.Write(chunk)
	plain := !bytes.ContainsFunc(chunk, func(r rune) bool { return r < 0x20 })
	if !lf && !esc && plain && len(chunk) < h.term.Width()-pos.X {
		return // too short to have reached the last column: nothing scrolled
	}
	cur := screenText(h.term)
	k := scrollCount(h.prev, cur)
	// How many rows this piece finished, and how they ended. A newline finishes
	// exactly one row, whichever way the cursor got off it. Anything else
	// finishes one row per row that scrolled off, plus — only for a piece with
	// no escape in it, since only then can printing be what moved the cursor
	// down — one per row the cursor moved down over rows the screen still
	// shows. A piece that scrolls without printing at all moved the screen with
	// an escape, not with a fold, so its rows stay whole.
	n := k
	if lf {
		n = 1
	} else if !esc && h.term.CursorPosition().Y > pos.Y {
		n += h.term.CursorPosition().Y - pos.Y
	}
	folded := !lf && (!esc || ansi.Strip(string(chunk)) != "")
	for range n {
		h.pushEnd(folded)
	}
	// A diff that emits more rows than the piece could have pushed records for
	// means the queue has lost its alignment: some earlier piece finished rows
	// without being diffed, so the records ahead no longer name their rows and
	// a fold record can land on a full-width row the program ended with a
	// newline — the wrong join the width test cannot catch. Degrade to one row
	// per line for the rest of the run instead (ADR-0057's fail-safe).
	if k > n {
		h.dropEnds()
	}
	for _, ln := range h.prev[:k] {
		h.emitLocked(ln)
	}
	h.prev = cur
}

// emitLocked writes one row that scrolled off the screen, or that Flush is
// handing over at the end of the stream, gluing it onto the row above when that
// row ended in a terminal fold instead of a newline. soft is the row's own
// record; it only counts when the row really does fill the terminal width,
// because that is what a fold mechanically requires and it is what a record
// that lost its alignment cannot produce. Caller holds h.mu.
func (h *ptyHistory) emitLocked(ln string) {
	folded := h.popEnd() && ln != "" && ansi.StringWidth(ln) == h.term.Width()
	if h.hasPending {
		h.pending += ln
		if !folded {
			h.writePendingLocked()
		}
		return
	}
	if ln == "" {
		return // blank rows carry no history
	}
	if folded {
		h.pending, h.hasPending = ln, true
		return
	}
	h.writeRowLocked(ln)
}

// writeRowLocked appends one finished line to the log, masked (logLine).
func (h *ptyHistory) writeRowLocked(ln string) {
	_, _ = io.WriteString(h.out, h.logLine(ln)+"\n")
}

// writePendingLocked writes the held fold, which is as much of its line as the
// screen has given up.
func (h *ptyHistory) writePendingLocked() {
	if !h.hasPending {
		return
	}
	ln := h.pending
	h.pending, h.hasPending = "", false
	h.writeRowLocked(ln)
}

// pushEnd records how the row the cursor just left ended. The queue is bounded:
// a stream that ends rows without ever scrolling them off (a TUI repainting one
// status line in place) would otherwise grow it for the life of the run, and
// once it is over the cap the records no longer name the rows they belong to,
// so joining stops there and the log falls back to one row per line.
func (h *ptyHistory) pushEnd(folded bool) {
	if h.noJoin {
		return
	}
	if h.head > 0 && h.head*2 >= len(h.ends) {
		h.ends = append(h.ends[:0], h.ends[h.head:]...)
		h.head = 0
	}
	h.ends = append(h.ends, folded)
	if len(h.ends)-h.head > historyEndsCap {
		h.dropEnds()
	}
}

// popEnd returns how the row about to be emitted ended, false when no record
// describes it (which is the same as a newline: no joining).
func (h *ptyHistory) popEnd() bool {
	if h.head >= len(h.ends) {
		h.ends, h.head = nil, 0
		return false
	}
	folded := h.ends[h.head]
	h.head++
	if h.head == len(h.ends) {
		h.ends, h.head = nil, 0
	}
	return folded
}

// dropEnds forgets the fold records and freezes joining: the rows on the screen
// have no records left, so from here on every row is treated as whole.
func (h *ptyHistory) dropEnds() {
	h.ends, h.head, h.pending, h.hasPending, h.noJoin = nil, 0, "", false, true
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
		h.emitLocked(ln)
	}
	h.writePendingLocked()
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
