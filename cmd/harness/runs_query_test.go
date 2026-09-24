package main

// SPEC-0022 REQ-14 and REQ-16 for `harness runs` (#448).

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/stump-wtf/harness/internal/ledger"
	"github.com/stump-wtf/harness/internal/protocol"
)

// REQ-14 "An unknown outcome": the command fails before dialing, and the
// error lists the valid outcomes, timed_out among them.
func TestRunsRejectsAnUnknownOutcome(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"--socket", "/nonexistent/hnd-runs-test.sock", "runs", "--outcome", "timeout"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "timed_out") {
		t.Fatalf("err = %v, want one listing timed_out", err)
	}
}

// hashDir fingerprints every file under dir, names and contents.
func hashDir(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	var names []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s\x00%x\x00", n, sha256.Sum256(b))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// REQ-16 "After a crash": with no daemon, `harness runs nightly` reads the
// ledger itself, shows the run the crash left open as running?, warns on
// stderr, and leaves every ledger file byte-identical: no reconciliation, no
// pruning, no marker.
func TestRunsReadsTheLedgerOfflineWithoutTouchingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Hour)
	for id := 1; id <= 4; id++ {
		at := now.Add(time.Duration(id) * time.Minute)
		rec := ledger.Record{Kind: ledger.KindOneshot, Trigger: "schedule", Outcome: "running", StartedAt: &at}
		if _, err := l.Append(ledger.Line{Type: ledger.TypeOpened, At: at, Harness: "nightly", RunID: id, Record: rec}, true); err != nil {
			t.Fatal(err)
		}
		if id < 4 { // run 4 is the one the daemon died under
			end := at.Add(time.Second)
			if _, err := l.Append(ledger.Line{Type: ledger.TypeClosed, At: end, Harness: "nightly", RunID: id,
				Record: ledger.Record{Outcome: "success", EndedAt: &end, ExitCode: new(int)}}, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := l.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	prev := offlineLedgerDir
	offlineLedgerDir = func(verbOpts) string { return dir }
	t.Cleanup(func() { offlineLedgerDir = prev })
	before := hashDir(t, dir)

	// Under /tmp: a unix socket path is capped near 100 bytes, and t.TempDir
	// on macOS is longer than that, which fails the dial for the wrong reason.
	sockDir, err := os.MkdirTemp("/tmp", "hnr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	o := verbOpts{socket: filepath.Join(sockDir, "no-daemon.sock")}
	var out, errOut bytes.Buffer
	rq := runsRequest{names: []string{"nightly"}, legacy: true, limit: 20, now: time.Now()}
	if err := cmdRunsQuery(o, rq, &out, &errOut); err != nil {
		t.Fatalf("offline runs: %v", err)
	}
	if !strings.Contains(errOut.String(), "daemon not running; read the ledger from disk") {
		t.Errorf("stderr = %q, want the offline warning", errOut.String())
	}
	if !strings.Contains(out.String(), "running?") || strings.Count(out.String(), "success") != 3 {
		t.Errorf("table:\n%s\nwant run 4 as running? and three successes", out.String())
	}
	if after := hashDir(t, dir); after != before {
		t.Error("the offline read changed the ledger")
	}

	// The JSON form is SPEC-0008's object for one harness (back-compat).
	out.Reset()
	o.json = true
	if err := cmdRunsQuery(o, rq, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var rd protocol.RunsData
	if err := json.Unmarshal(out.Bytes(), &rd); err != nil || rd.Name != "nightly" || len(rd.Runs) != 4 || rd.Runs[0].RunID != 4 {
		t.Errorf("--json = %s (%v), want {name: nightly, runs: [4 3 2 1]}", out.String(), err)
	}
	// A query's JSON is the record list itself.
	out.Reset()
	q := runsRequest{limit: 50, since: now.Add(-time.Hour), now: time.Now()}
	if err := cmdRunsQuery(o, q, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var list []protocol.RunInfo
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list) != 4 || list[0].Harness != "nightly" {
		t.Errorf("query --json = %s (%v), want a list of 4 records naming their harness", out.String(), err)
	}
}

// REQ-14: the table fits an 80-column terminal, with its longest values in
// every cell.
func TestRunsTableFitsEightyColumns(t *testing.T) {
	start := time.Date(2026, 9, 24, 15, 4, 0, 0, time.UTC)
	rd := protocol.RunsData{Runs: []protocol.RunInfo{{
		Harness: "crush-switchboard-2-weekly-studio", RunID: 99999, Trigger: "autostart", Outcome: "model_unattested",
		StartedAt: start.Format(time.RFC3339Nano), EndedAt: start.Add(12*time.Hour + 30*time.Minute).Format(time.RFC3339Nano),
		DurationMs: (12*time.Hour + 30*time.Minute).Milliseconds(), ExitCode: new(int),
	}}}
	var buf bytes.Buffer
	tbl := NewTable(&buf, runsHeaders(false)...)
	tbl.width = ttyBudget
	for _, r := range rd.Runs {
		tbl.Row(r.Harness, fmt.Sprint(r.RunID), runStartedCell(r), runDurationCell(r), r.Trigger, runOutcomeCell(r), runExitCell(r))
	}
	if err := tbl.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Errorf("line is %d columns: %q", w, line)
		}
	}
	for _, want := range []string{"model_unattested", "autostart", "99999"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("an 80-column table cut %q:\n%s", want, buf.String())
		}
	}
}
