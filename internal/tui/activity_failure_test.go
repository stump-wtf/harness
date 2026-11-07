package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/logview"
	"github.com/stump-wtf/harness/internal/protocol"
)

// failingSessionEvent is sessionEvent with a failing tool call carrying the
// masked excerpt agent-trace cut from its output.
func failingSessionEvent(kind tail.Harness, cwd, tool, ts, excerpt string) tail.Event {
	ev := sessionEvent(kind, cwd, tool, ts)
	ev.Classified.IsError = true
	ev.Classified.ErrorExcerpt = excerpt
	return ev
}

// The dashboard watcher captured the excerpt and nothing in the live path read
// it, so a harness showed its reason only in the post-mortem (issue #1036). The
// live action field is a 22-column column by design, so the reason is carried
// beside it, in the peek head (the dashboard's detail surface for the harness
// you are looking at), which means the model has to remember it.
func TestLiveFailureNamesWhyTheNewestCallFailed(t *testing.T) {
	m := &Model{}
	m.trackLastAction(failingSessionEvent(tail.HarnessCrush, "/srv/app", "Bash",
		"2026-10-31T11:00:00Z", "auth failed: the token was rejected\nretrying"))

	got := m.liveFailure(protocol.HarnessInfo{Name: "sweep", Adapter: "crush", Workdir: "/srv/app"})
	if want := "auth failed: the token was rejected retrying"; got != want {
		t.Errorf("liveFailure = %q, want %q", got, want)
	}
}

// A harness that recovers must stop being accused: the note follows the newest
// action, exactly as the action field does.
func TestLiveFailureClearsWhenANewerCallSucceeds(t *testing.T) {
	m := &Model{}
	m.trackLastAction(failingSessionEvent(tail.HarnessCrush, "/srv/app", "Bash",
		"2026-10-31T11:00:00Z", "exit status 1"))
	m.trackLastAction(sessionEvent(tail.HarnessCrush, "/srv/app", "Read", "2026-10-31T11:00:30Z"))

	if got := m.liveFailure(protocol.HarnessInfo{Adapter: "crush", Workdir: "/srv/app"}); got != "" {
		t.Errorf("liveFailure after a successful call = %q, want it gone", got)
	}
}

// The newest action wins, not the most alarming one: an older failure replayed
// after a newer success (the watcher replays in file order, and two sessions in
// one directory interleave) must not resurrect the note.
func TestLiveFailureFollowsTheNewestEventNotTheFailure(t *testing.T) {
	m := &Model{}
	m.trackLastAction(sessionEvent(tail.HarnessCrush, "/srv/app", "Read", "2026-10-31T11:30:00Z"))
	m.trackLastAction(failingSessionEvent(tail.HarnessCrush, "/srv/app", "Bash",
		"2026-10-31T11:00:00Z", "exit status 1"))

	if got := m.liveFailure(protocol.HarnessInfo{Adapter: "crush", Workdir: "/srv/app"}); got != "" {
		t.Errorf("liveFailure replayed out of order = %q, want the newer success", got)
	}
}

// Correlation is the same workdir-and-adapter evidence the action field uses,
// so one harness's failure never lands on another's row or preview.
func TestLiveFailureIsAttributedLikeLiveAction(t *testing.T) {
	m := &Model{}
	m.trackLastAction(failingSessionEvent(tail.HarnessClaudeCode, "/srv/alpha", "Bash",
		"2026-10-31T11:00:00Z", "exit status 1"))

	if got := m.liveFailure(protocol.HarnessInfo{Name: "beta", Adapter: "claude-code", Workdir: "/srv/beta"}); got != "" {
		t.Errorf("another harness's workdir reports %q, want nothing", got)
	}
	if got := m.liveFailure(protocol.HarnessInfo{Name: "alpha", Adapter: "crush", Workdir: "/srv/alpha"}); got != "" {
		t.Errorf("a different adapter in the same workdir reports %q, want nothing", got)
	}
	if got := m.liveFailure(protocol.HarnessInfo{Name: "alpha", Adapter: "claude-code", Workdir: "/srv/alpha"}); got != "exit status 1" {
		t.Errorf("liveFailure = %q, want the failure that happened there", got)
	}
}

// The note is stored flattened once, at the same width the post-mortem prints,
// so the two views cannot drift apart by clipping it twice at different widths.
func TestLiveFailureIsFlattenedAndBounded(t *testing.T) {
	m := &Model{}
	long := "FAIL\t" + time.Now().Format(time.RFC3339) + "\n" + strings.Repeat("e", 400)
	m.trackLastAction(failingSessionEvent(tail.HarnessCrush, "/srv/app", "Bash",
		"2026-10-31T11:00:00Z", long))

	got := m.liveFailure(protocol.HarnessInfo{Adapter: "crush", Workdir: "/srv/app"})
	if strings.ContainsAny(got, "\n\t") {
		t.Errorf("liveFailure spans more than one line: %q", got)
	}
	if len([]rune(got)) > logview.ExcerptWidth {
		t.Errorf("liveFailure is %d runes, want at most %d", len([]rune(got)), logview.ExcerptWidth)
	}
}
