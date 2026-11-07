package observe

// Stream Ingest — Tests
//
// The fixture's fake adapter stands in for a store; the fake normalizer below
// stands in for agent-trace's stream parser, so these tests are about the
// seam: who an item is attributed to, and that a session a stream claims is
// not also read from the store. What the real parser turns Claude Code's
// stream-json into is exercised through the real spawn path in
// cmd/harness/daemon_streamingest_test.go.
//
// Governing: issue #995; ADR-0033 "One activity stream per session";
// SPEC-0006 REQ "Run Correlation".
//
// @joestump-agent 10/26/2026 - Added with stream-fed ingest (harness#995).

import (
	"bufio"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/core"
)

// fakeStream is the fixture's stand-in normalizer. Its lines are test-only
// records, not any agent's format: "meta <id>" names the session, "tool
// <name>" is one completed call, "mark <type>" is a mark. Everything else is
// skipped, as a real parser skips what it cannot read.
type fakeStream struct {
	mu     sync.Mutex
	err    error  // what ParseStream returns after the records
	onRead func() // blocks the parser until it returns, to test a stuck consumer
}

func newFakeStream() *fakeStream { return &fakeStream{} }

func (p *fakeStream) StreamFormat() tail.StreamFormat {
	return tail.StreamFormat{Name: "test-stream"}
}

func (p *fakeStream) ParseStream(ctx context.Context, r io.Reader, h tail.StreamHandler) (tail.SessionMeta, *tail.StreamResult, error) {
	var (
		meta tail.SessionMeta
		seq  int
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if p.onRead != nil {
			p.onRead()
		}
		kind, arg, _ := strings.Cut(sc.Text(), " ")
		switch kind {
		case "meta":
			meta = tail.SessionMeta{Key: arg, ID: arg, Harness: tail.HarnessClaudeCode}
			h.Meta(meta)
		case "tool":
			seq++
			name, rest, _ := strings.Cut(arg, " ")
			if rest == "" {
				rest = name
			}
			h.Event(classify.Event{Seq: seq, Tool: name, Summary: rest})
		case "mark":
			seq++
			h.Mark(classify.Mark{Seq: seq, Type: arg})
		}
	}
	p.mu.Lock()
	err := p.err
	p.mu.Unlock()
	return meta, &tail.StreamResult{}, err
}

// streamFixture is an observer whose claude-code harness resolves to the fake
// normalizer. fa, when set, is the store that harness also writes.
func streamFixture(t *testing.T, fa *fakeAdapter) (*fixture, *fakeStream) {
	t.Helper()
	ps := newFakeStream()
	f := newFixture(t, func(o *Options) {
		if fa != nil {
			o.Sources = fa.sources(true)
		}
		o.StreamParser = func(h core.Harness) tail.StreamParser {
			if h.Adapter != "claude-code" {
				return nil
			}
			return ps
		}
	})
	return f, ps
}

// waitStreamed blocks until the observer has no feed open, so a test that
// closed a stream can read what that stream delivered.
func waitStreamed(t *testing.T, o *Observer) {
	t.Helper()
	if !waitFor(5*time.Second, func() bool { return o.Stats().Streams == 0 }) {
		t.Fatal("a run's stream feed never finished parsing")
	}
}

// waitClaimed blocks until run f's stream holds session id, so a scan the
// test runs next is judged against the claim rather than racing it.
func waitClaimed(t *testing.T, o *Observer, id string) {
	t.Helper()
	if !waitFor(5*time.Second, func() bool { return o.claimIsOpen(id) }) {
		t.Fatal("the stream never claimed session " + id)
	}
}

func waitFor(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return ok()
}

// TestStreamEventsFanOutAttributedToTheRun is the ask in its simplest shape:
// a structured one-shot's tool calls and marks reach the subscribers while it
// runs, attributed to the harness the supervisor spawned, marked as having
// come from the stream, and redacted like anything else this package delivers.
func TestStreamEventsFanOutAttributedToTheRun(t *testing.T) {
	f, _ := streamFixture(t, nil)
	f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: f.work}, running(f.clock.Now()))
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	sink := f.obs.OpenStream("claude", 7)
	sink.Line("meta cc-live-1")
	waitClaimed(t, f.obs, "claude-code/cc-live-1")
	sink.Line("tool Read")
	sink.Line("mark user")
	sink.Line("tool Bash")
	sink.Close()
	waitStreamed(t, f.obs)

	got := describe(drain(ch))
	want := []string{"claude:tool:Read@1", "claude:mark:user@2", "claude:tool:Bash@3"}
	if !equal(got, want) {
		t.Fatalf("stream delivered %v, want %v", got, want)
	}
	st := f.obs.Stats()
	if st.StreamItems != 3 {
		t.Errorf("StreamItems = %d, want 3", st.StreamItems)
	}
	if st.Delivered != 3 {
		t.Errorf("Delivered = %d, want 3", st.Delivered)
	}
}

