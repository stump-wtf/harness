package loopguard

// The 2026-09-21 Runaway Loop, Through The Real Observer
//
// Governing: stumpcloud/stumpcloud#469.
//
// The planted defect is the incident's transcript: a crush pool worker's
// session where every assistant row is one mcp_gitea_issue_write add_comment
// call with the same arguments and no text part, answered by a tool row, over
// and over, twelve seconds apart. It is written into a real crush store,
// read by agent-trace's real crush adapter inside the real observer, and
// delivered to the guard over a real subscription; only the Manager is a
// stand-in (daemon_wiring_test.go in cmd/harness covers the real one). A test
// fed hand-built events would pass against an adapter that never fills
// InputDigest, and every call would then look like a different call.
//
// @joestump-agent 09/23/2026 - Added with the guard.

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// source is the one-harness Manager stand-in the observer reads.
type source struct {
	h    core.Harness
	snap supervisor.Snapshot
}

func (s source) Snapshots() []supervisor.Snapshot { return []supervisor.Snapshot{s.snap} }

func (s source) HarnessRecord(name string) (core.Harness, string, bool) {
	return s.h, "", name == s.h.Name
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// loopRig is a crush worker's store under a real observer and a guard.
type loopRig struct {
	db    string
	c     *clock
	obs   *observe.Observer
	guard *Guard
	stops *fakeStopper
	at    time.Time
	n     int // calls written
}

func newLoopRig(t *testing.T, threshold int) *loopRig {
	t.Helper()
	start := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	home := t.TempDir()
	work := filepath.Join(home, "work")
	r := &loopRig{db: filepath.Join(work, ".crush", "crush.db"), c: &clock{t: start}, stops: &fakeStopper{}, at: start}

	src := source{
		h:    core.Harness{Name: "pool-worker", Adapter: "crush", Workdir: work},
		snap: supervisor.Snapshot{Name: "pool-worker", State: core.StateRunning, Enabled: true, PID: 4242, LastStarted: start.Add(-time.Hour)},
	}
	rt.WriteCrushDB(t, r.db, rt.CrushSession{
		ID: "sess-1", Created: start.Add(-time.Hour), Updated: start.Add(-time.Minute),
		Messages: []rt.CrushMessage{{Role: "user", At: start.Add(-time.Minute), Parts: `[{"type":"text","data":{"text":"triage harness#383"}}]`}},
	})
	r.obs = observe.New(src, observe.Options{
		PollInterval: 10 * time.Millisecond,
		Now:          r.c.Now,
		Since:        start,
		DaemonDir:    home,
		DiscoveryEnv: func(core.Harness) map[string]string { return map[string]string{"HOME": home} },
	})
	r.guard = New(r.stops, Options{Threshold: threshold, Logger: quietLogger()})
	r.guard.Start(r.obs)
	r.obs.Start()
	t.Cleanup(func() {
		r.obs.Stop()
		r.guard.Close()
	})
	eventually(t, "the observer's first scan", func() bool { return !r.obs.Stats().LastScan.IsZero() })
	return r
}

// call writes one tool_call row (no text, no reasoning) and its result row,
// twelve seconds after the last, and waits until the observer delivered it.
func (r *loopRig) call(t *testing.T, name string, input map[string]any) {
	t.Helper()
	r.n++
	r.at = r.at.Add(12 * time.Second)
	r.c.Set(r.at)
	id := fmt.Sprintf("call-%d", r.n)
	rt.AppendCrushMessages(t, r.db, "sess-1",
		rt.CrushMessage{Role: "assistant", At: r.at, Parts: rt.ToolCall(id, name, input)},
		// Each comment gets a new id, as Gitea's would: results differ, the
		// calls do not.
		rt.CrushMessage{Role: "tool", At: r.at, Parts: rt.ToolResult(id, fmt.Sprintf(`{"id":%d}`, 46000+r.n))},
	)
	// Not eventually(): this wait has been seen to time out intermittently in
	// a full `go test ./...`, and the observer's stats are what tell a call
	// the observer never read (Delivered short) from one it delivered and the
	// guard's subscription dropped (Dropped non-zero).
	want := uint64(r.n)
	deadline := time.Now().Add(10 * time.Second)
	for r.guard.Seen() < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the guard to handle call %d: seen=%d observer=%+v", r.n, r.guard.Seen(), r.obs.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// incident is the add_comment the worker repeated 608 times.
var incident = map[string]any{
	"method": "add_comment", "owner": "stump.wtf", "repo": "harness", "index": 383,
	"body": ".\n\n🤖 Posted on behalf of `@joestump` by glm-5.3-flash-balanced",
}

func TestPlantedIncidentIsStoppedAtExactlyN(t *testing.T) {
	const n = DefaultThreshold
	r := newLoopRig(t, n)
	for i := 1; i < n; i++ {
		r.call(t, "mcp_gitea_issue_write", incident)
		if got := r.stops.stops(); len(got) != 0 {
			t.Fatalf("stopped after %d identical calls, want %d: %v", i, n, got)
		}
	}
	r.call(t, "mcp_gitea_issue_write", incident)
	eventually(t, "the guard to stop the worker", func() bool { return len(r.stops.stops()) > 0 })
	if got := r.stops.stops(); len(got) != 1 || got[0] != "pool-worker" {
		t.Fatalf("stops = %v, want exactly [pool-worker]", got)
	}
	trips := r.guard.Trips()
	if len(trips) != 1 || trips[0].Count != n || trips[0].Tool != "mcp_gitea_issue_write" || trips[0].Session != "sess-1" {
		t.Fatalf("trips = %+v, want one at count %d for mcp_gitea_issue_write in sess-1", trips, n)
	}
	if s := r.obs.Stats(); s.Dropped[subscriberName] != 0 {
		t.Fatalf("guard dropped %d events; the count above is not what the guard saw", s.Dropped[subscriberName])
	}
}

// TestLegitimatePatternIsNeverStopped is the control, through the same path:
// the same tool twice with one issue's arguments, then twice with the next's,
// for four times the threshold's worth of calls. It is also #621's defect: that
// guard keyed an MCP call on its Summary, which ignores the arguments, and
// stopped this worker at its eighth comment.
func TestLegitimatePatternIsNeverStopped(t *testing.T) {
	const n = DefaultThreshold
	r := newLoopRig(t, n)
	for i := 0; i < 2*n; i++ {
		args := map[string]any{"method": "add_comment", "owner": "stump.wtf", "repo": "harness", "index": 400 + i, "body": "triaged"}
		r.call(t, "mcp_gitea_issue_write", args)
		r.call(t, "mcp_gitea_issue_write", args)
	}
	if got := r.stops.stops(); len(got) != 0 {
		t.Fatalf("stopped a worker commenting on different issues: %v (trips %+v)", got, r.guard.Trips())
	}
	if seen, s := r.guard.Seen(), r.obs.Stats(); seen != uint64(4*n) || s.Dropped[subscriberName] != 0 {
		t.Fatalf("guard saw %d of %d calls (stats %+v): this silence means nothing unless it saw them all", seen, 4*n, s)
	}
}
