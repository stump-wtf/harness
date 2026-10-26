package mcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/protocol"
)

func readLogs(t *testing.T, d Daemon, a LogsArgs) (LogsResult, error) {
	t.Helper()
	return ReadLogs(context.Background(), d, a)
}

func TestSummarySkipsSessionBookkeepingAndCarriesACursor(t *testing.T) {
	d := &fakeDaemon{logs: protocol.LogsData{
		Name:   "web",
		Source: protocol.LogSourceAgentTrace,
		Entries: entries(
			[3]string{protocol.LogEntrySession, "session", "session 1a2b"},
			[3]string{protocol.LogEntryTool, "edit", "main.go"},
			[3]string{protocol.LogEntryMark, "user-message", "fix the cursor"},
			[3]string{protocol.LogEntryLifecycle, "exited", "exit 0"},
		),
	}}
	res, err := readLogs(t, d, LogsArgs{Name: "web"})
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if res.Dropped != 1 {
		t.Errorf("dropped = %d, want only the session entry", res.Dropped)
	}
	if len(res.Entries) != 3 {
		t.Fatalf("entries = %+v, want the tool call, the mark and the exit", res.Entries)
	}
	if res.NextCursor != "e4" {
		t.Errorf("next_cursor = %q, want the last kept entry", res.NextCursor)
	}
	if !strings.Contains(res.Entries[0].Text, "main.go") {
		t.Errorf("entry text = %q, want the rendered line the CLI prints", res.Entries[0].Text)
	}
}

