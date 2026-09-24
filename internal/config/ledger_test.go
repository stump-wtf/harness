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
