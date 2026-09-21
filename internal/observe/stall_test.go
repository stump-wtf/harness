package observe

// Orphaned Tool Call Fallback Tests
//
// Every test here runs the real crush adapter over a real crush store
// (runtracetest), resolved through runtrace.Sources as the daemon resolves it.
// The first test also shows agent-trace's own ParseSince pinned on the same
// store, so that its passing half is a test of the fallback and not of a
// fixture that happens not to stall.
//
// @joestump-agent 09/21/2026 - Added with the orphaned-tool-call fallback.

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/runtrace"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
)

// crushCall is an assistant row issuing one tool call, and nothing else: the
// row a crush killed mid-call leaves behind.
func crushCall(id, name string, input map[string]any, at time.Time) rt.CrushMessage {
	return rt.CrushMessage{Role: "assistant", At: at, Parts: rt.ToolCall(id, name, input)}
}

// crushResult is the tool row answering id.
func crushResult(id string, at time.Time) rt.CrushMessage {
	return rt.CrushMessage{Role: "tool", At: at, Parts: rt.ToolResult(id, "ok")}
}

// crushGrep is a completed grep call, distinguishable from crushRead's view.
func crushGrep(id, pattern string, at time.Time) []rt.CrushMessage {
	return []rt.CrushMessage{
		crushCall(id, "grep", map[string]any{"pattern": pattern}, at),
		crushResult(id, at),
	}
}

// countingCrush is the production crush adapter with its full Parse counted.
// It embeds the adapter, so every other method — ParseSince, the filtered
// listing, the summary cache hook — is the real one.
type countingCrush struct {
	*tail.CrushAdapter
	parses *atomic.Int64
}

func (c countingCrush) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, tail.SessionMeta, error) {
	c.parses.Add(1)
	return c.CrushAdapter.Parse(ctx, path)
}

// countParses wraps every crush adapter runtrace.Sources resolves.
func countParses(parses *atomic.Int64) func(runtrace.Scope) ([]tail.Adapter, error) {
	return func(s runtrace.Scope) ([]tail.Adapter, error) {
		as, err := runtrace.Sources(s)
		for i, a := range as {
			if c, ok := a.(*tail.CrushAdapter); ok {
				as[i] = countingCrush{c, parses}
			}
		}
		return as, err
	}
}

// orphanedSession writes the bug's shape up to the kill: a user message, a
// completed view, then a grep call whose result never comes because crush died.
func orphanedSession(t *testing.T, f *fixture) {
	t.Helper()
	msgs := []rt.CrushMessage{userSays("go", start.Add(time.Second))}
	msgs = append(msgs, crushRead("c1", start.Add(2*time.Second))...)
	msgs = append(msgs, crushCall("orphan", "grep", map[string]any{"pattern": "never answered"}, start.Add(3*time.Second)))
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start, Updated: start.Add(3 * time.Second), Messages: msgs})
}

// resume appends what a resumed crush writes past the orphan during an
// outage: the resume prompt, a provider error, and a call that did complete.
func resume(t *testing.T, f *fixture) {
	t.Helper()
	msgs := []rt.CrushMessage{
		userSays("resume", start.Add(20*time.Second)),
		providerError("429 from the provider", start.Add(21*time.Second)),
	}
	msgs = append(msgs, crushGrep("c3", "after the orphan", start.Add(22*time.Second))...)
	rt.AppendCrushMessages(t, f.crushDB(), "s", msgs...)
}

