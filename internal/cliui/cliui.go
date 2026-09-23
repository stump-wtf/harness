// Package cliui is the shared plain-CLI surface for the harness command-line
// tools (the `harness` client today; the daemon's startup/shutdown messages
// tomorrow). It exists for two reasons:
//
//  1. Every fatal error, warning, and success banner across the CLI should
//     look like one product — same palette as the cockpit TUI (SPEC-0001
//     REQ "Zero And Error States"), same calm-ops voice, same graceful
//     degradation to plain text when stderr isn't a TTY or --json is in
//     effect.
//  2. The "daemon isn't running" case is by far the most common error a new
//     user sees, and it deserves an actionable hint ("start it with: harness
//     daemon") rather than a raw `dial unix … no such file or directory`.
//
// Governing: SPEC-0001 REQ "Zero And Error States" (shared voice + palette
// across cockpit and CLI); ADR-0001 (Charmbracelet stack — lipgloss + the
// theme package own the visual language); ADR-0004 (the local Unix socket is
// the transport, so a missing socket is the single most common error).
package cliui

import (
	"errors"
	"fmt"
	"image/color"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/tui/theme"
)

// Level is the severity of a user-facing CLI message. It selects the colour,
// the leading glyph, and the one-word label rendered in the styled block.
type Level int

const (
	// LevelError is a fatal problem; the process will exit non-zero.
	LevelError Level = iota
	// LevelWarn is a non-fatal but noteworthy condition (e.g. a soft
	// degradation the user should know about).
	LevelWarn
	// LevelInfo is a neutral status line (daemon listening, profile loaded).
	LevelInfo
	// LevelSuccess is a positive confirmation (profile activated, reload ok).
	LevelSuccess
)

// String returns the lowercase label used in both the styled block title and
// the plain-text fallback ("harness: error: …", "harness: info: …").
func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelSuccess:
		return "ok"
	default:
		return "note"
	}
}

// Glyph is the leading pictograph for the level (paired with colour so a mono
// terminal still reads it). Exported so other CLI surfaces (the doctor table,
// for example) can render the same status cells without duplicating the
// glyph table.
func (l Level) Glyph() string {
	switch l {
	case LevelError:
		return "✗"
	case LevelWarn:
		return "⚠"
	case LevelInfo:
		return "•"
	case LevelSuccess:
		return "✓"
	default:
		return "•"
	}
}

// Color returns the design-palette colour for the level (coral for errors,
// amber for warns, accent purple for info, mint for success). Exported so the
// doctor table and any other tabular surfaces can colour their cells with the
// same mapping the styled block uses.
func (l Level) Color(p theme.Colors) color.Color {
	switch l {
	case LevelError:
		return p.Coral
	case LevelWarn:
		return p.Amber
	case LevelInfo:
		return p.Accent
	case LevelSuccess:
		return p.Mint
	default:
		return p.Fg
	}
}

// Options configures a Printer. The zero value is a human-friendly Printer
// that writes to os.Stderr and renders styled boxes on a TTY.
type Options struct {
	// JSON forces single-line, ANSI-free output regardless of TTY status.
	// This is the contract --json scripts and log scrapers depend on.
	JSON bool
	// Out is where Report/Fatal write. Defaults to os.Stderr when nil.
	Out io.Writer
}

// Printer renders user-facing CLI messages. It replaces the old package-level
// `json` global — carrying the JSON/tty/output configuration on a value makes
// the package stateless (no racy globals) and lets tests inject their own
// configuration without fighting other parallel tests.
type Printer struct {
	opts Options
	out  io.Writer
}

// NewPrinter builds a Printer from opts. A nil Out defaults to os.Stderr.
func NewPrinter(opts Options) *Printer {
	p := &Printer{opts: opts}
	if opts.Out == nil {
		p.out = os.Stderr
	} else {
		p.out = opts.Out
	}
	return p
}

// Default is the conventional Printer: writes to os.Stderr, JSON off. Callers
// that want to honor --json call SetJSON on Default (or build their own
// Printer via NewPrinter).
var Default = NewPrinter(Options{})

// SetJSON toggles machine-readable mode on the Default printer. main.go calls
// this once after flag parsing. Returns the receiver for chaining.
func SetJSON(on bool) { Default.opts.JSON = on }

// JSON reports whether machine-readable mode is on for the Default printer.
func JSON() bool { return Default.opts.JSON }

