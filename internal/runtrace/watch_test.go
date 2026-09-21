package runtrace

// Turn-State Watcher Tests
//
// The watcher's contract on real stores: the freshest attributed event time,
// resumed-session credit, and the fail-closed ambiguity rule — built on the
// same crush fixtures the correlation tests use, read by the watcher's manual
// mode (poll <= 0, Refresh driven by the test), so no clock is involved.
//
// Governing: SPEC-0012 REQ "Turn State Signal"; SPEC-0006 REQ "Run
// Correlation" (resumed sessions); issue #384.
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#384.

import (
	"path/filepath"
	"testing"
	"time"

	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
)

// watchRig builds a watcher in manual mode, a crush store in workdir, and the
// scope that reads it.
func watchRig(t *testing.T, work string, sessions ...rt.CrushSession) (*Watcher, Scope) {
	t.Helper()
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"), sessions...)
	h := crushHarness(t, "sweep", work, nil)
	return NewWatcher(0), h
}

// turnOf is Turn with the error handling the tests share.
func turnOf(t *testing.T, w *Watcher, name string) TurnState {
	t.Helper()
	ts, ok := w.Turn(name)
	if !ok {
		t.Fatalf("Turn(%s) = ok=false (%s)", name, w.Unavailable(name))
	}
	return ts
}

func TestWatcherReportsTheFreshestAttributedEvent(t *testing.T) {
	work := filepath.Join(t.TempDir(), "src")
	w, h := watchRig(t, work,
		sess("during", spawn, read("c1", "a.go", spawn.Add(5*time.Second))),
	)
	w.Follow(h.Name, h, Window{Start: spawn}, nil, spawn.Add(time.Hour))

	// The first sample is synchronous: Turn has an answer with no Refresh.
	ts := turnOf(t, w, h.Name)
	if want := spawn.Add(5 * time.Second).Truncate(time.Second); !ts.LastEventAt.Equal(want) {
		t.Errorf("LastEventAt = %v, want %v", ts.LastEventAt, want)
	}
	if ts.TurnMarkers {
		t.Error("TurnMarkers = true with no reader shipping boundaries; closes would trust nothing")
	}

	// The agent does more work — a new session whose events land later in
	// the run — and a refresh advances the sample across both sessions.
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("during2", spawn.Add(8*time.Second), read("c2", "b.go", spawn.Add(9*time.Second))),
	)
	w.Refresh(h.Name)
	ts = turnOf(t, w, h.Name)
	if want := spawn.Add(9 * time.Second).Truncate(time.Second); !ts.LastEventAt.Equal(want) {
		t.Errorf("after refresh LastEventAt = %v, want %v", ts.LastEventAt, want)
	}
}

// SPEC-0012 Scenario "Resumed session": a session that began before the run
// credits its in-window events to the run that resumed it.
func TestWatcherCreditsAResumedSession(t *testing.T) {
	work := filepath.Join(t.TempDir(), "src")
	w, h := watchRig(t, work,
		// Begun an hour before the run; its new events land inside the
		// run's window when the agent resumes with --continue.
		sess("resumed", spawn.Add(-time.Hour),
			read("c0", "old.go", spawn.Add(-time.Hour)),
			read("c1", "a.go", spawn.Add(5*time.Second)),
			read("c2", "b.go", spawn.Add(6*time.Minute)),
		),
	)
	w.Follow(h.Name, h, Window{Start: spawn}, nil, spawn.Add(time.Hour))

	ts := turnOf(t, w, h.Name)
	if want := spawn.Add(6 * time.Minute).Truncate(time.Second); !ts.LastEventAt.Equal(want) {
		t.Errorf("LastEventAt = %v, want %v (the in-window events, credited)", ts.LastEventAt, want)
	}
}

// SPEC-0012 Scenario "Ambiguous session": two harnesses share a workdir and
// adapter, so the resumed session is credited to no one, and the watch reports
// unavailability instead of wearing a sibling's activity.
func TestWatcherRefusesAnAmbiguousResumedSession(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "src")
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("resumed", spawn.Add(-time.Hour), read("c1", "a.go", spawn.Add(5*time.Second))),
	)
	w := NewWatcher(0)
	h := crushHarness(t, "sweep", work, nil)
	sibling := crushHarness(t, "sibling", work, nil)
	sibling.Runs = []Window{{Start: spawn.Add(-time.Hour), End: spawn.Add(time.Hour)}}

	w.Follow(h.Name, h, Window{Start: spawn}, []Scope{sibling}, spawn.Add(time.Hour))
	if _, ok := w.Turn(h.Name); ok {
		t.Fatal("Turn = ok for a session a same-workdir sibling could have written")
	}
	if w.Unavailable(h.Name) == "" {
		t.Error("Unavailable is empty for an ambiguous run; the close log would say nothing")
	}
}

// A run with nothing attributable at all answers unavailable, with the reason
// a graceful close is required to log.
func TestWatcherReportsUnavailability(t *testing.T) {
	w, h := watchRig(t, filepath.Join(t.TempDir(), "empty"))
	w.Follow(h.Name, h, Window{Start: spawn}, nil, spawn.Add(time.Hour))
	if ts, ok := w.Turn(h.Name); ok {
		t.Errorf("Turn = %+v, ok for an empty store", ts)
	}
	if w.Unavailable(h.Name) == "" {
		t.Error("Unavailable is empty for a run with no attributable session")
	}
}

func TestWatcherIgnoresUnknownNamesAndCleansUp(t *testing.T) {
	w := NewWatcher(0)
	if ts, ok := w.Turn("nobody"); ok || ts != (TurnState{}) {
		t.Errorf("Turn(unknown) = %+v, %v; want zero, false", ts, ok)
	}
	w.Refresh("nobody") // no-op, no panic
	w.Unfollow("nobody")

	h := crushHarness(t, "sweep", filepath.Join(t.TempDir(), "src"), nil)
	w.Follow(h.Name, h, Window{Start: spawn}, nil, spawn.Add(time.Hour))
	w.Unfollow(h.Name)
	w.Unfollow(h.Name) // idempotent
	if ts, ok := w.Turn(h.Name); ok {
		t.Errorf("Turn after unfollow = %+v; the watch must be gone", ts)
	}
}
