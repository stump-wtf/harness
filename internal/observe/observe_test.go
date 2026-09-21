package observe

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/redact"
	"github.com/stump-wtf/harness/internal/runtrace"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// start is the observer's start (the daemon's, in production). A whole second,
// so the second-resolution timestamps crush stores compare exactly.
var start = time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)

// clock is a settable time source shared by the observer and the fixtures.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// fakeSource is a Manager stand-in: declared harnesses and their snapshots.
// The wiring test in cmd/harness covers the real Manager.
type fakeSource struct {
	mu    sync.Mutex
	snaps []supervisor.Snapshot
	defs  map[string]core.Harness
}

func (f *fakeSource) Snapshots() []supervisor.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]supervisor.Snapshot(nil), f.snaps...)
}

func (f *fakeSource) HarnessRecord(name string) (core.Harness, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.defs[name]
	return h, "", ok
}

func (f *fakeSource) add(h core.Harness, snap supervisor.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.defs == nil {
		f.defs = map[string]core.Harness{}
	}
	snap.Name = h.Name
	f.defs[h.Name] = h
	f.snaps = append(f.snaps, snap)
}

func (f *fakeSource) update(name string, fn func(*supervisor.Snapshot)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.snaps {
		if f.snaps[i].Name == name {
			fn(&f.snaps[i])
		}
	}
}

// running is a snapshot of a harness that has been up since started.
func running(started time.Time) supervisor.Snapshot {
	return supervisor.Snapshot{State: core.StateRunning, Enabled: true, LastStarted: started, PID: 4242}
}

// fixture is an observer over a fake source, with a hermetic HOME so crush's
// registry source never reads the real one.
type fixture struct {
	t     *testing.T
	home  string
	work  string
	clock *clock
	src   *fakeSource
	obs   *Observer
}

func newFixture(t *testing.T, mutate func(*Options)) *fixture {
	t.Helper()
	home := t.TempDir()
	f := &fixture{t: t, home: home, work: filepath.Join(home, "work"), clock: &clock{t: start}, src: &fakeSource{}}
	opts := Options{
		Now:          f.clock.Now,
		Since:        start,
		DaemonDir:    home,
		DiscoveryEnv: func(core.Harness) map[string]string { return map[string]string{"HOME": home} },
	}
	if mutate != nil {
		mutate(&opts)
	}
	f.obs = New(f.src, opts)
	t.Cleanup(f.obs.Stop)
	return f
}

// tick advances the clock to at and runs one scan synchronously.
func (f *fixture) tick(at time.Time) {
	f.clock.Set(at)
	f.obs.scan(context.Background())
}

func (f *fixture) crushDB() string { return filepath.Join(f.work, ".crush", "crush.db") }

