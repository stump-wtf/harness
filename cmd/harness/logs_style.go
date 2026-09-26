package main

// Styled Logs
//
// `harness logs` on a terminal, in the visual language of `harness list` and
// the cockpit: faint timestamps, the label column as coloured badges, lifecycle
// states as their SPEC-0003 glyph and colour, errors in the error colour, and a
// run of identical lines collapsed to one line with a count — a provider loop
// that logs the same error a thousand times reads as one line, not a thousand.
// The raw view re-renders the daemon's own charmbracelet/log lines the same way
// and leaves the agent's output between them exactly as it was.
//
// Styling is a terminal-only concern. A pipe, a file, --json or an agent
// reading stdout gets the plain renderers in logs.go and verbs.go byte for
// byte: a nil *logStyle selects them, and every styled path is keyed off one.
// NO_COLOR and degraded terminals are the theme's business — it resolves every
// colour through the detected colour profile, so under NO_COLOR the glyphs and
// words remain and the colour goes. The durable log on disk is untouched: the
// raw view parses newEventLogger's format read-only, and ReadLifecycle's
// parse of the same lines is the contract that format keeps.
//
// Governing: SPEC-0002 REQ "Control Operations" ("logs"), SPEC-0001 REQ
// "State Presentation" (paired glyph + colour, legible in mono), SPEC-0001 REQ
// "Zero And Error States" (one palette across cockpit and CLI), ADR-0001
// (Charmbracelet stack; lipgloss + the theme own the visual language),
// ADR-0007 (lifecycle events are charmbracelet/log lines in the durable log).
//
// @joestump-agent 09/26/2026 - Added: `harness logs` looked like crap on a
// terminal.

import (
	"fmt"
	"image/color"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/tui/theme"
)

// logStyle renders the styled forms. A nil *logStyle means plain output.
type logStyle struct {
	th *theme.Theme
	c  theme.Colors
}

func newLogStyle(th *theme.Theme) *logStyle {
	return &logStyle{th: th, c: th.Colors()}
}

// logStyleFor is the style for output written to w: nil — plain — unless w is
// a terminal and --json is off, the same rule the tables follow.
func logStyleFor(w io.Writer) *logStyle {
	if !useColorFor(w) {
		return nil
	}
	return newLogStyle(theme.Default())
}

func (s *logStyle) paint(c color.Color, bold bool, str string) string {
	if str == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(c).Bold(bold).Render(str)
}

func (s *logStyle) faint(str string) string { return s.paint(s.c.Faint, false, str) }

// --- the activity view -----------------------------------------------------

// activityView prints the activity view, plain or styled. The styled view
// collapses consecutive identical entries; a live one (--follow) does it in
// place, redrawing the open group's line as repeats arrive.
type activityView struct {
	w  io.Writer
	st *logStyle
	// live redraws the open group in place rather than printing it on flush.
	live bool
	// cols is the terminal width, for knowing how many rows a line took; 0
	// when unknown, which turns in-place redraws off.
	cols func() int
	grp  *entryGroup
	// rows is how many terminal rows the open group's line took, 0 when it
	// cannot be redrawn.
	rows int
}

// entryGroup is a run of consecutive entries that print identically but for
// their clock.
type entryGroup struct {
	e     protocol.LogEntry
	f     entryFields
	key   string
	count int
	last  string
}

func newActivityView(w io.Writer, st *logStyle) *activityView {
	return &activityView{w: w, st: st, cols: func() int { return termCols(w) }}
}

// termCols is w's terminal width, or 0.
func termCols(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		if c, _, err := term.GetSize(int(f.Fd())); err == nil && c > 0 {
			return c
		}
	}
	return 0
}

func (v *activityView) header(r protocol.LogRun) {
	if v.st == nil {
		fmt.Fprintln(v.w, runHeader(r))
		return
	}
	v.close()
	fmt.Fprintln(v.w, v.st.runHeader(r))
}

func (v *activityView) note(n string) {
	if v.st == nil {
		fmt.Fprintln(v.w, noteLine(n))
		return
	}
	v.close()
	fmt.Fprintln(v.w, v.st.noteLine(n))
}

