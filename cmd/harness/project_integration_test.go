package main

// Governing tests: SPEC-0004 REQ "Bring Up" (`harness up` happy path against
// a real in-process daemon; an omitted `enabled` key defaults to true so the
// first up actually starts things; no-project-file error before anything is
// sent; daemon-unreachable is the same dial error every verb produces), REQ
// "Tear Down" (down via discovery and via an explicit project name; global
// config byte-identical), and REQ "Project-Scoped Verbs" (ps filtering,
// purely lexical bare-name resolution to <project>/<name> with no global
// fallback). ADR-0009.

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

// shortSockDir returns a fresh short /tmp directory for Unix socket paths
// (socket paths are length-limited to ~108 bytes, and t.TempDir() can exceed
// that). Per-test dirs also mean no fixed world-predictable socket paths a
// foreign process could squat on.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hnp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// bootTestDaemon starts an in-process daemon with one disabled global harness
// ("demo") and returns the socket and global-config paths. It is the single
// shared boot helper for every cmd/harness integration test (the doctor
// integration test uses it too), so daemon/supervisor wiring changes happen
// in one place.
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
	socket = filepath.Join(shortSockDir(t), "d.sock")

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

// isolatedDir returns a nested working directory whose parent is installed as
// $HOME for the test. DiscoverProject stops its up-walk at the user's home
// directory, so tests chdir'd here are immune to a stray /tmp/harness.toml
// (or any other ancestor project file) on the machine.
func isolatedDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, ".config"))
	dir := filepath.Join(base, "work")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeProjectDir writes a harness.toml with the given contents into a fresh
