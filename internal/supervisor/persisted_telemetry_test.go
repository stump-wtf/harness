package supervisor

// Governing tests: SPEC-0014 REQ-2 — a project harness's telemetry opt-out
// survives a daemon restart.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"encoding/json"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

func TestPersistedProjectHarnessKeepsTelemetryOptOut(t *testing.T) {
	no := false
	h := core.Harness{Name: "proj/a", Adapter: "crush", Backend: core.BackendNative, Restart: core.RestartAlways, Quiet: true, ExportTelemetry: &no}
	raw, err := json.Marshal(toPersistedProjectHarness(h))
	if err != nil {
		t.Fatal(err)
	}
	var p persistedProjectHarness
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	back := p.toCore()
	if back.ExportTelemetry == nil || *back.ExportTelemetry {
		t.Fatalf("opt-out lost across persistence: %v (%s)", back.ExportTelemetry, raw)
	}
	if !harnessDefEqual(h, back) {
		t.Fatal("round-tripped definition differs")
	}
	h.ExportTelemetry = nil
	if harnessDefEqual(h, back) {
		t.Fatal("harnessDefEqual ignores export_telemetry, so a re-up changing only it is a no-op")
	}
}
