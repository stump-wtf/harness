package loopguard

// Runaway Tool-Loop Guard Tests
//
// The unit tests drive handle directly with observer events; loop_e2e_test.go
// plants the incident's transcript in a real crush store under the real
// observer. Each stop assertion is paired with a count one short of it, so a
// guard that fires early fails as surely as one that never fires.
//
// @joestump-agent 09/23/2026 - Added with the guard (stumpcloud/stumpcloud#469).

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/observe"
)

// fakeStopper records stops and harness-log lines.
type fakeStopper struct {
	mu      sync.Mutex
	stopped []string
	logged  []string
}

func (f *fakeStopper) Stop(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, name)
	return true
}

func (f *fakeStopper) LogLifecycle(name, msg string, _ ...any) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logged = append(f.logged, name+": "+msg)
	return true
}

func (f *fakeStopper) stops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...)
}

func quietLogger() *log.Logger { return log.New(io.Discard) }

var t0 = time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)

func call(harness, session, tool, digest string, i int) observe.Event {
	ev := observe.Event{Harness: harness, Adapter: "crush", Kind: observe.KindTool, Time: t0.Add(time.Duration(i) * 12 * time.Second)}
	ev.Session.Key, ev.Session.ID = "crush:"+session, session
	ev.Tool.Tool, ev.Tool.InputDigest = tool, digest
	ev.Tool.Summary = tool + " -> 0 targets, 0 outside"
	return ev
}

func mark(harness, session, typ string) observe.Event {
	ev := observe.Event{Harness: harness, Adapter: "crush", Kind: observe.KindMark, Time: t0}
	ev.Session.Key, ev.Session.ID = "crush:"+session, session
	ev.Mark.Type = typ
	return ev
}

// comment is the incident's call: the same add_comment, same body, every time.
func comment(i int) observe.Event {
	return call("pool-worker", "s1", "mcp_gitea_issue_write", "d-add-comment-dot", i)
}

// TestIncidentStreakStopsAtExactlyN is the 2026-09-21 shape: one identical
// call, back to back, nothing between. The stop lands on the Nth call, not
// the N-1th, and names the harness, the tool and the count.
func TestIncidentStreakStopsAtExactlyN(t *testing.T) {
	for _, n := range []int{5, DefaultThreshold, 10} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			st := &fakeStopper{}
			g := New(st, Options{Threshold: n, Logger: quietLogger()})
			for i := 1; i < n; i++ {
				if g.handle(comment(i)) {
					t.Fatalf("stopped at identical call %d of %d", i, n)
				}
			}
			if len(st.stops()) != 0 {
				t.Fatalf("stopped before call %d: %v", n, st.stops())
			}
			if !g.handle(comment(n)) {
				t.Fatalf("identical call %d did not stop the harness", n)
			}
			if got := st.stops(); len(got) != 1 || got[0] != "pool-worker" {
				t.Fatalf("stops = %v, want [pool-worker]", got)
			}
			want := Trip{Harness: "pool-worker", Session: "s1", Tool: "mcp_gitea_issue_write", Count: n}
			if trips := g.Trips(); len(trips) != 1 || trips[0] != want {
				t.Fatalf("trips = %+v, want [%+v]", trips, want)
			}
			if len(st.logged) != 1 {
				t.Fatalf("harness log lines = %v, want one", st.logged)
			}
		})
	}
}

// TestLegitimatePatternsNeverStop is the control: the same tool called twice
// then with different arguments, an edit-test cycle whose `make test` is
// byte-identical every time, and a worker that polls one queue identically on
// every doorbell. Each runs to fifty times the threshold without a stop.
func TestLegitimatePatternsNeverStop(t *testing.T) {
	cases := map[string]func(i int) []observe.Event{
		"same tool twice, then different args": func(i int) []observe.Event {
			return []observe.Event{
				call("w", "s", "mcp_gitea_issue_write", fmt.Sprintf("issue-%d", i), i),
				call("w", "s", "mcp_gitea_issue_write", fmt.Sprintf("issue-%d", i), i),
			}
		},
		"edit then an identical make test": func(i int) []observe.Event {
			return []observe.Event{
				call("w", "s", "edit", fmt.Sprintf("patch-%d", i), i),
				call("w", "s", "bash", "d-make-test", i),
			}
		},
		"identical poll per doorbell": func(i int) []observe.Event {
			return []observe.Event{
				mark("w", "s", "user-message"),
				call("w", "s", "mcp_switchboard_claim_next", "d-lane-m", i),
			}
		},
		"same args, different tools": func(i int) []observe.Event {
			tool := []string{"view", "grep"}[i%2]
			return []observe.Event{call("w", "s", tool, "d-same", i)}
		},
	}
	for name, step := range cases {
		t.Run(name, func(t *testing.T) {
			st := &fakeStopper{}
			g := New(st, Options{Logger: quietLogger()})
			for i := 0; i < 50*DefaultThreshold; i++ {
				for _, ev := range step(i) {
					g.handle(ev)
				}
			}
			if got := st.stops(); len(got) != 0 {
				t.Fatalf("stopped a legitimate pattern: %v", got)
			}
		})
	}
}