// drain takes everything currently buffered on ch.
func drain(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

func describe(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		if e.Kind == KindMark {
			out = append(out, fmt.Sprintf("%s:mark:%s@%d", e.Harness, e.Mark.Type, e.Mark.Seq))
		} else {
			out = append(out, fmt.Sprintf("%s:tool:%s@%d", e.Harness, e.Tool.Tool, e.Tool.Seq))
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// crushRead is a view call and its result: one tool event.
func crushRead(id string, at time.Time) []rt.CrushMessage {
	return []rt.CrushMessage{
		{Role: "assistant", At: at, Parts: rt.ToolCall(id, "view", map[string]any{"file_path": "README.md"})},
		{Role: "tool", At: at, Parts: rt.ToolResult(id, "contents")},
	}
}

func userSays(text string, at time.Time) rt.CrushMessage {
	return rt.CrushMessage{Role: "user", At: at, Parts: fmt.Sprintf(`[{"type":"text","data":{"text":%q}}]`, text)}
}

func providerError(msg string, at time.Time) rt.CrushMessage {
	return rt.CrushMessage{Role: "assistant", At: at, Parts: rt.FinishError(msg, "429 Too Many Requests")}
}

// TestOutageErrorMarksDeliverWithoutToolCalls is the 2026-09-14 shape: a
// long-running crush worker, resumed into a session it opened hours before
// this daemon started, gains only provider errors — no tool calls at all. The
// errors must arrive on their own, promptly, attributed to the worker, and
// redacted.
//
// The first half shows agent-trace's own Watcher delivering nothing for the
// very same store, which is what makes the second half a test of the property
// (marks travel alone) and not of a fixture that happens to carry a tool call.
func TestOutageErrorMarksDeliverWithoutToolCalls(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-3*time.Hour)))
	var history []rt.CrushMessage
	history = append(history, userSays("drain the queue", start.Add(-time.Hour)))
	history = append(history, crushRead("c1", start.Add(-time.Hour))...)
	history = append(history, providerError("old failure", start.Add(-30*time.Minute)))
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "sess-1", Created: start.Add(-2 * time.Hour), Updated: start.Add(-30 * time.Minute), Messages: history})

	watcher := tail.NewWatcherWithConfig(tail.WatchConfig{PollInterval: time.Hour, MaxAge: -1},
		[]tail.Adapter{&tail.CrushAdapter{DBPath: f.crushDB(), Cwd: f.work}})
	_ = watcher.ScanOnce(context.Background())
	for len(watcher.Events()) > 0 {
		<-watcher.Events()
	}

	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()
	f.tick(start.Add(5 * time.Second))
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("history delivered at first scan: %v", describe(got))
	}

	const secret = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	rt.AppendCrushMessages(t, f.crushDB(), "sess-1",
		userSays("new doorbell", start.Add(19*time.Second)),
		providerError("You exceeded your current quota (key "+secret+")", start.Add(20*time.Second)))

	_ = watcher.ScanOnce(context.Background())
	if n := len(watcher.Events()); n != 0 {
		t.Fatalf("tail.Watcher emitted %d events for an error-only tail; the premise of this test (it parks marks) no longer holds", n)
	}

	f.tick(start.Add(25 * time.Second))
	got := drain(ch)
	if want := []string{"worker:mark:user-message@1", "worker:mark:error@1"}; !equal(describe(got), want) {
		t.Fatalf("delivered %v, want %v", describe(got), want)
	}
	errEv := got[1]
	if errEv.Adapter != "crush" || errEv.Session.ID != "sess-1" {
		t.Errorf("error event adapter/session = %q/%q", errEv.Adapter, errEv.Session.ID)
	}
	if !errEv.Time.Equal(start.Add(20 * time.Second)) {
		t.Errorf("error Time = %v, want the item's own timestamp", errEv.Time)
	}
	if strings.Contains(errEv.Mark.Note, secret) || !strings.Contains(errEv.Mark.Note, redact.Mask) || !strings.Contains(errEv.Mark.Note, "quota") {
		t.Errorf("error note not redacted as expected: %q", errEv.Mark.Note)
	}

	// Already tracked now: the next error comes through alone too.
	rt.AppendCrushMessages(t, f.crushDB(), "sess-1", providerError("still over quota", start.Add(40*time.Second)))
	f.tick(start.Add(45 * time.Second))
	if got := describe(drain(ch)); !equal(got, []string{"worker:mark:error@1"}) {
		t.Fatalf("second tick delivered %v, want one error mark", got)
	}
	if st := f.obs.Stats(); st.Delivered != 3 || st.Sessions != 1 {
		t.Errorf("stats = %+v, want 3 delivered over 1 session", st)
	}
}

// TestNoHistoryReplay: items older than the daemon (or than the harness's
// current run, when that is later) are never delivered; items written after
// the daemon started but before the session was first seen are; and so is
// everything appended afterwards.
func TestNoHistoryReplay(t *testing.T) {
	f := newFixture(t, nil)
	runStart := start.Add(10 * time.Second) // the harness restarted after the daemon came up
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(runStart))
	var msgs []rt.CrushMessage
	msgs = append(msgs, crushRead("before-daemon", start.Add(-time.Minute))...)
	msgs = append(msgs, crushRead("before-run", start.Add(5*time.Second))...)
	msgs = append(msgs, crushRead("in-run", start.Add(30*time.Second))...)
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start.Add(-time.Hour), Updated: start.Add(30 * time.Second), Messages: msgs})

	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()
	f.tick(start.Add(35 * time.Second))
	got := drain(ch)
	if want := []string{"worker:tool:view@2"}; !equal(describe(got), want) {
		t.Fatalf("first scan delivered %v, want only the in-run call %v", describe(got), want)
	}

	rt.AppendCrushMessages(t, f.crushDB(), "s", crushRead("later", start.Add(50*time.Second))...)
	f.tick(start.Add(55 * time.Second))
	if got := describe(drain(ch)); !equal(got, []string{"worker:tool:view@3"}) {
		t.Fatalf("append delivered %v, want the new call", got)
	}
}

