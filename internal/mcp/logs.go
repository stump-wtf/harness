package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/logview"
	"github.com/stump-wtf/harness/internal/protocol"
)

// Log formats a caller may ask for.
const (
	// FormatSummary returns the run's activity as ordered entries: lifecycle
	// transitions, tool calls and marks, with session bookkeeping collapsed.
	FormatSummary = "summary"
	// FormatRaw returns the log tail exactly as `logs --raw` prints it.
	FormatRaw = "raw"
)

// defaultWaitSeconds caps a waiting trigger call when the caller names no cap;
// a tool call that hangs forever would hold the client's request open.
const defaultWaitSeconds = 600

// waitPoll is how often a waiting call re-reads the run ledger. The CLI polls
// at the same cadence, so a followed run feels the same from either surface.
var waitPoll = 2 * time.Second

// waitHistoryLimit is how many run records a follow reads each poll: enough to
// see the run it started plus its recent neighbours, few enough to stay cheap.
const waitHistoryLimit = 50

// LogsArgs are the arguments of harness_logs.
type LogsArgs struct {
	Name     string `json:"name"`
	Run      int    `json:"run"`
	Lines    int    `json:"lines"`
	Format   string `json:"format"`
	Cursor   string `json:"cursor"`
	Offset   int    `json:"offset"`
	MaxBytes int    `json:"max_bytes"`
}

