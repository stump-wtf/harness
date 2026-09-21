package config

// Telemetry Table Tests
//
// Governing tests: ADR-0022; SPEC-0015 REQ-1, REQ-2, REQ-13.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func TestTelemetryAbsentMeansDefaultsAndNoDestination(t *testing.T) {
	cfg, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "/etc/harness/harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.Enabled() {
		t.Fatal("no [telemetry] table must configure no destination")
	}
	if cfg.Telemetry != core.DefaultTelemetryConfig() {
		t.Fatalf("absent table = %+v, want the defaults", cfg.Telemetry)
	}
	if cfg.Harnesses["a"].ExportTelemetry != nil {
		t.Fatal("export_telemetry unset must stay nil (follow export_all)")
	}
}

func TestTelemetryParsesEveryKey(t *testing.T) {
	src := `[telemetry]
logs = true
traces = true
events_file = "events/events.jsonl"
export_all = true
omit_prompts = true
endpoint = "https://collector.example.com:4318/"
env_file = "~/otel.env"
compression = "gzip"
timeout = "3s"
queue_size = 100
batch_size = 10
batch_interval = "2s"
idle_flush = "1m"
shutdown_timeout = "7s"
events_file_max_mb = 12
events_file_keep = 3
`
	home, _ := os.UserHomeDir()
	cfg, err := Parse([]byte(src), "/etc/harness/harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	want := core.TelemetryConfig{
		Logs: true, Traces: true,
		EventsFile:  "/etc/harness/events/events.jsonl",
		ExportAll:   true,
		OmitPrompts: true,
		Endpoint:    "https://collector.example.com:4318",
		EnvFile:     filepath.Join(home, "otel.env"),
		Compression: "gzip", Timeout: 3 * time.Second,
		QueueSize: 100, BatchSize: 10,
		BatchInterval: 2 * time.Second, IdleFlush: time.Minute, ShutdownTimeout: 7 * time.Second,
		EventsFileMaxMB: 12, EventsFileKeep: 3,
	}
	if cfg.Telemetry != want {
		t.Fatalf("telemetry =\n %+v\nwant\n %+v", cfg.Telemetry, want)
	}
}

func TestTelemetryUnsetCompressionAndTimeoutStayZero(t *testing.T) {
	cfg, err := Parse([]byte("[telemetry]\nlogs = true\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	// Zero lets telemetry.Resolve report source=default rather than file.
	if cfg.Telemetry.Compression != "" || cfg.Telemetry.Timeout != 0 {
		t.Fatalf("compression %q timeout %s, want both unset", cfg.Telemetry.Compression, cfg.Telemetry.Timeout)
	}
}

func TestTelemetryValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		line             int
	}{
		{"headers key", `headers = { authorization = "x" }`, "OTEL_EXPORTER_OTLP_HEADERS", 3},
		{"headers string", `headers = "authorization=x"`, "env_file", 3},
		{"userinfo endpoint", `endpoint = "https://user:tok@collector.example.com"`, "userinfo", 3},
		{"relative endpoint", `endpoint = "collector:4318"`, "absolute http or https", 3},
		{"ftp endpoint", `endpoint = "ftp://collector"`, "absolute http or https", 3},
		{"bad compression", `compression = "zstd"`, `"none" or "gzip"`, 3},
		{"bad duration", `timeout = "soon"`, "positive duration", 3},
		{"zero duration", `batch_interval = "0s"`, "positive duration", 3},
		{"negative int", `queue_size = -1`, "positive integer", 3},
		{"zero keep", `events_file_keep = 0`, "positive integer", 3},
		{"batch over queue", "queue_size = 10\nbatch_size = 20", "must not exceed queue_size", 4},
		{"idle under interval", "batch_interval = \"10s\"\nidle_flush = \"5s\"", "at least batch_interval", 4},
		{"blank events file", `events_file = "  "`, "must not be blank", 3},
		{"unknown key", `logz = true`, `unknown key "logz"`, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "# lead\n[telemetry]\n" + tc.body + "\n"
			_, err := Parse([]byte(src), "t.toml")
			if err == nil {
				t.Fatalf("accepted:\n%s", src)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			var ce *Error
			if !errors.As(err, &ce) || ce.Line != tc.line {
				t.Fatalf("error line = %v, want %d (%v)", ce, tc.line, err)
			}
			if strings.Contains(err.Error(), "tok@") {
				t.Fatalf("error echoes the credential: %v", err)
			}
		})
	}
}

func TestTelemetryHeadersSubtableRejected(t *testing.T) {
	src := "[telemetry]\nlogs = true\n\n[telemetry.headers]\nauthorization = \"Bearer x\"\n"
	_, err := Parse([]byte(src), "t.toml")
	if err == nil || !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_HEADERS") {
		t.Fatalf("err = %v, want the headers refusal", err)
	}
}

func TestTelemetryDuplicateTable(t *testing.T) {
	_, err := Parse([]byte("[telemetry]\nlogs = true\n[telemetry]\ntraces = true\n"), "t.toml")
	if err == nil {
		t.Fatal("duplicate [telemetry] accepted")
	}
}

