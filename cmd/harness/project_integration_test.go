package main

// Governing tests: SPEC-0004 REQ "Bring Up" (`harness up` happy path against
// a real in-process daemon; no-project-file error before anything is sent;
// daemon-unreachable is the same dial error every verb produces), REQ "Tear
// Down" (down via discovery and via an explicit project name; global config
// byte-identical), and REQ "Project-Scoped Verbs" (ps filtering, bare-name
// resolution project-first with global fallback). ADR-0009.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/daemon"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// bootTestDaemon starts an in-process daemon (same pattern as the doctor
// integration test) with one disabled global harness ("demo") and returns the
// socket and global-config paths.
func bootTestDaemon(t *testing.T) (socket, configPath string) {
	t.Helper()
	tmp := t.TempDir()
	configPath = filepath.Join(tmp, "harness.toml")
	if err := writeMinimalConfig(configPath); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// Unix socket paths are length-limited (~108 bytes); use a short /tmp dir.
	sockDir, err := os.MkdirTemp("/tmp", "hnp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket = filepath.Join(sockDir, "d.sock")

	reg := attach.NewRegistry(1000)
	mgr := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		StatePath:   filepath.Join(tmp, "state.json"),
		LogDir:      filepath.Join(tmp, "logs"),
		ExtraOutFor: reg.WriterFor,
	})
	reg.SetController(mgr)

	srv := daemon.NewServer(daemon.Options{
		Manager:    mgr,
		Registry:   reg,
		SocketPath: socket,
		ConfigPath: configPath,
		Version:    buildinfo.Version,
	})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		srv.Close()
		mgr.Close()
	})
	return socket, configPath
}

// chdir switches cwd for the test and restores it on cleanup. Tests that use
// it must not run in parallel (cwd is process-global).
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// writeProjectFile writes a project harness.toml with an enabled `agent` and
// a disabled `helper` under project name "proj", returning the project dir.
func writeProjectFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	toml := `[project]
name = "proj"

[harness.agent]
cmd = "sleep"
args = ["60"]
enabled = true

[harness.helper]
cmd = "sleep"
args = ["60"]
`
	if err := os.WriteFile(filepath.Join(dir, "harness.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// dialTest returns a client for direct daemon assertions.
func dialTest(t *testing.T, socket string) *client.Client {
	t.Helper()
	c, err := client.Dial(socket, buildinfo.Version, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it wrote.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fnErr := fn()
	os.Stdout = old
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	_ = r.Close()
	return buf.String(), fnErr
}

// TestCmdUpDownRoundTrip is the SPEC-0004 "Bring Up" + "Tear Down" happy
// path: up registers proj/* (Enabled carried over the wire, so the enabled
// harness starts and the disabled one is registered stopped), prints the
// one-shot status table, and down stops + deregisters everything while the
// global config stays byte-identical.
func TestCmdUpDownRoundTrip(t *testing.T) {
	socket, configPath := bootTestDaemon(t)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	chdir(t, writeProjectFile(t))
	o := verbOpts{socket: socket, configPath: configPath}

	out, upErr := captureStdout(t, func() error { return cmdUp(o) })
	if upErr != nil {
		t.Fatalf("cmdUp: %v", upErr)
	}
	// One-shot status table of the project's harnesses (detached: cmdUp
	// already returned).
	for _, want := range []string{"NAME", "proj/agent", "proj/helper"} {
		if !strings.Contains(out, want) {
			t.Errorf("up output missing %q in:\n%s", want, out)
		}
	}

	c := dialTest(t, socket)
	hs, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]protocol.HarnessInfo{}
	for _, h := range hs {
		byName[h.Name] = h
	}
	agent, ok := byName["proj/agent"]
	if !ok {
		t.Fatal("proj/agent not registered after up")
	}
	if !agent.Enabled {
		t.Error("proj/agent Enabled = false; wire Enabled was dropped (daemon won't autostart it)")
	}
	helper, ok := byName["proj/helper"]
	if !ok {
		t.Fatal("proj/helper not registered after up")
	}
	if helper.Enabled {
		t.Error("proj/helper Enabled = true, want false (disabled in project file)")
	}
	// The enabled harness actually starts (SPEC-0004 REQ "Bring Up").
	waitForRunning(t, c, "proj/agent")

	// Re-up is idempotent: same set, no error (reconcile lives in the daemon).
	if _, err := captureStdout(t, func() error { return cmdUp(o) }); err != nil {
		t.Fatalf("re-up: %v", err)
	}

	// Down stops and forgets.
	if _, err := captureStdout(t, func() error { return cmdDown(o) }); err != nil {
		t.Fatalf("cmdDown: %v", err)
	}
	hs, err = c.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if strings.HasPrefix(h.Name, "proj/") {
			t.Errorf("harness %q still registered after down", h.Name)
		}
	}

	// Global config byte-for-byte untouched (SPEC-0004 scenario "Global
	// config untouched").
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("global harness.toml changed across up/down; must be byte-identical")
	}
}

