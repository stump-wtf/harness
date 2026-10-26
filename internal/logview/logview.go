// Package logview renders a harness's run activity — the `logs` reply — as
// text, plain or styled. It is the one renderer behind `harness logs`, the
// dashboard's preview of a one-shot, and the chatroom, so the three read the
// same: a faint clock, a label column of coloured badges, the detail, and a
// failure suffix.
//
// Every string rendered here came out of an agent transcript, and a transcript
// records whatever a model or a tool printed. Each field is flattened to one
// line with control characters removed before it reaches a terminal (#146).
//
// Governing: SPEC-0002 REQ "Control Operations" ("logs"), SPEC-0001 REQ "State
// Presentation" (paired glyph + colour, legible in mono), SPEC-0001 REQ "Zero
// And Error States" (one palette across cockpit and CLI), ADR-0001
// (Charmbracelet stack; lipgloss + the theme own the visual language), ADR-0007
// (lifecycle events are charmbracelet/log lines in the durable log).
//
// @joestump-agent 09/27/2026 - Extracted from cmd/harness/logs.go and
// logs_style.go so the TUI's one-shot preview and the chatroom render agent
// activity the way `harness logs` does, instead of three dialects of it.
package logview

import (
	"fmt"
	"image/color"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/tui/theme"
)

// DetailWidth caps one entry's detail. A summary can carry a whole heredoc;
// the full value is one --json away.
const DetailWidth = 160

// LabelWidth is the label column's width, measured before styling.
const LabelWidth = 8

// --- plain -------------------------------------------------------------------

// RunFields is a run header's columns, each already made inert.
type RunFields struct {
	// Parsed is false when Start is not a timestamp: Start then holds it
	// verbatim and every other field is empty.
	Parsed bool
	Start  string
	// End is the end clock, or "" while the run is in flight.
	End     string
	Exit    *int
	Adapter string
	Workdir string
}

// RunParts splits a run into its header columns.
func RunParts(r protocol.LogRun) RunFields {
	start, err := time.Parse(time.RFC3339Nano, r.Start)
	if err != nil {
		return RunFields{Start: Inert(r.Start)}
	}
	start = start.Local()
	f := RunFields{Parsed: true, Start: start.Format("2006-01-02 15:04:05"), Exit: r.ExitCode, Adapter: Inert(r.Adapter), Workdir: Inert(r.Workdir)}
	if end, err := time.Parse(time.RFC3339Nano, r.End); err == nil {
		end = end.Local()
		layout := "15:04:05"
		if end.Format("2006-01-02") != start.Format("2006-01-02") {
			layout = "2006-01-02 15:04:05"
		}
		f.End = end.Format(layout)
	}
	return f
}

// RunHeader names the run: when, how it ended, which adapter, which directory.
func RunHeader(r protocol.LogRun) string {
	f := RunParts(r)
	if !f.Parsed {
		return "run " + f.Start
	}
	parts := []string{"run " + f.Start}
	if f.End != "" {
		parts[0] += " → " + f.End
	} else {
		parts[0] += " → running"
	}
	if f.Exit != nil {
		parts = append(parts, fmt.Sprintf("exit %d", *f.Exit))
	}
	if f.Adapter != "" {
		parts = append(parts, f.Adapter)
	}
	if f.Workdir != "" {
		parts = append(parts, f.Workdir)
	}
	return strings.Join(parts, " · ")
}

// NoteLine prints a notice in the label column, under no timestamp.
func NoteLine(n string) string {
	return fmt.Sprintf("%8s  %-8s  %s", "", "note", Inert(n))
}

// FormatEntry is one entry: local clock time, a label, the detail.
func FormatEntry(e protocol.LogEntry) string {
	p := Parts(e)
	return fmt.Sprintf("%s  %-8s  %s%s", p.Clock, p.Label, p.Detail, p.Suffix)
}

// EntryFields is one entry's columns, each already made inert. The plain and
// the styled renderers both print these, so they differ only in styling.
type EntryFields struct {
	Clock, Label, Detail, Suffix string
}

