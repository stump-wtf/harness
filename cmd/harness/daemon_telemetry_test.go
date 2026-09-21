package main

// Daemon Telemetry Wiring
//
// Governing tests: ADR-0021; SPEC-0014 REQ-1, REQ-2, REQ-3.
//
// The telemetry package's tests build their own pipelines over fakes. What
// they cannot show is that the daemon builds one from a real parsed
// harness.toml, over its real observer and Manager, and that an agent's error
// reaches a collector through it — the gap #315 fell through with a Policy
// nobody wired. So this drives resolveDaemonTelemetry and startDaemonTelemetry
// with a config that went through config.Parse, against a real Manager and a
// stand-in crush process, and asserts on the decoded request a collector got.
// The companion proves the zero-config daemon subscribes to nothing and sends
// nothing, even with OTEL variables in its environment.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/telemetry"
)

// telemetryTestManager starts a real Manager running cfg's "worker" harness
// under a crush stand-in, and returns it with the harness's workdir.
func telemetryTestManager(t *testing.T, tmp string, cfg *core.Config) *supervisor.Manager {
	t.Helper()
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

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.StopGrace = 100 * time.Millisecond
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	if !mgr.Start("worker") {
		t.Fatal("Start returned false")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if snap, _ := mgr.Snapshot("worker"); snap.State == core.StateRunning && snap.PID != 0 {
			return mgr
		}
		if time.Now().After(deadline) {
			t.Fatal("harness never came up")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type collector struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
}

func (c *collector) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, string(b))
	c.paths = append(c.paths, r.URL.Path)
	c.mu.Unlock()
}

func (c *collector) snapshot() ([]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bodies...), append([]string(nil), c.paths...)
}

