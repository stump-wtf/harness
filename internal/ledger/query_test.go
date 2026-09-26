package ledger

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// REQ-15 "Paging": 2,500 records, three pages of 1,000, no duplicates and no
// gaps. Run once against the index and once against the files.
func TestQueryPagesWithoutGapsOrDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"memory", Options{}},
		// A one-record tail and a tiny window push every page to the files.
		{"files", Options{Window: time.Nanosecond, Tail: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			old := time.Now().Add(-48 * time.Hour)
			l := openT(t, dir, Options{})
			for i := 1; i <= 2500; i++ {
				ln := opened(fmt.Sprintf("h%d", i%3), i, old.Add(time.Duration(i)*time.Second))
				ln.Type, ln.Outcome = TypeDecided, "skipped"
				mustAppend(t, l, ln, i == 2500)
			}
			if err := l.Close(5 * time.Second); err != nil {
				t.Fatal(err)
			}
			l2 := openT(t, dir, tc.opts)
			seen := map[uint64]bool{}
			var before uint64
			var sizes []int
			for range 3 {
				recs, oldest, err := l2.Query(Query{Limit: 1000, BeforeSeq: before})
				if err != nil {
					t.Fatal(err)
				}
				sizes = append(sizes, len(recs))
				for i, r := range recs {
					if seen[r.Seq] {
						t.Fatalf("seq %d returned twice", r.Seq)
					}
					seen[r.Seq] = true
					if i > 0 && r.Seq >= recs[i-1].Seq {
						t.Fatalf("page not newest first at %d", i)
					}
				}
				before = oldest
			}
			if fmt.Sprint(sizes) != "[1000 1000 500]" || len(seen) != 2500 {
				t.Errorf("pages = %v covering %d records, want [1000 1000 500] covering 2500", sizes, len(seen))
			}
		})
	}
}