// TestStreakResets: each reset leaves the streak one short, so the calls
// after it must not stop until a full streak has built again.
func TestStreakResets(t *testing.T) {
	const n = DefaultThreshold
	resets := map[string]observe.Event{
		"a different call":      call("pool-worker", "s1", "view", "d-other", 0),
		"a user message":        mark("pool-worker", "s1", "user-message"),
		"a call with no digest": call("pool-worker", "s1", "mcp_gitea_issue_write", "", 0),
	}
	for name, reset := range resets {
		t.Run(name, func(t *testing.T) {
			st := &fakeStopper{}
			g := New(st, Options{Threshold: n, Logger: quietLogger()})
			for i := 1; i < n; i++ {
				g.handle(comment(i))
			}
			g.handle(reset)
			for i := 1; i < n; i++ {
				if g.handle(comment(i)) {
					t.Fatalf("stopped %d calls after %s", i, name)
				}
			}
			if !g.handle(comment(n)) {
				t.Fatalf("a full streak after %s did not stop", name)
			}
		})
	}
	t.Run("a new session", func(t *testing.T) {
		st := &fakeStopper{}
		g := New(st, Options{Threshold: n, Logger: quietLogger()})
		inS2 := func(i int) observe.Event {
			return call("pool-worker", "s2", "mcp_gitea_issue_write", "d-add-comment-dot", i)
		}
		for i := 1; i < n; i++ {
			g.handle(comment(i))
		}
		for i := 1; i < n; i++ {
			if g.handle(inS2(i)) {
				t.Fatalf("stopped at call %d of a new session", i)
			}
		}
		if !g.handle(inS2(n)) {
			t.Fatal("a full streak in the new session did not stop")
		}
	})
	t.Run("error marks do not reset", func(t *testing.T) {
		st := &fakeStopper{}
		g := New(st, Options{Threshold: n, Logger: quietLogger()})
		for i := 1; i < n; i++ {
			g.handle(comment(i))
			g.handle(mark("pool-worker", "s1", "error"))
		}
		if !g.handle(comment(n)) {
			t.Fatal("retrying one call through provider errors is still a streak")
		}
	})
	// A turn ending is the agent stopping, not new input: the same call
	// repeated across turns that restart with no prompt between is a loop.
	// With a prompt between, the user-message mark resets as usual.
	t.Run("turn-end marks do not reset", func(t *testing.T) {
		st := &fakeStopper{}
		g := New(st, Options{Threshold: n, Logger: quietLogger()})
		for i := 1; i < n; i++ {
			g.handle(comment(i))
			g.handle(mark("pool-worker", "s1", "turn-end"))
		}
		if !g.handle(comment(n)) {
			t.Fatal("the same call across prompt-less turns is still a streak")
		}
	})
	t.Run("turn-end then a prompt resets", func(t *testing.T) {
		st := &fakeStopper{}
		g := New(st, Options{Threshold: n, Logger: quietLogger()})
		for i := 1; i < n; i++ {
			g.handle(comment(i))
		}
		g.handle(mark("pool-worker", "s1", "turn-end"))
		g.handle(mark("pool-worker", "s1", "user-message"))
		for i := 1; i < n; i++ {
			if g.handle(comment(i)) {
				t.Fatalf("stopped %d calls into a new prompt's turn", i)
			}
		}
	})
}

// TestStopResetsTheStreak: after a stop, the dying run's last few polled
// calls do not stop it again, and a harness restarted into the same loop is
// stopped again after a fresh full streak.
func TestStopResetsTheStreak(t *testing.T) {
	const n = DefaultThreshold
	st := &fakeStopper{}
	g := New(st, Options{Threshold: n, Logger: quietLogger()})
	for i := 1; i <= n; i++ {
		g.handle(comment(i))
	}
	for i := 1; i < n; i++ {
		if g.handle(comment(n + i)) {
			t.Fatalf("stopped again %d calls after the first stop", i)
		}
	}
	if !g.handle(comment(2 * n)) {
		t.Fatal("a second full streak did not stop")
	}
	if got := st.stops(); len(got) != 2 {
		t.Fatalf("stops = %v, want two", got)
	}
}

// TestHarnessesAreIndependent: two harnesses interleaving the same call build
// separate streaks, and only the one that reaches N is stopped.
func TestHarnessesAreIndependent(t *testing.T) {
	const n = DefaultThreshold
	st := &fakeStopper{}
	g := New(st, Options{Threshold: n, Logger: quietLogger()})
	for i := 1; i <= n; i++ {
		g.handle(comment(i))
		if i < n {
			g.handle(call("other", "s9", "mcp_gitea_issue_write", "d-add-comment-dot", i))
		}
	}
	if got := st.stops(); len(got) != 1 || got[0] != "pool-worker" {
		t.Fatalf("stops = %v, want [pool-worker]", got)
	}
}

func TestDefaults(t *testing.T) {
	for _, n := range []int{-1, 0, 1} {
		if g := New(&fakeStopper{}, Options{Threshold: n}); g.threshold != DefaultThreshold {
			t.Errorf("Threshold %d: got %d, want the default %d", n, g.threshold, DefaultThreshold)
		}
	}
}
