// Runaway tool-loop guard tests.
//
// The kill is driven through a real transcript in the incident's shape
// (stumpcloud/stumpcloud#469): assistant tool_call, tool_result, assistant
// tool_call, with no reasoning between calls, the arguments degenerated to a
// minimum and repeated. The guard must kill the run on exactly the Nth
// identical call, never for a legitimate retry, and count sessions apart.
//
// Governing: stumpcloud/stumpcloud#469; ADR-0007.
//
// @joestump-agent 09/23/2026 - Added with the guard.
package observe

import (
	"io"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// fakeRunStopper records Stop calls. stopped carries the harness name of each
// kill, so a test can wait for the guard's asynchronous consumer instead of
// sleeping.
type fakeRunStopper struct {
	stopped chan string
}

func newFakeRunStopper() *fakeRunStopper {
	return &fakeRunStopper{stopped: make(chan string, 64)}
}

func (f *fakeRunStopper) Stop(name string) bool {
	f.stopped <- name
	return true
}

// awaitStop waits for one kill, or fails the test.
func awaitStop(t *testing.T, f *fakeRunStopper, want string) {
	t.Helper()
	select {
	case got := <-f.stopped:
		if got != want {
			t.Fatalf("guard stopped harness %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("guard never killed %q", want)
	}
}

// assertNoStop gives the guard's consumer a beat to act and fails if it did.
func assertNoStop(t *testing.T, f *fakeRunStopper) {
	t.Helper()
	select {
	case name := <-f.stopped:
		t.Fatalf("guard killed %q; it must not", name)
	case <-time.After(150 * time.Millisecond):
	}
}

// forgeComment is one round of the incident loop: a forge comment write and
// its result, no reasoning between rounds. The input is degenerate on purpose
// — the bodies really were ".".
func forgeComment(id string, at time.Time) []rt.CrushMessage {
	return []rt.CrushMessage{
		{Role: "assistant", At: at, Parts: rt.ToolCall(id, "mcp_gitea_issue_write", map[string]any{
			"method": "add_comment", "arguments": map[string]any{
				"owner": "stump.wtf", "repo": "harness", "issue_number": 384, "body": ".",
			},
		})},
		{Role: "tool", At: at.Add(time.Second), Parts: rt.ToolResult(id, "created")},
	}
}

// startGuard wires the guard over the fixture's observer with the test's
// threshold and a discard logger.
func startGuard(f *fixture, stop *fakeRunStopper, threshold int) *LoopGuard {
	return StartLoopGuard(f.obs, stop, threshold, log.New(io.Discard))
}

// TestLoopGuardKillsAfterExactlyTheNthIdenticalCall is the incident: a session
// that repeats one identical forge write forever. The kill lands on the
// threshold-th identical call — not the fourth, not the sixth — once.
func TestLoopGuardKillsAfterExactlyTheNthIdenticalCall(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start))
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "sess-1", Created: start, Updated: start})

	stopper := newFakeRunStopper()
	g := startGuard(f, stopper, 5)
	defer g.Stop()
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	// Four identical calls: a stubborn retry, not a runaway. Nothing dies.
	at := start.Add(10 * time.Second)
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		rt.AppendCrushMessages(t, f.crushDB(), "sess-1", forgeComment(id, at)...)
		at = at.Add(12 * time.Second)
		f.tick(at)
		drain(ch)
	}
	assertNoStop(t, stopper)

	// The fifth identical call is the runaway: killed exactly once.
	rt.AppendCrushMessages(t, f.crushDB(), "sess-1", forgeComment("c5", at)...)
	at = at.Add(12 * time.Second)
	f.tick(at)
	drain(ch)
	awaitStop(t, stopper, "worker")
	assertNoStop(t, stopper)

	// The loop running on (the sixth call) must not re-kill.
	rt.AppendCrushMessages(t, f.crushDB(), "sess-1", forgeComment("c6", at)...)
	at = at.Add(12 * time.Second)
	f.tick(at)
	drain(ch)
	assertNoStop(t, stopper)
}

// TestLoopGuardNeverKillsLegitimatePatterns: the same tool twice with the
// same arguments (a retry), then the same tool with different arguments, and
// a second session doing the same — no run dies.
func TestLoopGuardNeverKillsLegitimatePatterns(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start))
	rt.WriteCrushDB(t, f.crushDB(),
		rt.CrushSession{ID: "sess-1", Created: start, Updated: start},
		rt.CrushSession{ID: "sess-2", Created: start, Updated: start})

	stopper := newFakeRunStopper()
	g := startGuard(f, stopper, 5)
	defer g.Stop()
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	at := start.Add(10 * time.Second)
	// A retry: identical twice, then the body changes and the loop moved on.
	rt.AppendCrushMessages(t, f.crushDB(), "sess-1", forgeComment("r1", at)...)
	at = at.Add(12 * time.Second)
	rt.AppendCrushMessages(t, f.crushDB(), "sess-1", forgeComment("r2", at)...)
	at = at.Add(12 * time.Second)
	rt.AppendCrushMessages(t, f.crushDB(), "sess-1", []rt.CrushMessage{
		{Role: "assistant", At: at, Parts: rt.ToolCall("r3", "mcp_gitea_issue_write", map[string]any{
			"method": "add_comment", "arguments": map[string]any{
				"owner": "stump.wtf", "repo": "harness", "issue_number": 384, "body": "fixed it",
			},
		})},
		{Role: "tool", At: at.Add(time.Second), Parts: rt.ToolResult("r3", "created")},
	}...)
	// A second session working the same queue at the same time.
	rt.AppendCrushMessages(t, f.crushDB(), "sess-2", forgeComment("s1", at)...)
	at = at.Add(12 * time.Second)
	f.tick(at)
	drain(ch)
	assertNoStop(t, stopper)
}

// TestLoopGuardCountsSessionsApart: threshold-1 identical calls in each of two
// sessions kills nothing; the runaway is one session's tally, not the
// harness's.
func TestLoopGuardCountsSessionsApart(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start))
	rt.WriteCrushDB(t, f.crushDB(),
		rt.CrushSession{ID: "sess-1", Created: start, Updated: start},
		rt.CrushSession{ID: "sess-2", Created: start, Updated: start})

	stopper := newFakeRunStopper()
	g := startGuard(f, stopper, 5)
	defer g.Stop()
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	at := start.Add(10 * time.Second)
	for _, sess := range []string{"sess-1", "sess-2"} {
		for i := 0; i < 4; i++ {
			rt.AppendCrushMessages(t, f.crushDB(), sess, forgeComment(sess, at)...)
			at = at.Add(12 * time.Second)
		}
	}
	f.tick(at)
	drain(ch)
	assertNoStop(t, stopper)
}

// compile-time: *supervisor.Manager is what the daemon wires in.
var _ RunStopper = (*supervisor.Manager)(nil)
