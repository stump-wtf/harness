package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedDays writes one closed run per day for the last n days (day 0 is today),
// then closes the ledger.
func seedDays(t *testing.T, dir string, now time.Time, n int) {
	t.Helper()
	l := openT(t, dir, Options{})
	for d := n - 1; d >= 0; d-- {
		at := now.Add(-time.Duration(d) * 24 * time.Hour)
		mustAppend(t, l, opened("daily", n-d, at), false)
		mustAppend(t, l, closed("daily", n-d, at.Add(time.Minute), "success"), d == 0)
	}
	if err := l.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func dayFiles(t *testing.T, dir string) []string {
	t.Helper()
	names, err := listDays(dir)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func waitForFiles(t *testing.T, dir string, pred func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		names := dayFiles(t, dir)
		if pred(names) {
			return names
		}
		if time.Now().After(deadline) {
			t.Fatalf("day files never matched: %v", names)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// REQ-12 "Old files go": retention 30d and files back 45 days; the prune
// deletes those older than 30 days and leaves the rest byte-intact.
func TestRetentionDeletesOldDayFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	seedDays(t, dir, now, 45)
	keep := map[string][]byte{}
	for _, n := range dayFiles(t, dir) {
		d, _ := dayOf(n)
		if !d.Add(24 * time.Hour).Before(now.Add(-30 * 24 * time.Hour)) {
			b, _ := os.ReadFile(filepath.Join(dir, n))
			keep[n] = b
		}
	}
	l := openT(t, dir, Options{Retention: 30 * 24 * time.Hour})
	l.EnablePrune()
	names := waitForFiles(t, dir, func(n []string) bool { return len(n) == len(keep) })
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil || string(b) != string(keep[n]) {
			t.Errorf("%s was deleted or changed", n)
		}
	}
	if st := l.Stats(); st.Pruned != uint64(45-len(keep)) {
		t.Errorf("pruned = %d, want %d", st.Pruned, 45-len(keep))
	}
	if od := l.OldestDay(); od.Before(now.Add(-31 * 24 * time.Hour)) {
		t.Errorf("oldest day kept = %s", od)
	}
}

// REQ-12 "A resident open across the prune": the run opened 100 days ago and
// still running gets a fresh opened line in today's file before its file is
// deleted, and still folds into a record afterwards, from the files alone.
func TestPruneCarriesAnOpenRunForward(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	l := openT(t, dir, Options{})
	ln := opened("resident", 7, old)
	ln.Kind, ln.Trigger = KindResident, "autostart"
	mustAppend(t, l, ln, true)
	mustAppend(t, l, closed("other", 1, now, "success"), true)
	if err := l.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	oldFile := dayName(old)

	l2 := openT(t, dir, Options{Retention: 90 * 24 * time.Hour})
	if err := l2.Backfill([]string{"resident", "other"}); err != nil { // as the Manager's boot does
		t.Fatal(err)
	}
	l2.EnablePrune()
	waitForFiles(t, dir, func(n []string) bool { return !strings.Contains(strings.Join(n, " "), oldFile) })
	if st := l2.Stats(); st.CarriedForward != 1 {
		t.Errorf("carried forward = %d, want 1", st.CarriedForward)
	}
	if err := l2.Close(time.Second); err != nil {
		t.Fatal(err)
	}

	l3, err := OpenReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, ok, err := l3.Get("resident", 7)
	if err != nil || !ok || !r.Open() || r.Kind != KindResident || r.StartedAt == nil || !r.StartedAt.Equal(old) {
		t.Errorf("after the prune, run 7 = %+v (ok %v, err %v); want it still open with its original start", r, ok, err)
	}
}

// The size cap removes the oldest files until the total fits, never today's.
func TestSizeCapRemovesOldestButNeverToday(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	seedDays(t, dir, now, 5)
	l := openT(t, dir, Options{MaxBytes: 1}) // nothing fits
	l.EnablePrune()
	names := waitForFiles(t, dir, func(n []string) bool { return len(n) == 1 })
	if names[0] != dayName(now) {
		t.Errorf("kept %v, want only today's file", names)
	}
}

// REQ-19: a reload's retention applies at the next prune (the next UTC
// rollover), not at once and not only after a reopen.
func TestReloadedRetentionAppliesAtTheNextPrune(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	seedDays(t, dir, now, 20)
	clk := &usageClock{t: now}
	l := openT(t, dir, Options{Now: clk.Now})
	l.EnablePrune()
	mustAppend(t, l, opened("tick", 1, now), true) // let the first prune run
	time.Sleep(50 * time.Millisecond)
	if n := len(dayFiles(t, dir)); n != 20 {
		t.Fatalf("the default 90d retention deleted files: %d left", n)
	}

	l.SetRetention(10*24*time.Hour, 0)
	mustAppend(t, l, opened("tick", 2, now), true)
	time.Sleep(50 * time.Millisecond)
	if n := len(dayFiles(t, dir)); n != 20 {
		t.Fatalf("the new retention applied before the next prune: %d left", n)
	}

	clk.Add(24 * time.Hour) // the rollover
	mustAppend(t, l, opened("tick", 3, clk.Now()), true)
	names := waitForFiles(t, dir, func(n []string) bool { return len(n) < 20 })
	for _, n := range names {
		d, _ := dayOf(n)
		if d.Add(24 * time.Hour).Before(clk.Now().Add(-10 * 24 * time.Hour)) {
			t.Errorf("%s is past the 10d retention and survived", n)
		}
	}
}
