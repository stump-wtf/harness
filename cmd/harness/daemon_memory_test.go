package main

// Daemon Memory Guardrails Wiring Tests
//
// Governing tests: SPEC-0010 REQ "Go Memory Limit" (precedence, and that the
// limit reaches the runtime), SPEC-0013 REQ-7 (the runtime memory series on
// the daemon's endpoint) and REQ-8 (pprof: loopback only, served, stopped).
//
// internal/diag tests the policy and internal/metrics the collector. What they
// cannot show is that the daemon resolves the settings, applies the limit and
// binds pprof: the #315 gap. So the unit tests here drive the functions
// runDaemon calls, and the binary tests read the property off the running
// process — go_gc_gomemlimit_bytes is the limit the runtime holds, not a value
// the daemon computed and logged.
//
// @joestump-agent 09/28/2026 - Added for GitHub
// https://github.com/stump-wtf/harness/issues/18.

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/settings"
)

// resolveDaemonWithConfig is resolveDaemonFor (scrollback_settings_test.go)
// over a harness.toml holding cfgBody.
func resolveDaemonWithConfig(t *testing.T, cfgBody string, args ...string) (daemonOpts, error) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "harness.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_CONFIG", cfgPath)
	return resolveDaemonFor(t, args...)
}

// keepRuntimeMemoryLimit restores the test process's limit afterwards.
func keepRuntimeMemoryLimit(t *testing.T) {
	t.Helper()
	was := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(was) })
}

// flag > HARNESS_MEMORY_LIMIT > [daemon] memory_limit, each with its source,
// and each applied limit read back from the runtime.
func TestDaemonMemoryLimitResolvesAndApplies(t *testing.T) {
	keepRuntimeMemoryLimit(t)
	t.Setenv("HARNESS_MEMORY_LIMIT", "")
	noGOMEMLIMIT := func(string) string { return "" }
	const file = "[daemon]\nmemory_limit = \"2GiB\"\n"

	for _, tc := range []struct {
		name      string
		env       string
		args      []string
		wantBytes int64
		wantSrc   settings.Source
		wantChild string // the --detach child's flag, "" for none
	}{
		{"file", "", nil, 2 << 30, settings.SourceFile, "--memory-limit 2GiB"},
		{"env over file", "1GiB", nil, 1 << 30, settings.SourceEnv, "--memory-limit 1GiB"},
		{"flag over env", "1GiB", []string{"--memory-limit", "512MiB"}, 512 << 20, settings.SourceFlag, "--memory-limit 512MiB"},
		{"explicit 0 is off", "0", nil, 0, settings.SourceEnv, "--memory-limit 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARNESS_MEMORY_LIMIT", tc.env)
			d, err := resolveDaemonWithConfig(t, file, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if d.memoryLimit != tc.wantBytes || d.memoryLimitSource != tc.wantSrc {
				t.Fatalf("resolved %d from %s, want %d from %s", d.memoryLimit, d.memoryLimitSource, tc.wantBytes, tc.wantSrc)
			}
			if args := strings.Join(d.childArgs(), " "); !strings.Contains(args, tc.wantChild) {
				t.Errorf("--detach child args %q lack %q", args, tc.wantChild)
			}

			debug.SetMemoryLimit(math.MaxInt64)
			applyDaemonMemoryLimit(d.memoryLimit, d.memoryLimitSource, noGOMEMLIMIT)
			want := tc.wantBytes
			if want == 0 {
				want = math.MaxInt64
			}
			if rt := debug.SetMemoryLimit(-1); rt != want {
				t.Errorf("runtime limit = %d, want %d", rt, want)
			}
		})
	}
}

// Unset everywhere: the default source, no flag for the --detach child, and
// the runtime's limit (GOMEMLIMIT's, or none) left exactly as it was.
func TestDaemonMemoryLimitUnsetLeavesGOMEMLIMIT(t *testing.T) {
	keepRuntimeMemoryLimit(t)
	t.Setenv("HARNESS_MEMORY_LIMIT", "")
	d, err := resolveDaemonWithConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if d.memoryLimitSource != settings.SourceDefault || d.memoryLimit != 0 {
		t.Fatalf("unset resolved %d from %s, want 0 from default", d.memoryLimit, d.memoryLimitSource)
	}
	if args := strings.Join(d.childArgs(), " "); strings.Contains(args, "--memory-limit") {
		t.Errorf("--detach child args pass an unset limit as a flag, which would outrank GOMEMLIMIT: %s", args)
	}

	// As if the runtime had started under GOMEMLIMIT=768MiB.
	debug.SetMemoryLimit(768 << 20)
	l := applyDaemonMemoryLimit(d.memoryLimit, d.memoryLimitSource, func(k string) string {
		if k == "GOMEMLIMIT" {
			return "768MiB"
		}
		return ""
	})
	if l.Source != "GOMEMLIMIT" || l.Bytes != 768<<20 {
		t.Errorf("resolved %+v, want 768MiB from GOMEMLIMIT", l)
	}
	if rt := debug.SetMemoryLimit(-1); rt != 768<<20 {
		t.Errorf("runtime limit = %d, want GOMEMLIMIT's %d untouched", rt, int64(768<<20))
	}
}