// waitForRunning polls until the named harness reports running.
func waitForRunning(t *testing.T, c *client.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h, err := c.Describe(name)
		if err == nil && h.State == "running" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never reached running", name)
}

// TestCmdUpNoProjectFile: no harness.toml anywhere up the walk → the
// ErrNoProjectFound sentinel, non-zero exit path, and nothing is sent to the
// daemon (the bogus socket is never dialed — dialing it would fail with a
// different error shape).
func TestCmdUpNoProjectFile(t *testing.T) {
	chdir(t, t.TempDir())
	err := cmdUp(verbOpts{socket: "/tmp/harness-up-test-no-daemon.sock"})
	if err == nil {
		t.Fatal("cmdUp = nil, want no-project-file error")
	}
	if !errors.Is(err, config.ErrNoProjectFound) {
		t.Errorf("cmdUp error = %v, want errors.Is ErrNoProjectFound", err)
	}
	if !strings.Contains(err.Error(), "harness up:") {
		t.Errorf("error %q missing verb context wrap", err)
	}
}

// TestCmdUpDaemonUnreachable: with a project file but no daemon, up fails
// with the same dial error every other client verb produces (a net.OpError
// from the socket dial, which cliui classifies as daemon-not-running).
func TestCmdUpDaemonUnreachable(t *testing.T) {
	chdir(t, writeProjectFile(t))
	err := cmdUp(verbOpts{socket: "/tmp/harness-up-test-no-daemon-2.sock"})
	if err == nil {
		t.Fatal("cmdUp = nil, want dial error")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" {
		t.Errorf("cmdUp error = %v (%T), want the client dial *net.OpError", err, err)
	}
}