// TestSecretFromAStreamIsRedactedOnTheWayOut: a transcript records commands
// verbatim, and a stream is a transcript printed to stdout, so a credential
// inside a tool call leaves this package masked (ADR-0008).
func TestSecretFromAStreamIsRedactedOnTheWayOut(t *testing.T) {
	f, _ := streamFixture(t, nil)
	f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: f.work}, running(f.clock.Now()))
	ch, cancel := f.obs.Subscribe("test", 8)
	defer cancel()

	// Assembled at run time, not a literal: gitleaks reads GITEA_TOKEN=<shape>
	// in source as a credential, and the redact package's branch-time fixtures
	// are built the same way (see .gitleaksignore's notes on those tests).
	token := "abc" + "123456789"
	sink := f.obs.OpenStream("claude", 1)
	sink.Line("meta cc-live-2")
	sink.Line("tool Bash GITEA_TOKEN=" + token)
	sink.Close()
	waitStreamed(t, f.obs)

	got := drain(ch)
	if len(got) != 1 {
		t.Fatalf("delivered %v, want one call", describe(got))
	}
	if strings.Contains(got[0].Tool.Summary, token) || strings.Contains(got[0].Tool.Tool, token) {
		t.Errorf("a credential crossed the fan-out unredacted: %q / %q", got[0].Tool.Tool, got[0].Tool.Summary)
	}
	if got[0].Origin != OriginStream {
		t.Errorf("Origin = %q, want %q", got[0].Origin, OriginStream)
	}
}

// TestStreamClaimSuppressesTranscriptItems is ADR-0033's one stream per
// session: a Claude run that prints its calls and writes a transcript of the
// same session must be counted once. The stream is the record, so the
// transcript's copy is dropped where it is read, and counted.
func TestStreamClaimSuppressesTranscriptItems(t *testing.T) {
	const sid = "cc-1"
	fa := &fakeAdapter{kind: tail.HarnessClaudeCode, id: sid}
	f, _ := streamFixture(t, fa)
	fa.cwd = f.work
	f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: f.work}, running(start.Add(-time.Hour)))
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	// The poller sees the session first: a harness resumed into it. The first
	// read is a baseline, so what was already there is consumed in silence;
	// what the store gains after it is live, and arrives.
	fa.append(start, fakeItem{tool: "Bash"})
	f.tick(start.Add(time.Second))
	fa.append(start.Add(2*time.Second), fakeItem{tool: "Grep"})
	f.tick(start.Add(2 * time.Second))
	if got := describe(drain(ch)); !equal(got, []string{"claude:tool:Grep@1"}) {
		t.Fatalf("before the stream claimed it, delivered %v", got)
	}

	sink := f.obs.OpenStream("claude", 11)
	sink.Line("meta " + sid)
	waitClaimed(t, f.obs, "claude-code/"+sid)
	sink.Line("tool Read")
	sink.Line("tool Edit")
	if !waitFor(5*time.Second, func() bool { return f.obs.Stats().StreamItems == 2 }) {
		t.Fatal("the stream's own calls never reached the subscribers")
	}
	if got := describe(drain(ch)); !equal(got, []string{"claude:tool:Read@1", "claude:tool:Edit@2"}) {
		t.Fatalf("the stream delivered %v, want its own two calls", got)
	}
	// The same two calls land in the store: the other route must not add them.
	fa.append(start.Add(3*time.Second), fakeItem{tool: "Read"}, fakeItem{tool: "Edit"})
	f.tick(start.Add(3 * time.Second))
	if got := describe(drain(ch)); len(got) != 0 {
		t.Fatalf("while claimed, delivered %v, want the transcript's copy withheld", got)
	}
	if n := f.obs.Stats().Suppressed; n != 2 {
		t.Errorf("Suppressed = %d, want the 2 transcript items withheld", n)
	}
	sink.Close()
	waitStreamed(t, f.obs)
}