func TestSummaryCursorReturnsOnlyWhatIsNew(t *testing.T) {
	all := entries(
		[3]string{protocol.LogEntryTool, "read", "a.go"},
		[3]string{protocol.LogEntryTool, "edit", "b.go"},
		[3]string{protocol.LogEntryTool, "exec", "go test"},
	)
	d := &fakeDaemon{logs: protocol.LogsData{Name: "web", Source: protocol.LogSourceAgentTrace, Entries: all}}

	first, err := readLogs(t, d, LogsArgs{Name: "web"})
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	second, err := readLogs(t, d, LogsArgs{Name: "web", Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("ReadLogs(cursor): %v", err)
	}
	if len(second.Entries) != 0 {
		t.Fatalf("a poll with nothing new returned %d entries", len(second.Entries))
	}

	// A run that has produced one more line since the cursor returns exactly it.
	d.logs.Entries = append(all, protocol.LogEntry{ID: "e4", Kind: protocol.LogEntryTool, Action: "edit", Summary: "c.go"})
	third, err := readLogs(t, d, LogsArgs{Name: "web", Cursor: "e3"})
	if err != nil {
		t.Fatalf("ReadLogs(cursor e3): %v", err)
	}
	if len(third.Entries) != 1 || third.Entries[0].ID != "e4" {
		t.Fatalf("entries = %+v, want only e4", third.Entries)
	}
	if third.NextCursor != "e4" {
		t.Errorf("next_cursor = %q, want e4", third.NextCursor)
	}
}

func TestAgedOutCursorSaysSo(t *testing.T) {
	d := &fakeDaemon{logs: protocol.LogsData{Name: "web", Source: protocol.LogSourceAgentTrace,
		Entries: entries([3]string{protocol.LogEntryTool, "read", "a.go"})}}
	res, err := readLogs(t, d, LogsArgs{Name: "web", Cursor: "e999"})
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if res.Note == "" {
		t.Fatal("a cursor outside the window was answered as if it were current")
	}
	if len(res.Entries) != 1 {
		t.Fatalf("entries = %+v, want the whole tail rather than nothing", res.Entries)
	}
}

func TestSummaryFallsBackToTheRawTail(t *testing.T) {
	// A generic adapter records no activity, so the durable log is the record.
	d := &fakeDaemon{logs: protocol.LogsData{Name: "tool", Text: "make: *** [test] Error 1"}}
	res, err := readLogs(t, d, LogsArgs{Name: "tool"})
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if res.Note == "" || !strings.Contains(res.Text, "Error 1") {
		t.Fatalf("fallback = %+v, want the raw tail plus a note", res)
	}
}

func TestRawPagesWithOffsetAndCap(t *testing.T) {
	text := strings.Repeat("x", 5000)
	d := &fakeDaemon{logs: protocol.LogsData{Name: "web", Text: text}}

	first, err := readLogs(t, d, LogsArgs{Name: "web", Format: FormatRaw, MaxBytes: 2000})
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if len(first.Text) != 2000 || !first.Truncated || first.NextOffset != 2000 {
		t.Fatalf("first window: len=%d truncated=%v next=%d", len(first.Text), first.Truncated, first.NextOffset)
	}
	rest, err := readLogs(t, d, LogsArgs{Name: "web", Format: FormatRaw, Offset: first.NextOffset, MaxBytes: 2000})
	if err != nil {
		t.Fatalf("ReadLogs(offset): %v", err)
	}
	if len(rest.Text) != 2000 || rest.Offset != 2000 || rest.NextOffset != 4000 || !rest.Truncated {
		t.Fatalf("second window: %+v", rest)
	}
	tail, err := readLogs(t, d, LogsArgs{Name: "web", Format: FormatRaw, Offset: 4000, MaxBytes: 2000})
	if err != nil {
		t.Fatalf("ReadLogs(tail): %v", err)
	}
	if len(tail.Text) != 1000 || tail.Truncated {
		t.Fatalf("last window: len=%d truncated=%v", len(tail.Text), tail.Truncated)
	}
}

func TestSummaryHonoursTheSizeCapWithTheNewestLines(t *testing.T) {
	specs := make([][3]string, 0, 60)
	for i := range 60 {
		specs = append(specs, [3]string{protocol.LogEntryTool, "exec", "exec " + strings.Repeat("y", 100) + " " + padTwo(i)})
	}
	d := &fakeDaemon{logs: protocol.LogsData{Name: "web", Source: protocol.LogSourceAgentTrace, Entries: entries(specs...)}}
	res, err := readLogs(t, d, LogsArgs{Name: "web", MaxBytes: 1200})
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if !res.Truncated || res.Dropped == 0 {
		t.Fatalf("cap ignored: truncated=%v dropped=%d", res.Truncated, res.Dropped)
	}
	if res.NextCursor != "e60" {
		t.Errorf("next_cursor = %q, want the newest entry so a poll continues from there", res.NextCursor)
	}
	last := res.Entries[len(res.Entries)-1]
	if last.ID != "e60" {
		t.Errorf("newest kept entry = %q, want e60", last.ID)
	}
}

func TestLogsArgsAreValidated(t *testing.T) {
	d := &fakeDaemon{logs: protocol.LogsData{Name: "web", Text: "x"}}
	if _, err := readLogs(t, d, LogsArgs{Name: "  "}); err == nil {
		t.Error("a blank name was accepted")
	}
	if _, err := readLogs(t, d, LogsArgs{Name: "web", Format: "yaml"}); err == nil {
		t.Error("an unknown format was accepted")
	}
	if len(d.calls) != 0 {
		t.Fatalf("invalid arguments reached the daemon: %v", d.calls)
	}
}

func TestLogsDefaultsWithoutArguments(t *testing.T) {
	d := &fakeDaemon{logs: protocol.LogsData{Name: "web", Source: protocol.LogSourceAgentTrace,
		Entries: entries([3]string{protocol.LogEntryLifecycle, "state", "running"})}}
	res, err := readLogs(t, d, LogsArgs{Name: "web"})
	if err != nil {
		t.Fatalf("ReadLogs: %v", err)
	}
	if d.logsLines != DefaultLogLines {
		t.Errorf("lines = %d, want the default tail", d.logsLines)
	}
	if res.Format != FormatSummary {
		t.Errorf("format = %q, want the default summary", res.Format)
	}
}

// padTwo labels an entry so a truncated summary can be checked for keeping the
// newest lines.
func padTwo(i int) string {
	return fmt.Sprintf("%02d", i)
}