// The startup line names the limit in effect and its source, says when it
// overrides GOMEMLIMIT, and a small bare number draws the bytes warning.
func TestApplyDaemonMemoryLimitLogs(t *testing.T) {
	keepRuntimeMemoryLimit(t)
	var buf bytes.Buffer
	level := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetLevel(level)
	})
	withGOMEMLIMIT := func(k string) string {
		if k == "GOMEMLIMIT" {
			return "512MiB"
		}
		return ""
	}

	applyDaemonMemoryLimit(1<<30, settings.SourceFile, withGOMEMLIMIT)
	for _, want := range []string{"memory limit", "limit=1GiB", "source=file", "overrides=GOMEMLIMIT"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("startup log lacks %q:\n%s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "very small") {
		t.Errorf("1GiB drew the small-limit warning:\n%s", buf.String())
	}

	buf.Reset()
	applyDaemonMemoryLimit(2048, settings.SourceEnv, func(string) string { return "" })
	debug.SetMemoryLimit(math.MaxInt64) // a 2 KiB limit keeps the GC spinning; lift it at once
	for _, want := range []string{"limit=2KiB", "very small", "bare number is bytes"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("a 2048-byte limit's log lacks %q:\n%s", want, buf.String())
		}
	}

	buf.Reset()
	debug.SetMemoryLimit(math.MaxInt64)
	applyDaemonMemoryLimit(0, settings.SourceDefault, func(string) string { return "" })
	for _, want := range []string{"limit=off", "source=default"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the unset limit's log lacks %q:\n%s", want, buf.String())
		}
	}
}

// A bad size fails the start, naming its source (SPEC-0010).
func TestDaemonMemoryLimitInvalidIsFatal(t *testing.T) {
	t.Setenv("HARNESS_MEMORY_LIMIT", "lots")
	_, err := resolveDaemonWithConfig(t, "")
	if err == nil || !strings.Contains(err.Error(), "HARNESS_MEMORY_LIMIT") || !strings.Contains(err.Error(), "lots") {
		t.Fatalf("HARNESS_MEMORY_LIMIT=lots: err = %v, want one naming the variable and value", err)
	}
}

// A non-loopback pprof address is refused before anything starts, named by
// whichever source supplied it.
func TestDaemonPprofAddrRefusedOffLoopback(t *testing.T) {
	t.Setenv("HARNESS_PPROF_ADDR", "")
	for _, tc := range []struct {
		name, cfg, env string
		args           []string
		origin         string
	}{
		{"file", "[daemon]\npprof_addr = \":6060\"\n", "", nil, "daemon.pprof_addr"},
		{"env", "", "0.0.0.0:6060", nil, "HARNESS_PPROF_ADDR"},
		{"flag", "", "", []string{"--pprof-addr", "10.0.0.1:6060"}, "--pprof-addr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARNESS_PPROF_ADDR", tc.env)
			_, err := resolveDaemonWithConfig(t, tc.cfg, tc.args...)
			if err == nil {
				t.Fatal("resolved without error; want a refusal")
			}
			if !strings.Contains(err.Error(), tc.origin) || !strings.Contains(err.Error(), "loopback") {
				t.Errorf("error %q does not name %s and loopback", err, tc.origin)
			}
		})
	}

	t.Setenv("HARNESS_PPROF_ADDR", "127.0.0.1:6060")
	d, err := resolveDaemonWithConfig(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(d.childArgs(), " "); !strings.Contains(args, "--pprof-addr 127.0.0.1:6060") {
		t.Errorf("--detach child args drop pprof_addr: %s", args)
	}
}