// Parts splits an entry into its columns: local clock time, a label, the
// detail, and a failure suffix.
func Parts(e protocol.LogEntry) EntryFields {
	clock := "--:--:--"
	if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
		clock = t.Local().Format("15:04:05")
	}
	label, detail, suffix := e.Action, e.Summary, ""
	switch e.Kind {
	case protocol.LogEntryTool:
		// A read or an edit is about the file; everything else is about the
		// command.
		if (e.Action == "read" || e.Action == "edit") && e.Target != "" {
			detail = e.Target
		}
		if detail == "" {
			detail = e.Tool
		}
		if e.Error {
			suffix = "  (failed)"
		}
	case protocol.LogEntryMark:
		switch e.Action {
		case "error":
			label = "ERROR"
		case "user-message", "user":
			label = "prompt"
		}
	}
	if e.Ambiguous {
		label = "?" + label
	}
	return EntryFields{Clock: clock, Label: Inert(label), Detail: Clip(Inert(detail), DetailWidth), Suffix: suffix}
}

// Inert flattens s onto one line and drops every control character, so no
// escape sequence from a transcript reaches the terminal as one.
func Inert(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// Clip cuts s to n runes, marking the cut.
func Clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// --- styled ------------------------------------------------------------------

// Style renders the styled forms. A nil *Style means plain output.
type Style struct {
	th *theme.Theme
	c  theme.Colors
}

// NewStyle is the styled renderer for a theme.
func NewStyle(th *theme.Theme) *Style {
	return &Style{th: th, c: th.Colors()}
}

// Paint renders str in c, bold or not. The empty string stays empty, so a
// missing field costs no escape sequences.
func (s *Style) Paint(c color.Color, bold bool, str string) string {
	if str == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(c).Bold(bold).Render(str)
}

// Faint renders secondary text: clocks, separators, spans.
func (s *Style) Faint(str string) string { return s.Paint(s.c.Faint, false, str) }

// Group is a run of consecutive entries that print identically but for their
// clock. A single entry is a group of one.
type Group struct {
	Entry  protocol.LogEntry
	Fields EntryFields
	Key    string
	Count  int
	// Last is the clock of the newest entry in the run.
	Last string
}

// NewGroup opens a group on e.
func NewGroup(e protocol.LogEntry) *Group {
	f := Parts(e)
	return &Group{Entry: e, Fields: f, Key: GroupKey(e, f), Count: 1, Last: f.Clock}
}

// GroupKey is what two entries must share to collapse into one line: all of
// what prints, except the clock.
func GroupKey(e protocol.LogEntry, f EntryFields) string {
	return strings.Join([]string{e.Kind, f.Label, f.Detail, f.Suffix}, "\x00")
}

// Join folds e into g when it prints the same, reporting whether it did.
func (g *Group) Join(e protocol.LogEntry) bool {
	f := Parts(e)
	if GroupKey(e, f) != g.Key {
		return false
	}
	g.Count++
	g.Last = f.Clock
	return true
}

// RunHeader is RunHeader, styled: the run in accent, a live run as the running
// state, a failing exit in the error colour, the workdir faint.
func (s *Style) RunHeader(r protocol.LogRun) string {
	f := RunParts(r)
	head := s.Paint(s.c.Accent, true, "run") + " "
	if !f.Parsed {
		return head + s.Faint(f.Start)
	}
	sep := s.Faint(" · ")
	b := head + s.Paint(s.c.Fg, true, f.Start) + s.Faint(" → ")
	if f.End != "" {
		b += s.Paint(s.c.Fg, false, f.End)
	} else {
		b += s.th.RenderState(core.StateRunning)
	}
	if f.Exit != nil {
		c := s.c.Mint
		if *f.Exit != 0 {
			c = s.c.Coral
		}
		b += sep + s.Paint(c, true, fmt.Sprintf("exit %d", *f.Exit))
	}
	if f.Adapter != "" {
		b += sep + s.Paint(s.c.Cyan, true, f.Adapter)
	}
	if f.Workdir != "" {
		b += sep + s.Faint(f.Workdir)
	}
	return b
}

// NoteLine is NoteLine, styled.
func (s *Style) NoteLine(n string) string {
	label := lipgloss.NewStyle().Foreground(s.c.Faint).Italic(true).Render("note")
	return fmt.Sprintf("%8s  %s%s  %s", "", label, Pad("note", LabelWidth), s.Paint(s.c.Dim, false, Inert(n)))
}

// GroupLine is one entry, or a collapsed run of them: the first clock, the
// badge, then — for a run — its count and span before the detail, so the count
// stays in view however long the detail is.
func (s *Style) GroupLine(g *Group) string {
	return s.GroupLineWho(g, "")
}

// GroupLineWho is GroupLine with a column naming who did it between the clock
// and the badge — the chatroom's speaker. who arrives already styled and
// padded; "" leaves the column out.
func (s *Style) GroupLineWho(g *Group, who string) string {
	c, bold := s.LabelColor(g.Entry)
	var b strings.Builder
	b.WriteString(s.Faint(g.Fields.Clock))
	b.WriteString("  ")
	if who != "" {
		b.WriteString(who)
		b.WriteString("  ")
	}
	b.WriteString(s.Paint(c, bold, g.Fields.Label))
	b.WriteString(Pad(g.Fields.Label, LabelWidth))
	b.WriteString("  ")
	if g.Count > 1 {
		b.WriteString(s.Paint(s.c.Amber, true, fmt.Sprintf("×%d", g.Count)))
		if g.Last != g.Fields.Clock {
			b.WriteString(" ")
			b.WriteString(s.Faint(g.Fields.Clock + "–" + g.Last))
		}
		b.WriteString("  ")
	}
	b.WriteString(s.Detail(g.Entry, g.Fields.Detail))
	if g.Fields.Suffix != "" {
		b.WriteString("  ")
		b.WriteString(s.Paint(s.c.Coral, false, strings.TrimSpace(g.Fields.Suffix)))
	}
	return b.String()
}

// Pad is the spaces that bring label to width, measured before styling.
func Pad(label string, width int) string {
	if n := width - utf8.RuneCountInString(label); n > 0 {
		return strings.Repeat(" ", n)
	}
	return ""
}

// LabelColor picks a badge colour by what the entry is: lifecycle in accent,
// the session in pink, reads cyan, edits amber, commands mint, errors coral,
// anything unclassified dim.
func (s *Style) LabelColor(e protocol.LogEntry) (color.Color, bool) {
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
		case "result":
			if e.Error {
				return s.c.Coral, true
			}
			return s.c.Mint, true
		}
	}
	return s.c.Dim, true
}

