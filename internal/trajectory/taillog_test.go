package trajectory

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/adapter"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

func TestLineFormatsToolAndMarks(t *testing.T) {
	ev := tail.Event{
		Session: tail.SessionMeta{Key: "s", Harness: tail.HarnessCrush},
		Marks: []classify.Mark{{
			Seq:       1,
			Timestamp: "2026-09-06T12:00:00Z",
			Type:      "user",
			Note:      "fix the login bug",
		}},
		Classified: classify.Event{
			Seq:       2,
			Timestamp: "2026-09-06T12:00:01Z",
			Tool:      "bash",
			Action:    classify.ActionExec,
			Summary:   "go test ./...",
		},
		ReceivedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
	lines := Line(ev)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "USER") || !strings.Contains(lines[0], "fix the login bug") {
		t.Fatalf("mark line wrong: %q", lines[0])
	}
	for _, want := range []string{"crush", "exec", "bash", "go test ./..."} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("tool line missing %q: %q", want, lines[1])
		}
	}
}

func TestLineErrorBadge(t *testing.T) {
	ev := tail.Event{
		Session: tail.SessionMeta{Key: "s", Harness: tail.HarnessCodex},
		Classified: classify.Event{
			Tool: "edit", Action: classify.ActionEdit, Summary: "nope", IsError: true,
		},
		ReceivedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
	lines := Line(ev)
	if !strings.Contains(lines[0], "[ERROR]") {
		t.Fatalf("expected [ERROR] badge: %q", lines[0])
	}
}

func TestHarnessSnapshotLinesGenericAdapterIsNoop(t *testing.T) {
	reg := adapter.NewRegistryWithDefaults()
	lines, err := HarnessSnapshotLines(context.Background(), reg, "generic", "/tmp", 10)
	if err != nil {
		t.Fatalf("generic adapter should not error: %v", err)
	}
	if lines != nil {
		t.Fatalf("generic adapter should produce no lines, got %v", lines)
	}
}

func TestHarnessSnapshotLinesUnknownAdapterIsNoop(t *testing.T) {
	reg := adapter.NewRegistryWithDefaults()
	lines, err := HarnessSnapshotLines(context.Background(), reg, "does-not-exist", "", 10)
	if err != nil {
		t.Fatalf("unknown adapter should not error: %v", err)
	}
	if lines != nil {
		t.Fatalf("unknown adapter should produce no lines, got %v", lines)
	}
}
