package main

// Daemon Notify Wiring
//
// Governing tests: SPEC-0003 REQ "Operator Notification"; issue #725.
//
// internal/notify tests the dispatcher and the watcher against fakes. None of
// that proves the daemon connects them to anything, and a notifier nobody
// feeds reads exactly like a quiet one — which is how #315 hid give-up. So
// every test here drives what the daemon itself builds (startDaemonNotify,
// startDaemonLoopGuard, startDaemonSessionGuard, wireNotifyReload,
// beginDaemonMetrics) over a real Manager and a real hook script, and checks
// what the hook actually received. The doctor's end of it is in
// doctor_notify_test.go.
//
// @joestump-agent 09/26/2026 - Added for harness#725.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/loopguard"
	"github.com/stump-wtf/harness/internal/metrics"
	"github.com/stump-wtf/harness/internal/notify"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// notifyRecorder is a hook that saves each delivery's stdin and
// HARNESS_NOTIFY_* environment into its first argument's directory.
const notifyRecorder = `#!/bin/sh
dir="$1"
env | grep '^HARNESS_NOTIFY_' > "$dir/$$.env.tmp"
cat > "$dir/$$.json.tmp"
mv "$dir/$$.env.tmp" "$dir/$$.env"
mv "$dir/$$.json.tmp" "$dir/$$.json"
`

type hookDelivery struct {
	payload notify.Payload
	env     string
}

// newNotifyHook writes the recorder and returns the [notify] table that runs
// it, plus the directory its deliveries land in.
func newNotifyHook(t *testing.T) (core.NotifyConfig, string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "notify.sh")
	if err := os.WriteFile(script, []byte(notifyRecorder), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	return core.NotifyConfig{
		Command:  []string{script, out},
		Events:   slices.Clone(core.DefaultNotifyEvents),
		Timeout:  5 * time.Second,
		Cooldown: 15 * time.Minute,
	}, out
}

func readHookDeliveries(t *testing.T, dir string) []hookDelivery {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []hookDelivery
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var d hookDelivery
		if err := json.Unmarshal(raw, &d.payload); err != nil {
			t.Fatalf("hook stdin is not JSON: %v\n%s", err, raw)
		}
		env, _ := os.ReadFile(strings.TrimSuffix(f, ".json") + ".env")
		d.env = string(env)
		out = append(out, d)
	}
	return out
}

