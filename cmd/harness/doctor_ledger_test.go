package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/protocol"
)

// ledgerDirWith makes a ledger directory holding one day file.
func ledgerDirWith(t *testing.T, dirMode, fileMode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ledger")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "2026-09-24.jsonl")
	if err := os.WriteFile(file, []byte(`{"v":1,"seq":1,"type":"decided","at":"2026-09-24T00:00:00Z","harness":"a","run_id":1}`+"\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	return dir
}

func rowNamed(rows []check, name string) (check, bool) {
	for _, r := range rows {
		if r.name == name {
			return r, true
		}
	}
	return check{}, false
}

// The control: a healthy ledger fires no warning, so each test below that
// sees one is seeing it fire, not seeing a check that always speaks.
func TestLedgerDoctorIsQuietWhenHealthy(t *testing.T) {
	dir := ledgerDirWith(t, 0o700, 0o600)
	rows := ledgerChecks(dir, &protocol.LedgerInfo{Dir: dir, MaxMB: 256, Imported: true, FeedDropped: map[string]uint64{"metrics": 0}}, 256)
	if len(rows) != 1 || rows[0].level != cliui.LevelSuccess {
		t.Fatalf("rows = %+v, want only the summary row", rows)
	}
	for _, want := range []string{dir, "oldest day 2026-09-24", "import done"} {
		if !strings.Contains(rows[0].detail, want) {
			t.Errorf("summary %q lacks %q", rows[0].detail, want)
		}
	}
}

// REQ-18 "A ledger with wrong permissions": a 0755 directory warns that the
// ledger is readable by other users and names the path; so does a loose file.
func TestLedgerDoctorWarnsOnLoosePermissions(t *testing.T) {
	dir := ledgerDirWith(t, 0o755, 0o644)
	rows := ledgerChecks(dir, nil, 256)
	r, ok := rowNamed(rows, "ledger_permissions")
	if !ok || r.level != cliui.LevelWarn || !strings.Contains(r.detail, "readable by other users") || !strings.Contains(r.detail, dir) ||
		!strings.Contains(r.detail, "2026-09-24.jsonl") {
		t.Fatalf("permissions row = %+v", r)
	}
}

// Every other warning, each from its own counter.
func TestLedgerDoctorWarningsFire(t *testing.T) {
	dir := ledgerDirWith(t, 0o700, 0o600)
	for name, info := range map[string]protocol.LedgerInfo{
		"ledger_writes": {AppendErrors: 3, Queued: 2, LastError: "sync: read-only file system"},
		"ledger_lines":  {Skipped: 1},
		"ledger_fields": {Truncated: 4},
		"ledger_feed":   {FeedDropped: map[string]uint64{"metrics": 12}},
		"ledger_usage":  {UsageDropped: 9, UsageIncomplete: 2},
	} {
		t.Run(name, func(t *testing.T) {
			info.Dir, info.MaxMB = dir, 256
			rows := ledgerChecks(dir, &info, 256)
			r, ok := rowNamed(rows, name)
			if !ok || r.level != cliui.LevelWarn || r.hint == "" {
				t.Fatalf("%s did not fire: %+v", name, rows)
			}
			if len(rows) != 2 {
				t.Errorf("one counter fired %d rows: %+v", len(rows)-1, rows)
			}
		})
	}
	// Size against max_mb: any bytes over a 0 MB cap.
	if _, ok := rowNamed(ledgerChecks(dir, nil, 0), "ledger_size"); !ok {
		t.Error("ledger_size did not fire over max_mb")
	}
}
