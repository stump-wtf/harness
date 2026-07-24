package daemon

// Governing tests: SPEC-0004 REQ "Project Control Operations" — the
// project_up round-trip (register as <project>/<name>, start, reply success),
// reconcile-idempotent re-up, project_down happy path, and the structured
// ERROR scenarios (unknown project, global-name collision, invalid definition)
// — all over a real Unix socket. ADR-0009.

import (
	"errors"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// sleeperDef is a project-local wire definition for a long runner.
func sleeperDef(name string) protocol.ProjectHarness {
	return protocol.ProjectHarness{Name: name, Cmd: "sleep", Args: []string{"60"}}
}

// errCodeOf asserts err is a structured *protocol.ErrorMsg and returns its code.
func errCodeOf(t *testing.T, err error) protocol.ErrCode {
	t.Helper()
	var em *protocol.ErrorMsg
	if !errors.As(err, &em) {
		t.Fatalf("err = %v (%T), want *protocol.ErrorMsg", err, err)
	}
	if em.Message == "" {
		t.Error("structured error missing human message")
	}
	return em.Code
}

// TestProjectUpRoundTrip is SPEC-0004 scenario "project_up round-trip": the
// daemon registers each harness as reduit/<name> with provenance, starts them,
// and replies success; list shows them alongside globals.
func TestProjectUpRoundTrip(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)

	data, err := c.ProjectUp("reduit", []protocol.ProjectHarness{sleeperDef("agent"), sleeperDef("reviewer")})
	if err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	if data.Project != "reduit" || len(data.Harnesses) != 2 {
		t.Fatalf("ProjectUpData = %+v, want project reduit with 2 harnesses", data)
	}
	if data.Harnesses[0].Name != "reduit/agent" || data.Harnesses[1].Name != "reduit/reviewer" {
		t.Errorf("names = %q, %q; want namespaced reduit/agent, reduit/reviewer",
			data.Harnesses[0].Name, data.Harnesses[1].Name)
	}
	for _, h := range data.Harnesses {
		if h.Project != "reduit" {
			t.Errorf("harness %q provenance = %q, want reduit", h.Name, h.Project)
		}
		if h.Cmd != "sleep" {
			t.Errorf("harness %q cmd = %q, want sleep (definition projected)", h.Name, h.Cmd)
		}
	}
	// project_up starts them (SPEC-0004 REQ "Bring Up").
	waitForState(t, c, "reduit/agent", string(core.StateRunning))

	// list exposes project harnesses daemon-wide next to the global one.
	hs, err := c.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byName := map[string]protocol.HarnessInfo{}
	for _, h := range hs {
		byName[h.Name] = h
	}
	if _, ok := byName["sleeper"]; !ok {
		t.Error("global harness missing from list after project_up")
	}
	if byName["sleeper"].Project != "" {
		t.Errorf("global harness provenance = %q, want empty", byName["sleeper"].Project)
	}
	if _, ok := byName["reduit/agent"]; !ok {
		t.Error("reduit/agent missing from list")
	}
}

// TestProjectUpReconcileRoundTrip is SPEC-0004 scenario "Re-up reconciles":
// the second up adds the new harness and leaves the existing one untouched —
// no duplicate registration.
func TestProjectUpReconcileRoundTrip(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)

	if _, err := c.ProjectUp("reduit", []protocol.ProjectHarness{sleeperDef("agent")}); err != nil {
		t.Fatalf("first up: %v", err)
	}
	waitForState(t, c, "reduit/agent", string(core.StateRunning))
	before, err := c.Describe("reduit/agent")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}

	data, err := c.ProjectUp("reduit", []protocol.ProjectHarness{sleeperDef("agent"), sleeperDef("reviewer")})
	if err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if len(data.Harnesses) != 2 {
		t.Fatalf("re-up returned %d harnesses, want 2 (no duplicates)", len(data.Harnesses))
	}
	waitForState(t, c, "reduit/reviewer", string(core.StateRunning))
	after, err := c.Describe("reduit/agent")
	if err != nil {
		t.Fatalf("describe after re-up: %v", err)
	}
	if after.PID != before.PID {
		t.Errorf("re-up bounced reduit/agent: pid %d → %d", before.PID, after.PID)
	}
	if after.ConfigChanged {
		t.Error("unchanged definition flagged ConfigChanged on re-up")
	}
}

