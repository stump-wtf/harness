package daemon

// Governing tests: SPEC-0004 REQ "Project Control Operations" — the
// project_up round-trip (register as <project>/<name>, start, reply success),
// reconcile-idempotent re-up, project_down happy path, and the structured
// ERROR scenarios (unknown project, global-name collision, invalid definition)
// — all over a real Unix socket. ADR-0009.

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// sleeperDef is a project-local wire definition for a long runner, enabled so
// project_up starts it (SPEC-0004 REQ "Bring Up").
func sleeperDef(name string) protocol.ProjectHarness {
	return protocol.ProjectHarness{Name: name, Cmd: "sleep", Args: []string{"60"}, Enabled: true}
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

// TestProjectUpDisabledHarnessNotStarted: SPEC-0004 REQ "Project File Schema"
// — the `enabled` field crosses the wire with its global meaning intact: a
// disabled project harness registers (visible to describe/list) but is not
// started by project_up.
func TestProjectUpDisabledHarnessNotStarted(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)

	idle := protocol.ProjectHarness{Name: "idle", Cmd: "sleep", Args: []string{"60"}} // enabled=false
	data, err := c.ProjectUp("reduit", []protocol.ProjectHarness{sleeperDef("agent"), idle})
	if err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	if len(data.Harnesses) != 2 {
		t.Fatalf("ProjectUpData has %d harnesses, want 2 (disabled one still registers)", len(data.Harnesses))
	}
	waitForState(t, c, "reduit/agent", string(core.StateRunning))

	h, err := c.Describe("reduit/idle")
	if err != nil {
		t.Fatalf("describe disabled harness: %v", err)
	}
	if h.State != string(core.StateStopped) || h.Enabled {
		t.Errorf("disabled harness state=%s enabled=%v, want stopped/disabled", h.State, h.Enabled)
	}
}

// TestDaemonInfoCountsProjectHarnesses: daemon_info reports the registered
// count (globals + project harnesses), matching what list returns, and drops
// back after project_down (SPEC-0004; ADR-0009).
func TestDaemonInfoCountsProjectHarnesses(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)

	if _, err := c.ProjectUp("reduit", []protocol.ProjectHarness{sleeperDef("agent"), sleeperDef("reviewer")}); err != nil {
		t.Fatalf("up: %v", err)
	}
	di, err := c.DaemonInfo()
	if err != nil {
		t.Fatalf("DaemonInfo: %v", err)
	}
	if di.Harnesses != 3 {
		t.Errorf("harnesses = %d after project_up, want 3 (1 global + 2 project)", di.Harnesses)
	}
	if _, err := c.ProjectDown("reduit"); err != nil {
		t.Fatalf("down: %v", err)
	}
	di, err = c.DaemonInfo()
	if err != nil {
		t.Fatalf("DaemonInfo after down: %v", err)
	}
	if di.Harnesses != 1 {
		t.Errorf("harnesses = %d after project_down, want 1", di.Harnesses)
	}
}

// TestNoopReUpDoesNotBroadcast: a verbatim no-op re-up (the cron-style
// `harness up` loop SPEC-0004 encourages) must not push config_reloaded to
// subscribed clients — only a reconcile that changed something broadcasts.
func TestNoopReUpDoesNotBroadcast(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	ctl := td.dial(t, nil)

	// Disabled harnesses register without starting, so the event stream stays
	// free of unrelated state-change noise.
	defs := []protocol.ProjectHarness{{Name: "idle", Cmd: "sleep", Args: []string{"60"}}}
	if _, err := ctl.ProjectUp("reduit", defs); err != nil {
		t.Fatalf("first up: %v", err)
	}

	sub := td.dial(t, []string{"events"})
	pc := sub.Conn()

	if _, err := ctl.ProjectUp("reduit", defs); err != nil {
		t.Fatalf("no-op re-up: %v", err)
	}
	// No event may arrive for the no-op window (pings excepted).
	_ = sub.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		f, err := pc.ReadFrame()
		if err != nil {
			break // deadline hit: nothing was broadcast
		}
		switch f.Type {
		case protocol.TypePing:
			_ = pc.WriteFrame(protocol.TypePong, nil)
		case protocol.TypeEvent:
			ev := decodeEvent(t, f.Payload)
			if ev.Kind == protocol.EvConfigReload {
				t.Fatal("no-op re-up broadcast config_reloaded")
			}
		}
	}

	// The pipe still works: a real change (down) broadcasts.
	if _, err := ctl.ProjectDown("reduit"); err != nil {
		t.Fatalf("down: %v", err)
	}
	_ = sub.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		f, err := pc.ReadFrame()
		if err != nil {
			t.Fatalf("no config_reloaded after project_down: %v", err)
		}
		if f.Type == protocol.TypePing {
			_ = pc.WriteFrame(protocol.TypePong, nil)
			continue
		}
		if f.Type == protocol.TypeEvent && decodeEvent(t, f.Payload).Kind == protocol.EvConfigReload {
			return
		}
	}
}