func TestDaemonTelemetryExportsFromARealParsedConfig(t *testing.T) {
	tmp := t.TempDir()
	col := &collector{}
	srv := httptest.NewServer(http.HandlerFunc(col.handler))
	t.Cleanup(srv.Close)
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	events := filepath.Join(tmp, "state", "events.jsonl")

	toml := fmt.Sprintf(`[telemetry]
logs = true
events_file = %q
endpoint = %q
batch_interval = "50ms"
idle_flush = "1s"

[harness.worker]
harness = "crush"
workdir = %q
restart = "no"
export_telemetry = true
`, events, srv.URL, work)
	cfg, err := config.Parse([]byte(toml), filepath.Join(tmp, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := telemetryTestManager(t, tmp, cfg)

	db := filepath.Join(work, ".crush", "crush.db")
	now := time.Now()
	rt.WriteCrushDB(t, db, rt.CrushSession{
		ID: "live", Created: now, Updated: now,
		Messages: []rt.CrushMessage{{Role: "user", At: now, Parts: `[{"type":"text","data":{"text":"go"}}]`}},
	})

	res, err := resolveDaemonTelemetry(cfg.Telemetry, telemetry.Env{Getenv: func(string) string { return "" }, Version: "test"})
	if err != nil || res == nil {
		t.Fatalf("resolve: %v %v", res, err)
	}
	obsOpts := daemonObserverOptions()
	obsOpts.PollInterval = 10 * time.Millisecond
	obs := startDaemonObserver(mgr, obsOpts)
	t.Cleanup(obs.Stop)
	pipe := startDaemonTelemetry(res, obs, mgr, telemetry.Options{})
	if pipe == nil {
		t.Fatal("the daemon built no pipeline for a configured destination")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pipe.Shutdown(ctx)
	})

	rt.AppendCrushMessages(t, db, "live", rt.CrushMessage{Role: "assistant", At: time.Now(), Parts: rt.FinishError("quota exhausted", "429")})

	deadline := time.Now().Add(15 * time.Second)
	for {
		bodies, paths := col.snapshot()
		if rec, ok := findErrorRecord(bodies); ok {
			if paths[0] != "/v1/logs" {
				t.Fatalf("posted to %s", paths[0])
			}
			if rec["harness.name"] != "worker" || rec["agent.mark.type"] != "error" {
				t.Fatalf("error record attributes %v", rec)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ERROR log record reached the collector; bodies %v; stats %+v; observer %+v", bodies, pipe.Stats(), obs.Stats())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The same item reached the events file, under the daemon's resolved path.
	deadline = time.Now().Add(5 * time.Second)
	for {
		if data, _ := os.ReadFile(events); strings.Contains(string(data), `"severity":"ERROR"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the error mark never reached the events file")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// findErrorRecord returns the attributes of the first ERROR log record.
func findErrorRecord(bodies []string) (map[string]any, bool) {
	for _, b := range bodies {
		var env struct {
			ResourceLogs []struct {
				ScopeLogs []struct {
					LogRecords []struct {
						SeverityText string `json:"severityText"`
						Attributes   []struct {
							Key   string         `json:"key"`
							Value map[string]any `json:"value"`
						} `json:"attributes"`
					} `json:"logRecords"`
				} `json:"scopeLogs"`
			} `json:"resourceLogs"`
		}
		if json.Unmarshal([]byte(b), &env) != nil {
			continue
		}
		for _, rl := range env.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					if lr.SeverityText != "ERROR" {
						continue
					}
					out := map[string]any{}
					for _, a := range lr.Attributes {
						for _, v := range a.Value {
							out[a.Key] = v
						}
					}
					return out, true
				}
			}
		}
	}
	return nil, false
}

// The laptop: no [telemetry] table, an OTLP endpoint inherited from the shell.
// The daemon must subscribe to nothing, create nothing, and send nothing.
func TestDaemonTelemetryAbsentMeansNothing(t *testing.T) {
	tmp := t.TempDir()
	var hits sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { hits.Store(r.URL.Path, true) }))
	t.Cleanup(srv.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf("[harness.worker]\nharness = \"crush\"\nworkdir = %q\nrestart = \"no\"\nexport_telemetry = true\n", work)), filepath.Join(tmp, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := telemetryTestManager(t, tmp, cfg)

	res, err := resolveDaemonTelemetry(cfg.Telemetry, telemetry.ProcessEnv("test"))
	if err != nil || res != nil {
		t.Fatalf("resolve = %v, %v; want nothing at all", res, err)
	}
	obsOpts := daemonObserverOptions()
	obsOpts.PollInterval = 10 * time.Millisecond
	obs := startDaemonObserver(mgr, obsOpts)
	t.Cleanup(obs.Stop)
	if p := startDaemonTelemetry(res, obs, mgr, telemetry.Options{}); p != nil {
		t.Fatal("a pipeline was built with no destination")
	}

	db := filepath.Join(work, ".crush", "crush.db")
	now := time.Now()
	rt.WriteCrushDB(t, db, rt.CrushSession{ID: "live", Created: now, Updated: now,
		Messages: []rt.CrushMessage{{Role: "user", At: now, Parts: `[{"type":"text","data":{"text":"go"}}]`}}})
	rt.AppendCrushMessages(t, db, "live", rt.CrushMessage{Role: "assistant", At: time.Now(), Parts: rt.FinishError("quota exhausted", "429")})
	// Show the observer did deliver, so the silence below is not vacuous.
	deadline := time.Now().Add(10 * time.Second)
	for obs.Stats().Delivered == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the observer never delivered; this test would prove nothing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for name := range obs.Stats().Dropped {
		if strings.HasPrefix(name, "telemetry.") {
			t.Fatalf("telemetry subscribed to the observer (%s) with no destination", name)
		}
	}
	hits.Range(func(k, _ any) bool {
		t.Fatalf("a request reached the inherited OTLP endpoint: %v", k)
		return false
	})
	if _, err := os.Stat(filepath.Join(tmp, "state")); err == nil {
		t.Fatal("a telemetry directory was created")
	}
}

func TestDaemonRefusesAnUndeliverableSignal(t *testing.T) {
	cfg, err := config.Parse([]byte("[telemetry]\ntraces = true\n"), "harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDaemonTelemetry(cfg.Telemetry, telemetry.Env{Getenv: func(string) string { return "" }}); err == nil {
		t.Fatal("traces = true with no endpoint anywhere did not refuse the start")
	}
}

func TestTelemetryReloadWarnsOnlyOnATableChange(t *testing.T) {
	a := core.DefaultTelemetryConfig()
	b := a
	if warnTelemetryReload(a, b) {
		t.Fatal("warned with nothing changed")
	}
	b.Logs = true
	if !warnTelemetryReload(a, b) {
		t.Fatal("no warning for a changed [telemetry] table")
	}
}