func (v *activityView) entry(e protocol.LogEntry) {
	if v.st == nil {
		fmt.Fprintln(v.w, formatEntry(e))
		return
	}
	f := entryParts(e)
	key := strings.Join([]string{e.Kind, f.label, f.detail, f.suffix}, "\x00")
	if g := v.grp; g != nil && g.key == key && (!v.live || v.rows > 0) {
		g.count++
		g.last = f.clock
		if v.live {
			// Up to the first row of the group's line, clear to the end of
			// the screen, and draw it again with the new count.
			fmt.Fprintf(v.w, "\x1b[%dA\r\x1b[J", v.rows)
			v.draw()
		}
		return
	}
	v.close()
	v.grp = &entryGroup{e: e, f: f, key: key, count: 1, last: f.clock}
	if v.live {
		v.draw()
	}
}

// draw prints the open group's line and records how many rows it took.
func (v *activityView) draw() {
	line := v.st.groupLine(v.grp)
	fmt.Fprintln(v.w, line)
	v.rows = rowsFor(line, v.cols())
}

// flush prints the open group, if the view has not already drawn it. A live
// view keeps its group open across polls, so a repeat in the next poll still
// joins it.
func (v *activityView) flush() {
	if v.live {
		return
	}
	v.close()
}

// close ends the open group, printing it unless it is already on screen.
func (v *activityView) close() {
	if v.grp != nil && !v.live {
		fmt.Fprintln(v.w, v.st.groupLine(v.grp))
	}
	v.grp, v.rows = nil, 0
}

// rowsFor is how many terminal rows line takes at cols columns, or 0 when the
// width is unknown.
func rowsFor(line string, cols int) int {
	if cols <= 0 {
		return 0
	}
	w := lipgloss.Width(line)
	if w == 0 {
		return 1
	}
	return (w + cols - 1) / cols
}

// runHeader is runHeader, styled: the run in accent, a live run as the
// running state, a failing exit in the error colour, the workdir faint.
func (s *logStyle) runHeader(r protocol.LogRun) string {
	f := runParts(r)
	head := s.paint(s.c.Accent, true, "run") + " "
	if !f.parsed {
		return head + s.faint(f.start)
	}
	sep := s.faint(" · ")
	b := head + s.paint(s.c.Fg, true, f.start) + s.faint(" → ")
	if f.end != "" {
		b += s.paint(s.c.Fg, false, f.end)
	} else {
		b += s.th.RenderState(core.StateRunning)
	}
	if f.exit != nil {
		c := s.c.Mint
		if *f.exit != 0 {
			c = s.c.Coral
		}
		b += sep + s.paint(c, true, fmt.Sprintf("exit %d", *f.exit))
	}
	if f.adapter != "" {
		b += sep + s.paint(s.c.Cyan, true, f.adapter)
	}
	if f.workdir != "" {
		b += sep + s.faint(f.workdir)
	}
	return b
}

// noteLine is noteLine, styled.
func (s *logStyle) noteLine(n string) string {
	label := lipgloss.NewStyle().Foreground(s.c.Faint).Italic(true).Render("note")
	return fmt.Sprintf("%8s  %s%s  %s", "", label, pad("note", 8), s.paint(s.c.Dim, false, inert(n)))
}

// groupLine is one entry, or a collapsed run of them: the first clock, the
// badge, then — for a run — its count and span before the detail, so the
// count stays in view however long the detail is.
func (s *logStyle) groupLine(g *entryGroup) string {
	c, bold := s.labelColor(g.e)
	var b strings.Builder
	b.WriteString(s.faint(g.f.clock))
	b.WriteString("  ")
	b.WriteString(s.paint(c, bold, g.f.label))
	b.WriteString(pad(g.f.label, 8))
	b.WriteString("  ")
	if g.count > 1 {
		b.WriteString(s.paint(s.c.Amber, true, fmt.Sprintf("×%d", g.count)))
		if g.last != g.f.clock {
			b.WriteString(" ")
			b.WriteString(s.faint(g.f.clock + "–" + g.last))
		}
		b.WriteString("  ")
	}
	b.WriteString(s.detail(g.e, g.f.detail))
	if g.f.suffix != "" {
		b.WriteString("  ")
		b.WriteString(s.paint(s.c.Coral, false, strings.TrimSpace(g.f.suffix)))
	}
	return b.String()
}