// TestStreamEndFloorsTheSession: once the stream ends the poller takes the
// session back, but nothing that stream already delivered is delivered twice —
// only what the store gains after that instant is.
func TestStreamEndFloorsTheSession(t *testing.T) {
	const sid = "cc-2"
	fa := &fakeAdapter{kind: tail.HarnessClaudeCode, id: sid}
	f, _ := streamFixture(t, fa)
	fa.cwd = f.work
	f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: f.work}, running(start.Add(-time.Hour)))
	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()

	fa.append(start, fakeItem{tool: "Write"})
	f.tick(start.Add(time.Second)) // discovers the session, baselines the read

	sink := f.obs.OpenStream("claude", 12)
	sink.Line("meta " + sid)
	waitClaimed(t, f.obs, "claude-code/"+sid)
	sink.Line("tool Read")
	if !waitFor(5*time.Second, func() bool { return f.obs.Stats().StreamItems == 1 }) {
		t.Fatal("the stream's call never reached the subscribers")
	}
	drain(ch)                          // the stream's own call, seen as it arrived
	f.tick(start.Add(2 * time.Second)) // the store's copy of this run: suppressed
	if got := describe(drain(ch)); len(got) != 0 {
		t.Fatalf("while claimed, delivered %v", got)
	}

	sink.Close()
	waitStreamed(t, f.obs)
	f.tick(start.Add(3 * time.Second)) // the session is handed back, floored here
	f.tick(start.Add(4 * time.Second))
	if got := describe(drain(ch)); len(got) != 0 {
		t.Fatalf("at the handback, delivered %v, want nothing replayed", got)
	}
	fa.append(start.Add(5*time.Second), fakeItem{tool: "Grep"})
	f.tick(start.Add(6 * time.Second))
	got := describe(drain(ch))
	if len(got) != 1 || !strings.HasPrefix(got[0], "claude:tool:Grep@") {
		t.Fatalf("after the handback, delivered %v, want only the call the store gained", got)
	}
}

// TestStreamFromAnUndeclaredHarnessIsNotPublished: a run whose harness the
// daemon no longer declares has nothing to attribute to, so its lines are
// drained and counted rather than fanned out nameless — and the run's reader
// is never left blocked on a consumer that will not read.
func TestStreamFromAnUndeclaredHarnessIsNotPublished(t *testing.T) {
	f, _ := streamFixture(t, nil)
	ch, cancel := f.obs.Subscribe("test", 8)
	defer cancel()

	sink := f.obs.OpenStream("ghost", 3)
	for i := 0; i < 20; i++ {
		sink.Line("tool Read")
	}
	sink.Close()
	waitStreamed(t, f.obs)
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("delivered %v from a harness the daemon does not declare", describe(got))
	}
	if n := f.obs.Stats().Unattributed; n == 0 {
		t.Error("an undeclared harness's stream was not counted as unattributed")
	}
}

// TestStreamQueueDropsRatherThanBlocking: a consumer that stops reading must
// not slow the run that is printing (ADR-0007), so past the queue lines drop,
// and the drop is counted.
func TestStreamQueueDropsRatherThanBlocking(t *testing.T) {
	f, ps := streamFixture(t, nil)
	f.src.add(core.Harness{Name: "claude", Adapter: "claude-code", Workdir: f.work}, running(f.clock.Now()))
	stuck := make(chan struct{})
	release := make(chan struct{})
	var once, relOnce sync.Once
	unpark := func() { relOnce.Do(func() { close(release) }) }
	ps.onRead = func() {
		once.Do(func() { close(stuck) })
		<-release
	}
	t.Cleanup(unpark)

	sink := f.obs.OpenStream("claude", 5)
	sink.Line("tool Read") // the parser takes this one and parks
	<-stuck
	for i := 0; i < streamQueueLines+50; i++ {
		sink.Line("tool Read") // must not block on the parked parser
	}
	sink.Close()
	unpark()
	waitStreamed(t, f.obs)
	if n := f.obs.Stats().StreamDropped; n != 50 {
		t.Errorf("StreamDropped = %d, want the 50 lines past the queue", n)
	}
}

// TestStreamWithoutANormalizerIsCounted: a run that reached this seam on an
// adapter with no declared stream is a config that changed under the daemon;
// its lines are drained and counted, never parsed by hand (SPEC-0006). The
// default resolution stands in here: crush declares no structured stream.
func TestStreamWithoutANormalizerIsCounted(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(f.clock.Now()))
	ch, cancel := f.obs.Subscribe("test", 8)
	defer cancel()

	sink := f.obs.OpenStream("worker", 9)
	sink.Line("tool Read")
	sink.Close()
	waitStreamed(t, f.obs)
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("delivered %v for an adapter with no stream", describe(got))
	}
	if n := f.obs.Stats().StreamParseErrors; n != 1 {
		t.Errorf("StreamParseErrors = %d, want 1", n)
	}
}
