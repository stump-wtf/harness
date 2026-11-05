package chatroom

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/logview"
)

// The live action field and the post-mortem are two views of one failure, so
// they must show the same bytes: `logview.Parts` prints the excerpt beside
// (failed), and the live surfaces read it off the same event. The dashboard
// watcher produced the excerpt but nothing in the live path consumed it, so a
// run showed its reason only after it was over (issue #1036).
func TestFailureExcerptMatchesThePostMortem(t *testing.T) {
	tests := []struct {
		name  string
		ev    tail.Event
		want  string
		marks []classify.Mark
	}{
		{
			name: "a failing call says why, on one line",
			ev: failureEvent(classify.Event{
				Tool:         "bash",
				Action:       classify.ActionExec,
				IsError:      true,
				ErrorExcerpt: "go build ./...\nundefined: foo\nFAIL\texample.com/x",
			}),
			want: "go build ./... undefined: foo FAIL example.com/x",
		},
		{
			name: "a successful call carries nothing",
			ev: failureEvent(classify.Event{
				Tool:         "bash",
				Action:       classify.ActionExec,
				ErrorExcerpt: "exit status 0",
			}),
			want: "",
		},
		{
			name: "a failure with no excerpt says nothing extra",
			ev: failureEvent(classify.Event{
				Tool:    "bash",
				Action:  classify.ActionExec,
				IsError: true,
			}),
			want: "",
		},
		{
			name: "a mark carries no call excerpt",
			ev: failureEvent(classify.Event{
				ErrorExcerpt: "exit status 1",
			}),
			marks: []classify.Mark{{Seq: 1, Type: "error", Note: "rate limited"}},
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := tc.ev
			ev.Marks = tc.marks
			if got := FailureExcerpt(ev); got != tc.want {
				t.Errorf("FailureExcerpt = %q, want %q", got, tc.want)
			}
		})
	}
}

// Parity is the point of the change: the excerpt a live surface prints is
// clipped and flattened exactly as `harness logs` prints it, so the same
// failure reads the same in both.
func TestFailureExcerptIsWhatLogviewPrints(t *testing.T) {
	ev := failureEvent(classify.Event{
		Tool:         "bash",
		Action:       classify.ActionExec,
		IsError:      true,
		ErrorExcerpt: "auth failed: the token was rejected",
	})

	got := FailureExcerpt(ev)
	suffix := logview.Parts(MakeRenderable(ev).Entries()[0]).Suffix
	if !strings.Contains(suffix, got) {
		t.Errorf("post-mortem suffix %q does not carry the live excerpt %q", suffix, got)
	}
	if !strings.Contains(suffix, "(failed)") {
		t.Errorf("post-mortem suffix %q lost its failure marker", suffix)
	}
}

// An excerpt clipped for the post-mortem stays clipped for the live view: the
// bound is the same one agent-trace cut it to, and clipping twice at different
// widths is how two views disagree about one failure.
func TestFailureExcerptRespectsTheLogviewWidth(t *testing.T) {
	ev := failureEvent(classify.Event{
		Tool:         "bash",
		Action:       classify.ActionExec,
		IsError:      true,
		ErrorExcerpt: strings.Repeat("f", logview.ExcerptWidth+200),
	})
	if got := FailureExcerpt(ev); len([]rune(got)) > logview.ExcerptWidth {
		t.Errorf("excerpt is %d runes, want at most %d", len([]rune(got)), logview.ExcerptWidth)
	}
}

// failureEvent wraps classified in the session metadata every event carries.
func failureEvent(c classify.Event) tail.Event {
	c.Timestamp = "2026-10-31T14:00:00Z"
	return tail.Event{
		Session:    tail.SessionMeta{Harness: tail.HarnessCrush, ID: "s1", Cwd: "/srv/app"},
		Classified: c,
		ReceivedAt: time.Now(),
	}
}