// The key-line helper must not land on a same-named key in another table.
func TestTelemetryErrorLineIsInsideTheTable(t *testing.T) {
	src := `[harness.a]
harness = "claude-code"
prompt = "x"
schedule = "0 3 * * *"
timeout = "5m"

[telemetry]
timeout = "never"
`
	_, err := Parse([]byte(src), "t.toml")
	var ce *Error
	if !errors.As(err, &ce) || ce.Line != 8 {
		t.Fatalf("err = %v, want line 8", err)
	}
}

func TestRemovedOTelEndpointIsAMigrationError(t *testing.T) {
	for _, body := range []string{`otel_endpoint = "https://cairn.stump.wtf"`, `otel_endpoint = ""`} {
		src := "[daemon]\nwatch_config = true\n" + body + "\n"
		_, err := Parse([]byte(src), "t.toml")
		if err == nil {
			t.Fatalf("otel_endpoint accepted: %s", body)
		}
		for _, want := range []string{"[telemetry]", "traces = true", "OTEL_EXPORTER_OTLP_ENDPOINT", "endpoint"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("migration error %q does not name %q", err, want)
			}
		}
		var ce *Error
		if !errors.As(err, &ce) || ce.Line != 3 {
			t.Errorf("error line = %v, want 3", err)
		}
	}
}

func TestExportTelemetryTriState(t *testing.T) {
	src := `[harness.on]
harness = "crush"
export_telemetry = true

[harness.off]
harness = "crush"
export_telemetry = false

[harness.unset]
harness = "crush"
`
	cfg, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	get := func(n string) *bool { return cfg.Harnesses[n].ExportTelemetry }
	if v := get("on"); v == nil || !*v {
		t.Fatalf("on = %v", v)
	}
	if v := get("off"); v == nil || *v {
		t.Fatalf("off = %v", v)
	}
	if v := get("unset"); v != nil {
		t.Fatalf("unset = %v", *v)
	}
}

func TestContributesTelemetryPrecedence(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name      string
		per       *bool
		exportAll bool
		want      bool
	}{
		{"false beats export_all", &no, true, false},
		{"false alone", &no, false, false},
		{"true alone", &yes, false, true},
		{"true with export_all", &yes, true, true},
		{"unset follows export_all on", nil, true, true},
		{"unset follows export_all off", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := core.Harness{Name: "h", ExportTelemetry: tc.per}
			if got := core.ContributesTelemetry(h, tc.exportAll); got != tc.want {
				t.Fatalf("contributes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProjectFileRejectsTelemetryTable(t *testing.T) {
	src := "[harness.a]\nharness = \"crush\"\n\n[telemetry]\nlogs = true\n"
	_, err := ParseProject([]byte(src), filepath.Join(t.TempDir(), "harness.toml"))
	if err == nil || !strings.Contains(err.Error(), "[telemetry]") {
		t.Fatalf("err = %v, want the project refusal of [telemetry]", err)
	}
}

func TestProjectFileMayOptOutButNotIn(t *testing.T) {
	dir := t.TempDir()
	p, err := ParseProject([]byte("[harness.a]\nharness = \"crush\"\nexport_telemetry = false\n"), filepath.Join(dir, "harness.toml"))
	if err != nil {
		t.Fatalf("opt-out rejected: %v", err)
	}
	if v := p.Config.Harnesses["a"].ExportTelemetry; v == nil || *v {
		t.Fatalf("opt-out lost: %v", v)
	}
	_, err = ParseProject([]byte("[harness.a]\nharness = \"crush\"\nexport_telemetry = true\n"), filepath.Join(dir, "harness.toml"))
	if err == nil || !strings.Contains(err.Error(), "global harness.toml") {
		t.Fatalf("project opt-in err = %v, want a refusal", err)
	}
}

func TestHarnessDRejectsTelemetryButAllowsTheHarnessKey(t *testing.T) {
	dir := t.TempDir()
	dropins := filepath.Join(dir, "harness.d")
	if err := os.MkdirAll(dropins, 0o755); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(main, []byte("[server]\nharness_d = \"harness.d\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok := "[harness.in]\nharness = \"crush\"\nexport_telemetry = true\n\n[harness.out]\nharness = \"crush\"\nexport_telemetry = false\n"
	if err := os.WriteFile(filepath.Join(dropins, "a.toml"), []byte(ok), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(main)
	if err != nil {
		t.Fatalf("drop-in export_telemetry rejected: %v", err)
	}
	if v := cfg.Harnesses["in"].ExportTelemetry; v == nil || !*v {
		t.Fatal("drop-in opt-in lost")
	}
	if err := os.WriteFile(filepath.Join(dropins, "b.toml"), []byte("[telemetry]\nlogs = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(main); err == nil || !strings.Contains(err.Error(), "[telemetry]") {
		t.Fatalf("drop-in [telemetry] err = %v, want a refusal", err)
	}
}