// TestProjectUpCollisionError is SPEC-0004 scenario "Name collides with a
// global harness": structured ERROR with the project_collision code, and
// nothing registered (scenario "No silent swallow on registration failure").
func TestProjectUpCollisionError(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML) // global harness is named "sleeper"
	c := td.dial(t, nil)

	_, err := c.ProjectUp("sleeper", []protocol.ProjectHarness{sleeperDef("agent"), sleeperDef("reviewer")})
	if code := errCodeOf(t, err); code != protocol.ErrProjectCollision {
		t.Errorf("code = %s, want %s", code, protocol.ErrProjectCollision)
	}
	// No partial state: the daemon still lists exactly the global harness.
	hs, err := c.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hs) != 1 || hs[0].Name != "sleeper" {
		t.Errorf("list after failed up = %+v, want only the global sleeper", hs)
	}
}

// TestProjectUpInvalidDefinitionError: a definition failing validation (missing
// cmd) is a structured invalid_project ERROR and registers nothing.
func TestProjectUpInvalidDefinitionError(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)

	_, err := c.ProjectUp("reduit", []protocol.ProjectHarness{
		sleeperDef("agent"),
		{Name: "broken"}, // no cmd
	})
	if code := errCodeOf(t, err); code != protocol.ErrInvalidProject {
		t.Errorf("code = %s, want %s", code, protocol.ErrInvalidProject)
	}
	hs, err := c.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hs) != 1 {
		t.Errorf("partial registration left behind: %+v", hs)
	}
}

// TestProjectDownRoundTrip is SPEC-0004 scenario "Down stops and forgets":
// every project harness is stopped and deregistered; the daemon retains no
// record.
func TestProjectDownRoundTrip(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)

	if _, err := c.ProjectUp("reduit", []protocol.ProjectHarness{sleeperDef("agent"), sleeperDef("reviewer")}); err != nil {
		t.Fatalf("up: %v", err)
	}
	waitForState(t, c, "reduit/agent", string(core.StateRunning))

	data, err := c.ProjectDown("reduit")
	if err != nil {
		t.Fatalf("ProjectDown: %v", err)
	}
	if data.Project != "reduit" || len(data.Removed) != 2 {
		t.Errorf("ProjectDownData = %+v, want reduit with 2 removed", data)
	}
	// Deregistered: describe now fails with unknown_harness, list is global-only.
	_, err = c.Describe("reduit/agent")
	if code := errCodeOf(t, err); code != protocol.ErrUnknownHarness {
		t.Errorf("describe after down code = %s, want %s", code, protocol.ErrUnknownHarness)
	}
	hs, err := c.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hs) != 1 || hs[0].Name != "sleeper" {
		t.Errorf("list after down = %+v, want only the global sleeper", hs)
	}
}

// TestProjectDownUnknownProjectError is SPEC-0004 scenario "project_down on
// unknown project": structured ERROR with the unknown_project code, no state
// change.
func TestProjectDownUnknownProjectError(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)

	_, err := c.ProjectDown("nope")
	if code := errCodeOf(t, err); code != protocol.ErrUnknownProject {
		t.Errorf("code = %s, want %s", code, protocol.ErrUnknownProject)
	}
	hs, err := c.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(hs) != 1 {
		t.Errorf("state changed on unknown-project down: %+v", hs)
	}
}

// waitForState polls describe until the harness reports the wanted state.
func waitForState(t *testing.T, c interface {
	Describe(string) (protocol.HarnessInfo, error)
}, name, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h, err := c.Describe(name); err == nil && h.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to reach %s", name, want)
}