// TestOrphanedToolCallDoesNotStallSession is the reviewer's reproduction:
// crush killed mid-call, then resumed into the same session. Everything after
// the orphan must be delivered once, in order, with the seq a full Parse gives
// it; the orphan itself is not delivered.
func TestOrphanedToolCallDoesNotStallSession(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	orphanedSession(t, f)
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	f.tick(start.Add(5 * time.Second))
	if got, want := describe(drain(ch)), []string{"worker:mark:user-message@0", "worker:tool:view@0"}; !equal(got, want) {
		t.Fatalf("before the kill delivered %v, want %v", got, want)
	}
	st := f.obs.sessions["crush/s"]
	if st == nil {
		t.Fatal("session not tracked")
	}
	pinned := st.watermark

	resume(t, f)

	// The premise: agent-trace alone stays pinned on this store, although a
	// full Parse holds everything the resumed session wrote.
	a := &tail.CrushAdapter{DBPath: f.crushDB(), Cwd: f.work}
	path := f.crushDB() + "/s"
	ev, mk, _, wm, err := a.ParseSince(context.Background(), path, pinned, st.nextSeq)
	if err != nil || wm != pinned || len(ev)+len(mk) != 0 {
		t.Fatalf("ParseSince from the pinned watermark = %d events, %d marks, wm %d (was %d), err %v; the premise (an orphan pins it) no longer holds — the upstream fix may have landed", len(ev), len(mk), wm, pinned, err)
	}
	fullEvents, fullMarks, _, err := a.Parse(context.Background(), path)
	if err != nil || len(fullMarks) != 3 {
		t.Fatalf("full Parse: %d marks, err %v; want the resumed marks present", len(fullMarks), err)
	}

	f.tick(start.Add(25 * time.Second))
	got := drain(ch)
	want := []string{"worker:mark:user-message@1", "worker:mark:error@1", "worker:tool:grep@1"}
	if !equal(describe(got), want) {
		t.Fatalf("after resuming delivered %v, want %v (stats %+v)", describe(got), want, f.obs.Stats())
	}
	// The same number a full Parse gives the completed grep, and a different
	// call from the orphan (which Parse flushes last, with no result).
	grep := got[2].Tool
	var fromParse *classify.Event
	for i := range fullEvents {
		if fullEvents[i].Summary == grep.Summary && fullEvents[i].Seq == grep.Seq {
			fromParse = &fullEvents[i]
		}
	}
	if fromParse == nil {
		t.Fatalf("delivered grep %q@%d matches no event of the full Parse %+v", grep.Summary, grep.Seq, fullEvents)
	}
	if last := fullEvents[len(fullEvents)-1]; last.Seq == grep.Seq {
		t.Fatalf("delivered event carries the orphan's flushed seq %d", last.Seq)
	}

	// Exactly once, and the session is unpinned: a later call arrives through
	// the ordinary incremental read with the next positional seq.
	f.tick(start.Add(30 * time.Second))
	if got := describe(drain(ch)); len(got) != 0 {
		t.Fatalf("an idle tick redelivered %v", got)
	}
	rt.AppendCrushMessages(t, f.crushDB(), "s", crushRead("c4", start.Add(40*time.Second))...)
	f.tick(start.Add(45 * time.Second))
	if got, want := describe(drain(ch)), []string{"worker:tool:view@2"}; !equal(got, want) {
		t.Fatalf("after recovery delivered %v, want %v", got, want)
	}
	if s := f.obs.Stats(); s.Stalls != 1 || s.OrphansSkipped != 1 || s.StallChecks != 1 {
		t.Errorf("stats = %+v, want one check, one stall, one orphan skipped", s)
	}
}

// TestHealthyPathNeverFullParses: sessions that never orphan a call — idle
// ticks, a row crush is still streaming into, a tool call open across many
// ticks, discovery itself — cost no full Parse. The last step orphans a call
// and resumes, which must parse: the counter can fire, so its zero is real.
func TestHealthyPathNeverFullParses(t *testing.T) {
	var parses atomic.Int64
	f := newFixture(t, func(o *Options) { o.Sources = countParses(&parses) })
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	msgs := []rt.CrushMessage{userSays("go", start.Add(time.Second))}
	msgs = append(msgs, crushRead("c1", start.Add(2*time.Second))...)
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start, Updated: start.Add(2 * time.Second), Messages: msgs})
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	at := start.Add(5 * time.Second)
	step := func(d time.Duration) {
		at = at.Add(d)
		f.tick(at)
	}
	step(0)
	step(5 * time.Second) // idle
	// A turn begins: crush inserts the assistant row and streams into it.
	rt.AppendCrushMessages(t, f.crushDB(), "s", rt.CrushMessage{Role: "assistant", At: at, Parts: `[{"type":"text","data":{"text":"thinking"}}]`})
	step(5 * time.Second)
	// The stream settles on a tool call that then runs for minutes.
	rt.RewriteLastCrushMessage(t, f.crushDB(), "s", rt.ToolCall("long", "bash", map[string]any{"command": "make test"}))
	for i := 0; i < 20; i++ {
		step(40 * time.Second) // past StallCheckInterval every time
	}
	rt.AppendCrushMessages(t, f.crushDB(), "s", crushResult("long", at))
	step(5 * time.Second)
	if got, want := describe(drain(ch)), []string{"worker:mark:user-message@0", "worker:tool:view@0", "worker:tool:bash@1"}; !equal(got, want) {
		t.Fatalf("healthy session delivered %v, want %v", got, want)
	}
	if n := parses.Load(); n != 0 {
		t.Fatalf("healthy path ran %d full parses (stats %+v)", n, f.obs.Stats())
	}
	if s := f.obs.Stats(); s.StallChecks != 0 || s.Stalls != 0 {
		t.Fatalf("healthy path stats = %+v", s)
	}

	// Now orphan a call and resume past it: the seam must see a parse.
	rt.AppendCrushMessages(t, f.crushDB(), "s", crushCall("orphan", "grep", map[string]any{"pattern": "x"}, at))
	step(5 * time.Second)
	rt.AppendCrushMessages(t, f.crushDB(), "s", userSays("resume", at.Add(10*time.Second)), providerError("503", at.Add(11*time.Second)))
	step(15 * time.Second)
	if got, want := describe(drain(ch)), []string{"worker:mark:user-message@2", "worker:mark:error@2"}; !equal(got, want) {
		t.Fatalf("after the orphan delivered %v, want %v", got, want)
	}
	if n := parses.Load(); n != 1 {
		t.Fatalf("recovery ran %d full parses through the seam, want 1", n)
	}
}