// TestUntimestampedBaseline: an item with no timestamp that is already in a
// session when it is first seen is history (it cannot be dated); one that
// appears afterwards is live and is delivered, stamped with when it was read.
func TestUntimestampedBaseline(t *testing.T) {
	for _, incremental := range []bool{true, false} {
		t.Run(fmt.Sprintf("incremental=%v", incremental), func(t *testing.T) {
			fa := &fakeAdapter{kind: tail.HarnessClaudeCode, id: "cc-1"}
			f := newFixture(t, func(o *Options) { o.Sources = fa.sources(incremental) })
			fa.cwd = f.work
			f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: f.work}, running(start.Add(-time.Hour)))
			fa.append(f.clock.Now(), fakeItem{mark: "compaction"}, fakeItem{tool: "Bash"})

			ch, cancel := f.obs.Subscribe("test", 64)
			defer cancel()
			f.tick(start.Add(5 * time.Second))
			if got := drain(ch); len(got) != 0 {
				t.Fatalf("baseline delivered %v", describe(got))
			}
			fa.append(start.Add(9*time.Second), fakeItem{mark: "compaction"}, fakeItem{tool: "Read"})
			at := start.Add(10 * time.Second)
			f.tick(at)
			got := drain(ch)
			if want := []string{"claude:mark:compaction@1", "claude:tool:Read@1"}; !equal(describe(got), want) {
				t.Fatalf("delivered %v, want %v", describe(got), want)
			}
			for _, e := range got {
				if !e.Time.Equal(at) || !e.ObservedAt.Equal(at) {
					t.Errorf("untimestamped item Time/ObservedAt = %v/%v, want the read time %v", e.Time, e.ObservedAt, at)
				}
			}
		})
	}
}

// TestOrderingAndExactlyOnce: across many ticks, a session's items arrive in
// seq order (a mark before the call that shares its seq), each exactly once;
// a tool call whose result has not landed yet is withheld, not delivered
// twice; and idle ticks deliver nothing.
func TestOrderingAndExactlyOnce(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start, Updated: start.Add(time.Second),
		Messages: []rt.CrushMessage{userSays("go", start.Add(time.Second))}})
	ch, cancel := f.obs.Subscribe("test", 256)
	defer cancel()

	var got []Event
	at := start.Add(2 * time.Second)
	step := func(msgs ...rt.CrushMessage) {
		if len(msgs) > 0 {
			rt.AppendCrushMessages(t, f.crushDB(), "s", msgs...)
		}
		at = at.Add(5 * time.Second)
		f.tick(at)
		got = append(got, drain(ch)...)
	}
	step()
	step(crushRead("a", at)...)
	step(userSays("more", at), rt.CrushMessage{Role: "assistant", At: at, Parts: rt.ToolCall("b", "bash", map[string]any{"command": "sleep 60"})})
	step() // the call is open: nothing to deliver yet
	step(rt.CrushMessage{Role: "tool", At: at, Parts: rt.ToolResult("b", "done")})
	step(providerError("boom", at))
	step()
	step()

	want := []string{
		"worker:mark:user-message@0",
		"worker:tool:view@0",
		"worker:mark:user-message@1",
		"worker:tool:bash@1",
		"worker:mark:error@2",
	}
	if !equal(describe(got), want) {
		t.Fatalf("delivered\n  %v\nwant\n  %v", describe(got), want)
	}
}