// Report renders a user-facing message at the given level on the receiver's
// output. msg is the primary line (already human-readable; do not include
// "harness:" here — Report adds it). hint is an optional actionable follow-up
// ("start it with: harness daemon"); pass "" for none. title is the short
// label shown in the styled block header; pass "" to fall back to the level's
// name.
//
// Output goes to the Printer's writer (so it never collides with --json data
// on stdout). In a TTY it renders a rounded box in the cockpit palette;
// otherwise it falls back to a single plain line.
func (p *Printer) Report(level Level, title, msg, hint string) {
	if msg == "" {
		return
	}
	if title == "" {
		title = level.String()
	}
	if p.opts.JSON || !p.outIsTTY() {
		fmt.Fprintf(p.out, "harness %s: %s\n", title, msg)
		return
	}
	p.renderStyled(level, title, msg, hint)
}

// Fatal reports an error on the Default printer and returns exit code 1 so
// callers can write `os.Exit(cliui.Fatal(err))`. It classifies known error
// shapes (daemon not running, permission denied on the socket, missing config
// file) into friendly messages with hints; anything else is surfaced verbatim,
// stripped of redundant prefixes.
func Fatal(err error) int { return Default.Fatal(err) }

// FatalMsg reports a message at LevelError on the Default printer and returns
// 1. Use it when the caller already has a clean message (no error to
// classify).
func FatalMsg(title, msg, hint string) int { return Default.FatalMsg(title, msg, hint) }

// Fatal reports an error on the receiver and returns exit code 1.
func (p *Printer) Fatal(err error) int {
	if err == nil {
		return 0
	}
	level, title, msg, hint := classify(err)
	p.Report(level, title, msg, hint)
	if level == LevelError {
		return 1
	}
	return 0
}

// FatalMsg reports a message at LevelError on the receiver and returns 1.
func (p *Printer) FatalMsg(title, msg, hint string) int {
	p.Report(LevelError, title, msg, hint)
	return 1
}

// Report renders a message on the Default printer. Convenience wrapper so
// existing top-level `cliui.Report(...)` call sites keep working.
func Report(level Level, title, msg, hint string) {
	Default.Report(level, title, msg, hint)
}

// renderStyled writes the rounded box form of the message. Split out so
// tests can capture output without touching stderr.
//
// The box is always a fixed width — 65% of the terminal, clamped to
// [minBlockWidth, maxBlockWidth] — regardless of message length, so two
// consecutive errors don't render at different widths. Long messages wrap
// inside the box; short ones get padded out.
func (p *Printer) renderStyled(level Level, title, msg, hint string) {
	th := theme.Default()
	pal := th.Colors()
	width := p.blockWidth()

	header := lipgloss.NewStyle().
		Foreground(level.Color(pal)).
		Bold(true).
		Render(fmt.Sprintf("%s harness %s", level.Glyph(), title))

	// Wrap the body to the inner content width (box width minus border + the
	// 1-col padding on each side). lipgloss.Width governs the *content* column
	// count, so the rendered block ends up exactly `width` cells wide.
	innerWidth := width - 4 // 2 border cols + 2 padding cols
	if innerWidth < 10 {
		innerWidth = 10
	}
	body := lipgloss.NewStyle().
		Foreground(pal.Faint).
		Width(innerWidth).
		Render(wordWrap(msg, innerWidth))

	block := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(level.Color(pal)).
		Padding(0, 1).
		Width(width).
		Render(lipgloss.JoinVertical(lipgloss.Left, header, body))

	var out string
	if hint != "" {
		hintLine := lipgloss.NewStyle().
			Foreground(pal.Dim).
			Italic(true).
			Width(width).
			Render("→ " + hint)
		out = lipgloss.JoinVertical(lipgloss.Left, block, "", hintLine)
	} else {
		out = block
	}
	fmt.Fprintln(p.out, out)
}

// Block-width policy. The styled error/warn/info/success box targets 65% of
// the terminal window, clamped so it stays legible on tiny terminals and
// doesn't sprawl on huge ones. When we can't read a width (piped output,
// unknown fd), we fall back to maxBlockWidth — but renderStyled is only
// called on a TTY anyway (Report guards it), so this is just defensive.
const (
	widthRatio    = 0.65
	minBlockWidth = 48
	maxBlockWidth = 80
)

