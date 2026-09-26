package main

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	clog "github.com/charmbracelet/log"
	"github.com/charmbracelet/x/ansi"

	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/tui/theme"
)

// sgrParams matches one SGR escape and captures its parameters.
var sgrParams = regexp.MustCompile("\x1b\\[([0-9;:]*)m")

// hasColour reports whether s sets a foreground or background colour.
func hasColour(s string) bool {
	for _, m := range sgrParams.FindAllStringSubmatch(s, -1) {
		for _, p := range strings.Split(m[1], ";") {
			if n, err := strconv.Atoi(p); err == nil && (n >= 30 && n <= 49 || n >= 90 && n <= 107) {
				return true
			}
		}
	}
	return false
}

// trueColor is a styled renderer pinned to a truecolor dark terminal, so
// tests do not depend on the terminal running them.
func trueColor() *logStyle {
	return newLogStyle(theme.New(colorprofile.TrueColor, true, theme.DefaultPalette()))
}

// plainLines strips styling and splits into lines, for asserting on what a
// reader sees.
func plainLines(s string) []string {
	return strings.Split(strings.TrimSuffix(ansi.Strip(s), "\n"), "\n")
}

// TestStyledActivity: the styled view is the plain view's content, styled —
// state changes as glyph + name, and a run of identical errors as one line
// with its count and span.
func TestStyledActivity(t *testing.T) {
	var buf bytes.Buffer
	newActivityView(&buf, trueColor()).render(repeatingErrors())
	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Fatal("styled view carries no styling")
	}
	want := []string{
		"run 2026-09-11 07:40:00 → 07:56:21 · exit 1 · crush · /home/agent/sweeps",
		"          note      1 session(s) in this run's window not shown",
		"07:40:00  state     ○ stopped → ◌ starting",
		"07:40:02  session   e088ec4e · crush · litellm/Qwen3.8-27B",
		"07:40:05  read      /home/agent/.config/dotfiles/stumpcloud-sweep.prompt.md",
		"07:41:10  exec      ssh nuc01 docker ps  (failed)",
		"07:56:20  ERROR     Bad Request: litellm.ContextWindowExceededError[31m",
		"07:56:21  ERROR     ×3 07:56:21–07:56:23  Bad Request: litellm.ContextWindowExceededError",
		"07:56:21  exited    code=1",
		"07:56:22  ?exec     gh pr list",
	}
	got := plainLines(out)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("styled view reads\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestStyledActivityCollapsesAThousandRepeats is the production shape: a
// provider loop logging one error 1,070 times reads as one line.
func TestStyledActivityCollapsesAThousandRepeats(t *testing.T) {
	ld := protocol.LogsData{Source: protocol.LogSourceAgentTrace}
	for i := 0; i < 1070; i++ {
		ld.Entries = append(ld.Entries, protocol.LogEntry{
			ID: string(rune(i)), Time: stampAt(20, 16+i/60, i%60), Kind: protocol.LogEntryMark,
			Action: "error", Summary: "Bad Request: litellm.ContextWindowExceededError",
		})
	}
	var buf bytes.Buffer
	newActivityView(&buf, trueColor()).render(ld)
	got := plainLines(buf.String())
	if len(got) != 1 || !strings.Contains(got[0], "×1070 20:16:00–20:33:49") {
		t.Errorf("1,070 identical errors printed as %d line(s):\n%s", len(got), strings.Join(got, "\n"))
	}
}

// TestStyledActivityMonoIsLegible: under NO_COLOR (an Ascii colour profile)
// no colour escapes are emitted, and the states still read from glyph + name.
func TestStyledActivityMonoIsLegible(t *testing.T) {
	var buf bytes.Buffer
	st := newLogStyle(theme.New(colorprofile.Ascii, true, theme.DefaultPalette()))
	newActivityView(&buf, st).render(repeatingErrors())
	out := buf.String()
	if hasColour(out) {
		t.Errorf("mono output carries a colour escape\n%q", out)
	}
	// Show the check can fire: the truecolor view does carry colour.
	var colour bytes.Buffer
	newActivityView(&colour, trueColor()).render(repeatingErrors())
	if !hasColour(colour.String()) {
		t.Fatal("hasColour finds no colour in the truecolor view; the mono check proves nothing")
	}
	if !strings.Contains(ansi.Strip(out), "○ stopped → ◌ starting") {
		t.Errorf("mono output lost the state glyphs\n%s", ansi.Strip(out))
	}
}

// TestFollowActivityCollapsesInPlace: a styled --follow redraws the open
// group's line as repeats arrive, including across polls.
func TestFollowActivityCollapsesInPlace(t *testing.T) {
	errAt := func(id string, s int) protocol.LogEntry {
		return protocol.LogEntry{ID: id, Time: stampAt(8, 0, s), Kind: protocol.LogEntryMark, Action: "error", Summary: "Bad Request"}
	}
	first := protocol.LogsData{Source: protocol.LogSourceAgentTrace, Run: &protocol.LogRun{Start: stampAt(8, 0, 0)},
		Entries: []protocol.LogEntry{errAt("e1", 1)}}
	polls := []protocol.LogsData{
		{Source: protocol.LogSourceAgentTrace, Run: first.Run, Entries: []protocol.LogEntry{errAt("e1", 1), errAt("e2", 2)}},
		{Source: protocol.LogSourceAgentTrace, Run: first.Run, Entries: []protocol.LogEntry{errAt("e2", 2), errAt("e3", 3),
			{ID: "x", Time: stampAt(8, 0, 4), Kind: protocol.LogEntryTool, Action: "exec", Summary: "make test"}}},
	}
	follow := func(cols int) string {
		var buf bytes.Buffer
		v := newActivityView(&buf, trueColor())
		v.live = true
		v.cols = func() int { return cols }
		ps := append([]protocol.LogsData(nil), polls...)
		err := followActivity(v, first, func(int) (protocol.LogsData, error) {
			ld := ps[0]
			ps = ps[1:]
			return ld, nil
		}, 50, func() bool { return len(ps) > 0 })
		if err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	out := follow(200)
	if n := strings.Count(out, "\x1b[1A\r\x1b[J"); n != 2 {
		t.Errorf("redrew the group %d times, want 2 (one per repeat)\n%q", n, out)
	}
	// Replay the redraws the way a terminal would: each one replaces the
	// line above it.
	var screen []string
	for _, chunk := range strings.SplitAfter(out, "\n") {
		if chunk == "" {
			continue
		}
		if strings.HasPrefix(chunk, "\x1b[1A\r\x1b[J") {
			screen = screen[:len(screen)-1]
			chunk = strings.TrimPrefix(chunk, "\x1b[1A\r\x1b[J")
		}
		screen = append(screen, ansi.Strip(strings.TrimSuffix(chunk, "\n")))
	}
	want := []string{
		"run 2026-09-11 08:00:00 → ● running",
		"08:00:01  ERROR     ×3 08:00:01–08:00:03  Bad Request",
		"08:00:04  exec      make test",
	}
	if strings.Join(screen, "\n") != strings.Join(want, "\n") {
		t.Errorf("terminal shows\n%s\nwant\n%s", strings.Join(screen, "\n"), strings.Join(want, "\n"))
	}

	// With no known width a redraw cannot be placed, so every repeat prints.
	out = follow(0)
	if strings.Contains(out, "\x1b[1A") {
		t.Error("redrew in place without knowing the terminal width")
	}
	if n := strings.Count(ansi.Strip(out), "ERROR"); n != 3 {
		t.Errorf("printed %d ERROR lines with no width, want 3\n%s", n, ansi.Strip(out))
	}
}

func TestRowsFor(t *testing.T) {
	for _, c := range []struct {
		line       string
		cols, want int
	}{
		{"abc", 0, 0},
		{"", 80, 1},
		{strings.Repeat("x", 80), 80, 1},
		{strings.Repeat("x", 81), 80, 2},
		{"\x1b[31m" + strings.Repeat("x", 80) + "\x1b[m", 80, 1},
	} {
		if got := rowsFor(c.line, c.cols); got != c.want {
			t.Errorf("rowsFor(%d visible, %d cols) = %d, want %d", len(ansi.Strip(c.line)), c.cols, got, c.want)
		}
	}
}

// TestRawLineMatchesTheDurableLogFormat pins the parse against what
// newEventLogger writes — charmbracelet/log with timestamps and its text
// formatter — rather than against a hand-written line.
func TestRawLineMatchesTheDurableLogFormat(t *testing.T) {
	var buf bytes.Buffer
	l := clog.New(&buf)
	l.SetReportTimestamp(true)
	l.Info("state changed", "from", "restarting", "to", "starting")
	l.Info("exited", "code", 1)
	l.Warn("run interrupted", "run_id", 7, "reason", "the daemon exited while this run was in flight")
	st := trueColor()
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	want := []struct{ plain, key string }{
		{" INFO state changed from=◌ restarting to=◌ starting", ""},
		{" INFO exited code=1", ""},
		{` WARN run interrupted run_id=7 reason="the daemon exited while this run was in flight"`, ""},
	}
	for i, line := range lines {
		got := st.rawLine(line)
		if got == line {
			t.Fatalf("durable-log line %q was not recognised", line)
		}
		if p := ansi.Strip(got); p != line[:19]+want[i].plain {
			t.Errorf("rawLine(%q) reads %q", line, p)
		}
	}
}

// TestRawLineLeavesAgentOutputAlone: anything that is not a daemon line —
// the agent's own output, even output that mentions a level — passes through.
func TestRawLineLeavesAgentOutputAlone(t *testing.T) {
	st := trueColor()
	for _, line := range []string{
		"Error: You must be logged in to use Remote Control.",
		"INFO state changed from=a to=b",
		"2026/09/25 20:14:36 NOTE something",
		"",
	} {
		if got := st.rawLine(line); got != line {
			t.Errorf("rawLine(%q) = %q, want it untouched", line, got)
		}
	}
}

func TestSplitLogFields(t *testing.T) {
	for _, c := range []struct {
		in, msg string
		kvs     []logKV
	}{
		{"state changed from=running to=stopping", "state changed", []logKV{{"from", "running"}, {"to", "stopping"}}},
		{"run queued", "run queued", nil},
		{`config auto-reload failed (keeping last-good config) err="line 3: bad = value"`, "config auto-reload failed (keeping last-good config)", []logKV{{"err", `"line 3: bad = value"`}}},
		{"odd a=b c", "odd a=b c", nil},
	} {
		msg, kvs := splitLogFields(c.in)
		if msg != c.msg || len(kvs) != len(c.kvs) {
			t.Errorf("splitLogFields(%q) = %q %v, want %q %v", c.in, msg, kvs, c.msg, c.kvs)
			continue
		}
		for i := range kvs {
			if kvs[i] != c.kvs[i] {
				t.Errorf("splitLogFields(%q) pair %d = %v, want %v", c.in, i, kvs[i], c.kvs[i])
			}
		}
	}
}

// TestFollowRawStylesAcrossSplitWrites: --follow appends suffixes that can
// end mid-line. A daemon line is styled wherever it starts; the continuation
// of a line already on screen is printed as it came. The plain writer prints
// the same bytes the old follower did.
func TestFollowRawStylesAcrossSplitWrites(t *testing.T) {
	tails := []string{
		"agent says hel",
		"agent says hello\n2026/09/25 20:14:36 INFO exited code=1",
		"agent says hello\n2026/09/25 20:14:36 INFO exited code=1\n2026/09/25 20:14:37 INFO state changed from=running to=failed",
	}
	follow := func(st *logStyle) string {
		var buf bytes.Buffer
		ts := append([]string(nil), tails...)
		err := followRawLogs(newRawWriter(&buf, st), func(int) (string, error) {
			s := ts[0]
			ts = ts[1:]
			return s, nil
		}, 10, func() bool { return len(ts) > 0 })
		if err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	if got := follow(nil); got != tails[2] {
		t.Errorf("plain follow printed %q, want %q", got, tails[2])
	}
	styled := follow(trueColor())
	want := "agent says hello\n2026/09/25 20:14:36 INFO exited code=1\n2026/09/25 20:14:37 INFO state changed from=● running to=✖ failed"
	if got := ansi.Strip(styled); got != want {
		t.Errorf("styled follow reads %q, want %q", got, want)
	}
	if strings.Contains(strings.SplitN(styled, "\n", 2)[0], "\x1b[") {
		t.Error("styled the continuation of an agent line")
	}
	if strings.Count(styled, "\x1b[") == 0 {
		t.Error("daemon lines were not styled")
	}
}

// TestFollowRawReprintStartsANewLine: a rotated log is reprinted whole; the
// styled view starts it on its own line so its first daemon line is styled.
func TestFollowRawReprintStartsANewLine(t *testing.T) {
	tails := []string{"partial", "2026/09/25 20:14:36 INFO exited code=0"}
	var buf bytes.Buffer
	err := followRawLogs(newRawWriter(&buf, trueColor()), func(int) (string, error) {
		s := tails[0]
		tails = tails[1:]
		return s, nil
	}, 10, func() bool { return len(tails) > 0 })
	if err != nil {
		t.Fatal(err)
	}
	if got := ansi.Strip(buf.String()); got != "partial\n2026/09/25 20:14:36 INFO exited code=0" {
		t.Errorf("reprint reads %q", got)
	}
}
