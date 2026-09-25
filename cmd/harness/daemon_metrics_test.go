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
// process, and the rest exec the real binary.
//
// @joestump-agent 09/21/2026 - Added for harness#356.
//
// @joestump-agent 09/21/2026 - Review: added a real-binary scrape asserting
// every mandated family (scheduler and token file wired from TOML), and the
// refusal of a named-but-missing token file.

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
	dm := beginDaemonMetrics(mgr, l)
	t.Cleanup(dm.Stop)
	dm.serve(obs, nil)
	if dm.srv == nil {
		t.Fatal("metrics listener did not bind")
	}
	url := "http://" + dm.srv.Addr() + "/metrics"

	rt.AppendCrushMessages(t, db, "live", rt.CrushMessage{
		Role: "assistant", At: time.Now(),
		Parts: rt.FinishError("Too Many Requests", `{"type":"error","error":{"type":"rate_limit_error"}}`),
	})

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

	dm := beginDaemonMetrics(mgr, metrics.Listener{Addr: held.Addr().String()})
	dm.serve(nil, nil)
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
// command, its combined output buffer, and the socket path. extraEnv entries
// are appended last, so they win over the inherited environment.
func runDaemonBinary(t *testing.T, bin, cfgBody string, extraEnv ...string) (*exec.Cmd, *bytes.Buffer, string) {
	t.Helper()
	env, _ := isolatedEnv(t)
	env = append(env, extraEnv...)
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
	deadline := time.Now().Add(daemonStartupCeiling(t))
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
	case <-time.After(daemonStartupCeiling(t)):
		t.Fatalf("daemon did not refuse to start within %s\n%s", daemonStartupCeiling(t), out)
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

	deadline := time.Now().Add(daemonStartupCeiling(t))
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

// Every family SPEC-0013 mandates, scraped from the real binary on a real
// harness.toml, behind the bearer token the file names.
//
// The package tests prove each family against a stand-in Manager and a
// hand-built NextRun; the tests above prove state through the binary. None of
// them shows that the daemon hands the collector its real scheduler (a nil
// NextRun silently omits harness_scheduled_next_run_timestamp, which reads as
// "no next window"), that metrics_token_file survives the trip from TOML to
// the listener, or that an observable harness declared in TOML gets its
// model-call families. A daemon that dropped any of that wiring would pass
// every other test. (review: harness#356)
func TestDaemonServesEveryMandatedFamily(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	dir := shortSockDir(t)
	home := filepath.Join(dir, "home")
	work := filepath.Join(dir, "work")
	fakeBin := filepath.Join(dir, "bin")
	for _, d := range []string{home, work, fakeBin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "crush"), []byte("#!/bin/sh\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const token = "review-token-356"
	tokenFile := filepath.Join(dir, "metrics.token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + freePort(t)
	cfg := fmt.Sprintf(`[server]
metrics_listen = %q
metrics_token_file = %q

[harness.script]
harness = "generic"
args = ["-c", "sleep 600"]
enabled = false

[harness.worker]
harness = "crush"
workdir = %q
enabled = false

[harness.job]
harness = "crush"
workdir = %q
schedule = "*/5 * * * *"
prompt = "sweep"
`, addr, tokenFile, work, work)
	_, out, _ := runDaemonBinary(t, bin, cfg,
		"HOME="+home,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_DATA_HOME=", "CRUSH_GLOBAL_DATA=")

	url := "http://" + addr + "/metrics"
	deadline := time.Now().Add(daemonStartupCeiling(t))
	for {
		resp, err := http.Get(url)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			// The token from the file is enforced even on loopback.
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthenticated scrape: status %d, want 401\n%s", resp.StatusCode, body)
			}
			if strings.Contains(string(body), "harness_") {
				t.Fatal("unauthenticated scrape leaked metrics")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics listener never came up on %s\n%s", addr, out)
		}
		time.Sleep(100 * time.Millisecond)
	}

	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated scrape: status %d\n%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want text/plain; version=0.0.4 (REQ-1)", ct)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("exposition does not parse: %v", err)
	}
	if strings.Contains(string(body), token) {
		t.Error("the bearer token appears in the exposition")
	}

	// REQ-2 for every declared harness, REQ-3/REQ-4 for the observable ones.
	for _, h := range []string{"script", "worker", "job"} {
		for _, st := range []string{"running", "stopped", "failed", "flapping"} {
			if _, ok := sample(fams, "harness_harness_state", "harness", h, "state", st); !ok {
				t.Errorf("%s: no harness_harness_state{state=%q}", h, st)
			}
		}
		for _, name := range []string{"harness_restarts_total", "harness_consecutive_failures"} {
			if _, ok := sample(fams, name, "harness", h); !ok {
				t.Errorf("%s: no %s", h, name)
			}
		}
		if _, ok := sample(fams, "harness_state_transitions_total", "harness", h, "to", "failed"); !ok {
			t.Errorf("%s: no harness_state_transitions_total{to=\"failed\"}", h)
		}
	}
	for _, h := range []string{"worker", "job"} {
		for _, o := range []string{"success", "error"} {
			if _, ok := sample(fams, "harness_model_calls_total", "harness", h, "outcome", o); !ok {
				t.Errorf("%s: no harness_model_calls_total{outcome=%q}", h, o)
			}
		}
		for _, c := range metrics.Classes {
			if _, ok := sample(fams, "harness_model_call_errors_total", "harness", h, "class", string(c)); !ok {
				t.Errorf("%s: no harness_model_call_errors_total{class=%q}", h, c)
			}
		}
		for _, name := range []string{"harness_model_call_errors_unclassified_total", "harness_sessions_started_total", "harness_session_active"} {
			if _, ok := sample(fams, name, "harness", h); !ok {
				t.Errorf("%s: no %s", h, name)
			}
		}
		// Never succeeded in this daemon's lifetime: omitted, not zero.
		if _, ok := sample(fams, "harness_last_successful_call_timestamp", "harness", h); ok {
			t.Errorf("%s: last-success timestamp present before any success", h)
		}
	}
	for _, o := range []string{"success", "failure"} {
		if _, ok := sample(fams, "harness_scheduled_runs_total", "harness", "job", "outcome", o); !ok {
			t.Errorf("job: no harness_scheduled_runs_total{outcome=%q}", o)
		}
	}
	// The daemon's scheduler, not a stand-in: a real next window, in the
	// future and within one cron period.
	next, ok := sample(fams, "harness_scheduled_next_run_timestamp", "harness", "job")
	if !ok {
		t.Error("job: no harness_scheduled_next_run_timestamp — the daemon did not wire its scheduler")
	} else if now := float64(time.Now().Unix()); next < now-1 || next > now+5*60+1 {
		t.Errorf("job next run = %v, want within the next five minutes of %v", next, now)
	}
	if _, ok := fams["harness_scheduled_runs_total"]; ok {
		if got := len(fams["harness_scheduled_runs_total"].GetMetric()); got != 2 {
			t.Errorf("harness_scheduled_runs_total has %d series, want 2 (job only)", got)
		}
	}
	for _, c := range []string{"supervisor", "schedule", "observer", "lifecycle"} {
		if v, ok := sample(fams, "harness_metrics_collection_errors_total", "collector", c); !ok || v != 0 {
			t.Errorf("collection errors{collector=%q} = %v (present %v), want 0", c, v, ok)
		}
	}
	for _, name := range []string{"go_goroutines", "process_start_time_seconds", "harness_metrics_harnesses_overflowed"} {
		if _, ok := fams[name]; !ok {
			t.Errorf("no %s", name)
		}
	}
}

// A token file that is named but unreadable refuses the daemon even on
// loopback: serving without the auth the operator asked for is the same
// mistake as a missing token, arrived at by accident. (review: harness#356)
func TestDaemonRefusesAMissingTokenFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	missing := filepath.Join(shortSockDir(t), "no-such.token")
	cmd, out, socket := runDaemonBinary(t, bin, fmt.Sprintf(`[server]
metrics_listen = "127.0.0.1:%s"
metrics_token_file = %q
`, freePort(t), missing))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("daemon exited 0; want a refusal\n%s", out)
		}
	case <-time.After(daemonStartupCeiling(t)):
		t.Fatalf("daemon did not refuse to start within %s\n%s", daemonStartupCeiling(t), out)
	}
	if !strings.Contains(out.String(), "refusing to start") || !strings.Contains(out.String(), "metrics_token_file") {
		t.Errorf("refusal does not say why:\n%s", out)
	}
	if _, err := os.Stat(socket); err == nil {
		t.Error("the control socket was created by a daemon that refused to start")
	}
}