func httpGet(t *testing.T, url string) (int, string, error) {
	t.Helper()
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// startDaemonPprof serves on loopback, is nil when off or when the port is
// taken (the daemon carries on), and stops.
func TestStartDaemonPprof(t *testing.T) {
	if s := startDaemonPprof(""); s != nil {
		t.Fatal("pprof started with no address")
	}

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if s := startDaemonPprof(held.Addr().String()); s != nil {
		stopDaemonPprof(s)
		t.Fatal("pprof claimed a port another listener holds")
	}

	s := startDaemonPprof("127.0.0.1:0")
	if s == nil {
		t.Fatal("pprof did not start on 127.0.0.1:0")
	}
	base := "http://" + s.Addr()
	code, body, err := httpGet(t, base+"/debug/pprof/heap?debug=1")
	if err != nil || code != http.StatusOK || !strings.Contains(body, "heap profile") {
		t.Fatalf("GET heap = %d %v: %.200q", code, err, body)
	}
	stopDaemonPprof(s)
	stopDaemonPprof(nil) // a no-op, as runDaemon calls it when pprof is off
	if _, _, err := httpGet(t, base+"/debug/pprof/"); err == nil {
		t.Error("pprof still answering after stopDaemonPprof")
	}
}

// waitHTTP polls url until it answers or the startup ceiling passes. On
// failure it stops the daemon and prints its output.
func waitHTTP(t *testing.T, url string, stop func() string) {
	t.Helper()
	deadline := time.Now().Add(daemonStartupCeiling(t))
	for {
		if _, _, err := httpGet(t, url); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never answered\n%s", url, stop())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The real binary: the limit the running daemon holds (read off its own
// go_gc_gomemlimit_bytes) follows the precedence, the runtime memory series
// are on its endpoint, and pprof answers on the configured loopback port.
func TestDaemonBinaryMemoryLimitAndPprof(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)

	for _, tc := range []struct {
		name    string
		limit   string // [daemon] memory_limit, "" to omit
		env     []string
		want    float64
		wantLog string // the startup line's limit and source
	}{
		{"file", `"1GiB"`, []string{"HARNESS_MEMORY_LIMIT=", "GOMEMLIMIT="}, 1 << 30, "limit=1GiB source=file"},
		{"env over file", `"1GiB"`, []string{"HARNESS_MEMORY_LIMIT=768MiB", "GOMEMLIMIT="}, 768 << 20, "limit=768MiB source=env"},
		{"file over GOMEMLIMIT", `"1GiB"`, []string{"HARNESS_MEMORY_LIMIT=", "GOMEMLIMIT=512MiB"}, 1 << 30, "limit=1GiB source=file overrides=GOMEMLIMIT"},
		{"GOMEMLIMIT when unset", "", []string{"HARNESS_MEMORY_LIMIT=", "GOMEMLIMIT=512MiB"}, 512 << 20, "limit=512MiB source=GOMEMLIMIT"},
		{"off when nothing is set", "", []string{"HARNESS_MEMORY_LIMIT=", "GOMEMLIMIT="}, math.MaxInt64, "limit=off source=default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The Go families below ride the /metrics listener, so it binds
			// 127.0.0.1:0 and the real address is read back from the daemon's
			// log: two daemons handed the same reserved port under load is how
			// main lost a listener in run 15678 (harness#857). pprof keeps a
			// reserved port, because its endpoint is the assertion here.
			pprofAddr := "127.0.0.1:" + freePort(t)
			logPath := filepath.Join(shortSockDir(t), "daemon.log")
			cfg := fmt.Sprintf("[daemon]\npprof_addr = %q\n", pprofAddr)
			if tc.limit != "" {
				cfg += "memory_limit = " + tc.limit + "\n"
			}
			cfg += "[server]\nmetrics_listen = \"127.0.0.1:0\"\n"
			env := append([]string{"HARNESS_PPROF_ADDR="}, tc.env...)
			cmd, _ := spawnDaemonWithLog(t, bin, cfg, logPath, env...)
			metricsAddr := boundAddr(t, logPath, "metrics listening")
			// Killing first makes the log complete: the daemon writes the log
			// file itself, so once it has stopped the whole startup is there.
			stop := func() string {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return logged(logPath)
			}

			waitHTTP(t, "http://"+metricsAddr+"/metrics", stop)
			fams := scrapeURL(t, "http://"+metricsAddr+"/metrics")
			var failures []string
			if v, ok := sample(fams, "go_gc_gomemlimit_bytes"); !ok || v != tc.want {
				failures = append(failures, fmt.Sprintf("go_gc_gomemlimit_bytes = %v (present %v), want %v", v, ok, tc.want))
			}
			for _, name := range []string{"go_memory_classes_heap_objects_bytes", "go_gc_heap_goal_bytes", "go_goroutines"} {
				if _, ok := fams[name]; !ok {
					failures = append(failures, name+" missing from the daemon's endpoint")
				}
			}

			waitHTTP(t, "http://"+pprofAddr+"/debug/pprof/", stop)
			code, body, err := httpGet(t, "http://"+pprofAddr+"/debug/pprof/heap?debug=1")
			if err != nil || code != http.StatusOK || !strings.Contains(body, "heap profile") {
				failures = append(failures, fmt.Sprintf("daemon pprof heap = %d %v: %.200q", code, err, body))
			}

			output := stop()
			if !strings.Contains(output, "memory limit "+tc.wantLog) {
				failures = append(failures, "the startup log lacks \"memory limit "+tc.wantLog+"\"")
			}
			if len(failures) > 0 {
				t.Errorf("%s\ndaemon output:\n%s", strings.Join(failures, "\n"), output)
			}
		})
	}
}

// The real binary refuses a non-loopback pprof address and never serves.
func TestDaemonBinaryRefusesNonLoopbackPprof(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	port := freePort(t)
	cmd, out, socket := runDaemonBinary(t, bin, "", "HARNESS_PPROF_ADDR=0.0.0.0:"+port)
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
	if !strings.Contains(out.String(), "HARNESS_PPROF_ADDR") || !strings.Contains(out.String(), "loopback") {
		t.Errorf("refusal does not say why:\n%s", out)
	}
	if _, err := os.Stat(socket); err == nil {
		t.Error("the control socket was created by a daemon that refused to start")
	}
	if _, _, err := httpGet(t, "http://127.0.0.1:"+port+"/debug/pprof/"); err == nil {
		t.Error("something answers pprof on the refused port")
	}
}