// isolated directory and returns that directory.
func writeProjectDir(t *testing.T, toml string) string {
	t.Helper()
	dir := isolatedDir(t)
	if err := os.WriteFile(filepath.Join(dir, "harness.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeProjectFile writes a project harness.toml with an enabled `agent` and
// an explicitly disabled `helper` under project name "proj", returning the
// project dir.
func writeProjectFile(t *testing.T) string {
	t.Helper()
	return writeProjectDir(t, `[project]
name = "proj"

[harness.agent]
cmd = "sleep"
args = ["60"]
enabled = true

[harness.helper]
cmd = "sleep"
args = ["60"]
enabled = false
`)
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
	out, fnErr := captureFile(t, &os.Stdout, fn)
	return out, fnErr
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what
// it wrote (the scoped-verb warning path writes there).
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	out, fnErr := captureFile(t, &os.Stderr, fn)
	return out, fnErr
}

// captureFile redirects *fp (os.Stdout / os.Stderr) into a pipe around fn.
func captureFile(t *testing.T, fp **os.File, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := *fp
	*fp = w
	fnErr := fn()
	*fp = old
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	_ = r.Close()
	return buf.String(), fnErr
}

// TestCmdUpDownRoundTrip is the SPEC-0004 "Bring Up" + "Tear Down" happy
// path: up registers proj/* (Enabled carried over the wire, so the enabled
// harness starts and the explicitly disabled one is registered stopped),
// prints the one-shot status table, and down stops + deregisters everything
// while the global config stays byte-identical.
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
		t.Error("proj/helper Enabled = true, want false (explicit enabled = false in project file)")
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

// TestCmdUpDefaultEnabled: a project file that omits `enabled` entirely (the
// spec's own example files) must have its harnesses registered enabled AND
// started by the first `harness up` (SPEC-0004 REQ "Bring Up": "register ...
// and start each one").
func TestCmdUpDefaultEnabled(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	chdir(t, writeProjectDir(t, `[project]
name = "defproj"

[harness.agent]
cmd = "sleep"
args = ["60"]

[harness.reviewer]
cmd = "sleep"
args = ["60"]
`))
	o := verbOpts{socket: socket}
	if _, err := captureStdout(t, func() error { return cmdUp(o) }); err != nil {
		t.Fatalf("cmdUp: %v", err)
	}
	c := dialTest(t, socket)
	waitForRunning(t, c, "defproj/agent")
	waitForRunning(t, c, "defproj/reviewer")
	// Tear down so the sleeps don't outlive the test daemon shutdown path.
	if _, err := captureStdout(t, func() error { return cmdDown(o) }); err != nil {
		t.Fatalf("cmdDown: %v", err)
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

// TestCmdUpNoProjectFile: no harness.toml anywhere up the (HOME-bounded) walk
// → the ErrNoProjectFound sentinel, non-zero exit path, and nothing is sent
// to the daemon (the bogus socket is never dialed — dialing it would fail
// with a different error shape).
func TestCmdUpNoProjectFile(t *testing.T) {
	chdir(t, isolatedDir(t))
	err := cmdUp(verbOpts{socket: filepath.Join(shortSockDir(t), "n.sock")})
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
	err := cmdUp(verbOpts{socket: filepath.Join(shortSockDir(t), "n.sock")})
	if err == nil {
		t.Fatal("cmdUp = nil, want dial error")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" {
		t.Errorf("cmdUp error = %v (%T), want the client dial *net.OpError", err, err)
	}
}

// TestUpPsRejectStrayArgs: `up` and `ps` operate on the discovered scope only
// — a positional would be silently swallowed (acting on the wrong target with
// exit 0), so the dispatcher rejects it before any discovery or dialing.
// `down [PROJECT]` keeps its documented positional.
func TestUpPsRejectStrayArgs(t *testing.T) {
	for _, verb := range []string{"up", "ps"} {
		err := run(verb, verbOpts{name: "b"})
		if err == nil {
			t.Errorf("run(%q) with a positional = nil, want error", verb)
			continue
		}
		if !strings.Contains(err.Error(), "takes no arguments") {
			t.Errorf("run(%q) error = %q, want 'takes no arguments'", verb, err)
		}
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
	chdir(t, isolatedDir(t)) // no project file discoverable

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

// TestCmdDownSanitizesExplicitName: discovery registers a derived project
// name in sanitized form ("My-Cool-Project" → "my-cool-project"), so the
// explicit positional applies the same normalization as a fallback — the
// documented escape hatch must work when the user types the directory-name
// form (SPEC-0004 REQ "Project Naming And Namespacing").
func TestCmdDownSanitizesExplicitName(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	c := dialTest(t, socket)
	if _, err := c.ProjectUp("my-cool-project", []protocol.ProjectHarness{
		{Name: "agent", Cmd: "sleep", Args: []string{"60"}, Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	chdir(t, isolatedDir(t))

	o := verbOpts{socket: socket, name: "My-Cool-Project"}
	if _, err := captureStdout(t, func() error { return cmdDown(o) }); err != nil {
		t.Fatalf("cmdDown My-Cool-Project: %v", err)
	}
	hs, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if h.Project == "my-cool-project" {
			t.Errorf("harness %q still registered after sanitized-name down", h.Name)
		}
	}
}

// TestCmdDownErrors: no discoverable project and no explicit name → the
// sentinel (the actionable hint now renders via cliui's classify path); an
// unknown explicit project surfaces the daemon's structured ERROR verbatim
// (code unknown_project).
func TestCmdDownErrors(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	chdir(t, isolatedDir(t))

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
		out, err := captureStdout(t, func() error { return cmdPs(o) })
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
	chdir(t, isolatedDir(t))
	hs = psJSON()
	names := map[string]bool{}
	for _, h := range hs {
		names[h.Name] = true
	}
	if !names["demo"] || !names["proj/agent"] {
		t.Errorf("global ps = %v, want demo and proj/agent both listed", names)
	}
}

// TestScopeVerbName covers SPEC-0004 bare-name resolution: purely lexical,
// project-local always (no global fallback, no daemon round-trip), qualified
// names untouched, --all short-circuit gated to the lifecycle verbs.
func TestScopeVerbName(t *testing.T) {
	chdir(t, writeProjectFile(t))

	// Bare name inside a project → <project>/<name>, unconditionally — even
	// when a global harness of the same name exists (SPEC-0004: no fallback).
	for _, name := range []string{"agent", "demo", "nope"} {
		got := scopeVerbName(verbOpts{name: name}, true)
		if got.name != "proj/"+name {
			t.Errorf("scopeVerbName(%q) = %q, want %q", name, got.name, "proj/"+name)
		}
	}

	// Qualified names pass through untouched.
	got := scopeVerbName(verbOpts{name: "other/agent"}, true)
	if got.name != "other/agent" {
		t.Errorf("qualified name rewritten to %q", got.name)
	}

	// --all keeps its daemon-wide meaning for the lifecycle verbs only…
	got = scopeVerbName(verbOpts{name: "agent", all: true}, true)
	if got.name != "agent" {
		t.Errorf("lifecycle --all: name = %q, want bare %q", got.name, "agent")
	}
	// …while verbs that ignore --all (logs) resolve exactly like without it.
	got = scopeVerbName(verbOpts{name: "agent", all: true}, false)
	if got.name != "proj/agent" {
		t.Errorf("logs --all: name = %q, want %q", got.name, "proj/agent")
	}

	// Outside any project: everything passes through.
	chdir(t, isolatedDir(t))
	got = scopeVerbName(verbOpts{name: "agent"}, true)
	if got.name != "agent" {
		t.Errorf("outside project: name = %q, want %q", got.name, "agent")
	}
}

// TestProjectScopedLifecycle: inside a project, `stop agent` resolves to
// proj/agent, stops it, and leaves it registered (non-destructive —
// deregistration stays exclusive to down). A bare global name resolves
// project-local too (no fallback) so it fails with the qualified name in the
// error; fully-qualified names pass through the same wrapper untouched.
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
		return withClient(stop, nil, projectScoped("stop", true, lifecycle("stop")))
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

	// A bare global name inside the project resolves to proj/demo — no global
	// fallback (SPEC-0004 REQ "Project-Scoped Verbs"). The daemon's error
	// names the qualified name that was tried, wrapped with verb context.
	stop.name = "demo"
	_, err = captureStdout(t, func() error {
		return withClient(stop, nil, projectScoped("stop", true, lifecycle("stop")))
	})
	if err == nil {
		t.Fatal("bare global name inside a project resolved (want unknown proj/demo error)")
	}
	if !strings.Contains(err.Error(), "proj/demo") {
		t.Errorf("error %q should name the qualified %q it tried", err, "proj/demo")
	}
	if !strings.Contains(err.Error(), "harness stop:") {
		t.Errorf("error %q missing verb context wrap", err)
	}

	// A NAME containing "/" is fully qualified and passes through the
	// wrapper untouched.
	stop.name = "proj/helper"
	if _, err := captureStdout(t, func() error {
		return withClient(stop, nil, projectScoped("stop", true, lifecycle("stop")))
	}); err != nil {
		t.Fatalf("qualified stop: %v", err)
	}

	// start brings the project harness back without re-running up.
	start := verbOpts{socket: socket, json: true, name: "agent"}
	if _, err := captureStdout(t, func() error {
		return withClient(start, nil, projectScoped("start", true, lifecycle("start")))
	}); err != nil {
		t.Fatalf("scoped start: %v", err)
	}
	waitForRunning(t, c, "proj/agent")
}

// TestProjectScopedDescribe: describe rides the same projectScoped wrapper as
// the other name-taking verbs, so inside a project a bare NAME describes
// <project>/NAME (previously it hit the daemon with the unresolved bare name
// and failed with unknown_harness).
func TestProjectScopedDescribe(t *testing.T) {
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	socket, _ := bootTestDaemon(t)
	chdir(t, writeProjectFile(t))
	o := verbOpts{socket: socket, json: true}

	if _, err := captureStdout(t, func() error { return cmdUp(o) }); err != nil {
		t.Fatal(err)
	}

	desc := verbOpts{socket: socket, json: true, name: "helper"}
	out, err := captureStdout(t, func() error {
		return withClient(desc, nil, projectScoped("describe", false, cmdDescribe))
	})
	if err != nil {
		t.Fatalf("scoped describe: %v", err)
	}
	var h protocol.HarnessInfo
	if err := json.Unmarshal([]byte(out), &h); err != nil {
		t.Fatalf("describe output not JSON: %v\n%s", err, out)
	}
	if h.Name != "proj/helper" {
		t.Errorf("describe resolved to %q, want %q", h.Name, "proj/helper")
	}
}

// TestScopedVerbMalformedProjectFileWarns: a malformed (or foreign) ancestor
// harness.toml must not break the reused global verbs — the parse error is
// surfaced as a one-line stderr warning and the verb proceeds on global
// scope (SPEC-0004 REQ "Error Handling Standards": surfaced, not swallowed;
// the warning IS the surfacing).
func TestScopedVerbMalformedProjectFileWarns(t *testing.T) {
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	socket, _ := bootTestDaemon(t)
	// A committed global-style config: [server] is forbidden in project files.
	chdir(t, writeProjectDir(t, `[server]
listen = ":9"

[harness.demo2]
cmd = "sleep"
`))

	stop := verbOpts{socket: socket, json: true, name: "demo"}
	var stopErr error
	warn, _ := captureStderr(t, func() error {
		_, stopErr = captureStdout(t, func() error {
			return withClient(stop, nil, projectScoped("stop", true, lifecycle("stop")))
		})
		return nil
	})
	if stopErr != nil {
		t.Fatalf("stop of a global harness under a malformed project file = %v, want success", stopErr)
	}
	if !strings.Contains(warn, "warning: ignoring project file") {
		t.Errorf("stderr = %q, want the ignoring-project-file warning", warn)
	}
	if !strings.Contains(warn, "server") {
		t.Errorf("warning %q should carry the parse error detail", warn)
	}
}

// TestProjectVerbsMalformedProjectFileFatal: for the project verbs (up/ps)
// the malformed project file stays fatal — they cannot mean anything without
// a valid project scope.
func TestProjectVerbsMalformedProjectFileFatal(t *testing.T) {
	chdir(t, writeProjectDir(t, `[server]
listen = ":9"
`))
	sock := filepath.Join(shortSockDir(t), "n.sock")

	err := cmdUp(verbOpts{socket: sock})
	if err == nil || !strings.Contains(err.Error(), "harness up:") {
		t.Errorf("cmdUp under malformed project file = %v, want fatal wrapped parse error", err)
	}
	err = cmdPs(verbOpts{socket: sock})
	if err == nil || !strings.Contains(err.Error(), "harness ps:") {
		t.Errorf("cmdPs under malformed project file = %v, want fatal wrapped parse error", err)
	}
}

// TestCmdUpExcludesActiveConfig: a custom-located global config passed via
// --config must never be adopted as a project file by discovery — the walk
// skips it exactly like the conventional DefaultPath() (SPEC-0004 REQ
// "Project File Discovery").
func TestCmdUpExcludesActiveConfig(t *testing.T) {
	// A global-style config (contains [profile.*]) sitting in cwd: without
	// the exclusion, discovery would adopt it and fail with the
	// forbidden-table parse error instead of ErrNoProjectFound.
	dir := writeProjectDir(t, `[harness.demo]
cmd = "sleep"

[profile.dev]
harnesses = ["demo"]
`)
	chdir(t, dir)
	o := verbOpts{
		socket:     filepath.Join(shortSockDir(t), "n.sock"),
		configPath: filepath.Join(dir, "harness.toml"),
	}
	err := cmdUp(o)
	if !errors.Is(err, config.ErrNoProjectFound) {
		t.Errorf("cmdUp with --config in cwd = %v, want ErrNoProjectFound (active config excluded from discovery)", err)
	}
}

// TestWireHarnessesCarriesEnabledAndOrder: the wire conversion preserves file
// order and the Enabled bit (the wire default is false, so dropping it would
// register everything permanently stopped).
func TestWireHarnessesCarriesEnabledAndOrder(t *testing.T) {
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
		t.Error("helper Enabled = true, want false (explicit enabled = false)")
	}
	if wire[0].Cmd != "sleep" || len(wire[0].Args) != 1 {
		t.Errorf("agent definition not projected: %+v", wire[0])
	}
}

// TestWireHarnessesCarriesPrompt: a prompt harness crosses the wire as
// prompt-only — Prompt carried, Cmd/Args empty (the daemon synthesizes the
// argv at spawn, ADR-0011) — and its parse-normalized restart="no" one-shot
// default travels explicitly so the daemon never re-defaults it to always.
func TestWireHarnessesCarriesPrompt(t *testing.T) {
	dir := writeProjectDir(t, `[harness.oneshot]
prompt = "summarize the day"
enabled = false
`)
	proj, err := config.LoadProject(filepath.Join(dir, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	wire := wireHarnesses(proj)
	if len(wire) != 1 {
		t.Fatalf("wireHarnesses len = %d, want 1", len(wire))
	}
	if wire[0].Prompt != "summarize the day" {
		t.Errorf("Prompt = %q, want it carried onto the wire", wire[0].Prompt)
	}
	if wire[0].Cmd != "" || len(wire[0].Args) != 0 {
		t.Errorf("Cmd/Args = %q/%v, want empty (spawn-time synthesis)", wire[0].Cmd, wire[0].Args)
	}
	if wire[0].Restart != "no" {
		t.Errorf("Restart = %q, want %q (one-shot default made explicit by parse)", wire[0].Restart, "no")
	}
}

// TestWireHarnessesCarriesModel: a prompt harness's model selection crosses
// the wire (additive ProtoMinor 3 field) with Cmd/Args still empty — the
// daemon folds --model into the synthesized argv at spawn (ADR-0011, issue
// #57), so no client ever re-persists a synthesized flag.
func TestWireHarnessesCarriesModel(t *testing.T) {
	dir := writeProjectDir(t, `[harness.oneshot]
prompt = "summarize the day"
model = "claude-opus-5"
enabled = false
`)
	proj, err := config.LoadProject(filepath.Join(dir, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	wire := wireHarnesses(proj)
	if len(wire) != 1 {
		t.Fatalf("wireHarnesses len = %d, want 1", len(wire))
	}
	if wire[0].Model != "claude-opus-5" {
		t.Errorf("Model = %q, want it carried onto the wire", wire[0].Model)
	}
	if wire[0].Cmd != "" || len(wire[0].Args) != 0 {
		t.Errorf("Cmd/Args = %q/%v, want empty (spawn-time synthesis)", wire[0].Cmd, wire[0].Args)
	}
}

// TestWireHarnessesCarriesAutoAccept: a prompt harness's unattended mode
// crosses the wire (additive ProtoMinor 4 field) with Cmd/Args still empty —
// the daemon folds --yolo into the synthesized argv at spawn (ADR-0011, issue
// #58), so no client ever re-persists a synthesized flag.
func TestWireHarnessesCarriesAutoAccept(t *testing.T) {
	dir := writeProjectDir(t, `[harness.oneshot]
prompt = "summarize the day"
auto_accept = true
enabled = false
`)
	proj, err := config.LoadProject(filepath.Join(dir, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	wire := wireHarnesses(proj)
	if len(wire) != 1 {
		t.Fatalf("wireHarnesses len = %d, want 1", len(wire))
	}
	if !wire[0].AutoAccept {
		t.Error("AutoAccept = false, want it carried onto the wire")
	}
	if wire[0].Cmd != "" || len(wire[0].Args) != 0 {
		t.Errorf("Cmd/Args = %q/%v, want empty (spawn-time synthesis)", wire[0].Cmd, wire[0].Args)
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