// Entry is one line of a run's activity, rendered and structured: the fields an
// agent acts on, plus one line of text it can quote.
type Entry struct {
	ID      string `json:"id,omitempty"`
	Time    string `json:"time,omitempty"`
	Kind    string `json:"kind"`
	Action  string `json:"action,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Target  string `json:"target,omitempty"`
	Summary string `json:"summary,omitempty"`
	Failed  bool   `json:"failed,omitempty"`
	Text    string `json:"text"`
}

// LogsResult is what harness_logs returns. Exactly one of Text and Entries is
// populated, by Format; the size fields say how much of the payload the caller
// actually got.
type LogsResult struct {
	Name   string           `json:"name"`
	Format string           `json:"format"`
	Source string           `json:"source,omitempty"`
	Run    *protocol.LogRun `json:"run,omitempty"`

	Text       string  `json:"text,omitempty"`
	Entries    []Entry `json:"entries,omitempty"`
	NextCursor string  `json:"next_cursor,omitempty"`
	Offset     int     `json:"offset,omitempty"`
	NextOffset int     `json:"next_offset,omitempty"`
	Truncated  bool    `json:"truncated,omitempty"`
	Dropped    int     `json:"dropped,omitempty"`
	Note       string  `json:"note,omitempty"`
}

// ReadLogs fetches and shapes one run's output. It makes the same control call
// `harness logs` makes (with --run when a run id is given) and only then
// reshapes the reply, so the tool can never show more than the verb does.
func ReadLogs(_ context.Context, d Daemon, a LogsArgs) (LogsResult, error) {
	if strings.TrimSpace(a.Name) == "" {
		return LogsResult{}, fmt.Errorf("a harness name is required")
	}
	format := a.Format
	if format == "" {
		format = FormatSummary
	}
	if format != FormatSummary && format != FormatRaw {
		return LogsResult{}, fmt.Errorf("unknown format %q (want %s or %s)", format, FormatSummary, FormatRaw)
	}
	lines := a.Lines
	if lines <= 0 {
		lines = DefaultLogLines
	}
	maxBytes := a.MaxBytes
	if maxBytes <= 0 || maxBytes > MaxLogBytes {
		maxBytes = MaxLogBytes
	}

	var (
		ld  protocol.LogsData
		err error
	)
	if a.Run > 0 {
		ld, err = d.RunLogs(a.Name, a.Run, lines)
	} else {
		ld, err = d.Logs(a.Name, lines)
	}
	if err != nil {
		return LogsResult{}, err
	}

	out := LogsResult{Name: a.Name, Format: format, Source: ld.Source, Run: ld.Run}
	if format == FormatSummary && ld.Source == protocol.LogSourceAgentTrace && len(ld.Entries) > 0 {
		fillSummary(&out, ld.Entries, a.Cursor, maxBytes)
		return out, nil
	}
	if format == FormatSummary {
		// No attributed activity for this run (a generic adapter, an old
		// daemon, or nothing recorded yet): the durable log is the record, so
		// say so rather than returning an empty summary.
		out.Note = "no structured activity for this run; text is the raw tail"
	}
	fillRaw(&out, ld.Text, a.Offset, maxBytes)
	return out, nil
}

// fillSummary applies the cursor, drops session bookkeeping, and keeps the
// newest entries that fit the size cap.
func fillSummary(out *LogsResult, entries []protocol.LogEntry, cursor string, maxBytes int) {
	kept := entries
	if cursor != "" {
		kept = afterCursor(entries, cursor)
		if kept == nil {
			kept = entries
			out.Note = "cursor is not in the returned window; the full tail is included"
		}
	}
	live := make([]protocol.LogEntry, 0, len(kept))
	for _, e := range kept {
		// Session bookkeeping is the transcript's own bookkeeping, not the run's
		// activity, so it is collapsed out of the summary.
		if e.Kind == protocol.LogEntrySession {
			out.Dropped++
			continue
		}
		live = append(live, e)
	}
	// The cap protects the caller's context, and the newest lines are the ones
	// a poller is missing, so drop from the front.
	size := 0
	start := 0
	for i := len(live) - 1; i >= 0; i-- {
		size += len(logview.FormatEntry(live[i])) + 1
		if size > maxBytes {
			start = i + 1
			break
		}
	}
	if start > 0 {
		out.Dropped += start
		out.Truncated = true
		live = live[start:]
	}
	for _, e := range live {
		out.Entries = append(out.Entries, Entry{
			ID: e.ID, Time: e.Time, Kind: e.Kind, Action: e.Action, Tool: e.Tool,
			Target: e.Target, Summary: e.Summary, Failed: e.Error,
			Text: logview.FormatEntry(e),
		})
	}
	if len(live) > 0 {
		out.NextCursor = live[len(live)-1].ID
	}
}

// afterCursor returns the entries newer than id, or nil when id is not in the
// window (the caller's cursor has aged out).
func afterCursor(entries []protocol.LogEntry, id string) []protocol.LogEntry {
	for i, e := range entries {
		if e.ID == id {
			return entries[i+1:]
		}
	}
	return nil
}

// fillRaw pages through the log tail by bytes, so a caller that hit the cap can
// ask for the next window rather than re-reading the whole thing.
func fillRaw(out *LogsResult, text string, offset, maxBytes int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(text) {
		offset = len(text)
	}
	rest := text[offset:]
	if len(rest) > maxBytes {
		rest = rest[:maxBytes]
		out.Truncated = true
	}
	if offset+len(rest) < len(text) {
		out.Truncated = true
	}
	out.Text = rest
	out.Offset = offset
	out.NextOffset = offset + len(rest)
}

// waitForRun follows a triggered run to its outcome, mirroring what
// `harness trigger --wait` does, but returning the record instead of printing
// it (stdout here is the protocol channel).
func waitForRun(ctx context.Context, d Daemon, name string, td protocol.TriggerData, before, seconds int) (protocol.RunInfo, error) {
	if seconds <= 0 {
		seconds = defaultWaitSeconds
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	wantID := 0
	if td.Run != nil {
		wantID = td.Run.RunID
	}
	for {
		rd, err := d.Runs(name, waitHistoryLimit)
		if err != nil {
			return protocol.RunInfo{}, err
		}
		if rec, ok := pickRun(rd.Runs, wantID, before); ok && rec.Outcome != "running" {
			return rec, nil
		}
		if time.Now().After(deadline) {
			return protocol.RunInfo{}, fmt.Errorf("run did not finish within %ds", seconds)
		}
		select {
		case <-ctx.Done():
			return protocol.RunInfo{}, ctx.Err()
		case <-time.After(waitPoll):
		}
	}
}

// pickRun finds the run to follow: by id when the daemon named one, otherwise
// the oldest manual run newer than before (a skipped record is a different
// trigger, so it is not the run this call started).
func pickRun(runs []protocol.RunInfo, id, before int) (protocol.RunInfo, bool) {
	var match protocol.RunInfo
	found := false
	for _, r := range runs {
		switch {
		case id > 0:
			if r.RunID == id {
				return r, true
			}
		case r.RunID > before && r.Trigger == "manual" && r.Outcome != "skipped":
			match, found = r, true
		}
	}
	return match, found
}
