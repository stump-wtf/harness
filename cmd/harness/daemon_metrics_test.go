package main

// Daemon Metrics Wiring
//
// Governing tests: SPEC-0013 REQ-1 (the listener, and the startup refusal of a
// non-loopback bind without a token), REQ-3 (reachability through the real
// observer); issue #356; design.md "Testing" ("a startup test: non-loopback
// bind with no token refuses to start").
//
// internal/metrics tests the collector against a stand-in Manager. What they
// cannot show is that the daemon builds it over its real Manager and its real
// observer, and that runDaemon refuses the bad listener before any harness
// starts — the gap #315 fell through with a Policy nobody wired. So the first
// test drives startDaemonMetrics against a real Manager and a stand-in crush
// process, and the last two exec the real binary.
//
// @joestump-agent 09/21/2026 - Added for harness#356.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/metrics"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// scrapeURL fetches and parses a /metrics endpoint.
func scrapeURL(t *testing.T, url string) map[string]*dto.MetricFamily {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("scrape %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape %s: status %d: %s", url, resp.StatusCode, body)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("parse %s: %v", url, err)
	}
	return fams
}

// sample returns the value of name with exactly the labels kv, and whether it
// exists.
func sample(fams map[string]*dto.MetricFamily, name string, kv ...string) (float64, bool) {
	want := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		want[kv[i]] = kv[i+1]
	}
	fam, ok := fams[name]
	if !ok {
		return 0, false
	}
	for _, m := range fam.GetMetric() {
		if len(m.GetLabel()) != len(want) {
			continue
		}
		match := true
		for _, lp := range m.GetLabel() {
			if want[lp.GetName()] != lp.GetValue() {
				match = false
			}
		}
		if !match {
			continue
		}
		if m.Counter != nil {
			return m.GetCounter().GetValue(), true
		}
		return m.GetGauge().GetValue(), true
	}
	return 0, false
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	return port
}

// The daemon's own construction — real Manager, the observer the daemon
// builds, the listener it binds — reports a crush worker's quota error as the
// 2026-09-14 shape: running, a quota error, no last success.
func TestDaemonMetricsReportTheRealManager(t *testing.T) {
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

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.StopGrace = 100 * time.Millisecond
	h := core.Harness{Name: "worker", Adapter: "crush", Workdir: work, Backend: core.BackendNative, Restart: core.RestartNo, Enabled: true}
	idle := core.Harness{Name: "script", Adapter: "generic", Backend: core.BackendNative, Restart: core.RestartNo}
	cfg := &core.Config{
		Harnesses:    map[string]core.Harness{h.Name: h, idle.Name: idle},
		HarnessOrder: []string{h.Name, idle.Name},
		Profiles:     map[string]core.Profile{},
	}
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	if !mgr.Start(h.Name) {
		t.Fatal("Start returned false for a configured harness")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if snap, _ := mgr.Snapshot(h.Name); snap.State == core.StateRunning && snap.PID != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("harness never came up")
		}
		time.Sleep(5 * time.Millisecond)
	}

	db := filepath.Join(work, ".crush", "crush.db")
	now := time.Now()
	rt.WriteCrushDB(t, db, rt.CrushSession{
		ID: "live", Created: now, Updated: now,
		Messages: []rt.CrushMessage{{Role: "user", At: now, Parts: `[{"type":"text","data":{"text":"go"}}]`}},
	})

	obsOpts := daemonObserverOptions()
	obsOpts.PollInterval = 10 * time.Millisecond
	obs := startDaemonObserver(mgr, obsOpts)
	t.Cleanup(obs.Stop)

	l, err := daemonMetricsListener(core.ServerConfig{MetricsListen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	dm := startDaemonMetrics(mgr, obs, nil, l)
	t.Cleanup(dm.Stop)
	if dm.srv == nil {
		t.Fatal("metrics listener did not bind")
	}
	url := "http://" + dm.srv.Addr() + "/metrics"

	rt.AppendCrushMessages(t, db, "live", rt.CrushMessage{Role: "assistant", At: time.Now(),
		Parts: rt.FinishError("Too Many Requests", `{"type":"error","error":{"type":"rate_limit_error"}}`)})

	deadline = time.Now().Add(10 * time.Second)
	var fams map[string]*dto.MetricFamily
	for {
		fams = scrapeURL(t, url)
		if v, _ := sample(fams, "harness_model_call_errors_total", "harness", "worker", "class", "quota"); v == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no quota error through the daemon's wiring; observer stats %+v", obs.Stats())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v, ok := sample(fams, "harness_harness_state", "harness", "worker", "state", "running"); !ok || v != 1 {
		t.Errorf("worker running = %v (present %v)", v, ok)
	}
	if _, ok := sample(fams, "harness_last_successful_call_timestamp", "harness", "worker"); ok {
		t.Error("last-success timestamp present for a worker that never succeeded")
	}
	// The generic harness is declared, so it reports every state, and it is
	// not observable, so it has no model series.
	for _, st := range []string{"running", "stopped", "failed", "flapping"} {
		if _, ok := sample(fams, "harness_harness_state", "harness", "script", "state", st); !ok {
			t.Errorf("script has no state=%s series", st)
		}
	}
	if _, ok := sample(fams, "harness_model_calls_total", "harness", "script", "outcome", "error"); ok {
		t.Error("generic harness reports model calls it cannot observe")
	}
}

// A taken port is logged and survived: the collector still runs and nothing
// panics on shutdown.
func TestDaemonMetricsSurviveABusyPort(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	tmp := t.TempDir()
	reg := attach.NewRegistry(10)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	mgr := supervisor.NewManager(&core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}}, opts)
	t.Cleanup(mgr.Close)

	dm := startDaemonMetrics(mgr, nil, nil, metrics.Listener{Addr: held.Addr().String()})
	if dm.srv != nil {
		t.Fatal("bound a port another listener holds")
	}
	if dm.m == nil {
		t.Fatal("collector not built when only the bind failed")
	}
	dm.Stop()
	dm.Stop() // idempotent
}