// TestStallCheckIsRateLimited: a session that keeps writing past an orphan it
// cannot yet be proven stalled behind (no mark newer than what was read) is
// fully parsed at most once per StallCheckInterval, however often it writes.
func TestStallCheckIsRateLimited(t *testing.T) {
	var parses atomic.Int64
	f := newFixture(t, func(o *Options) { o.Sources = countParses(&parses) })
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	orphanedSession(t, f)
	f.tick(start.Add(5 * time.Second))
	// Rows with no marks after the orphan: a suspect that no check confirms.
	at := start.Add(5 * time.Second)
	for i := 0; i < 12; i++ {
		at = at.Add(5 * time.Second)
		rt.AppendCrushMessages(t, f.crushDB(), "s", crushCall("x"+string(rune('a'+i)), "view", map[string]any{"file_path": "a"}, at))
		f.tick(at)
	}
	// Writes every 5s from +10s to +65s at a 30s interval: checks at +10s
	// and +40s only.
	if n := parses.Load(); n != 2 {
		t.Fatalf("%d full parses in 55s of writes, want 2 at a 30s interval", n)
	}
	if s := f.obs.Stats(); s.Stalls != 0 || s.OrphansSkipped != 0 {
		t.Fatalf("stats = %+v: skipped records with no mark to prove a stall", s)
	}
}

// TestStallRecoverySurvivesDaemonRestart: a new observer over the same store —
// the daemon restarted — finds the same orphan, recovers past it without
// replaying what the previous one delivered (it predates the new daemon), and
// delivers what was written after it started, including an error mark that
// landed before its first scan and nothing after, with the same seq numbering.
func TestStallRecoverySurvivesDaemonRestart(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	orphanedSession(t, f)
	ch, cancel := f.obs.Subscribe("first", 64)
	f.tick(start.Add(5 * time.Second))
	resume(t, f)
	f.tick(start.Add(25 * time.Second))
	if got, want := describe(drain(ch)), []string{
		"worker:mark:user-message@0", "worker:tool:view@0",
		"worker:mark:user-message@1", "worker:mark:error@1", "worker:tool:grep@1",
	}; !equal(got, want) {
		t.Fatalf("first daemon delivered %v, want %v", got, want)
	}
	cancel()
	f.obs.Stop()

	// The second daemon starts at +60s; crush writes one more error at +62s,
	// before the new observer's first scan, and then nothing.
	restart := start.Add(60 * time.Second)
	rt.AppendCrushMessages(t, f.crushDB(), "s", providerError("still 429", start.Add(62*time.Second)))
	f.clock.Set(restart)
	second := New(f.src, Options{
		Now:          f.clock.Now,
		Since:        restart,
		DaemonDir:    f.home,
		DiscoveryEnv: func(core.Harness) map[string]string { return map[string]string{"HOME": f.home} },
	})
	t.Cleanup(second.Stop)
	ch2, cancel2 := second.Subscribe("second", 64)
	defer cancel2()
	tick := func(at time.Time) {
		f.clock.Set(at)
		second.scan(context.Background())
	}
	tick(restart.Add(5 * time.Second))
	tick(restart.Add(10 * time.Second))
	if got, want := describe(drain(ch2)), []string{"worker:mark:error@2"}; !equal(got, want) {
		t.Fatalf("second daemon delivered %v, want only the error written after it started (stats %+v)", got, second.Stats())
	}
	rt.AppendCrushMessages(t, f.crushDB(), "s", crushRead("c5", restart.Add(20*time.Second))...)
	tick(restart.Add(25 * time.Second))
	tick(restart.Add(30 * time.Second))
	if got, want := describe(drain(ch2)), []string{"worker:tool:view@2"}; !equal(got, want) {
		t.Fatalf("second daemon then delivered %v, want the next call at the seq the first daemon would give it", got)
	}
	if s := second.Stats(); s.Stalls != 1 || s.OrphansSkipped != 1 {
		t.Errorf("second daemon stats = %+v, want one stall recovered", s)
	}
}

// TestJSONLRecordsAfter pins the record reader the Claude Code and Codex
// fallback uses: offsets land just past each complete line, a line longer
// than the read buffer is one record, and a trailing line with no newline is
// not a record yet.
func TestJSONLRecordsAfter(t *testing.T) {
	long := make([]byte, 200<<10)
	for i := range long {
		long[i] = 'x'
	}
	body := "{\"a\":1}\n" + string(long) + "\n{\"c\":3}\n{\"partial\""
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	first := int64(len("{\"a\":1}\n"))
	second := first + int64(len(long)) + 1
	third := second + int64(len("{\"c\":3}\n"))
	got, err := jsonlRecordsAfter(path, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{first, second, third}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("records = %v, want %v", got, want)
	}
	got, err = jsonlRecordsAfter(path, first, 1)
	if err != nil || len(got) != 1 || got[0] != second {
		t.Fatalf("one record after %d = %v, %v; want [%d]", first, got, err, second)
	}
	if got, err := jsonlRecordsAfter(path, third, 2); err != nil || len(got) != 0 {
		t.Fatalf("records after the last complete line = %v, %v; want none", got, err)
	}
}
