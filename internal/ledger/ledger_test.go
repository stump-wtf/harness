package ledger

// Tests read the day files, not the index, wherever the claim is about the
// ledger on disk: a fold of the Ledger's own memory would pass against a writer
// that never wrote anything.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func openT(t *testing.T, dir string, opts Options) *Ledger {
	t.Helper()
	l, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(5 * time.Second) })
	return l
}

func ptime(t time.Time) *time.Time { return &t }
func pint(i int) *int              { return &i }

// fileLines returns every line of every day file in dir, in file then line
// order, as raw maps: what is actually on disk.
func fileLines(t *testing.T, dir string) []map[string]any {
	t.Helper()
	names, err := listDays(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, n := range names {
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				continue
			}
			out = append(out, m)
		}
		_ = f.Close()
	}
	return out
}

func opened(h string, id int, at time.Time) Line {
	return Line{Type: TypeOpened, At: at, Harness: h, RunID: id, Record: Record{
		Kind: KindOneshot, Trigger: "schedule", Outcome: OutcomeRunning, StartedAt: ptime(at),
	}}
}

func closed(h string, id int, at time.Time, outcome string) Line {
	return Line{Type: TypeClosed, At: at, Harness: h, RunID: id, Record: Record{
		Outcome: outcome, EndedAt: ptime(at), ExitCode: pint(0),
	}}
}

// REQ-1 "A first run creates the ledger".
func TestFirstRunCreatesTheLedger(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := openT(t, dir, Options{})
	now := time.Now()
	if _, err := l.Append(opened("nightly", 1, now), true); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Errorf("ledger dir mode = %o, want 700", st.Mode().Perm())
	}
	file := filepath.Join(dir, dayName(now))
	fst, err := os.Stat(file)
	if err != nil {
		t.Fatalf("today's file: %v", err)
	}
	if fst.Mode().Perm() != 0o600 {
		t.Errorf("day file mode = %o, want 600", fst.Mode().Perm())
	}
	lines := fileLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("want 1 line on disk, got %d", len(lines))
	}
	first := lines[0]
	for _, k := range []string{"v", "seq", "type", "at", "harness", "run_id"} {
		if _, ok := first[k]; !ok {
			t.Errorf("line lacks envelope field %q: %v", k, first)
		}
	}
	if first["type"] != "opened" || first["seq"].(float64) != 1 || first["v"].(float64) != 1 {
		t.Errorf("first line = %v, want an opened line with seq 1 and v 1", first)
	}
	at, err := time.Parse(time.RFC3339Nano, first["at"].(string))
	if err != nil || at.Location() != time.UTC {
		t.Errorf("at %q is not RFC 3339 UTC: %v", first["at"], err)
	}
}

// REQ-1 "Sequence survives a restart".
func TestSequenceSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	// A ledger whose last line has seq 812.
	body := fmt.Sprintf(`{"v":1,"seq":811,"type":"opened","at":%q,"harness":"a","run_id":1}
{"v":1,"seq":812,"type":"closed","at":%q,"harness":"a","run_id":1,"outcome":"success"}
`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, dayName(now)), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l := openT(t, dir, Options{})
	seq, err := l.Append(opened("a", 2, now), true)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 813 {
		t.Errorf("seq after restart = %d, want 813", seq)
	}
	lines := fileLines(t, dir)
	if got := lines[len(lines)-1]["seq"].(float64); got != 813 {
		t.Errorf("seq on disk = %v, want 813", got)
	}
}

