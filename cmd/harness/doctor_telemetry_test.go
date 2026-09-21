package main

// Doctor Telemetry Section
//
// Governing tests: SPEC-0015 REQ-14 (doctor reports the resolved telemetry
// settings with their sources), REQ-3 (header names and fingerprints only —
// never a value, anywhere in the output).
//
// These run the real runDoctor against a real config file and env_file, with
// no daemon (the section is resolved before the daemon is dialled), and read
// what it printed: the JSON on stdout, and the human table on stderr.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/telemetry"
)

const doctorSecret = "s3cr3t-collector-token"

// doctorTelemetryConfig writes a harness.toml whose [telemetry] resolves its
// endpoint and a credential header from env_file, and clears the process's
// OTEL variables so only the files decide.
func doctorTelemetryConfig(t *testing.T, telemetryTable string) string {
	t.Helper()
	for _, k := range telemetry.EnvKeys {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	env := filepath.Join(dir, "otel.env")
	if err := os.WriteFile(env, []byte("OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.com\nOTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer%20"+doctorSecret+",x-tenant=acme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(telemetryTable, env)), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// captureStd runs fn with os.Stdout and os.Stderr redirected, returning both.
func captureStd(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	read := func(f **os.File) func() string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		orig := *f
		*f = w
		done := make(chan string)
		go func() { b, _ := io.ReadAll(r); done <- string(b) }()
		return func() string { w.Close(); *f = orig; return <-done }
	}
	out, errOut := read(&os.Stdout), read(&os.Stderr)
	fn()
	return out(), errOut()
}

func TestDoctorReportsResolvedTelemetry(t *testing.T) {
	cfg := doctorTelemetryConfig(t, `[telemetry]
logs = true
env_file = %q
timeout = "3s"
`)
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	o := verbOpts{configPath: cfg, socket: filepath.Join(t.TempDir(), "no-daemon.sock")}
	stdout, stderr := captureStd(t, func() { runDoctor(o) })

	var res doctorResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("doctor --json is not a doctorResult: %v\n%s", err, stdout)
	}
	want := map[string]settingResult{
		"logs.enabled":              {Value: "true", Source: "file"},
		"logs.endpoint":             {Value: "https://collector.example.com/v1/logs", Source: "env_file"},
		"logs.compression":          {Value: "none", Source: "default"},
		"logs.timeout":              {Value: "3s", Source: "file"},
		"logs.header.authorization": {Value: "sha256:" + telemetry.Fingerprint("Bearer "+doctorSecret), Source: "env_file"},
		"logs.header.x-tenant":      {Value: "sha256:" + telemetry.Fingerprint("acme"), Source: "env_file"},
		"traces.enabled":            {Value: "false", Source: "default"},
		"events_file.enabled":       {Value: "false", Source: "default"},
	}
	for k, w := range want {
		if got, ok := res.Telemetry[k]; !ok || got != w {
			t.Errorf("telemetry[%s] = %+v (present %v), want %+v", k, got, ok, w)
		}
	}
	if len(res.Telemetry) != len(want) {
		t.Errorf("telemetry has %d settings, want %d: %+v", len(res.Telemetry), len(want), res.Telemetry)
	}
	if strings.Contains(stdout+stderr, doctorSecret) || strings.Contains(stdout+stderr, "acme") {
		t.Fatal("doctor printed a header value")
	}

	// The human table carries the same section, and no value either.
	cliui.SetJSON(false)
	_, table := captureStd(t, func() { runDoctor(o) })
	for _, s := range []string{"TELEMETRY", "logs.header.authorization", "sha256:" + telemetry.Fingerprint("Bearer "+doctorSecret), "env_file"} {
		if !strings.Contains(table, s) {
			t.Errorf("doctor table lacks %q:\n%s", s, table)
		}
	}
	if strings.Contains(table, doctorSecret) {
		t.Fatal("doctor table printed a header value")
	}
}

func TestDoctorTelemetryOffPrintsNoSection(t *testing.T) {
	cfg := doctorTelemetryConfig(t, "# env_file %q is not referenced\n")
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	stdout, _ := captureStd(t, func() {
		runDoctor(verbOpts{configPath: cfg, socket: filepath.Join(t.TempDir(), "no-daemon.sock")})
	})
	var res doctorResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.Telemetry != nil || res.TelemetryCheck != nil {
		t.Fatalf("telemetry reported with no destination: %+v %+v", res.Telemetry, res.TelemetryCheck)
	}
}

func TestDoctorWarnsOnAnUnresolvableTelemetryConfig(t *testing.T) {
	cfg := doctorTelemetryConfig(t, "# %q unused\n[telemetry]\ntraces = true\nenv_file = \"missing.env\"\n")
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	stdout, _ := captureStd(t, func() {
		runDoctor(verbOpts{configPath: cfg, socket: filepath.Join(t.TempDir(), "no-daemon.sock")})
	})
	var res doctorResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.TelemetryCheck == nil || res.TelemetryCheck.Status != cliui.LevelWarn.String() || !strings.Contains(res.TelemetryCheck.Detail, "env_file") {
		t.Fatalf("telemetry_check = %+v, want a warning naming env_file", res.TelemetryCheck)
	}
}