// TestAmbiguousSessionIsWithheld: two crush harnesses share a workdir and are
// both running, so nothing identifies whose session it is. Its items are
// consumed, counted and never delivered — and they stay undelivered when the
// peer stops, while what the survivor writes afterwards comes through. The
// second half is what shows the withholding is the rule firing, not a session
// that was never read.
func TestAmbiguousSessionIsWithheld(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "alpha", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	f.src.add(core.Harness{Name: "beta", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	// History from before the daemon, then one live call. Only the live one
	// counts as withheld: history was never going to be delivered.
	msgs := append(crushRead("old", start.Add(-time.Hour)), crushRead("x", start.Add(10*time.Second))...)
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start.Add(-time.Hour), Updated: start.Add(10 * time.Second), Messages: msgs})
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	f.tick(start.Add(15 * time.Second))
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("contested session delivered %v", describe(got))
	}
	st := f.obs.Stats()
	if st.Ambiguous != 1 || st.Contested != 1 || st.Delivered != 0 {
		t.Fatalf("stats = %+v, want Ambiguous=1 Contested=1 Delivered=0", st)
	}

	f.src.update("beta", func(s *supervisor.Snapshot) {
		s.State, s.PID, s.LastExitAt = core.StateStopped, 0, start.Add(20*time.Second)
	})
	rt.AppendCrushMessages(t, f.crushDB(), "s", crushRead("y", start.Add(40*time.Second))...)
	f.tick(start.Add(45 * time.Second))
	if got := describe(drain(ch)); !equal(got, []string{"alpha:tool:view@2"}) {
		t.Fatalf("after the peer stopped: delivered %v, want only the new call, attributed to alpha", got)
	}
	if st := f.obs.Stats(); st.Contested != 0 {
		t.Errorf("Contested = %d after the peer stopped, want 0", st.Contested)
	}
}

// TestStreamedRowIsReadWhileUnlisted: crush streams a turn into its assistant
// row in place, so a provider error landing in that row moves neither the row
// nor the session's updated_at — the session can drop out of the activity
// listing while its last turn is still being written. A tracked session is
// read whether or not the listing reports it, and the error arrives.
func TestStreamedRowIsReadWhileUnlisted(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.ForgetAfter = 10 * time.Second })
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	streaming := rt.CrushMessage{Role: "assistant", At: start.Add(10 * time.Second), Parts: `[{"type":"text","data":{"text":"thinking"}}]`}
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start, Updated: start.Add(10 * time.Second),
		Messages: []rt.CrushMessage{userSays("go", start.Add(time.Second)), streaming}})
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	f.tick(start.Add(15 * time.Second))
	if got := describe(drain(ch)); !equal(got, []string{"worker:mark:user-message@0"}) {
		t.Fatalf("first scan delivered %v, want the user message and the streaming row held back", got)
	}
	rt.RewriteLastCrushMessage(t, f.crushDB(), "s", rt.FinishError("rate limited", "429"))
	// Past the listing window for an updated_at of +10s, inside ForgetAfter
	// of the last read.
	f.tick(start.Add(22 * time.Second))
	if got := describe(drain(ch)); !equal(got, []string{"worker:mark:error@0"}) {
		t.Fatalf("delivered %v, want the error streamed into the held row", got)
	}
}

// TestSlowSubscriberDropsFastOneDoesNot: a subscriber that never reads loses
// what does not fit its buffer, counted; a subscriber that keeps up gets every
// event; and the scan is never held up by either.
func TestSlowSubscriberDropsFastOneDoesNot(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	var msgs []rt.CrushMessage
	for i := 0; i < 5; i++ {
		msgs = append(msgs, crushRead(fmt.Sprintf("c%d", i), start.Add(10*time.Second))...)
	}
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start, Updated: start.Add(10 * time.Second), Messages: msgs})
	slow, cancelSlow := f.obs.Subscribe("slow", 1)
	defer cancelSlow()
	fast, cancelFast := f.obs.Subscribe("fast", 100)
	defer cancelFast()

	done := make(chan struct{})
	go func() {
		f.tick(start.Add(15 * time.Second))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("scan blocked on a subscriber that does not read")
	}
	if n := len(drain(fast)); n != 5 {
		t.Errorf("fast subscriber got %d events, want 5", n)
	}
	if n := len(drain(slow)); n != 1 {
		t.Errorf("slow subscriber holds %d, want its buffer of 1", n)
	}
	st := f.obs.Stats()
	if st.Dropped["slow"] != 4 || st.Dropped["fast"] != 0 || st.Delivered != 5 {
		t.Errorf("stats = %+v, want slow dropped 4, fast 0, delivered 5", st)
	}
}