// blockWidth returns the target content width for the styled box, applying
// the 65%-of-terminal policy with floor/ceiling clamps. It keys off the
// Printer's actual output writer (not always stderr): a Printer built with
// Out: os.Stdout reads stdout's width, which is the right answer when the
// caller has wired the Printer to a non-default stream.
func (p *Printer) blockWidth() int {
	if f, ok := p.out.(*os.File); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			target := int(float64(w) * widthRatio)
			if target < minBlockWidth {
				return minBlockWidth
			}
			if target > maxBlockWidth {
				return maxBlockWidth
			}
			return target
		}
	}
	return maxBlockWidth
}

// outIsTTY reports whether the Printer's output writer is a terminal. This
// replaces the old hard-coded `IsTTY(os.Stderr)` check, which made the wrong
// decision whenever a Printer was built with a non-stderr Out (e.g. tests
// writing to a buffer, or a future caller writing reports to a file). A
// non-*os.File writer is never a TTY.
func (p *Printer) outIsTTY() bool {
	if f, ok := p.out.(*os.File); ok {
		return IsTTY(f)
	}
	return false
}

// WriterIsTTY reports whether w is an *os.File attached to a terminal.
// Exported so other CLI surfaces (cmd/harness's table renderer, which writes
// to either stdout or stderr depending on the verb) can make the same
// per-writer styling decision Report makes internally, instead of always
// consulting stderr.
func WriterIsTTY(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		return IsTTY(f)
	}
	return false
}

// wordWrap breaks s into lines no longer than width visible runes, splitting
// on spaces. Long words are broken at the width boundary rather than
// overflowing. This keeps the box tidy for verbose error messages (e.g. full
// file paths in a "no such file" error) without pulling in a wrapping
// dependency.
//
// Width math counts runes, not bytes, so non-ASCII text (paths with accented
// letters, CJK) wraps at the right column instead of mid-rune (PR #23 nit).
// This is approximate for combining/wide glyphs — those need display-width
// math (unicode/vpt, lipgloss.Width) which is overkill for a styled error
// block; rune-count is correct for the common cases and never corrupts a
// multibyte sequence.
func wordWrap(s string, width int) string {
	if width < 1 {
		return s
	}
	var (
		out   strings.Builder
		line  []rune
		word  []rune
		flush = func() {
			if len(line) > 0 {
				out.WriteString(string(line))
				out.WriteByte('\n')
				line = line[:0]
			}
		}
	)
	for _, r := range s {
		switch r {
		case ' ', '\t':
			if len(line)+1+len(word) > width && len(line) > 0 {
				flush()
			} else if len(line) > 0 {
				line = append(line, ' ')
			}
			line = append(line, word...)
			word = word[:0]
		case '\n':
			line = append(line, word...)
			word = word[:0]
			flush()
		default:
			word = append(word, r)
			// Hard-break a word longer than the entire width.
			if len(word) > width && len(line) == 0 {
				line = append(line, word[:width]...)
				word = word[width:]
				flush()
			}
		}
	}
	if len(word) > 0 {
		if len(line)+1+len(word) > width && len(line) > 0 {
			flush()
		} else if len(line) > 0 {
			line = append(line, ' ')
		}
		line = append(line, word...)
	}
	flush()
	// Trim the trailing newline so lipgloss doesn't render an empty line.
	if strings.HasSuffix(out.String(), "\n") {
		return strings.TrimSuffix(out.String(), "\n")
	}
	return out.String()
}

// classify turns an error into (level, title, message, hint). Known shapes
// get a friendly rewrite and a short title describing the *what* (used in
// the styled block header); unknown errors pass through at LevelError with
// the message cleaned of redundant "harness:" / "client: " prefixes and a
// generic title.
func classify(err error) (Level, string, string, string) {
	// PermissionDenied is checked before DaemonDown because an EACCES on the
	// socket is a different problem (stale socket / wrong user) than ENOENT
	// (daemon not running).
	switch {
	case isPermissionDenied(err):
		return LevelError, "permission denied",
			"can't access the harness daemon socket.",
			"the socket may be stale — stop any old harness daemon and restart it"
	case isNotAnswering(err):
		return daemonNotAnsweringDetail(err)
	case isDaemonDown(err):
		return daemonDownDetail(err)
	case isMissingConfig(err):
		path := pathFromError(err)
		return LevelError, "no config file",
			fmt.Sprintf("harness config not found%s.", where(path)),
			"create one (see `harness daemon -h`) or pass --config PATH"
	case isNoProject(err):
		return LevelError, "no project file",
			cleanMessage(err.Error()),
			noProjectHint(err)
	default:
		return LevelError, "error", cleanMessage(err.Error()), ""
	}
}

