package main

// Agent Activity Logs
//
// Renders the structured view of `harness logs`: the latest run of a harness as
// a time-ordered list of lifecycle lines and the agent-trace events attributed
// to that run — what the agent read, ran, edited, and the error it died on —
// instead of the PTY history of a full-screen TUI. A harness with no native
// transcript, or a daemon too old to build the view, answers with the durable
// log tail and it prints exactly as `--raw` does.
//
// Every string here came out of an agent transcript, and a transcript records
// whatever a model or a tool printed. Each field is flattened to one line with
// control characters removed before it reaches the terminal (#146).
//
// Governing: SPEC-0002 REQ "Control Operations" ("logs"), SPEC-0006 REQ "Run
// Correlation", issue #302.
//
// @joestump-agent 09/11/2026 - Added for harness#302.

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

const (
	// activityPoll is the --follow re-fetch interval, matching the agent-trace
	// watcher's own poll.
	activityPoll = 2 * time.Second
	// detailWidth caps one entry's detail. A summary can carry a whole
	// heredoc; the full value is one --json away.
	detailWidth = 160
)

// cmdLogEvents prints one structured logs reply, or follows the run.
func cmdLogEvents(c *client.Client, o verbOpts, w io.Writer) error {
	fetch := func(lines int) (protocol.LogsData, error) {
		return c.LogEvents(o.name, client.LogOptions{Lines: lines, IncludeAmbiguous: o.ambiguous, Run: o.run})
	}
	if o.follow && !o.json {
		ld, err := fetch(o.lines)
		if err != nil {
			return err
		}
		if ld.Source != protocol.LogSourceAgentTrace {
			return followLogs(c, o)
		}
		return followActivity(w, ld, fetch, o.lines, func() bool {
			time.Sleep(activityPoll)
			return true
		})
	}
	ld, err := fetch(o.lines)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(ld)
	}
	renderActivity(w, ld)
	return nil
}

// renderActivity prints a logs reply. A reply that is not the activity view is
// the durable log tail and prints as such.
func renderActivity(w io.Writer, ld protocol.LogsData) {
	if ld.Source != protocol.LogSourceAgentTrace {
		printLogText(w, ld.Text)
		return
	}
	if ld.Run != nil {
		fmt.Fprintln(w, runHeader(*ld.Run))
	}
	for _, n := range ld.Notices {
		fmt.Fprintln(w, noteLine(n))
	}
	for _, e := range ld.Entries {
		fmt.Fprintln(w, formatEntry(e))
	}
	// The activity view never prints the durable log, even if a daemon sent
	// one: this view is agent activity, and the stored history of a
	// full-screen agent is its repainted screen. `--raw` is the way to read
	// the log, and the daemon's notices say so (#279).
}

// followActivity prints first, then re-fetches until wait reports false,
// printing each entry the first time it appears. A new run (the harness
// restarted, or a schedule fired) prints its own header.
//
// Re-fetches ask for four times the line budget: more than that arriving within
// one poll interval is not a rate an agent runs tools at, and the entries' IDs
// make the overlap harmless.
func followActivity(w io.Writer, first protocol.LogsData, fetch func(lines int) (protocol.LogsData, error), lines int, wait func() bool) error {
	renderActivity(w, first)
	seen := map[string]bool{}
	for _, e := range first.Entries {
		seen[e.ID] = true
	}
	run := ""
	if first.Run != nil {
		run = first.Run.Start
	}
	for wait() {
		ld, err := fetch(lines * 4)
		if err != nil {
			return err
		}
		if ld.Run != nil && ld.Run.Start != run {
			run = ld.Run.Start
			fmt.Fprintln(w, runHeader(*ld.Run))
		}
		for _, e := range ld.Entries {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			fmt.Fprintln(w, formatEntry(e))
		}
	}
	return nil
}

// runHeader names the run: when, how it ended, which adapter, which directory.
func runHeader(r protocol.LogRun) string {
	start, err := time.Parse(time.RFC3339Nano, r.Start)
	if err != nil {
		return "run " + inert(r.Start)
	}
	start = start.Local()
	parts := []string{"run " + start.Format("2006-01-02 15:04:05")}
	if end, err := time.Parse(time.RFC3339Nano, r.End); err == nil {
		end = end.Local()
		layout := "15:04:05"
		if end.Format("2006-01-02") != start.Format("2006-01-02") {
			layout = "2006-01-02 15:04:05"
		}
		parts[0] += " → " + end.Format(layout)
	} else {
		parts[0] += " → running"
	}
	if r.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("exit %d", *r.ExitCode))
	}
	if r.Adapter != "" {
		parts = append(parts, inert(r.Adapter))
	}
	if r.Workdir != "" {
		parts = append(parts, inert(r.Workdir))
	}
	return strings.Join(parts, " · ")
}

// noteLine prints a notice in the label column, under no timestamp.
func noteLine(n string) string {
	return fmt.Sprintf("%8s  %-8s  %s", "", "note", inert(n))
}

// formatEntry is one entry: local clock time, a label, the detail.
func formatEntry(e protocol.LogEntry) string {
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
	return fmt.Sprintf("%s  %-8s  %s%s", clock, inert(label), clip(inert(detail), detailWidth), suffix)
}

// inert flattens s onto one line and drops every control character, so no
// escape sequence from a transcript reaches the terminal as one.
func inert(s string) string {
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

// clip cuts s to n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