// TestForgottenSessionDoesNotReplay: a session idle past ForgetAfter is
// dropped from memory, and when it wakes up only what is new is delivered.
// Without the tombstone the rediscovered session's first read would deliver
// every item since the daemon started a second time.
func TestForgottenSessionDoesNotReplay(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.ForgetAfter = time.Hour })
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start, Updated: start.Add(10 * time.Second),
		Messages: crushRead("early", start.Add(10*time.Second))})
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	f.tick(start.Add(15 * time.Second))
	if got := describe(drain(ch)); !equal(got, []string{"worker:tool:view@0"}) {
		t.Fatalf("first scan delivered %v", got)
	}
	f.tick(start.Add(2 * time.Hour))
	if st := f.obs.Stats(); st.Sessions != 0 {
		t.Fatalf("Sessions = %d after two idle hours, want the session forgotten", st.Sessions)
	}
	rt.AppendCrushMessages(t, f.crushDB(), "s", crushRead("late", start.Add(2*time.Hour+10*time.Second))...)
	f.tick(start.Add(2*time.Hour + 15*time.Second))
	if got := describe(drain(ch)); !equal(got, []string{"worker:tool:view@1"}) {
		t.Fatalf("after waking delivered %v, want only the new call", got)
	}
}

// TestRewrittenStoreRebaselines: a store that moves backwards (a truncated or
// replaced transcript) is read again from the top as a baseline, so what is
// already there is not delivered a second time.
func TestRewrittenStoreRebaselines(t *testing.T) {
	fa := &fakeAdapter{kind: tail.HarnessClaudeCode, id: "cc-1"}
	f := newFixture(t, func(o *Options) { o.Sources = fa.sources(true) })
	fa.cwd = f.work
	f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: f.work}, running(start.Add(-time.Hour)))
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()
	fa.append(start, fakeItem{tool: "Bash"})
	f.tick(start.Add(time.Second))
	fa.append(start.Add(2*time.Second), fakeItem{tool: "Read"}, fakeItem{tool: "Edit"})
	f.tick(start.Add(3 * time.Second))
	if got := describe(drain(ch)); !equal(got, []string{"claude:tool:Read@1", "claude:tool:Edit@2"}) {
		t.Fatalf("before rewrite delivered %v", got)
	}
	fa.rewrite(start.Add(4*time.Second), fakeItem{tool: "Grep"})
	f.tick(start.Add(5 * time.Second)) // notices the rewrite
	f.tick(start.Add(6 * time.Second)) // baseline read of the new content
	fa.append(start.Add(7*time.Second), fakeItem{tool: "Write"})
	f.tick(start.Add(8 * time.Second))
	got := describe(drain(ch))
	if len(got) != 1 || !strings.HasPrefix(got[0], "claude:tool:Write@") {
		t.Fatalf("after rewrite delivered %v, want only the call appended after it", got)
	}
}

// TestParseErrorsAreCountedAndDoNotStarveOthers: one store that fails to read
// is counted per adapter kind, and the healthy harness beside it still
// delivers.
func TestParseErrorsAreCountedAndDoNotStarveOthers(t *testing.T) {
	broken := &fakeAdapter{kind: tail.HarnessClaudeCode, id: "cc-broken", failParse: true}
	f := newFixture(t, func(o *Options) { o.Sources = broken.sources(true) })
	broken.cwd = filepath.Join(f.home, "other")
	f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: broken.cwd}, running(start.Add(-time.Hour)))
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	broken.append(start.Add(time.Second), fakeItem{tool: "Bash"})
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "s", Created: start, Updated: start.Add(time.Second),
		Messages: crushRead("ok", start.Add(time.Second))})
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	f.tick(start.Add(5 * time.Second))
	if got := describe(drain(ch)); !equal(got, []string{"worker:tool:view@0"}) {
		t.Fatalf("delivered %v, want the healthy crush call", got)
	}
	if n := f.obs.Stats().ParseErrors["claude-code"]; n != 1 {
		t.Errorf("ParseErrors[claude-code] = %d, want 1", n)
	}
}