// pad is the spaces that bring label to width, measured before styling.
func pad(label string, width int) string {
	if n := width - utf8.RuneCountInString(label); n > 0 {
		return strings.Repeat(" ", n)
	}
	return ""
}

// labelColor picks a badge colour by what the entry is: lifecycle in accent,
// the session in pink, reads cyan, edits amber, commands mint, errors coral,
// anything unclassified dim.
func (s *logStyle) labelColor(e protocol.LogEntry) (color.Color, bool) {
	switch e.Kind {
	case protocol.LogEntryLifecycle:
		switch {
		case e.Action == "flapping":
			return s.c.Amber, true
		case e.Error:
			return s.c.Coral, true
		}
		return s.c.Accent, true
	case protocol.LogEntrySession:
		return s.c.Pink, true
	case protocol.LogEntryTool:
		switch e.Action {
		case "read", "search":
			return s.c.Cyan, true
		case "edit":
			return s.c.Amber, true
		case "exec", "verify":
			return s.c.Mint, true
		}
		return s.c.Dim, true
	case protocol.LogEntryMark:
		switch e.Action {
		case "error":
			return s.c.Coral, true
		case "user-message", "user":
			return s.c.Accent, true
		}
	}
	return s.c.Dim, true
}

// detail styles an entry's detail: a state change as two coloured states, an
// exit code by success, an error in the error colour, a path with its
// directory dimmed. Anything else prints as it is.
func (s *logStyle) detail(e protocol.LogEntry, d string) string {
	switch e.Kind {
	case protocol.LogEntryLifecycle:
		switch e.Action {
		case "state":
			if from, to, ok := strings.Cut(d, " → "); ok && core.State(from).Valid() && core.State(to).Valid() {
				return s.th.RenderState(core.State(from)) + s.faint(" → ") + s.th.RenderState(core.State(to))
			}
		case "exited":
			if k, val, ok := strings.Cut(d, "="); ok {
				c := s.c.Mint
				if e.Error {
					c = s.c.Coral
				}
				return s.faint(k+"=") + s.paint(c, true, val)
			}
		case "flapping":
			return s.paint(s.c.Amber, false, d)
		}
	case protocol.LogEntrySession:
		parts := strings.Split(d, " · ")
		parts[0] = s.paint(s.c.Fg, true, parts[0])
		return strings.Join(parts, s.faint(" · "))
	case protocol.LogEntryMark:
		if e.Action == "error" {
			return s.paint(s.c.Coral, false, d)
		}
	case protocol.LogEntryTool:
		if e.Action == "read" || e.Action == "edit" {
			if i := strings.LastIndexByte(d, '/'); i >= 0 && i < len(d)-1 {
				return s.paint(s.c.Dim, false, d[:i+1]) + d[i+1:]
			}
		}
	}
	return d
}

// --- the raw view ------------------------------------------------------------

// rawWriter writes durable-log text, which arrives whole or — under --follow —
// as appended suffixes that can split a line. Styled, it re-renders each
// daemon line that starts at a line start and passes everything else through;
// plain, it is a plain write.
type rawWriter struct {
	w  io.Writer
	st *logStyle
	// mid is true when the last write ended inside a line, so the next one
	// starts with that line's continuation, not a line of its own.
	mid bool
}

func newRawWriter(w io.Writer, st *logStyle) *rawWriter { return &rawWriter{w: w, st: st} }

func (r *rawWriter) write(s string) {
	if s == "" {
		return
	}
	if r.st == nil {
		io.WriteString(r.w, s)
		return
	}
	var b strings.Builder
	for s != "" {
		seg, nl := s, false
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			seg, nl, s = s[:i], true, s[i+1:]
		} else {
			s = ""
		}
		if r.mid {
			b.WriteString(seg)
		} else {
			b.WriteString(r.st.rawLine(seg))
		}
		if nl {
			b.WriteByte('\n')
		}
		r.mid = !nl
	}
	io.WriteString(r.w, b.String())
}

