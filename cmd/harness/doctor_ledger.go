package main

// Doctor: The Run Ledger Admission Met
//
// SPEC-0021's admission writes every start's ledger record under its lock, so
// a ledger that cannot be written changes what the daemon starts: a budgeted
// harness is refused with ledger_unavailable, an unbudgeted one starts with
// its record still queued (REQ-4 Scenario "The ledger cannot be written").
// And boot rebuilds today's budget counters from the ledger, so records it
// lost undercount the day (REQ-7 Scenario "A lost ledger file"). Both are on
// the daemon log; this row is where an operator who did not read it finds
// out. The budget section proper (caps, counters, unmeasurable caps) is the
// doctor story's, stump.wtf/harness#486.
//
// Governing: ADR-0027; SPEC-0021 REQ-4, REQ-7, REQ-17.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"fmt"
	"strings"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/protocol"
)

// runLedgerCheck is the doctor row for what admission met of the run ledger,
// nil when the daemon reported nothing (a healthy ledger, or a daemon older
// than ProtoMinor 25).
func runLedgerCheck(di *protocol.DaemonInfo) *check {
	if di == nil || di.RunLedger == nil {
		return nil
	}
	rl := di.RunLedger
	var parts []string
	if rl.Refused > 0 {
		parts = append(parts, fmt.Sprintf("%d budgeted start(s) refused: the ledger could not record them", rl.Refused))
	}
	if rl.Unrecorded > 0 {
		parts = append(parts, fmt.Sprintf("%d start(s) ran with their record still queued", rl.Unrecorded))
	}
	if rl.LastError != "" {
		last := "last: " + rl.LastError
		if rl.LastAt != "" {
			last += " at " + rl.LastAt
		}
		parts = append(parts, last)
	}
	for _, g := range rl.Gaps {
		parts = append(parts, fmt.Sprintf("%s is missing run records %d-%d (today's budget counters were rebuilt without them)", g.Harness, g.FirstMissing, g.LastMissing))
	}
	return &check{
		name:   "run ledger",
		level:  cliui.LevelWarn,
		detail: strings.Join(parts, "; "),
		hint:   "check the disk under the daemon's state directory (ledger/); a deleted day file cannot be recovered",
	}
}