// Detail styles an entry's detail: a state change as two coloured states, an
// exit code by success, an error in the error colour, a path with its
// directory dimmed. Anything else prints as it is.
func (s *Style) Detail(e protocol.LogEntry, d string) string {
	switch e.Kind {
	case protocol.LogEntryLifecycle:
		switch e.Action {
		case "state":
			if from, to, ok := strings.Cut(d, " → "); ok && core.State(from).Valid() && core.State(to).Valid() {
				return s.th.RenderState(core.State(from)) + s.Faint(" → ") + s.th.RenderState(core.State(to))
			}
		case "exited":
			if k, val, ok := strings.Cut(d, "="); ok {
				c := s.c.Mint
				if e.Error {
					c = s.c.Coral
				}
				return s.Faint(k+"=") + s.Paint(c, true, val)
			}
		case "flapping":
			return s.Paint(s.c.Amber, false, d)
		}
	case protocol.LogEntrySession:
		parts := strings.Split(d, " · ")
		parts[0] = s.Paint(s.c.Fg, true, parts[0])
		return strings.Join(parts, s.Faint(" · "))
	case protocol.LogEntryMark:
		if e.Action == "error" || (e.Action == "result" && e.Error) {
			return s.Paint(s.c.Coral, false, d)
		}
	case protocol.LogEntryTool:
		if e.Action == "read" || e.Action == "edit" {
			if i := strings.LastIndexByte(d, '/'); i >= 0 && i < len(d)-1 {
				return s.Paint(s.c.Dim, false, d[:i+1]) + d[i+1:]
			}
		}
	}
	return d
}

// --- a whole reply -----------------------------------------------------------