// TestStopIsIdempotentAndLeavesNoGoroutines: the loop polls on its own once
// started, Stop ends it and closes subscriber channels, a second Stop is a
// no-op, and no goroutine outlives it.
func TestStopIsIdempotentAndLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	fa := &fakeAdapter{kind: tail.HarnessClaudeCode, id: "cc-1"}
	f := newFixture(t, func(o *Options) {
		o.Sources = fa.sources(true)
		o.PollInterval = 5 * time.Millisecond
	})
	fa.cwd = f.work
	f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: f.work}, running(start.Add(-time.Hour)))
	ch, _ := f.obs.Subscribe("test", 64)
	// Present before the loop's first scan, so it is baseline; what follows
	// is live.
	fa.append(start, fakeItem{tool: "Setup"})

	f.obs.Start()
	f.obs.Start() // no second loop
	deadline := time.Now().Add(5 * time.Second)
	for f.obs.Stats().Sessions == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	fa.append(start.Add(time.Second), fakeItem{tool: "Bash"})
	select {
	case ev := <-ch:
		if ev.Tool.Tool != "Bash" {
			t.Fatalf("got %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the running loop never delivered an appended item")
	}

	f.obs.Stop()
	f.obs.Stop()
	if _, ok := <-ch; ok {
		t.Error("subscriber channel still open after Stop")
	}
	late, _ := f.obs.Subscribe("late", 1)
	if _, ok := <-late; ok {
		t.Error("Subscribe after Stop returned an open channel")
	}
	f.obs.Start() // after Stop: must not restart the loop

	deadline = time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines: %d before, %d after Stop", before, after)
	}

	never := New(&fakeSource{}, Options{})
	stopped := make(chan struct{})
	go func() { never.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop without Start hung")
	}
}

// TestCancelUnsubscribes: a cancelled subscriber stops receiving, its channel
// closes, and cancelling twice is safe.
func TestCancelUnsubscribes(t *testing.T) {
	f := newFixture(t, nil)
	ch, cancel := f.obs.Subscribe("test", 4)
	cancel()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel open after cancel")
	}
	f.obs.publish([]Event{{Harness: "x"}}) // must not panic on the closed channel
	if st := f.obs.Stats(); st.Delivered != 1 || st.Dropped["test"] != 0 {
		t.Errorf("stats = %+v", st)
	}
}

// TestRedactToolCopies: a tool event's summary and paths are masked, and the
// caller's slices are not edited in place.
func TestRedactToolCopies(t *testing.T) {
	in := classify.Event{
		Summary: "curl -H token=ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Targets: []classify.Target{{Path: "GITEA_TOKEN=abc123"}},
		Outside: []classify.OutsideTouch{{Path: "/tmp/password=hunter2"}},
	}
	out := redactTool(in)
	for _, s := range []string{out.Summary, out.Targets[0].Path, out.Outside[0].Path} {
		if !strings.Contains(s, redact.Mask) {
			t.Errorf("not redacted: %q", s)
		}
	}
	if in.Targets[0].Path != "GITEA_TOKEN=abc123" || in.Outside[0].Path != "/tmp/password=hunter2" {
		t.Error("redactTool edited the input's slices in place")
	}
}

// fakeItem is one entry of a fake transcript: a tool call (tool != "") or a
// mark (mark != ""). Neither carries a timestamp, which is the case the real
// crush fixtures cannot produce.
type fakeItem struct {
	tool string
	mark string
}

// fakeAdapter is an in-memory agent-trace adapter holding one session.
// ParseSince's watermark is the item index; a rewrite shrinks the list, which
// makes the next ParseSince report watermark 0, as the JSONL adapters do for a
// truncated file.
type fakeAdapter struct {
	kind      tail.Harness
	id        string
	cwd       string
	failParse bool
	failList  bool
	parseErr  error // returned by ParseSince when set

	mu     sync.Mutex
	items  []fakeItem
	active time.Time
}