// The index answers from memory only when its floors prove the answer whole.
// Here the window is one day and the tail two records, so a query for the
// newest five of a harness must read the files, and must get the true five.
func TestQueryBeyondTheIndexReadsTheFiles(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	base := time.Now().Add(-30 * 24 * time.Hour)
	for i := 1; i <= 10; i++ {
		at := base.Add(time.Duration(i) * 24 * time.Hour)
		mustAppend(t, l, opened("weekly", i, at), false)
		mustAppend(t, l, closed("weekly", i, at.Add(time.Minute), "success"), i == 10)
	}
	if err := l.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	l2 := openT(t, dir, Options{Window: 24 * time.Hour, Tail: 2})
	if err := l2.Backfill([]string{"weekly"}); err != nil {
		t.Fatal(err)
	}
	if n := len(l2.Records("weekly")); n < 2 {
		t.Fatalf("backfill left %d records in memory, want at least the tail of 2", n)
	}
	recs, _, err := l2.Query(Query{Names: []string{"weekly"}, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int
	for _, r := range recs {
		ids = append(ids, r.RunID)
		if !r.HasOpened || r.Outcome != "success" {
			t.Errorf("run %d folded partially: %+v", r.RunID, r)
		}
	}
	if fmt.Sprint(ids) != "[10 9 8 7 6]" {
		t.Errorf("newest five = %v, want [10 9 8 7 6]", ids)
	}
	if got := l2.MaxRunID("weekly"); got < 9 {
		t.Errorf("MaxRunID = %d after backfill", got)
	}
}

// Backfill finds a run left open before the window, so boot can reconcile a
// resident that was up for a fortnight when the daemon died.
func TestBackfillFindsAnOldOpenRecord(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	mustAppend(t, l, opened("resident", 1, time.Now().Add(-14*24*time.Hour)), true)
	// Something recent, so the old file is not also the newest (which boot
	// always reads, for the seq).
	mustAppend(t, l, closed("other", 1, time.Now(), "success"), true)
	if err := l.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	l2 := openT(t, dir, Options{})
	if n := len(l2.OpenRecords()); n != 0 {
		t.Fatalf("the boot scan should not reach a 14-day-old file; found %d open", n)
	}
	if err := l2.Backfill([]string{"resident"}); err != nil {
		t.Fatal(err)
	}
	open := l2.OpenRecords()
	if len(open) != 1 || open[0].Harness != "resident" || open[0].RunID != 1 {
		t.Errorf("open records after backfill = %+v", open)
	}
}

// REQ-12's read side: a record whose log file is gone reads log_pruned.
func TestLogPrunedIsReadFromTheFile(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(t.TempDir(), "1.log")
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l := openT(t, dir, Options{})
	ln := opened("a", 1, time.Now())
	ln.Log = log
	mustAppend(t, l, ln, true)
	if r, _, _ := l.Get("a", 1); r.LogPruned {
		t.Error("log_pruned set while the log exists")
	}
	_ = os.Remove(log)
	if r, ok, _ := l.Get("a", 1); !ok || !r.LogPruned {
		t.Errorf("log_pruned not set once the log is gone: %+v", r)
	}
	if lines := fileLines(t, dir); lines[0]["log_pruned"] != nil {
		t.Error("log_pruned was written to disk; it is read, never written")
	}
}

// REQ-13: the import runs once, lands each line in the day of its own time,
// and marks every record imported.
func TestImportRunsOnceIntoTheRightDays(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	if l.Imported() {
		t.Fatal("a fresh ledger reads imported")
	}
	day1 := time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	var lines []Line
	for i, at := range []time.Time{day2, day1} {
		lines = append(lines, opened("nightly", i+1, at), closed("nightly", i+1, at.Add(time.Minute), "success"))
	}
	if err := l.Import(lines); err != nil {
		t.Fatal(err)
	}
	if !l.Imported() {
		t.Error("marker not recorded")
	}
	if _, err := os.Stat(filepath.Join(dir, importedMarker)); err != nil {
		t.Errorf("marker file: %v", err)
	}
	for _, d := range []time.Time{day1, day2} {
		if _, err := os.Stat(filepath.Join(dir, dayName(d))); err != nil {
			t.Errorf("no day file for %s: %v", d.Format(time.DateOnly), err)
		}
	}
	for _, ln := range fileLines(t, dir) {
		if ln["imported"] != true {
			t.Errorf("line not marked imported: %v", ln)
		}
	}
	if err := l.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	if l2 := openT(t, dir, Options{}); !l2.Imported() {
		t.Error("marker not seen on the next boot")
	}
}

// crashChildEnv makes the test binary a child that appends to a ledger until
// it is killed.
const crashChildEnv = "HARNESS_LEDGER_CRASH_CHILD"

func TestMain(m *testing.M) {
	// Before the crash child too: a SIGKILL leaves the page cache alone, so
	// no property here needs the disk, and a child whose sync ran past
	// SyncTimeout on a loaded runner exited and failed its parent.
	SkipSyncForTesting()
	if dir := os.Getenv(crashChildEnv); dir != "" {
		crashChild(dir)
		return
	}
	os.Exit(m.Run())
}

// crashChild appends runs forever: a synced opened line, then (after saying so
// on stdout) buffered updates and a synced close, as fast as it can.
func crashChild(dir string) {
	l, err := Open(dir, Options{})
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(2)
	}
	for id := 1; ; id++ {
		if _, err := l.Append(opened("crash", id, time.Now()), true); err != nil {
			fmt.Println("append:", err)
			os.Exit(2)
		}
		fmt.Println("opened", id)
		for i := range 5 {
			_, _ = l.Append(Line{Type: TypeUpdated, Harness: "crash", RunID: id, Record: Record{ModelCalls: i + 1}}, false)
		}
		_, _ = l.Append(closed("crash", id, time.Now(), "success"), true)
	}
}

// A daemon SIGKILLed mid-write: every line it reported synced is on disk, at
// most one torn line is skipped, the seq continues, and the next boot's fold
// sees every complete record. The process really dies, so nothing here can be
// a flush a clean exit would have done.
func TestKilledMidWriteLosesNoSyncedLine(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	for round := range 3 {
		dir := t.TempDir()
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), crashChildEnv+"="+dir)
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Let it get some way in, then kill it wherever it happens to be.
		lastReported := 0
		buf := make([]byte, 4096)
		deadline := time.Now().Add(time.Duration(50+round*40) * time.Millisecond)
		var pending string
		for time.Now().Before(deadline) || lastReported == 0 {
			n, rerr := out.Read(buf)
			pending += string(buf[:n])
			for {
				i := strings.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				var id int
				if _, err := fmt.Sscanf(pending[:i], "opened %d", &id); err == nil {
					lastReported = id
				}
				pending = pending[i+1:]
			}
			if rerr != nil {
				t.Fatalf("child stdout: %v (last %q)", rerr, pending)
			}
		}
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()

		l := openT(t, dir, Options{})
		st := l.Stats()
		if st.Skipped > 1 {
			t.Errorf("round %d: %d lines skipped; a kill tears at most the last one", round, st.Skipped)
		}
		for id := 1; id <= lastReported; id++ {
			r, ok, err := l.Get("crash", id)
			if err != nil || !ok || !r.HasOpened {
				t.Fatalf("round %d: run %d was reported synced but its opened line is not on disk (%v)", round, id, err)
			}
		}
		seq, err := l.Append(opened("after", 1, time.Now()), true)
		if err != nil {
			t.Fatal(err)
		}
		if lines := fileLines(t, dir); uint64(len(lines)) != seq {
			t.Errorf("round %d: %d parseable lines but the next seq is %d: a seq was skipped or repeated", round, len(lines), seq)
		}
		t.Logf("round %d: killed after run %s, %d lines, %d skipped", round, strconv.Itoa(lastReported), seq, st.Skipped)
	}
}
