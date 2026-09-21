package telemetry

// Resolution and Events File Tests
//
// Governing tests: SPEC-0015 REQ-1 (env alone never enables), REQ-3
// (precedence, header merge, env_file isolation, refusals), REQ-5 (resource),
// REQ-8 (rotation), REQ-14 (value-free log fields).
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func fileEnv(vals map[string]string) func(string) (map[string]string, string, error) {
	return func(string) (map[string]string, string, error) { return vals, "", nil }
}

func otlpCfg(mut func(*core.TelemetryConfig)) core.TelemetryConfig {
	c := core.DefaultTelemetryConfig()
	c.Logs, c.Traces = true, true
	if mut != nil {
		mut(&c)
	}
	return c
}

func TestResolveNothingConfiguredIsNil(t *testing.T) {
	env := Env{Getenv: envOf(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"})}
	res, err := Resolve(core.DefaultTelemetryConfig(), env)
	if err != nil || res != nil {
		t.Fatalf("res %+v err %v: env vars alone must configure nothing", res, err)
	}
	note := IgnoredEnvNote(core.DefaultTelemetryConfig(), env.Getenv)
	if !strings.Contains(note, "OTEL_EXPORTER_OTLP_ENDPOINT") || !strings.Contains(note, "logs = true") {
		t.Fatalf("note %q", note)
	}
	if IgnoredEnvNote(core.DefaultTelemetryConfig(), envOf(nil)) != "" {
		t.Fatal("a note with nothing set")
	}
}

func TestResolvePrecedence(t *testing.T) {
	cfg := otlpCfg(func(c *core.TelemetryConfig) {
		c.Endpoint = "https://file.example"
		c.Compression = "none"
		c.Timeout = 3 * time.Second
		c.EnvFile = "/etc/harness/otel.env"
	})
	proc := map[string]string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://proc-traces.example/custom",
		"OTEL_EXPORTER_OTLP_COMPRESSION":     "gzip",
		"OTEL_EXPORTER_OTLP_LOGS_TIMEOUT":    "1500",
		"OTEL_EXPORTER_OTLP_HEADERS":         "", // empty = unset
	}
	file := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":        "https://envfile.example/",
		"OTEL_EXPORTER_OTLP_HEADERS":         "authorization=Bearer%20generic,x-tenant=t1",
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS":    "authorization=Bearer%20logs",
		"OTEL_EXPORTER_OTLP_TIMEOUT":         "2500",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://envfile-traces.example",
	}
	res, err := Resolve(cfg, Env{Getenv: envOf(proc), ReadEnvFile: fileEnv(file), Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	l, tr := res.Logs, res.Traces
	// logs: generic env_file base URL beats the file's endpoint.
	if l.URL != "https://envfile.example/v1/logs" || l.URLSource != SourceEnvFile {
		t.Errorf("logs url %s (%s)", l.URL, l.URLSource)
	}
	// traces: the process's signal-specific full URL, used as-is, beats all.
	if tr.URL != "https://proc-traces.example/custom" || tr.URLSource != SourceEnv {
		t.Errorf("traces url %s (%s)", tr.URL, tr.URLSource)
	}
	// headers: generic then signal-specific per key; decoded.
	if l.Headers["authorization"] != "Bearer logs" || l.Headers["x-tenant"] != "t1" {
		t.Errorf("logs headers %v", l.Headers)
	}
	if tr.Headers["authorization"] != "Bearer generic" || tr.HeaderSources["authorization"] != SourceEnvFile {
		t.Errorf("traces headers %v %v", tr.Headers, tr.HeaderSources)
	}
	// compression: process generic env beats the file.
	if l.Compression != "gzip" || l.CompressionSource != SourceEnv {
		t.Errorf("compression %s (%s)", l.Compression, l.CompressionSource)
	}
	// timeout: signal-specific (process) > generic (env_file) > file.
	if l.Timeout != 1500*time.Millisecond || l.TimeoutSource != SourceEnv {
		t.Errorf("logs timeout %s (%s)", l.Timeout, l.TimeoutSource)
	}
	if tr.Timeout != 2500*time.Millisecond || tr.TimeoutSource != SourceEnvFile {
		t.Errorf("traces timeout %s (%s)", tr.Timeout, tr.TimeoutSource)
	}
}

func TestResolveFileAndDefaults(t *testing.T) {
	res, err := Resolve(otlpCfg(func(c *core.TelemetryConfig) { c.Endpoint = "http://127.0.0.1:4318/" }), Env{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Logs.URL != "http://127.0.0.1:4318/v1/logs" || res.Logs.URLSource != SourceFile {
		t.Errorf("url %s", res.Logs.URL)
	}
	if res.Logs.Compression != "none" || res.Logs.CompressionSource != SourceDefault || res.Logs.Timeout != 10*time.Second || res.Logs.TimeoutSource != SourceDefault {
		t.Errorf("defaults %+v", res.Logs)
	}
}

func TestResolveRefusesAnEnabledSignalWithoutAnEndpoint(t *testing.T) {
	_, err := Resolve(otlpCfg(func(c *core.TelemetryConfig) { c.Traces = false }), Env{Getenv: envOf(nil)})
	if err == nil {
		t.Fatal("logs = true with no endpoint started")
	}
	for _, want := range []string{"logs", "[telemetry] endpoint", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT", "env_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestResolveEnvFileErrorsAndIsolation(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.env")
	cfg := otlpCfg(func(c *core.TelemetryConfig) { c.Endpoint = "http://127.0.0.1:1"; c.EnvFile = missing })
	if _, err := Resolve(cfg, Env{}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing env_file err = %v", err)
	}

	secret := "hunter2-hunter2-hunter2"
	path := filepath.Join(dir, "otel.env")
	body := "OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer%20" + secret + "\nHARNESS_UNRELATED_TOKEN=shouldnotleak\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.EnvFile = path
	res, err := Resolve(cfg, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Logs.Headers["authorization"] != "Bearer "+secret {
		t.Fatalf("header not read from env_file: %v", res.Logs.HeaderSources)
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_HEADERS") != "" || os.Getenv("HARNESS_UNRELATED_TOKEN") != "" {
		t.Fatal("env_file values leaked into the process environment, where every harness inherits them")
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, "\n"), "chmod 600") {
		t.Fatalf("no permissions warning for a 0644 env_file: %v", res.Warnings)
	}
	// Nothing printable carries the value; the name and a fingerprint do.
	line := fmt.Sprint(res.Logs.LogKeyvals()...)
	if strings.Contains(line, secret) {
		t.Fatalf("log fields carry the header value: %s", line)
	}
	if !strings.Contains(line, "authorization=sha256:"+Fingerprint("Bearer "+secret)) {
		t.Fatalf("log fields lack the fingerprint: %s", line)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, secret) {
			t.Fatalf("warning carries the value: %s", w)
		}
	}
}

func TestResolveWarningsAndSDKDisabled(t *testing.T) {
	cfg := otlpCfg(func(c *core.TelemetryConfig) { c.Endpoint = "http://collector.internal:4318" })
	res, err := Resolve(cfg, Env{Getenv: envOf(map[string]string{
		"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
		"OTEL_EXPORTER_OTLP_HEADERS":  "x-api-key=abc",
	})})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(res.Warnings, "\n")
	if !strings.Contains(all, "grpc") || !strings.Contains(all, "cleartext") {
		t.Fatalf("warnings %v", res.Warnings)
	}
	if strings.Contains(all, "abc") {
		t.Fatal("a warning carries a header value")
	}

	cfg.EventsFile = "/tmp/e.jsonl"
	res, err = Resolve(cfg, Env{Getenv: envOf(map[string]string{"OTEL_SDK_DISABLED": "true"})})
	if err != nil || res.Logs != nil || res.Traces != nil || !res.Enabled() {
		t.Fatalf("OTEL_SDK_DISABLED: %+v %v (the events file should survive)", res, err)
	}
}

func TestResolveResourceAttributes(t *testing.T) {
	cfg := otlpCfg(func(c *core.TelemetryConfig) { c.Endpoint = "http://127.0.0.1:1" })
	res, err := Resolve(cfg, Env{
		Getenv:   envOf(map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "deployment.environment=prod,service.name=evil,team=a%20b,k8s.pod.Name=Web-1"}),
		Hostname: func() (string, error) { return "box", nil },
		Version:  "v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, kv := range res.Resource {
		got[kv.Key] = kv.Value
	}
	if got["service.name"] != "harness" || got["service.version"] != "v2" || got["host.name"] != "box" ||
		got["deployment.environment"] != "prod" || got["team"] != "a b" ||
		got["k8s.pod.Name"] != "Web-1" { // attribute keys are case-sensitive; values untouched
		t.Fatalf("resource %v", got)
	}
}

func TestSafeURL(t *testing.T) {
	if got := SafeURL("https://u:p@host:4318/v1/logs?api_key=zzz"); got != "https://host:4318/v1/logs" {
		t.Fatalf("SafeURL = %s", got)
	}
}

func TestEventsFileRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	w := newEventsFile(path, 1, 2)
	line := func(i int) []byte {
		b, _ := json.Marshal(map[string]any{"i": i, "pad": strings.Repeat("x", 100<<10)})
		return append(b, '\n')
	}
	// ~100 KiB lines into a 1 MiB file: rotation after about ten.
	for i := range 45 {
		if err := w.write(line(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.flushClose(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"events.jsonl", "events.jsonl.1", "events.jsonl.2"} {
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if info.Size() > 1<<20 {
			t.Errorf("%s is %d bytes, past the cap", name, info.Size())
		}
		f, _ := os.Open(p)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 256<<10), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				t.Fatalf("%s holds a partial line", name)
			}
		}
		f.Close()
	}
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl.3")); err == nil {
		t.Fatal("kept more than events_file_keep rotated files")
	}
	// The newest line is in the live file; .1 is older than it.
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"i":44`) {
		t.Fatal("newest line not in the live file")
	}
}

func TestEventsFileWriteFailureIsCountedNotFatal(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := testResolved("", func(c *core.TelemetryConfig) {
		c.Logs, c.Traces = false, false
		c.EventsFile = filepath.Join(blocker, "events.jsonl") // parent is a file
	})
	p, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{})
	obs.publish(toolEv("w", "k", 0, "x", false, t0))
	waitFor(t, "the failure to be counted", func() bool { return p.Stats()[SignalEventsFile].Failed == 1 })
	if st := p.Stats()[SignalEventsFile]; st.RequestsPermanent != 1 || st.LastError == "" {
		t.Fatalf("stats %+v", st)
	}
}
