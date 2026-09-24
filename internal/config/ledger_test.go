package config

import (
	"strings"
	"testing"
)

// SPEC-0022 REQ-9 and REQ-19: [ledger] trace_url, global only.

func TestLedgerTraceURLParses(t *testing.T) {
	const url = "https://grafana.example.com/explore?traceId={trace_id}"
	cfg, err := Parse([]byte("[ledger]\ntrace_url = \""+url+"\"\n"), "/etc/harness/harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ledger.TraceURL != url {
		t.Errorf("trace_url = %q", cfg.Ledger.TraceURL)
	}
	if _, ok := cfg.Harnesses["ledger"]; ok {
		t.Error("[ledger] was read as a bare harness table")
	}
}

// REQ-9 "A template with another placeholder": the load fails, naming it.
func TestLedgerTraceURLRejectsOtherPlaceholders(t *testing.T) {
	_, err := Parse([]byte("[ledger]\ntrace_url = \"https://x.example/{harness}/{trace_id}\"\n"), "/etc/harness/harness.toml")
	if err == nil || !strings.Contains(err.Error(), "{harness}") {
		t.Fatalf("err = %v, want a failure naming {harness}", err)
	}
}

// REQ-19 "A project file with a ledger table".
func TestLedgerIsRejectedInAProjectFile(t *testing.T) {
	_, err := ParseProject([]byte("[ledger]\ntrace_url = \"https://x.example/{trace_id}\"\n"), "/src/app/harness.toml")
	if err == nil || !strings.Contains(err.Error(), "configured globally") {
		t.Fatalf("err = %v, want a refusal saying the ledger is configured globally", err)
	}
}

// A harness_d drop-in refuses it too.
func TestLedgerIsRejectedInADropIn(t *testing.T) {
	cfg, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "/etc/harness/harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	err = parseHarnessDFile(cfg, newLoadState(), []byte("[ledger]\ntrace_url = \"https://x.example/{trace_id}\"\n"), "/etc/harness/harness.d/x.toml")
	if err == nil {
		t.Fatal("a drop-in carrying [ledger] loaded")
	}
}

// REQ-12, REQ-19: retention (at least 1d) and max_mb (at least 16), with
// defaults when absent.
func TestLedgerRetentionAndMaxMB(t *testing.T) {
	cfg, err := Parse([]byte("[ledger]\nretention = \"30d\"\nmax_mb = 64\n"), "/etc/harness/harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ledger.RetentionOrDefault().Hours() != 30*24 || cfg.Ledger.MaxMBOrDefault() != 64 {
		t.Errorf("ledger = %+v", cfg.Ledger)
	}
	def, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "/etc/harness/harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	if def.Ledger.RetentionOrDefault().Hours() != 90*24 || def.Ledger.MaxMBOrDefault() != 256 {
		t.Errorf("defaults = %v, %d; want 90d and 256", def.Ledger.RetentionOrDefault(), def.Ledger.MaxMBOrDefault())
	}
	for _, bad := range []string{"retention = \"12h\"", "retention = \"soon\"", "max_mb = 8"} {
		if _, err := Parse([]byte("[ledger]\n"+bad+"\n"), "/etc/harness/harness.toml"); err == nil {
			t.Errorf("%s loaded", bad)
		}
	}
}
