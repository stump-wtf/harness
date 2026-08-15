package tui

// Governing: SPEC-0001 REQ "Attached Mode" — render the harness's real terminal
// from the daemon's x/vt screen: colors, cursor, and TUI apps inside it all
// work (ADR-0003, embedded terminal pane). The daemon streams a screen snapshot
// then live ATTACH_DATA bytes (SPEC-0002 REQ "Attach Session"); we feed those
// into a CLIENT-side x/vt emulator and render its cell grid into lines Bubble
// Tea prints. Because both ends run the same emulator, a full-screen TUI app
// (colors + cursor) reproduces faithfully.

import (
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// vtView is a client-side embedded terminal: an x/vt emulator fed the daemon's
// ATTACH_DATA byte stream, rendered on demand into styled lines.
type vtView struct {
	term vt.Terminal
	cols int
	rows int
	// cursorHidden shadows the emulator's DECTCEM state. The x/vt Terminal
	// interface exposes no reader for it, so it has to be reconstructed from
	// callbacks — see newVTView for why one callback isn't enough.
	cursorHidden bool
	// closeOnce guards the reply-pump teardown; close is called from several
	// unrelated paths (detach, hop, peek replay) and must be idempotent.
	closeOnce sync.Once
}

// newVTView creates an embedded terminal of the given size.
func newVTView(cols, rows int) *vtView {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	v := &vtView{term: vt.NewEmulator(cols, rows), cols: cols, rows: rows}
	// CursorVisibility alone is not enough to shadow DECTCEM: x/vt only fires
	// it when Cursor.Hidden actually flips, and a full reset (RIS, "\x1bc" —
	// what `reset`/`tput reset` and many TUIs on exit emit) clears the screen's
	// cursor to visible *before* re-applying the modes, so the re-applied ?25h
	// is a no-op change and never announces itself. Without the mode callbacks
	// below, a guest that hid its cursor and then reset would leave this view
	// convinced the cursor is still hidden — permanently, until the guest
	// happens to toggle ?25 again. EnableMode/DisableMode fire unconditionally,
	// so they catch that re-application; CursorVisibility is still needed for
	// the alt-screen switch, which changes the effective cursor without a mode
	// change.
	v.term.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { v.cursorHidden = !visible },
		EnableMode: func(m ansi.Mode) {
			if m == ansi.ModeTextCursorEnable {
				v.cursorHidden = false
			}
		},
		DisableMode: func(m ansi.Mode) {
			if m == ansi.ModeTextCursorEnable {
				v.cursorHidden = true
			}
		},
	})
	go v.pumpReplies()
	return v
}

// pumpReplies drains the emulator's reply pipe for the view's lifetime.
//
// Several CSI handlers (DECRQM mode reports, cursor position reports, device
// attributes) synthesize a reply and write it into an internal io.Pipe,
// expecting the caller to drain the paired reader. That pipe is unbuffered, so
// the write blocks until something reads it — and write() is called from
// Update, on the attachDataMsg branch. Bubble Tea runs Update on a single event
// loop, so blocking there wedges the whole program: no keystrokes, no Ctrl-C,
// and no terminal-capability handshake, with the last frame still on screen so
// the session looks alive. The client then can't be detached or even SIGTERMed,
// and the abandoned process keeps its attach session registered, clamping the
// harness PTY for every other client (stump.wtf/harness#183).
//
// A shell guest never queries the terminal, so this only fires against a real
// agent TUI — which is every guest that matters here.
//
// The replies are DISCARDED rather than forwarded. The daemon's Mux drains its
// own emulator and forwards the reply to the real PTY (internal/attach/mux.go,
// pumpReplies), and it is the authoritative responder: it is the end actually
// attached to the guest's terminal. Every attached client mirrors the same byte
// stream through its own emulator, so forwarding here too would answer a single
// query once per client, and the surplus replies would land in the guest as
// spurious input.
//
// Governing: ADR-0003 (client-side emulator mirrors the daemon's screen), and
// the daemon-side precedent in f03e493.
func (v *vtView) pumpReplies() {
	buf := make([]byte, 1024)
	for {
		if _, err := v.term.Read(buf); err != nil {
			return
		}
	}
}

// close tears down the emulator and, with it, the reply pump. Emulator.Close
// closes the pipe, so the parked Read returns an error and the goroutine exits;
// a later write() is a no-op rather than a panic. Safe to call more than once,
// and required wherever a view is discarded — a hop and a detach both replace
// or drop the attached view, and the peek pane builds a throwaway view per
// replay, so leaking one goroutine per view would be a steady drip.
func (v *vtView) close() {
	v.closeOnce.Do(func() { _ = v.term.Close() })
}