// The seq recovered at boot comes from the newest file even when it is outside
// the in-memory window.
func TestSequenceRecoveredFromAnOldLedger(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	l := openT(t, dir, Options{})
	for i := 1; i <= 3; i++ {
		if _, err := l.Append(opened("a", i, old), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	l2 := openT(t, dir, Options{})
	if seq, _ := l2.Append(opened("a", 4, time.Now()), true); seq != 4 {
		t.Errorf("seq = %d, want 4", seq)
	}
}

// REQ-2 "A run's three lines fold into one record", plus unknown fields.
func TestLinesFoldIntoOneRecord(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	l := openT(t, dir, Options{})
	mustAppend(t, l, opened("pr-review", 7, now), true)
	mustAppend(t, l, Line{Type: TypeUpdated, Harness: "pr-review", RunID: 7, Record: Record{Tokens: &Tokens{Input: 10, Output: 1}}}, false)
	mustAppend(t, l, Line{Type: TypeUpdated, Harness: "pr-review", RunID: 7, Record: Record{Tokens: &Tokens{Input: 40, Output: 9}}}, false)
	mustAppend(t, l, closed("pr-review", 7, now.Add(time.Minute), "failed"), true)
	if err := l.Close(time.Second); err != nil {
		t.Fatal(err)
	}

	// Reopen: the fold is rebuilt from the files, not remembered.
	l2 := openT(t, dir, Options{})
	recs, _, err := l2.Query(Query{Names: []string{"pr-review"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want one record, got %d: %+v", len(recs), recs)
	}
	r := recs[0]
	if r.Tokens == nil || r.Tokens.Input != 40 || r.Tokens.Output != 9 {
		t.Errorf("tokens = %+v, want the last checkpoint {40 9}", r.Tokens)
	}
	if r.Outcome != "failed" || r.Kind != KindOneshot || r.StartedAt == nil || r.EndedAt == nil {
		t.Errorf("record = %+v, want the closed outcome over the opened fields", r.Record)
	}
	if r.Open() || r.Partial() {
		t.Errorf("a closed record reads open=%v partial=%v", r.Open(), r.Partial())
	}
}

func TestUnknownFieldsAreIgnored(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	line := fmt.Sprintf(`{"v":2,"seq":1,"type":"decided","at":%q,"harness":"a","run_id":1,"outcome":"skipped","from_the_future":{"x":1}}`+"\n", now.Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, dayName(now)), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	l := openT(t, dir, Options{})
	if st := l.Stats(); st.Skipped != 0 {
		t.Errorf("a line with unknown fields was skipped")
	}
	if recs := l.Records("a"); len(recs) != 1 || recs[0].Outcome != "skipped" {
		t.Errorf("records = %+v", recs)
	}
}

// REQ-2: a closed line with no opened line is a partial record.
func TestClosedWithoutOpenedIsPartial(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	mustAppend(t, l, Line{Type: TypeUpdated, Harness: "a", RunID: 3, Record: Record{ModelCalls: 2}}, true)
	r := l.Records("a")
	if len(r) != 1 || !r[0].Partial() || r[0].Open() {
		t.Fatalf("an updated line alone should fold to a partial, not-open record: %+v", r)
	}
}

// REQ-2 "A torn last line": skipped and counted, the next append starts on a
// line of its own, and every complete line still folds.
func TestTornTailIsSkippedAndTheNextAppendStartsClean(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	complete := fmt.Sprintf(`{"v":1,"seq":1,"type":"opened","at":%q,"harness":"a","run_id":1,"outcome":"running"}`+"\n", now.Format(time.RFC3339Nano))
	torn := `{"v":1,"seq":2,"type":"closed","at":"` // killed mid-write
	file := filepath.Join(dir, dayName(now))
	if err := os.WriteFile(file, []byte(complete+torn), 0o600); err != nil {
		t.Fatal(err)
	}
	l := openT(t, dir, Options{})
	if st := l.Stats(); st.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 for the torn tail", st.Skipped)
	}
	if seq, err := l.Append(closed("a", 1, now, "success"), true); err != nil || seq != 2 {
		t.Fatalf("append after a torn tail: seq %d err %v", seq, err)
	}
	if err := l.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	if !strings.HasPrefix(string(raw), complete+torn+"\n{") {
		t.Errorf("the next line did not start on a line of its own:\n%s", raw)
	}
	lines := fileLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("want 2 parseable lines, got %d", len(lines))
	}
	l2 := openT(t, dir, Options{})
	recs := l2.Records("a")
	if len(recs) != 1 || recs[0].Outcome != "success" || !recs[0].HasOpened {
		t.Errorf("complete lines did not fold: %+v", recs)
	}
}

// REQ-4 caps: a 4 KiB todo id keeps its first 256 bytes and is counted.
func TestOversizedFieldsAreCutAndCounted(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	ln := opened("sb-drain", 3, time.Now())
	ln.TodoID = strings.Repeat("t", 4096)
	for i := range 20 {
		ln.Sessions = append(ln.Sessions, Session{ID: fmt.Sprint(i)})
	}
	mustAppend(t, l, ln, true)
	if err := l.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	lines := fileLines(t, dir)
	got := lines[0]["todo_id"].(string)
	if got != strings.Repeat("t", 256) {
		t.Errorf("todo_id on disk is %d bytes, want the first 256", len(got))
	}
	if n := len(lines[0]["sessions"].([]any)); n != MaxListEntries {
		t.Errorf("sessions on disk = %d, want %d", n, MaxListEntries)
	}
	if st := l.Stats(); st.Truncated != 2 {
		t.Errorf("truncated = %d, want 2", st.Truncated)
	}
}

// REQ-20 "Shutdown drains the queue": buffered updated lines and the closed
// lines behind them are all on disk when Close returns.
func TestShutdownDrainsTheQueue(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{FlushEvery: time.Hour})
	now := time.Now()
	mustAppend(t, l, opened("a", 1, now), true)
	for i := range 3 {
		mustAppend(t, l, Line{Type: TypeUpdated, Harness: "a", RunID: 1, Record: Record{ModelCalls: i + 1}}, false)
	}
	// Queued, not synced by anyone yet.
	if _, err := l.Append(closed("a", 1, now, "interrupted"), false); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(5 * time.Second); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(fileLines(t, dir)); n != 5 {
		t.Errorf("lines on disk after Close = %d, want 5", n)
	}
	if _, err := l.Append(opened("a", 2, now), true); !errors.Is(err, ErrClosed) {
		t.Errorf("append after close: %v, want ErrClosed", err)
	}
}

// REQ-6 "A read-only disk": the failure is counted, a synced append does not
// wait on the retry, and the queued lines land in order once the disk is back.
func TestFailingDiskQueuesInOrderAndDoesNotBlock(t *testing.T) {
	var mu sync.Mutex
	failing := true
	realWrite := writeFile
	writeFile = func(f *os.File, b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return 0, fs.ErrPermission
		}
		return realWrite(f, b)
	}
	t.Cleanup(func() { writeFile = realWrite })

	dir := t.TempDir()
	l, err := Open(dir, Options{RetryMin: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close(5 * time.Second) }()
	now := time.Now()

	if _, err := l.Append(opened("a", 1, now), true); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("first append on a failing disk: %v, want ErrLedgerUnavailable", err)
	}
	// Degraded now: a synced append must fail fast, not wait out SyncTimeout.
	start := time.Now()
	for i := 2; i <= 5; i++ {
		if _, err := l.Append(opened("a", i, now), true); !errors.Is(err, ErrLedgerUnavailable) {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("four appends on a degraded ledger took %s; supervision would have waited on the retry", took)
	}
	st := l.Stats()
	if st.AppendErrors == 0 || st.Queued != 5 || !st.Degraded {
		t.Errorf("stats = %+v, want errors counted and 5 lines queued", st)
	}
	// Committed before counted: nothing on disk, so no reader reports it.
	if recs := l.Records("a"); len(recs) != 0 {
		t.Errorf("readers see %d records whose lines are not on disk", len(recs))
	}
	if recs, _, err := l.Query(Query{Names: []string{"a"}}); err != nil || len(recs) != 0 {
		t.Errorf("Query sees %d records whose lines are not on disk (%v)", len(recs), err)
	}
	if f, ok, _ := l.Pending("a", 3); !ok || !f.Open() {
		t.Errorf("Pending, the writer's own view, lost a queued line: %+v", f)
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	if l.Stats().Degraded {
		t.Error("still degraded after the disk recovered")
	}
	if n := len(l.Records("a")); n != 5 {
		t.Errorf("after recovery readers see %d records, want 5", n)
	}
	lines := fileLines(t, dir)
	var seqs []float64
	for _, ln := range lines {
		seqs = append(seqs, ln["seq"].(float64))
	}
	if !slices.Equal(seqs, []float64{1, 2, 3, 4, 5}) {
		t.Errorf("seqs on disk = %v, want 1..5 in order", seqs)
	}
}

// A failed sync must not make the retry write the line a second time: REQ-1's
// seq never repeats.
func TestFailedSyncDoesNotDuplicateTheLine(t *testing.T) {
	var mu sync.Mutex
	fails := 2
	realSync := syncFileFn
	syncFileFn = func(f *os.File) error {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return fs.ErrPermission
		}
		return realSync(f)
	}
	t.Cleanup(func() { syncFileFn = realSync })

	dir := t.TempDir()
	l, err := Open(dir, Options{RetryMin: 5 * time.Millisecond, RetryMax: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(opened("a", 1, time.Now()), true); !errors.Is(err, ErrLedgerUnavailable) {
		t.Fatalf("append with a failing sync: %v", err)
	}
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	if err := l.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	if n := len(fileLines(t, dir)); n != 1 {
		t.Errorf("lines on disk = %d, want the line exactly once", n)
	}
}

// REQ-20: one writer, many callers, under -race: every seq unique, and the
// file in seq order.
func TestConcurrentAppendsKeepSeqOrder(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 50 {
				h := fmt.Sprintf("h%d", g)
				mustAppend(t, l, opened(h, i+1, time.Now()), i%5 == 0)
				_ = l.Records(h)
			}
		})
	}
	wg.Wait()
	if err := l.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	lines := fileLines(t, dir)
	if len(lines) != 400 {
		t.Fatalf("lines = %d, want 400", len(lines))
	}
	for i, ln := range lines {
		if got := ln["seq"].(float64); got != float64(i+1) {
			t.Fatalf("line %d has seq %v: the file is not in seq order", i, got)
		}
	}
}

func mustAppend(t *testing.T, l *Ledger, ln Line, sync bool) uint64 {
	t.Helper()
	seq, err := l.Append(ln, sync)
	if err != nil {
		t.Fatalf("append %s %s/%d: %v", ln.Type, ln.Harness, ln.RunID, err)
	}
	return seq
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
