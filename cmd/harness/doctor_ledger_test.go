package main

// Governing: SPEC-0021 REQ-4 Scenario "The ledger cannot be written" (doctor
// reports the ledger failure), REQ-7 Scenario "A lost ledger file" (doctor
// reports the gap), REQ-17 (each warning shown firing in a test).
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/protocol"
)

func TestRunLedgerCheck(t *testing.T) {
	if r := runLedgerCheck(nil); r != nil {
		t.Errorf("no daemon info: %+v, want no row", r)
	}
	if r := runLedgerCheck(&protocol.DaemonInfo{}); r != nil {
		t.Errorf("a healthy ledger (or an older daemon): %+v, want no row", r)
	}
	r := runLedgerCheck(&protocol.DaemonInfo{RunLedger: &protocol.RunLedgerInfo{
		Refused: 2, Unrecorded: 1, LastError: "ledger: unavailable: disk full", LastAt: "2026-10-04T12:00:00Z",
		Gaps: []protocol.RunLedgerGap{{Harness: "sweep", FirstMissing: 41, LastMissing: 45}},
	}})
	if r == nil || r.level != cliui.LevelWarn {
		t.Fatalf("row = %+v, want a warning", r)
	}
	for _, want := range []string{"2 budgeted start(s) refused", "1 start(s) ran with their record still queued", "disk full", "sweep is missing run records 41-45"} {
		if !strings.Contains(r.detail, want) {
			t.Errorf("detail %q is missing %q", r.detail, want)
		}
	}
}