// TestHarnessFromWirePrompt: the wire → core copier is field-complete for
// prompt harnesses — Prompt survives the JSON wire encode/decode (the additive
// ProtoMinor 2 field) — and an absent restart defaults to "no" for a one-shot,
// matching the config parsers' normalization, instead of the always default
// that would respawn a completed agent run.
func TestHarnessFromWirePrompt(t *testing.T) {
	ph := protocol.ProjectHarness{Name: "oneshot", Prompt: "do the thing", Enabled: true}
	raw, err := json.Marshal(ph)
	if err != nil {
		t.Fatal(err)
	}
	var decoded protocol.ProjectHarness
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	h := harnessFromWire(decoded)
	if h.Prompt != "do the thing" || h.Cmd != "" {
		t.Errorf("harnessFromWire Prompt/Cmd = %q/%q, want prompt-only", h.Prompt, h.Cmd)
	}
	if h.Restart != core.RestartNo {
		t.Errorf("Restart = %q, want %q (one-shot wire default)", h.Restart, core.RestartNo)
	}
	// An explicit restart still wins over the one-shot default.
	decoded.Restart = string(core.RestartOnFailure)
	if h := harnessFromWire(decoded); h.Restart != core.RestartOnFailure {
		t.Errorf("explicit Restart = %q, want %q", h.Restart, core.RestartOnFailure)
	}
}

// TestHarnessFromWireModel: the wire → core copier carries the additive
// ProtoMinor 3 model field through the JSON encode/decode, so a prompt
// harness's model selection survives project_up intact — the daemon folds it
// into the synthesized argv at spawn (issue #57), never into Args.
func TestHarnessFromWireModel(t *testing.T) {
	ph := protocol.ProjectHarness{Name: "oneshot", Prompt: "do the thing", Model: "claude-opus-5", Enabled: true}
	raw, err := json.Marshal(ph)
	if err != nil {
		t.Fatal(err)
	}
	var decoded protocol.ProjectHarness
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	h := harnessFromWire(decoded)
	if h.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want it carried across the wire", h.Model)
	}
	if h.Prompt != "do the thing" || h.Cmd != "" || h.Args != nil {
		t.Errorf("Prompt/Cmd/Args = %q/%q/%v, want prompt-only with untouched args", h.Prompt, h.Cmd, h.Args)
	}
}

// TestHarnessFromWireAutoAccept: the wire → core copier carries the additive
// ProtoMinor 4 auto_accept field through the JSON encode/decode, so a prompt
// harness's unattended mode survives project_up intact — the daemon folds the
// vendor's yolo flag into the synthesized argv at spawn (issue #58), never
// into Args.
func TestHarnessFromWireAutoAccept(t *testing.T) {
	ph := protocol.ProjectHarness{Name: "oneshot", Prompt: "do the thing", AutoAccept: true, Enabled: true}
	raw, err := json.Marshal(ph)
	if err != nil {
		t.Fatal(err)
	}
	var decoded protocol.ProjectHarness
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	h := harnessFromWire(decoded)
	if !h.AutoAccept {
		t.Error("AutoAccept = false, want it carried across the wire")
	}
	if h.Prompt != "do the thing" || h.Cmd != "" || h.Args != nil {
		t.Errorf("Prompt/Cmd/Args = %q/%q/%v, want prompt-only with untouched args", h.Prompt, h.Cmd, h.Args)
	}
}

// TestHarnessFromWireMaxTurns carries the ProtoMinor max_turns budget through
// the JSON encode/decode, so a prompt harness's turn cap survives project_up
// intact — the daemon folds --max-turns into the synthesized argv at spawn
// (issue #59), never into Args.
func TestHarnessFromWireMaxTurns(t *testing.T) {
	ph := protocol.ProjectHarness{Name: "oneshot", Prompt: "do the thing", MaxTurns: 12, Enabled: true}
	raw, err := json.Marshal(ph)
	if err != nil {
		t.Fatal(err)
	}
	var decoded protocol.ProjectHarness
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	h := harnessFromWire(decoded)
	if h.MaxTurns != 12 {
		t.Errorf("MaxTurns = %d, want it carried across the wire as 12", h.MaxTurns)
	}
	if h.Prompt != "do the thing" || h.Cmd != "" || h.Args != nil {
		t.Errorf("Prompt/Cmd/Args = %q/%q/%v, want prompt-only with untouched args", h.Prompt, h.Cmd, h.Args)
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
