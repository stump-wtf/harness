package main

// Doctor: The Run Ledger
//
// The ledger's health, from the directory on disk (so it shows with the daemon
// down) and, when the daemon answered, its live counters: permissions, size
// against max_mb, the oldest day kept, append errors and queued lines,
// skipped lines, cut fields, feed and accumulator drops, and the first-boot
// import (SPEC-0022 REQ-18). Every warning is a row of its own, so a script
// reading --json sees which one fired.
//
// Governing: SPEC-0022 REQ-18; ADR-0028.
//
// @joestump 09/24/2026 - Added for harness#463.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// doctorLedgerDir is the ledger a default daemon writes, beside state.json.
// A variable so a test can point it at a temp directory.
var doctorLedgerDir = func() string {
	return filepath.Join(filepath.Dir(supervisor.DefaultStatePath()), "ledger")
}

// appendLedgerChecks adds the ledger rows. info is the daemon's report, nil
// when the daemon is down or predates it; cfg supplies max_mb then.
func appendLedgerChecks(rows []check, info *protocol.LedgerInfo, cfg *core.Config) []check {
	dir := doctorLedgerDir()
	maxMB := core.DefaultLedgerMaxMB
	if cfg != nil {
		maxMB = cfg.Ledger.MaxMBOrDefault()
	}
	if info != nil {
		dir, maxMB = info.Dir, info.MaxMB
	}
	return append(rows, ledgerChecks(dir, info, maxMB)...)
}

// ledgerChecks is the ledger section: a summary row, then one row per
// warning that fires.
func ledgerChecks(dir string, info *protocol.LedgerInfo, maxMB int) []check {
	st, err := os.Stat(dir)
	if err != nil {
		return []check{{name: "ledger", level: cliui.LevelSuccess, detail: fmt.Sprintf("not created yet at %s (the daemon creates it on first start)", dir)}}
	}
	var bytes int64
	var loose []string
	if st.Mode().Perm()&0o077 != 0 {
		loose = append(loose, fmt.Sprintf("%s (%o)", dir, st.Mode().Perm()))
	}
	entries, _ := os.ReadDir(dir)
	var days []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		days = append(days, strings.TrimSuffix(e.Name(), ".jsonl"))
		if fi, err := e.Info(); err == nil {
			bytes += fi.Size()
			if fi.Mode().Perm()&0o077 != 0 {
				loose = append(loose, fmt.Sprintf("%s (%o)", filepath.Join(dir, e.Name()), fi.Mode().Perm()))
			}
		}
	}
	slices.Sort(days)
	oldest := "none"
	if len(days) > 0 {
		oldest = days[0]
	}
	imported := "import status unknown (daemon not reachable)"
	if info != nil {
		imported = "first-boot import pending"
		if info.Imported {
			imported = "first-boot import done"
		}
	} else if _, err := os.Stat(filepath.Join(dir, ".imported")); err == nil {
		imported = "first-boot import done"
	}
	rows := []check{{
		name:  "ledger",
		level: cliui.LevelSuccess,
		detail: fmt.Sprintf("%s — %s of %d MB, oldest day %s, %s",
			dir, humanBytes(bytes), maxMB, oldest, imported),
	}}
	warn := func(name, detail, hint string) {
		rows = append(rows, check{name: name, level: cliui.LevelWarn, detail: detail, hint: hint})
	}

	if len(loose) > 0 {
		warn("ledger_permissions", "the run ledger is readable by other users: "+strings.Join(loose, ", "),
			fmt.Sprintf("chmod 700 %s && chmod 600 %s/*.jsonl", dir, dir))
	}
	if limit := int64(maxMB) << 20; bytes > limit {
		warn("ledger_size", fmt.Sprintf("%s is over max_mb = %d; today's file is never pruned, so it may be one busy day", humanBytes(bytes), maxMB),
			"raise [ledger] max_mb, or look for a resident crash-looping")
	}
	if info == nil {
		return rows
	}
	if info.AppendErrors > 0 || info.Queued > 0 {
		d := fmt.Sprintf("%d failed writes, %d lines queued for retry", info.AppendErrors, info.Queued)
		if info.LastError != "" {
			d += "; last: " + info.LastError
		}
		warn("ledger_writes", d, "check the disk under "+info.Dir+" (full, read-only, or failing)")
	}
	if info.Skipped > 0 {
		warn("ledger_lines", fmt.Sprintf("%d unparseable lines skipped (a torn write or a damaged file)", info.Skipped),
			"the records around them still fold; a count that grows means the disk is losing writes")
	}
	if info.Truncated > 0 {
		warn("ledger_fields", fmt.Sprintf("%d record fields cut to their caps (256 bytes, 16 entries)", info.Truncated),
			"an oversized todo id or model name was recorded shortened")
	}
	var drops []string
	for sub, n := range info.FeedDropped {
		if n > 0 {
			drops = append(drops, fmt.Sprintf("%s missed %d", sub, n))
		}
	}
	slices.Sort(drops)
	if len(drops) > 0 {
		warn("ledger_feed", "run feed subscribers fell behind: "+strings.Join(drops, ", "),
			"counts fed by them read low; harness runs reads the ledger itself and is complete")
	}
	if info.UsageDropped > 0 {
		warn("ledger_usage", fmt.Sprintf("the observer dropped %d items for the usage accumulator; %d runs read usage_complete: false", info.UsageDropped, info.UsageIncomplete),
			"those runs' model calls, errors and sessions are undercounted")
	}
	return rows
}

// humanBytes renders a size in the largest whole unit.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