// Unreachable Daemon Messages
//
// "can't reach the daemon" is three states, not one, and the single message
// they used to share asserted the daemon was not running. On tars that was
// false for twenty minutes — the daemon was up and supervising with its socket
// deleted — and the message sent the operator to `systemctl start` on a
// running unit (#578).
//
// The distinguishing fact goes in the MESSAGE, not the hint: Report's non-TTY
// form prints "harness <title>: <message>" and drops the hint entirely, and
// scripts and agents are exactly the callers who cannot afford to be told the
// daemon is down when it isn't.
//
// @joestump 09/22/2026 - Split out of classify for issue #578.

// daemonDownDetail names which dial failure happened. ENOENT means only that
// the socket file is gone, which says nothing about the process; ECONNREFUSED
// means the file is there and no daemon has it open, which is the one case
// where "not running" is true.
func daemonDownDetail(err error) (Level, string, string, string) {
	socket := socketFromError(err)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return LevelError, "daemon socket missing",
			fmt.Sprintf("no socket%s. That is the socket file, not the daemon: it may still be running and supervising.%s",
				where(socket), runtimeDirNote(socket)),
			"if no daemon is running, start it with: harness daemon — check first (pgrep -fl 'harness daemon'): a running daemon re-creates a deleted socket within a second, so one that stays missing is usually a stopped daemon or an older build"
	case errors.Is(err, syscall.ECONNREFUSED):
		return LevelError, "daemon not running",
			fmt.Sprintf("nothing is listening%s — the socket file is there, but no daemon has it open.", where(socket)),
			"start it with: harness daemon"
	default:
		return LevelError, "daemon unreachable",
			fmt.Sprintf("can't reach the harness daemon%s: %s", where(socket), cleanMessage(err.Error())),
			"check whether it is running (systemctl --user status harness) before starting another"
	}
}

// daemonNotAnsweringDetail is the third state: the connection was accepted, so
// something is bound to that socket, and then no HELLO came back. Telling the
// operator to start one would add a second daemon to a box that already has
// one.
//
// It says which of the two shapes it saw, because they point different ways.
// Holding the connection open in silence until the handshake deadline is a
// daemon that is running and not serving. Hanging up is what a daemon does
// while it shuts down (Close refuses new connections), so calling that one
// "stuck" and prescribing a restart would be a guess dressed as a finding.
func daemonNotAnsweringDetail(err error) (Level, string, string, string) {
	var silent *client.NoHandshakeError
	if !errors.As(err, &silent) {
		silent = &client.NoHandshakeError{}
	}
	if silent.TimedOut() {
		return LevelError, "daemon not answering",
			fmt.Sprintf("the daemon%s accepted the connection but sent nothing for %s — it is running, and not serving.",
				where(silent.Socket), client.HandshakeTimeout),
			"read its log, then restart it: systemctl --user restart harness"
	}
	return LevelError, "daemon not answering",
		fmt.Sprintf("the daemon%s accepted the connection and hung up without answering — it may be shutting down or restarting.",
			where(silent.Socket)),
		"retry in a moment; if it keeps happening, read the daemon's log"
}

// isNotAnswering reports whether err is the bound-but-silent state.
func isNotAnswering(err error) bool {
	var silent *client.NoHandshakeError
	return errors.As(err, &silent)
}

// runtimeDirNote warns about the trap where the CLIENT resolved a different
// default socket path than the daemon did. DefaultSocketPath falls back to the
// state home when $XDG_RUNTIME_DIR is unset (ADR-0008), and it is routinely
// unset in a non-login shell while set in the session systemd runs the daemon
// in — the same "socket missing" error for a completely different reason.
//
// Only for a path the client defaulted to: an explicit --socket that does not
// exist is exactly what the operator asked for, and telling them about the
// fallback there would be noise about a path they never used.
func runtimeDirNote(socket string) string {
	if os.Getenv("XDG_RUNTIME_DIR") != "" || socket != protocol.DefaultSocketPath() {
		return ""
	}
	return " ($XDG_RUNTIME_DIR is unset in this shell, so this is the fallback path; a daemon running where it is set listens somewhere else — pass --socket PATH.)"
}