// runDaemonBinary execs `harness daemon start` on cfgBody and returns the
// command, its combined output buffer, and the socket path.
func runDaemonBinary(t *testing.T, bin, cfgBody string) (*exec.Cmd, *bytes.Buffer, string) {
	t.Helper()
	env, _ := isolatedEnv(t)
	dir := shortSockDir(t)
	socket := filepath.Join(dir, "h.sock")
	cfg := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfg, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := exec.Command(bin, "daemon", "start", "--socket", socket, "--config", cfg)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd, &out, socket
}

// The startup test design.md asks for: a non-loopback bind with no token
// refuses to start — exits non-zero, says why, never serves the socket, and
// never starts the harness it was configured to autostart.
//
// The control run first proves the same config with a loopback listener DOES
// start that harness; without it, "the marker never appeared" would pass just
// as well for a harness that could not have started anyway.
func TestDaemonRefusesNonLoopbackMetricsWithoutToken(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	config := func(listen, marker string) string {
		return fmt.Sprintf(`[server]
metrics_listen = %q

[harness.eager]
harness = "generic"
args = ["-c", "touch %s; sleep 600"]
enabled = true
`, listen, marker)
	}

	control := filepath.Join(shortSockDir(t), "started")
	ctl, ctlOut, _ := runDaemonBinary(t, bin, config("127.0.0.1:"+freePort(t), control))
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(control); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: the autostart harness never ran under a loopback listener\n%s", ctlOut)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = ctl.Process.Kill()

	marker := filepath.Join(shortSockDir(t), "started")
	cmd, out, socket := runDaemonBinary(t, bin, config("0.0.0.0:"+freePort(t), marker))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("daemon exited 0; want a refusal\n%s", out)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("daemon did not refuse to start within 15s\n%s", out)
	}
	if !strings.Contains(out.String(), "refusing to start") || !strings.Contains(out.String(), "metrics_token_file") {
		t.Errorf("refusal does not say why:\n%s", out)
	}
	if _, err := os.Stat(socket); err == nil {
		t.Error("the control socket was created by a daemon that refused to start")
	}
	// Give a harness that had been spawned time to write its marker.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("an autostart harness ran under a daemon that refused to start")
	}
}

// The full runDaemon path: the configured listener serves /metrics with the
// declared harness's every state value.
func TestDaemonServesMetricsOnConfiguredListener(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	addr := "127.0.0.1:" + freePort(t)
	cfg := fmt.Sprintf(`[server]
metrics_listen = %q

[harness.demo]
harness = "generic"
args = ["-c", "sleep 600"]
enabled = false
`, addr)
	_, out, _ := runDaemonBinary(t, bin, cfg)

	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics listener never came up on %s\n%s", addr, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	fams := scrapeURL(t, "http://"+addr+"/metrics")
	for _, st := range []string{"running", "stopped", "failed", "flapping"} {
		want := 0.0
		if st == "stopped" {
			want = 1
		}
		if v, ok := sample(fams, "harness_harness_state", "harness", "demo", "state", st); !ok || v != want {
			t.Errorf("demo state=%s = %v (present %v), want %v", st, v, ok, want)
		}
	}
	if _, ok := fams["go_goroutines"]; !ok {
		t.Error("go_* collector missing from the daemon's endpoint")
	}
}
