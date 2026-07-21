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
	"io"
	"net"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
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
func (l Level) Color(p theme.Palette) lipgloss.AdaptiveColor {
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
	if p.opts.JSON || !IsTTY(os.Stderr) {
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
	pal := th.Palette
	width := blockWidth()

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
// the 65%-of-terminal policy with floor/ceiling clamps.
func blockWidth() int {
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
		target := int(float64(w) * widthRatio)
		if target < minBlockWidth {
			return minBlockWidth
		}
		if target > maxBlockWidth {
			return maxBlockWidth
		}
		return target
	}
	return maxBlockWidth
}

// wordWrap breaks s into lines no longer than width, splitting on spaces.
// Long words are broken at the width boundary rather than overflowing. This
// keeps the box tidy for verbose error messages (e.g. full file paths in a
// "no such file" error) without pulling in a wrapping dependency.
func wordWrap(s string, width int) string {
	if width < 1 {
		return s
	}
	var (
		out   strings.Builder
		line  strings.Builder
		word  strings.Builder
		flush = func() {
			if line.Len() > 0 {
				out.WriteString(line.String())
				out.WriteByte('\n')
				line.Reset()
			}
		}
	)
	for _, r := range s {
		switch r {
		case ' ', '\t':
			if line.Len()+1+word.Len() > width && line.Len() > 0 {
				flush()
			} else if line.Len() > 0 {
				line.WriteByte(' ')
			}
			line.WriteString(word.String())
			word.Reset()
		case '\n':
			line.WriteString(word.String())
			word.Reset()
			flush()
		default:
			word.WriteRune(r)
			// Hard-break a word longer than the entire width.
			if word.Len() > width && line.Len() == 0 {
				line.WriteString(word.String()[:width])
				rest := word.String()[width:]
				word.Reset()
				word.WriteString(rest)
				flush()
			}
		}
	}
	if word.Len() > 0 {
		if line.Len()+1+word.Len() > width && line.Len() > 0 {
			flush()
		} else if line.Len() > 0 {
			line.WriteByte(' ')
		}
		line.WriteString(word.String())
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
	case isDaemonDown(err):
		socket := socketFromError(err)
		return LevelError, "daemon not running",
			fmt.Sprintf("can't reach the harness daemon%s.", where(socket)),
			"start it with: harness daemon"
	case isMissingConfig(err):
		path := pathFromError(err)
		return LevelError, "no config file",
			fmt.Sprintf("harness config not found%s.", where(path)),
			"create one (see `harness daemon -h`) or pass --config PATH"
	default:
		return LevelError, "error", cleanMessage(err.Error()), ""
	}
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

// isPermissionDenied sniffs for EACCES/EPERM on the socket path.
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Err == os.ErrPermission {
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
