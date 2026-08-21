package chatroom

import (
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

func TestIdentityFor(t *testing.T) {
	tests := []struct {
		harness  tail.Harness
		username string
	}{
		{tail.HarnessClaudeCode, "@claude-code"},
		{"codex", "@codex"},
		{tail.HarnessCrush, "@crush-signal"},
		{tail.HarnessOpenCode, "@opencode"},
		{"pi", "@pi"},
		{"unknown", "@unknown"},
	}
	for _, tt := range tests {
		t.Run(string(tt.harness), func(t *testing.T) {
			id := IdentityFor(tt.harness)
			if id.Username != tt.username {
				t.Errorf("Username = %q, want %q", id.Username, tt.username)
			}
		})
	}
}

func TestActionBadge(t *testing.T) {
	tests := []struct {
		action, want string
	}{
		{classify.ActionSearch, "[SEARCH]"},
		{classify.ActionRead, "[READ]"},
		{classify.ActionEdit, "[EDIT]"},
		{classify.ActionExec, "[EXEC]"},
		{classify.ActionVerify, "[VERIFY]"},
		{"unknown", "[OTHER]"},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got := ActionBadge(tt.action)
			if got != tt.want {
				t.Errorf("ActionBadge(%q) = %q, want %q", tt.action, got, tt.want)
			}
		})
	}
}

func TestMakeRenderable(t *testing.T) {
	ev := tail.Event{
		Session: tail.SessionMeta{
			Harness: tail.HarnessCrush,
			ID:      "test-session",
		},
		Classified: classify.Event{
			Seq:       1,
			Timestamp: "2026-08-21T14:00:00Z",
			Tool:      "bash",
			Action:    classify.ActionExec,
			Summary:   "ran go test",
		},
		ReceivedAt: time.Now(),
	}

	re := MakeRenderable(ev)

	if re.Identity.Username != "@crush-signal" {
		t.Errorf("Username = %q, want @crush-signal", re.Identity.Username)
	}
	if re.Badge != "[EXEC]" {
		t.Errorf("Badge = %q, want [EXEC]", re.Badge)
	}
	if re.Tool != "bash" {
		t.Errorf("Tool = %q, want bash", re.Tool)
	}
	if re.Summary != "ran go test" {
		t.Errorf("Summary = %q, want 'ran go test'", re.Summary)
	}
	if re.Time != "14:00:00" {
		t.Errorf("Time = %q, want 14:00:00", re.Time)
	}
}

func TestEventBufferInsert(t *testing.T) {
	b := NewEventBuffer(100)

	events := []tail.Event{
		{Session: tail.SessionMeta{Harness: tail.HarnessCrush}, Classified: classify.Event{Timestamp: "2026-08-21T14:00:03Z"}},
		{Session: tail.SessionMeta{Harness: tail.HarnessClaudeCode}, Classified: classify.Event{Timestamp: "2026-08-21T14:00:01Z"}},
		{Session: tail.SessionMeta{Harness: "codex"}, Classified: classify.Event{Timestamp: "2026-08-21T14:00:02Z"}},
	}

	for _, ev := range events {
		b.Insert(MakeRenderable(ev))
	}

	if b.Len() != 3 {
		t.Fatalf("Len = %d, want 3", b.Len())
	}

	// Should be sorted by timestamp.
	visible := b.Visible()
	if visible[0].Event.Classified.Timestamp != "2026-08-21T14:00:01Z" {
		t.Errorf("First event ts = %q, want earliest", visible[0].Event.Classified.Timestamp)
	}
	if visible[2].Event.Classified.Timestamp != "2026-08-21T14:00:03Z" {
		t.Errorf("Last event ts = %q, want latest", visible[2].Event.Classified.Timestamp)
	}
}

func TestEventBufferMaxSize(t *testing.T) {
	b := NewEventBuffer(3)

	for i := 0; i < 5; i++ {
		ts := "2026-08-21T14:00:0" + string(rune('0'+i)) + "Z"
		b.Insert(MakeRenderable(tail.Event{
			Classified: classify.Event{Timestamp: ts},
			Session:    tail.SessionMeta{Harness: tail.HarnessCrush},
		}))
	}

	if b.Len() != 3 {
		t.Errorf("Len = %d, want 3 (maxSize)", b.Len())
	}
	// Oldest should have been evicted.
	visible := b.Visible()
	if visible[0].Event.Classified.Timestamp == "2026-08-21T14:00:00Z" {
		t.Error("Oldest event was not evicted")
	}
}

func TestEventBufferFilter(t *testing.T) {
	b := NewEventBuffer(100)

	b.Insert(MakeRenderable(tail.Event{Session: tail.SessionMeta{Harness: tail.HarnessClaudeCode}}))
	b.Insert(MakeRenderable(tail.Event{Session: tail.SessionMeta{Harness: tail.HarnessCrush}}))
	b.Insert(MakeRenderable(tail.Event{Session: tail.SessionMeta{Harness: "codex"}}))

	// Filter to only claude-code (index 0).
	b.SetFilter(FilterSet(0).Toggle(0))
	visible := b.Visible()
	if len(visible) != 1 {
		t.Fatalf("Visible len = %d, want 1", len(visible))
	}
	if visible[0].Identity.Harness != tail.HarnessClaudeCode {
		t.Errorf("Visible harness = %q, want claude-code", visible[0].Identity.Harness)
	}

	// All harnesses.
	b.SetFilter(AllHarnesses())
	if len(b.Visible()) != 3 {
		t.Errorf("All visible len = %d, want 3", len(b.Visible()))
	}
}

func TestFilterSetString(t *testing.T) {
	if AllHarnesses().String() != "all" {
		t.Errorf("AllHarnesses().String() = %q, want 'all'", AllHarnesses().String())
	}

	f := FilterSet(0) // none
	if f.String() != "none" {
		t.Errorf("Empty filter String() = %q, want 'none'", f.String())
	}
}

func TestLastAction(t *testing.T) {
	ev := tail.Event{
		Classified: classify.Event{
			Tool:    "read_file",
			Action:  classify.ActionRead,
			Summary: "read form.go",
		},
	}
	got := LastAction(ev)
	if got != "[READ] read_file" {
		t.Errorf("LastAction = %q, want '[READ] read_file'", got)
	}

	// Marks only.
	ev2 := tail.Event{
		Marks: []classify.Mark{{Type: "user", Note: "hello"}},
	}
	got2 := LastAction(ev2)
	if got2 != "[USER]" {
		t.Errorf("LastAction (marks) = %q, want '[USER]'", got2)
	}
}

func TestLastForHarness(t *testing.T) {
	b := NewEventBuffer(100)
	b.Insert(MakeRenderable(tail.Event{Session: tail.SessionMeta{Harness: tail.HarnessClaudeCode}, Classified: classify.Event{Tool: "grep"}}))
	b.Insert(MakeRenderable(tail.Event{Session: tail.SessionMeta{Harness: tail.HarnessCrush}, Classified: classify.Event{Tool: "bash"}}))

	last := b.LastForHarness(tail.HarnessClaudeCode)
	if last == nil {
		t.Fatal("LastForHarness returned nil")
	}
	if last.Tool != "grep" {
		t.Errorf("Tool = %q, want 'grep'", last.Tool)
	}

	if b.LastForHarness("nonexistent") != nil {
		t.Error("LastForHarness for unknown harness should return nil")
	}
}

func TestTruncateShort(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hell…"},
		{"hello", 5, "hello"},
	}
	for _, tt := range tests {
		got := truncateShort(tt.input, tt.n)
		if got != tt.want {
			t.Errorf("truncateShort(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
		}
	}
}