// isDaemonDown reports whether err looks like a failed dial of the Unix
// socket: ENOENT (no socket file), ECONNREFUSED (nothing listening), or an
// fs.PathError naming a ".sock" path. This is the signature of "the daemon
// isn't running" regardless of which OS reports it how.
func isDaemonDown(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		s := strings.ToLower(pathErr.Path)
		if strings.HasSuffix(s, ".sock") || strings.Contains(s, "harness.sock") {
			return true
		}
	}
	if strings.Contains(err.Error(), "no such file or directory") &&
		strings.Contains(strings.ToLower(err.Error()), ".sock") {
		return true
	}
	return false
}

// isPermissionDenied sniffs for EACCES/EPERM on the socket path. The typed
// check uses errors.Is so it matches syscall.EACCES as well as the wrapped
// os.ErrPermission sentinel (PR #23 nit: the old `pathErr.Err == os.ErrPermission`
// direct compare missed EACCES on Linux).
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && errors.Is(pathErr.Err, os.ErrPermission) {
		return true
	}
	// Some dial errors wrap syscall.EACCES directly without a PathError.
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "permission denied") && strings.Contains(msg, ".sock")
}

// IsMissingConfig reports whether err is a "config file not found" — a
// *os.PathError (or wrapped fs.ErrNotExist) naming a .toml path. This is
// what `harness daemon` emits on first run before the user has created
// harness.toml. Exported so the doctor command can classify the same shape
// without re-implementing the sniff.
func IsMissingConfig(err error) bool {
	if err == nil {
		return false
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		if !errors.Is(pathErr.Err, os.ErrNotExist) {
			return false
		}
		s := strings.ToLower(pathErr.Path)
		return strings.HasSuffix(s, ".toml") || strings.Contains(s, "harness.toml")
	}
	if errors.Is(err, os.ErrNotExist) {
		msg := strings.ToLower(err.Error())
		return strings.Contains(msg, ".toml") || strings.Contains(msg, "harness.toml")
	}
	return false
}

// isMissingConfig is the private alias kept for classify's switch.
func isMissingConfig(err error) bool { return IsMissingConfig(err) }

// isNoProject reports whether err is the config.ErrNoProjectFound sentinel —
// a project verb (up/down/ps) ran outside any project directory. Matched via
// errors.Is so verb-context wraps (SPEC-0004 REQ "Error Handling Standards")
// pass through.
func isNoProject(err error) bool {
	return errors.Is(err, config.ErrNoProjectFound)
}

// noProjectHint picks the actionable follow-up for a no-project-file error.
// `harness down` has an escape hatch (the explicit PROJECT positional, for the
// "I deleted the project file first" case), so its hint names it; the other
// project verbs can only be run inside a project.
func noProjectHint(err error) string {
	if strings.Contains(err.Error(), "harness down") {
		return "pass the project explicitly: harness down PROJECT"
	}
	return "run this inside a project directory (one with a harness.toml)"
}

// pathFromError pulls the path out of an *os.PathError if present.
func pathFromError(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Path
	}
	return ""
}

// socketFromError pulls the socket path out of a net.OpError / os.PathError
// payload if present, else returns "". Used to make the daemon-down message
// specific ("at /run/user/1000/harness.sock") without leaking noise.
func socketFromError(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if addr := opErr.Addr; addr != nil {
			return addr.String()
		}
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Path
	}
	return ""
}

// where formats an " at <path>" suffix when socket is non-empty.
func where(socket string) string {
	if socket == "" {
		return ""
	}
	return " at " + socket
}

// cleanMessage strips redundant prefixes the wrapping packages prepend so the
// user-facing message is the actual content, not the wrap chain.
func cleanMessage(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "harness: ")
	s = strings.TrimPrefix(s, "harness daemon: ")
	s = strings.TrimPrefix(s, "client: ")
	return s
}

// IsTTY reports whether f is a terminal. Exposed so other CLI surfaces (e.g.
// `harness doctor`'s tabular report) can make the same TTY/no-TTY styling
// decision Report makes internally. We use golang.org/x/term (already an
// indirect dep via bubbletea/x/term) rather than isatty directly.
func IsTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}