// Boot transitions reach the first scrape, through the real runDaemon order.
//
// runDaemon used to build the collector after Autostart, so the starting and
// running transitions Autostart causes were published to a bus nobody from
// metrics was subscribed to yet, and harness_state_transitions_total read 0
// for every harness the daemon brought up at boot (SPEC-0013 REQ-2). The
// collector now subscribes before Autostart; this test fails if that order
// regresses.
//
// The same daemon also boots a gated harness outside its operating hours.
// Autostart holds it (SPEC-0012), and a held harness is down on purpose: it
// must read stopped, never failed. (review, harness#356)
func TestDaemonCountsBootTransitionsAndHeldIsStopped(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	addr := "127.0.0.1:" + freePort(t)
	// A one-minute window twelve hours away is closed now, whenever now is.
	open := time.Now().UTC().Add(12 * time.Hour)
	hours := fmt.Sprintf("TZ=UTC %s-%s", open.Format("15:04"), open.Add(time.Minute).Format("15:04"))
	cfg := fmt.Sprintf(`[server]
metrics_listen = %q

[harness.eager]
harness = "generic"
args = ["-c", "sleep 600"]
enabled = true

[harness.afterhours]
harness = "generic"
args = ["-c", "sleep 600"]
enabled = true
operating_hours = %q
`, addr, hours)
	_, out, _ := runDaemonBinary(t, bin, cfg)

	// The first scrape that answers at all is the one under test: the
	// listener only binds after Autostart has run.
	var fams map[string]*dto.MetricFamily
	deadline := time.Now().Add(daemonStartupCeiling(t))
	for {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			_ = resp.Body.Close()
			fams = scrapeURL(t, "http://"+addr+"/metrics")
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics listener never came up on %s\n%s", addr, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, to := range []string{"starting", "running"} {
		if v, ok := sample(fams, "harness_state_transitions_total", "harness", "eager", "to", to); !ok || v < 1 {
			t.Errorf("eager transitions to %s = %v (present %v) on the first scrape, want >= 1: Autostart's transitions were not counted\n%s", to, v, ok, out)
		}
	}
	if v, _ := sample(fams, "harness_harness_state", "harness", "eager", "state", "running"); v != 1 {
		t.Errorf("eager state=running = %v, want 1", v)
	}

	// The held harness settles as stopped. Poll: the hold is applied on the
	// supervisor's actor loop, so the first scrape may predate it.
	deadline = time.Now().Add(10 * time.Second)
	for {
		stopped, _ := sample(fams, "harness_harness_state", "harness", "afterhours", "state", "stopped")
		failed, okF := sample(fams, "harness_harness_state", "harness", "afterhours", "state", "failed")
		if !okF {
			t.Fatalf("afterhours has no state=failed series\n%s", out)
		}
		if failed != 0 {
			t.Fatalf("held harness reads failed = %v; the gate holding it is not a failure", failed)
		}
		if stopped == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("held harness never read stopped; state series %v\n%s", fams["harness_harness_state"], out)
		}
		time.Sleep(50 * time.Millisecond)
		fams = scrapeURL(t, "http://"+addr+"/metrics")
	}
	if v, _ := sample(fams, "harness_state_transitions_total", "harness", "afterhours", "to", "running"); v != 0 {
		t.Errorf("held harness transitioned to running %v times; boot out of hours must begin held, not start", v)
	}
}