// resize resizes the emulator (the client viewport changed; smallest-attached-
// wins is enforced server-side, but the local emulator must match what the
// daemon renders into).
func (v *vtView) resize(cols, rows int) {
	if cols < 1 || rows < 1 || (cols == v.cols && rows == v.rows) {
		return
	}
	v.cols, v.rows = cols, rows
	v.term.Resize(cols, rows)
}

// write feeds raw terminal bytes (the ATTACH_DATA payload) into the emulator.
func (v *vtView) write(p []byte) {
	if len(p) > 0 {
		_, _ = v.term.Write(p)
	}
}

// render serializes the current screen into styled lines joined by newlines,
// suitable for embedding in a Bubble Tea view. Each cell emits an SGR sequence
// only when the style changes (compact), and every line resets attributes at
// its end so a truncated line can't bleed color into the chrome around it.
//
// The emulator's cursor is painted into the grid as a reverse-video cell
// (#48): Bubble Tea owns the hardware cursor — it hides it for the program's
// lifetime and re-parks it after every flush — so the only way to show the
// guest cursor is to draw it as cell content. The guest's DECTCEM state is
// respected: a program that hides its cursor (\x1b[?25l) gets no painted
// cursor either, tracked via the CursorVisibility callback.
func (v *vtView) render() string { return v.renderScreen(true) }

// renderNoCursor is render without the painted cursor cell — for frozen
// frames (the scrollback snapshot, #50), where a reverse-video cursor cell
// would masquerade as live state in a view that has no live cursor to show.
func (v *vtView) renderNoCursor() string { return v.renderScreen(false) }

// renderScreen is the shared implementation behind render / renderNoCursor.
func (v *vtView) renderScreen(paintCursor bool) string {
	w, h := v.term.Width(), v.term.Height()
	cur := v.term.CursorPosition()
	showCursor := paintCursor && !v.cursorHidden
	var lines []string
	for y := 0; y < h; y++ {
		var b strings.Builder
		prevSeq := ""
		skip := 0
		for x := 0; x < w; x++ {
			if skip > 0 {
				skip--
				continue
			}
			atCursor := showCursor && x == cur.X && y == cur.Y
			cell := v.term.CellAt(x, y)
			if cell == nil {
				if atCursor {
					b.WriteString("\x1b[0m\x1b[7m \x1b[0m")
					// The reset above invalidated the tracked run; force the
					// next styled cell to restate its sequence.
					prevSeq = "\x00"
				} else {
					b.WriteByte(' ')
				}
				continue
			}
			if atCursor {
				// Cursor cell: the cell's own style plus reverse video,
				// closed immediately so the inversion can't bleed.
				b.WriteString("\x1b[0m")
				if seq := cell.Style.String(); seq != "" {
					b.WriteString(seq)
				}
				b.WriteString("\x1b[7m")
			} else if seq := cell.Style.String(); seq != prevSeq {
				b.WriteString("\x1b[0m")
				if seq != "" {
					b.WriteString(seq)
				}
				prevSeq = seq
			}
			s := cell.String()
			if s == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(s)
				if cell.Width > 1 {
					skip = cell.Width - 1
				}
			}
			if atCursor {
				b.WriteString("\x1b[0m")
				prevSeq = "\x00"
			}
		}
		b.WriteString("\x1b[0m")
		lines = append(lines, b.String())
	}
	return strings.Join(lines, "\n")
}

// peekCache memoizes the vt-emulator replay of the peek tail so viewPeek
// doesn't rebuild it on every frame. The replay is invalidated when the tail
// text changes (new peek fetch) or when the pane dimensions change (resize).
type peekCache struct {
	tailHash uint64 // FNV-1a of the tail text; zero means empty
	cols     int
	rows     int
	screen   string // last renderNoCursor output
}

// render returns the cached screen for the given tail and dimensions, or
// rebuilds it by replaying the tail through a fresh vt emulator.
func (pc *peekCache) render(tail string, cols, rows int) string {
	h := fnvHash(tail)
	if pc.screen != "" && pc.tailHash == h && pc.cols == cols && pc.rows == rows {
		return pc.screen
	}
	pv := newVTView(cols, rows)
	// The replay view is thrown away at the end of this call; close it so its
	// reply pump doesn't outlive it. The tail is guest output and can carry the
	// same queries the attached stream does, so this path needs the drain too.
	defer pv.close()
	pv.write([]byte(tail))
	pc.tailHash = h
	pc.cols = cols
	pc.rows = rows
	pc.screen = pv.renderNoCursor()
	return pc.screen
}

// fnvHash is a cheap non-cryptographic hash for cache invalidation.
func fnvHash(s string) uint64 {
	const offset uint64 = 14695981039346656037
	const prime uint64 = 1099511628211
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}