// waitHookEvent waits for a delivery of event and returns it.
func waitHookEvent(t *testing.T, dir, event string) hookDelivery {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, d := range readHookDeliveries(t, dir) {
			if d.payload.Event == event {
				return d
			}
		}
		if time.Now().After(deadline) {
			var got []string
			for _, d := range readHookDeliveries(t, dir) {
				got = append(got, d.payload.Event)
			}
			t.Fatalf("the hook never received %q (got %v)", event, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The claude-rc incident, end to end through the daemon's construction: a
// harness that prints why it cannot run and exits 1 on every start gives up
// into `failed`, and the hook is told — with that line as the cause. When the
// operator fixes it and restarts, the hook hears it recovered.
func TestDaemonNotifyFiresOnGiveUpAndRecovery(t *testing.T) {
	tmp := t.TempDir()
	nc, out := newNotifyHook(t)
	fixed := filepath.Join(tmp, "logged-in")

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	// Only the durations shrink; MaxRestarts is the daemon's (see
	// TestDaemonPolicyParksAReliablyFailingHarness).
	opts.Policy.CrashWindow = 20 * time.Millisecond
	opts.Policy.CrashThreshold = 3
	opts.Policy.BackoffBase = 2 * time.Millisecond
	opts.Policy.BackoffCap = 10 * time.Millisecond
	opts.Policy.HealthyRun = 0
	opts.Policy.StopGrace = 80 * time.Millisecond

	script := fmt.Sprintf(`if [ -f %q ]; then exec sleep 30; fi; echo "Error: You must be logged in to use Remote Control."; exit 1`, fixed)
	h := core.Harness{
		Name: "claude-rc", Adapter: "generic", Args: []string{"-c", script},
		Backend: core.BackendNative, Restart: core.RestartOnFailure, RestartDelay: time.Millisecond, Enabled: true,
	}
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{h.Name: h}, HarnessOrder: []string{h.Name},
		Profiles: map[string]core.Profile{}, Notify: nc,
	}
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	n := startDaemonNotify(mgr, notify.Options{})
	t.Cleanup(n.Close)

	if !mgr.Start(h.Name) {
		t.Fatal("Start returned false for a configured harness")
	}
	got := waitHookEvent(t, out, core.NotifyFailed)
	p := got.payload
	wantMsg := fmt.Sprintf(`claude-rc failed: gave up after %d consecutive failures (last exit 1): "Error: You must be logged in to use Remote Control." — restart with `+
		"`harness restart claude-rc`; see `harness logs claude-rc`", opts.Policy.MaxRestarts+1)
	if p.Message != wantMsg {
		t.Errorf("message = %q\nwant      %q", p.Message, wantMsg)
	}
	if p.Harness != h.Name || p.State != "failed" || p.Cause != "Error: You must be logged in to use Remote Control." ||
		p.ExitCode == nil || *p.ExitCode != 1 || p.Hint != "harness logs claude-rc" {
		t.Fatalf("failed payload = %+v", p)
	}
	for _, want := range []string{
		"HARNESS_NOTIFY_EVENT=failed", "HARNESS_NOTIFY_HARNESS=claude-rc", "HARNESS_NOTIFY_STATE=failed",
		"HARNESS_NOTIFY_HOST=" + n.d.Host(),
		`HARNESS_NOTIFY_MESSAGE=claude-rc failed: gave up after`,
		`"Error: You must be logged in to use Remote Control." — restart with ` + "`harness restart claude-rc`",
	} {
		if !strings.Contains(got.env, want) {
			t.Errorf("hook environment lacks %q:\n%s", want, got.env)
		}
	}

	if err := os.WriteFile(fixed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !mgr.Restart(h.Name) {
		t.Fatal("Restart returned false")
	}
	rec := waitHookEvent(t, out, core.NotifyRecovered)
	if rec.payload.Harness != h.Name || !strings.Contains(rec.payload.Message, "running again (was failed)") {
		t.Fatalf("recovered payload = %+v", rec.payload)
	}
}

// The crush-qwen incident: the loop guard the daemon builds stops a looping
// harness, and — through the daemon's wiring, not a hand-built callback — the
// hook hears which tool, how many times, and that it stays down.
func TestDaemonNotifyFiresOnLoopStop(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRUSH_GLOBAL_DATA", "")
	t.Setenv("XDG_DATA_HOME", "")
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "crush"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	nc, out := newNotifyHook(t)

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.StopGrace = 100 * time.Millisecond
	h := core.Harness{Name: "crush-qwen", Adapter: "crush", Workdir: work, Backend: core.BackendNative, Restart: core.RestartAlways, Enabled: true}
	cfg := &core.Config{Harnesses: map[string]core.Harness{h.Name: h}, HarnessOrder: []string{h.Name}, Profiles: map[string]core.Profile{}, Notify: nc}
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	n := startDaemonNotify(mgr, notify.Options{})
	t.Cleanup(n.Close)
	if !mgr.Start(h.Name) {
		t.Fatal("Start returned false for a configured harness")
	}
	waitFor(t, "the harness to come up", func() bool {
		snap, _ := mgr.Snapshot(h.Name)
		return snap.State == core.StateRunning && snap.PID != 0
	})

	db := filepath.Join(work, ".crush", "crush.db")
	now := time.Now()
	rt.WriteCrushDB(t, db, rt.CrushSession{
		ID: "live", Created: now, Updated: now,
		Messages: []rt.CrushMessage{{Role: "user", At: now, Parts: `[{"type":"text","data":{"text":"triage harness#383"}}]`}},
	})
	obsOpts := daemonObserverOptions()
	obsOpts.PollInterval = 10 * time.Millisecond
	obs := startDaemonObserver(mgr, obsOpts)
	t.Cleanup(obs.Stop)
	guard := startDaemonLoopGuard(mgr, obs, n, loopguard.Options{})
	t.Cleanup(guard.Close)

	incident := map[string]any{"method": "add_comment", "owner": "stump.wtf", "repo": "harness", "index": 383, "body": "."}
	for i := 1; i <= loopguard.DefaultThreshold; i++ {
		id := fmt.Sprintf("c%d", i)
		at := time.Now()
		rt.AppendCrushMessages(t, db, "live",
			rt.CrushMessage{Role: "assistant", At: at, Parts: rt.ToolCall(id, "mcp_gitea_issue_write", incident)},
			rt.CrushMessage{Role: "tool", At: at, Parts: rt.ToolResult(id, fmt.Sprintf(`{"id":%d}`, i))},
		)
	}

	got := waitHookEvent(t, out, core.NotifyLoopStopped)
	p := got.payload
	if p.Harness != h.Name || p.Tool != "mcp_gitea_issue_write" || p.Count != loopguard.DefaultThreshold || p.State != "stopped" {
		t.Fatalf("loop_stopped payload = %+v", p)
	}
	if !strings.Contains(got.env, "HARNESS_NOTIFY_MESSAGE=crush-qwen stopped by the runaway tool-loop guard: mcp_gitea_issue_write called 8 times in a row") {
		t.Fatalf("hook environment:\n%s", got.env)
	}
}

// The tars incident: the session guard the daemon builds rotates a crush
// harness wedged on context-limit errors, and the hook is told.
func TestDaemonNotifyFiresOnSessionRotation(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "crush"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := filepath.Join(tmp, "work")
	nc, out := newNotifyHook(t)

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.StopGrace = 100 * time.Millisecond
	h := core.Harness{Name: "crush-qwen", Adapter: "crush", Workdir: work, Backend: core.BackendNative, Restart: core.RestartAlways, Enabled: true}
	cfg := &core.Config{Harnesses: map[string]core.Harness{h.Name: h}, HarnessOrder: []string{h.Name}, Profiles: map[string]core.Profile{}, Notify: nc}
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	n := startDaemonNotify(mgr, notify.Options{})
	t.Cleanup(n.Close)

	writeWedgedCrushStore(t, filepath.Join(work, ".crush", "crush.db"))
	if !mgr.Start(h.Name) {
		t.Fatal("Start returned false for a configured harness")
	}
	waitFor(t, "the harness to come up", func() bool {
		snap, _ := mgr.Snapshot(h.Name)
		return snap.State == core.StateRunning && snap.PID != 0
	})
	g := startDaemonSessionGuard(mgr, n, 20*time.Millisecond, 0)
	t.Cleanup(func() { g.Close(); mgr.SetSessionGuard(nil) })

	p := waitHookEvent(t, out, core.NotifySessionRotated).payload
	if p.Harness != h.Name || p.State != "running" || p.Cause != "3 of 3 recent turns failed on context-limit errors" ||
		!strings.Contains(p.Message, "restarted on a fresh session") {
		t.Fatalf("session_rotated payload = %+v", p)
	}
}

// writeWedgedCrushStore builds a crush-shaped store whose recent assistant
// turns all failed on a context-limit error.
func writeWedgedCrushStore(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, parts TEXT NOT NULL DEFAULT '[]', model TEXT, created_at INTEGER, updated_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	const ctxErr = `["{\"type\":\"finish\",\"data\":{\"reason\":\"error\",\"message\":\"Bad Request\",\"details\":\"Prompt exceeds max length\"}}"]`
	now := time.Now()
	for i := 3; i >= 1; i-- {
		at := now.Add(-time.Duration(i) * time.Minute).Unix()
		if _, err := db.Exec(`INSERT INTO messages (role, parts, created_at, updated_at) VALUES ('assistant', ?, ?, ?)`, ctxErr, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// A [notify] table added by a reload reaches the dispatcher the daemon runs,
// and the composition keeps the hook registered before it.
func TestDaemonNotifyFollowsReload(t *testing.T) {
	tmp := t.TempDir()
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	cfg := &core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}}
	mgr := supervisor.NewManager(cfg, opts)
	t.Cleanup(mgr.Close)
	n := startDaemonNotify(mgr, notify.Options{})
	t.Cleanup(n.Close)

	var prevRan bool
	mgr.SetReloadHook(func() { prevRan = true })
	wireNotifyReload(mgr, n)

	nc, out := newNotifyHook(t)
	next := &core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}, Notify: nc}
	mgr.Reload(next)
	if !prevRan {
		t.Fatal("wireNotifyReload replaced the reload hook instead of composing onto it")
	}
	if !n.d.Config().Equal(nc) {
		t.Fatalf("dispatcher config after reload = %+v, want %+v", n.d.Config(), nc)
	}
	n.w.LoopStopped(loopguard.Trip{Harness: "x", Tool: "t", Count: 8})
	waitHookEvent(t, out, core.NotifyLoopStopped)
}

// harness_notify_deliveries_total is on the /metrics registry the daemon
// serves, not just on the dispatcher.
func TestDaemonNotifyMetricsRegistered(t *testing.T) {
	tmp := t.TempDir()
	nc, _ := newNotifyHook(t)
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	mgr := supervisor.NewManager(&core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}, Notify: nc}, opts)
	t.Cleanup(mgr.Close)
	n := startDaemonNotify(mgr, notify.Options{})
	t.Cleanup(n.Close)
	dm := beginDaemonMetrics(mgr, metrics.Listener{Addr: "127.0.0.1:0"})
	t.Cleanup(dm.Stop)
	n.registerMetrics(dm)

	if del := n.d.Test(t.Context()); del.Result != notify.ResultOK {
		t.Fatalf("test delivery = %+v", del)
	}
	families, err := dm.m.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "harness_notify_deliveries_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["event"] == "test" && labels["result"] == "ok" && m.GetCounter().GetValue() == 1 {
				return
			}
		}
		t.Fatalf("harness_notify_deliveries_total has no {event=test,result=ok} 1: %v", f)
	}
	t.Fatal("harness_notify_deliveries_total is not on the daemon's /metrics registry")
}