// TestCmdDownExplicitProject covers the design.md open question resolved by
// #31: after the project file is gone, `harness down NAME` still tears the
// registered project down without any discovery.
func TestCmdDownExplicitProject(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	c := dialTest(t, socket)
	if _, err := c.ProjectUp("ghost", []protocol.ProjectHarness{
		{Name: "agent", Cmd: "sleep", Args: []string{"60"}, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	chdir(t, t.TempDir()) // no project file discoverable

	o := verbOpts{socket: socket, name: "ghost"}
	if _, err := captureStdout(t, func() error { return cmdDown(o) }); err != nil {
		t.Fatalf("cmdDown ghost: %v", err)
	}
	hs, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if h.Project == "ghost" {
			t.Errorf("harness %q still registered after explicit down", h.Name)
		}
	}
}

// TestCmdDownErrors: no discoverable project and no explicit name → the
// sentinel plus a hint; an unknown explicit project surfaces the daemon's
// structured ERROR verbatim (code unknown_project).
func TestCmdDownErrors(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	chdir(t, t.TempDir())

	err := cmdDown(verbOpts{socket: socket})
	if !errors.Is(err, config.ErrNoProjectFound) {
		t.Errorf("cmdDown (no file, no name) = %v, want ErrNoProjectFound", err)
	}

	err = cmdDown(verbOpts{socket: socket, name: "nope"})
	var em *protocol.ErrorMsg
	if !errors.As(err, &em) {
		t.Fatalf("cmdDown nope = %v (%T), want wrapped *protocol.ErrorMsg", err, err)
	}
	if em.Code != protocol.ErrUnknownProject {
		t.Errorf("code = %q, want %q", em.Code, protocol.ErrUnknownProject)
	}
	if em.Message == "" {
		t.Error("structured error missing human message")
	}
}

// TestCmdPsProjectScoped: inside a project, ps lists only that project's
// harnesses; outside, it lists everything (alias for list). SPEC-0004
// scenario "ps is project-scoped".
func TestCmdPsProjectScoped(t *testing.T) {
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	socket, _ := bootTestDaemon(t)
	projDir := writeProjectFile(t)
	chdir(t, projDir)
	o := verbOpts{socket: socket, json: true}

	if _, err := captureStdout(t, func() error { return cmdUp(o) }); err != nil {
		t.Fatal(err)
	}

	psJSON := func() []protocol.HarnessInfo {
		t.Helper()
		out, err := captureStdout(t, func() error {
			return withClient(o, nil, cmdPs)
		})
		if err != nil {
			t.Fatalf("cmdPs: %v", err)
		}
		var hs []protocol.HarnessInfo
		if err := json.Unmarshal([]byte(out), &hs); err != nil {
			t.Fatalf("ps output not JSON: %v\n%s", err, out)
		}
		return hs
	}

	// Inside the project: only proj/*.
	hs := psJSON()
	if len(hs) != 2 {
		t.Fatalf("project ps listed %d harnesses, want 2: %+v", len(hs), hs)
	}
	for _, h := range hs {
		if h.Project != "proj" {
			t.Errorf("project ps leaked non-project harness %q", h.Name)
		}
	}

	// Outside any project: global scope, the demo harness is visible again.
	chdir(t, t.TempDir())
	hs = psJSON()
	names := map[string]bool{}
	for _, h := range hs {
		names[h.Name] = true
	}
	if !names["demo"] || !names["proj/agent"] {
		t.Errorf("global ps = %v, want demo and proj/agent both listed", names)
	}
}

// TestResolveScopedName covers SPEC-0004 bare-name resolution: project-local
// first, exact global fallback, error naming both candidates otherwise.
func TestResolveScopedName(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	c := dialTest(t, socket)
	if _, err := c.ProjectUp("proj", []protocol.ProjectHarness{
		{Name: "agent", Cmd: "sleep", Args: []string{"60"}, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := resolveScopedName(c, "proj", "agent")
	if err != nil || got != "proj/agent" {
		t.Errorf("resolve agent = %q, %v; want proj/agent (project-local first)", got, err)
	}
	got, err = resolveScopedName(c, "proj", "demo")
	if err != nil || got != "demo" {
		t.Errorf("resolve demo = %q, %v; want demo (exact global fallback)", got, err)
	}
	_, err = resolveScopedName(c, "proj", "nope")
	if err == nil {
		t.Fatal("resolve nope = nil error, want failure")
	}
	for _, want := range []string{"proj/nope", `"nope"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("resolution error %q should mention %s", err, want)
		}
	}
}

// TestProjectScopedLifecycle: inside a project, `stop agent` resolves to
// proj/agent, stops it, and leaves it registered (non-destructive —
// deregistration stays exclusive to down). Fully-qualified and global names
// pass through the same wrapper untouched.
func TestProjectScopedLifecycle(t *testing.T) {
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	socket, _ := bootTestDaemon(t)
	chdir(t, writeProjectFile(t))
	o := verbOpts{socket: socket, json: true}

	if _, err := captureStdout(t, func() error { return cmdUp(o) }); err != nil {
		t.Fatal(err)
	}
	c := dialTest(t, socket)
	waitForRunning(t, c, "proj/agent")

	// Bare local name → proj/agent.
	stop := verbOpts{socket: socket, json: true, name: "agent"}
	if _, err := captureStdout(t, func() error {
		return withClient(stop, nil, projectScoped(lifecycle("stop")))
	}); err != nil {
		t.Fatalf("scoped stop: %v", err)
	}
	h, err := c.Describe("proj/agent")
	if err != nil {
		t.Fatalf("proj/agent deregistered by stop (must stay registered): %v", err)
	}
	if h.Enabled {
		t.Error("stop left proj/agent enabled")
	}

	// Bare global name falls back to the global harness (demo exists, is
	// disabled; stop is idempotent so this round-trips fine).
	stop.name = "demo"
	if _, err := captureStdout(t, func() error {
		return withClient(stop, nil, projectScoped(lifecycle("stop")))
	}); err != nil {
		t.Fatalf("global fallback stop: %v", err)
	}

	// start brings the project harness back without re-running up.
	start := verbOpts{socket: socket, json: true, name: "agent"}
	if _, err := captureStdout(t, func() error {
		return withClient(start, nil, projectScoped(lifecycle("start")))
	}); err != nil {
		t.Fatalf("scoped start: %v", err)
	}
	waitForRunning(t, c, "proj/agent")
}

// TestWireHarnessesCarriesEnabledAndOrder: the wire conversion preserves file
// order and the Enabled bit (the wire default is false, so dropping it would
// register everything permanently stopped).
func TestWireHarnessesCarriesEnabledAndOrder(t *testing.T) {
	t.Parallel()
	dir := writeProjectFile(t)
	proj, err := config.LoadProject(filepath.Join(dir, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	wire := wireHarnesses(proj)
	if len(wire) != 2 {
		t.Fatalf("wireHarnesses len = %d, want 2", len(wire))
	}
	if wire[0].Name != "agent" || wire[1].Name != "helper" {
		t.Errorf("order = %q, %q; want agent, helper (file order)", wire[0].Name, wire[1].Name)
	}
	if !wire[0].Enabled {
		t.Error("agent Enabled not carried onto the wire")
	}
	if wire[1].Enabled {
		t.Error("helper Enabled = true, want false")
	}
	if wire[0].Cmd != "sleep" || len(wire[0].Args) != 1 {
		t.Errorf("agent definition not projected: %+v", wire[0])
	}
}

// TestFilterProjectHarnesses filters on provenance, not name prefix.
func TestFilterProjectHarnesses(t *testing.T) {
	t.Parallel()
	hs := []protocol.HarnessInfo{
		{Name: "demo"},
		{Name: "proj/agent", Project: "proj"},
		{Name: "other/agent", Project: "other"},
	}
	got := filterProjectHarnesses(hs, "proj")
	if len(got) != 1 || got[0].Name != "proj/agent" {
		t.Errorf("filter = %+v, want just proj/agent", got)
	}
}