// restart begins a reprinted tail on a line of its own. The plain view glues
// the reprint to whatever line it left open, and keeps doing so: a pipe reads
// the bytes it always has.
func (r *rawWriter) restart() {
	if r.st != nil && r.mid {
		io.WriteString(r.w, "\n")
		r.mid = false
	}
}

// daemonLine matches newEventLogger's text format: charmbracelet/log's
// timestamp, its four-letter level, then the message and key=value pairs.
// It is wider than supervisor.lifecycleLine on purpose — every level and
// message is styled here, where that one reads back three.
var daemonLine = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) (DEBU|INFO|WARN|ERRO|FATA) (.*)$`)

// rawLine re-renders one daemon line, or returns anything else — the agent's
// own output — untouched.
func (s *logStyle) rawLine(line string) string {
	m := daemonLine.FindStringSubmatch(line)
	if m == nil {
		return line
	}
	lc, mc := s.levelColor(m[2])
	msg, kvs := splitLogFields(m[3])
	var b strings.Builder
	b.WriteString(s.faint(m[1]))
	b.WriteString(" ")
	b.WriteString(s.paint(lc, true, m[2]))
	b.WriteString(" ")
	if mc != nil {
		b.WriteString(s.paint(mc, false, msg))
	} else {
		b.WriteString(msg)
	}
	for _, kv := range kvs {
		b.WriteString(" ")
		b.WriteString(s.faint(kv.key + "="))
		b.WriteString(s.logValue(kv.key, kv.val))
	}
	return b.String()
}

// levelColor is a level's badge colour and, for a warning or worse, its
// message colour (nil: the message prints unstyled).
func (s *logStyle) levelColor(level string) (badge, msg color.Color) {
	switch level {
	case "DEBU":
		return s.c.Dim, nil
	case "WARN":
		return s.c.Amber, s.c.Amber
	case "ERRO", "FATA":
		return s.c.Coral, s.c.Coral
	}
	return s.c.Cyan, nil
}

// logValue styles a value by its key: states as their glyph and colour, exit
// codes and outcomes by success, errors in the error colour.
func (s *logStyle) logValue(key, val string) string {
	switch key {
	case "from", "to":
		if st := core.State(val); st.Valid() {
			return s.th.RenderState(st)
		}
	case "code", "exit_code":
		if val == "0" {
			return s.paint(s.c.Mint, true, val)
		}
		return s.paint(s.c.Coral, true, val)
	case "outcome":
		if val == "success" {
			return s.paint(s.c.Mint, true, val)
		}
		return s.paint(s.c.Coral, true, val)
	case "err":
		return s.paint(s.c.Coral, false, val)
	}
	return val
}

type logKV struct{ key, val string }

// splitLogFields splits the text after a line's level into its message and
// its trailing key=value pairs: the message ends at the first space after
// which everything parses as pairs.
func splitLogFields(rest string) (string, []logKV) {
	for i := 0; i < len(rest); i++ {
		if rest[i] != ' ' {
			continue
		}
		if kvs, ok := parseLogKVs(rest[i+1:]); ok {
			return rest[:i], kvs
		}
	}
	return rest, nil
}

// logKey is a charmbracelet/log key, up to and including its "=".
var logKey = regexp.MustCompile(`^[A-Za-z0-9_.\-]+=`)

// parseLogKVs parses space-separated key=value pairs, a value either a bare
// token or a Go-quoted string, and fails unless it consumes all of s.
func parseLogKVs(s string) ([]logKV, bool) {
	var out []logKV
	for {
		k := logKey.FindString(s)
		if k == "" {
			return nil, false
		}
		s = s[len(k):]
		var val string
		if strings.HasPrefix(s, `"`) {
			q, err := strconv.QuotedPrefix(s)
			if err != nil {
				return nil, false
			}
			val, s = q, s[len(q):]
		} else {
			i := strings.IndexByte(s, ' ')
			if i < 0 {
				i = len(s)
			}
			val, s = s[:i], s[i:]
			if strings.ContainsRune(val, '"') {
				return nil, false
			}
		}
		out = append(out, logKV{key: strings.TrimSuffix(k, "="), val: val})
		if s == "" {
			return out, true
		}
		if s[0] != ' ' {
			return nil, false
		}
		s = s[1:]
	}
}