func (a *fakeAdapter) append(at time.Time, items ...fakeItem) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.items = append(a.items, items...)
	a.active = at
}

func (a *fakeAdapter) rewrite(at time.Time, items ...fakeItem) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.items = append([]fakeItem(nil), items...)
	a.active = at
}

// sources is an Options.Sources hook returning this adapter for harnesses of
// its kind, either as itself (incremental) or wrapped so only Parse is visible.
func (a *fakeAdapter) sources(incremental bool) func(s runtrace.Scope) ([]tail.Adapter, error) {
	return func(s runtrace.Scope) ([]tail.Adapter, error) {
		if s.Adapter != string(a.kind) {
			return runtrace.Sources(s)
		}
		if incremental {
			return []tail.Adapter{a}, nil
		}
		return []tail.Adapter{fullParseOnly{a}}, nil
	}
}

func (a *fakeAdapter) Harness() tail.Harness        { return a.kind }
func (a *fakeAdapter) SessionDir() string           { return "/fake/" + a.id }
func (a *fakeAdapter) WithRoot(string) tail.Adapter { return a }

func (a *fakeAdapter) ListSessions(context.Context) ([]tail.SessionMeta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failList {
		return nil, fmt.Errorf("fake: store unreadable")
	}
	if len(a.items) == 0 {
		return nil, nil
	}
	return []tail.SessionMeta{{
		Key: a.id, ID: a.id, Harness: a.kind, Path: "/fake/" + a.id, Cwd: a.cwd,
		StartedAt: start.Add(-time.Hour).Format(time.RFC3339Nano),
		EndedAt:   a.active.Format(time.RFC3339Nano),
	}}, nil
}

func (a *fakeAdapter) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, tail.SessionMeta, error) {
	ev, mk, meta, _, err := a.ParseSince(ctx, path, 0, 0)
	return ev, mk, meta, err
}

func (a *fakeAdapter) ParseSince(_ context.Context, _ string, wm int64, seq int) ([]classify.Event, []classify.Mark, tail.SessionMeta, int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failParse {
		return nil, nil, tail.SessionMeta{}, 0, fmt.Errorf("fake: corrupt transcript")
	}
	if a.parseErr != nil {
		return nil, nil, tail.SessionMeta{}, 0, a.parseErr
	}
	if int(wm) > len(a.items) {
		return nil, nil, tail.SessionMeta{}, 0, nil
	}
	var events []classify.Event
	var marks []classify.Mark
	for _, it := range a.items[wm:] {
		if it.mark != "" {
			marks = append(marks, classify.Mark{Seq: seq, Type: it.mark})
			continue
		}
		events = append(events, classify.Event{Seq: seq, Tool: it.tool, Summary: it.tool})
		seq++
	}
	return events, marks, tail.SessionMeta{}, int64(len(a.items)), nil
}

func (a *fakeAdapter) Watermark(context.Context, string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return int64(len(a.items))
}

// fullParseOnly hides fakeAdapter's IncrementalParser, exercising the
// re-parse fallback.
type fullParseOnly struct{ a *fakeAdapter }

func (f fullParseOnly) Harness() tail.Harness        { return f.a.Harness() }
func (f fullParseOnly) SessionDir() string           { return f.a.SessionDir() }
func (f fullParseOnly) WithRoot(string) tail.Adapter { return f }
func (f fullParseOnly) ListSessions(ctx context.Context) ([]tail.SessionMeta, error) {
	return f.a.ListSessions(ctx)
}

func (f fullParseOnly) Parse(ctx context.Context, path string) ([]classify.Event, []classify.Mark, tail.SessionMeta, error) {
	return f.a.Parse(ctx, path)
}