// Lines renders one logs reply as display lines, the way `harness logs` prints
// it to a terminal: a run header, the notices, then the entries with runs of
// identical ones collapsed. st nil renders the plain form, uncollapsed.
//
// A reply that is not the activity view is the durable log tail. Its lines
// come back with the daemon's own lines re-rendered, and everything else — the
// agent's output — exactly as it was, so the caller must still make it inert.
func Lines(ld protocol.LogsData, st *Style) []string {
	if ld.Source != protocol.LogSourceAgentTrace {
		text := strings.TrimRight(ld.Text, "\n")
		if text == "" {
			return nil
		}
		out := strings.Split(text, "\n")
		if st != nil {
			for i, l := range out {
				out[i] = st.RawLine(l)
			}
		}
		return out
	}
	var out []string
	if ld.Run != nil {
		if st == nil {
			out = append(out, RunHeader(*ld.Run))
		} else {
			out = append(out, st.RunHeader(*ld.Run))
		}
	}
	for _, n := range ld.Notices {
		if st == nil {
			out = append(out, NoteLine(n))
		} else {
			out = append(out, st.NoteLine(n))
		}
	}
	if st == nil {
		for _, e := range ld.Entries {
			out = append(out, FormatEntry(e))
		}
		return out
	}
	var g *Group
	for _, e := range ld.Entries {
		if g != nil && g.Join(e) {
			continue
		}
		if g != nil {
			out = append(out, st.GroupLine(g))
		}
		g = NewGroup(e)
	}
	if g != nil {
		out = append(out, st.GroupLine(g))
	}
	return out
}

// --- the durable log ---------------------------------------------------------

// daemonLine matches newEventLogger's text format: charmbracelet/log's
// timestamp, its four-letter level, then the message and key=value pairs.
// It is wider than supervisor.lifecycleLine on purpose — every level and
// message is styled here, where that one reads back three.
var daemonLine = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) (DEBU|INFO|WARN|ERRO|FATA) (.*)$`)

// RawLine re-renders one daemon line, or returns anything else — the agent's
// own output — untouched.
func (s *Style) RawLine(line string) string {
	m := daemonLine.FindStringSubmatch(line)
	if m == nil {
		return line
	}
	lc, mc := s.levelColor(m[2])
	msg, kvs := SplitLogFields(m[3])
	var b strings.Builder
	b.WriteString(s.Faint(m[1]))
	b.WriteString(" ")
	b.WriteString(s.Paint(lc, true, m[2]))
	b.WriteString(" ")
	if mc != nil {
		b.WriteString(s.Paint(mc, false, msg))
	} else {
		b.WriteString(msg)
	}
	for _, kv := range kvs {
		b.WriteString(" ")
		b.WriteString(s.Faint(kv.Key + "="))
		b.WriteString(s.logValue(kv.Key, kv.Val))
	}
	return b.String()
}

// levelColor is a level's badge colour and, for a warning or worse, its
// message colour (nil: the message prints unstyled).
func (s *Style) levelColor(level string) (badge, msg color.Color) {
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
func (s *Style) logValue(key, val string) string {
	switch key {
	case "from", "to":
		if st := core.State(val); st.Valid() {
			return s.th.RenderState(st)
		}
	case "code", "exit_code":
		if val == "0" {
			return s.Paint(s.c.Mint, true, val)
		}
		return s.Paint(s.c.Coral, true, val)
	case "outcome":
		if val == "success" {
			return s.Paint(s.c.Mint, true, val)
		}
		return s.Paint(s.c.Coral, true, val)
	case "err":
		return s.Paint(s.c.Coral, false, val)
	}
	return val
}

// KV is one key=value pair from a daemon line.
type KV struct{ Key, Val string }

// SplitLogFields splits the text after a line's level into its message and
// its trailing key=value pairs: the message ends at the first space after
// which everything parses as pairs.
func SplitLogFields(rest string) (string, []KV) {
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
func parseLogKVs(s string) ([]KV, bool) {
	var out []KV
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
		out = append(out, KV{Key: strings.TrimSuffix(k, "="), Val: val})
		if s == "" {
			return out, true
		}
		if s[0] != ' ' {
			return nil, false
		}
		s = s[1:]
	}
}