// TestSummaryCacheBoundedDespiteFailingStore: the shared summary cache is swept
// only after a scan in which every listing succeeded. A store that fails every
// scan must not pin the cache for the daemon's lifetime — here a claude-code
// transcript is summarised, then deleted, while a second harness's store never
// lists; the cache entry must still go once ForgetAfter has passed.
//
// @joestump-agent 09/21/2026 - review: added with the forced-sweep fix.
func TestSummaryCacheBoundedDespiteFailingStore(t *testing.T) {
	broken := &fakeAdapter{kind: tail.HarnessCodex, id: "codex-broken", failList: true}
	f := newFixture(t, func(o *Options) {
		o.Sources = broken.sources(true)
		o.ForgetAfter = time.Minute
	})
	broken.cwd = filepath.Join(f.home, "other")
	f.src.add(core.Harness{Name: "codex", Adapter: "codex", Workdir: broken.cwd}, running(start.Add(-time.Hour)))
	f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: f.work}, running(start.Add(-time.Hour)))
	if err := os.MkdirAll(f.work, 0o755); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(f.home, ".claude", "projects", "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "cc-sess.jsonl")
	line := fmt.Sprintf(`{"type":"user","timestamp":%q,"sessionId":"cc-sess","cwd":%q,"message":{"role":"user","content":"hello"}}`+"\n",
		start.Add(time.Second).Format(time.RFC3339), f.work)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, start.Add(time.Second), start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	f.tick(start.Add(5 * time.Second))
	if n := f.obs.summaries.Len(); n == 0 {
		t.Fatalf("summary cache empty after listing a live transcript; the test cannot show eviction (stats %+v)", f.obs.Stats())
	}
	if st := f.obs.Stats(); st.ScanErrors == 0 {
		t.Fatalf("the broken store listed cleanly; the test needs a failing listing (stats %+v)", st)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Sweep is mark-then-sweep (an entry survives the first sweep after its
	// last use), so a forced sweep every ForgetAfter drops a dead entry
	// within two of them.
	for at := start.Add(30 * time.Second); !at.After(start.Add(3 * time.Minute)); at = at.Add(30 * time.Second) {
		f.tick(at)
	}
	if n := f.obs.summaries.Len(); n != 0 {
		t.Fatalf("summary cache holds %d entries for a deleted transcript while another store keeps failing; it is never swept", n)
	}
}

// TestDeletedTranscriptIsForgottenNotAnError: a transcript that disappears
// under a tracked session is not a parse failure. It must not raise
// ParseErrors on every scan (consumers report those as collection errors,
// SPEC-0013 REQ-6); the session is dropped with a tombstone at once. The
// control half shows a genuine parse failure still counts.
//
// @joestump-agent 09/21/2026 - review: added with the gone-session fix.
func TestDeletedTranscriptIsForgottenNotAnError(t *testing.T) {
	fa := &fakeAdapter{kind: tail.HarnessClaudeCode, id: "cc-1"}
	f := newFixture(t, func(o *Options) { o.Sources = fa.sources(true) })
	fa.cwd = f.work
	f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: f.work}, running(start.Add(-time.Hour)))
	fa.append(start, fakeItem{tool: "Bash"})
	f.tick(start.Add(time.Second))
	if st := f.obs.Stats(); st.Sessions != 1 {
		t.Fatalf("Sessions = %d, want the session tracked", st.Sessions)
	}

	fa.mu.Lock()
	fa.parseErr = fmt.Errorf("open transcript: %w", fs.ErrNotExist)
	fa.mu.Unlock()
	for i := 2; i < 6; i++ {
		f.tick(start.Add(time.Duration(i) * time.Second))
	}
	st := f.obs.Stats()
	if st.ParseErrors["claude-code"] != 0 {
		t.Errorf("ParseErrors[claude-code] = %d for a deleted transcript, want 0", st.ParseErrors["claude-code"])
	}
	if st.Sessions != 0 {
		t.Errorf("Sessions = %d, want the deleted session dropped", st.Sessions)
	}
	if _, ok := f.obs.tombstones["claude-code/cc-1"]; !ok {
		t.Error("no tombstone for the dropped session: a reappearance could replay")
	}

	fa.mu.Lock()
	fa.parseErr = fmt.Errorf("fake: corrupt transcript")
	fa.mu.Unlock()
	f.tick(start.Add(10 * time.Second))
	if n := f.obs.Stats().ParseErrors["claude-code"]; n != 1 {
		t.Errorf("ParseErrors[claude-code] = %d after a real parse failure, want 1", n)
	}
}
