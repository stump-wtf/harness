package modelapi

// Gate Tests
//
// Governing tests: SPEC-0031 REQ-6 and REQ-8 (#837) — a gate call cannot be
// built without an entry point from the closed set, the content hash is
// taken before redaction, every error class this story produces is
// triggered for real and asserted with errors.Is, attestation fires even
// on a usable response, served_model is sanitized, the slot wait counts
// against the timeout, and the counters equal a scripted mix exactly.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const tolerance = 500 * time.Millisecond

// fakeGuard is an httptest model endpoint that records what reached it.
type fakeGuard struct {
	srv     *httptest.Server
	mu      sync.Mutex
	bodies  []string
	models  []string
	arrival []time.Time
	respond func() (int, string)
	block   chan struct{}
}

func newFakeGuard(t *testing.T, respond func() (int, string)) *fakeGuard {
	t.Helper()
	fg := &fakeGuard{respond: respond}
	fg.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// block is nil unless a test holds every call: an open channel
		// stalls the call and closing it releases them.
		fg.mu.Lock()
		block := fg.block
		fg.mu.Unlock()
		if block != nil {
			<-block
		}
		var body struct {
			Model    string    `json:"model"`
			Messages []Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fg.mu.Lock()
		fg.bodies = append(fg.bodies, body.Messages[len(body.Messages)-1].Content)
		fg.models = append(fg.models, body.Model)
		fg.arrival = append(fg.arrival, time.Now())
		fg.mu.Unlock()
		code, payload := fg.respond()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(fg.srv.Close)
	return fg
}

func (fg *fakeGuard) received() ([]string, []string, []time.Time) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return append([]string{}, fg.bodies...), append([]string{}, fg.models...), append([]time.Time{}, fg.arrival...)
}

func (fg *fakeGuard) hits() int {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	return len(fg.bodies)
}

// hold stalls every call the server receives until the returned release
// func is called.
func (fg *fakeGuard) hold() (release func()) {
	fg.mu.Lock()
	ch := make(chan struct{})
	fg.block = ch
	fg.mu.Unlock()
	return func() { close(ch) }
}

func yesBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"choices":[{"message":{"role":"assistant","content":"yes"},"logprobs":{"content":[{"token":"yes","top_logprobs":[{"token":"yes","logprob":-0.1},{"token":"no","logprob":-3.2}]}]}}],"usage":{"prompt_tokens":7,"completion_tokens":1}}`, model)
}

func spec(entry Entry) CallSpec {
	logprobs := true
	top := 20
	temp := 0.0
	return CallSpec{
		Entry:          entry,
		Guard:          "g1",
		Policy:         "p1",
		Model:          "mistralai/Shieldstral-1.0-3B",
		Timeout:        2 * time.Second,
		MaxConcurrency: 2,
		MaxTokens:      1,
		Temperature:    &temp,
		Logprobs:       &logprobs,
		TopLogprobs:    &top,
	}
}

func TestGateCallNeedsEntryPoint(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") })
	g := NewGate(New(fg.srv.URL, ""))
	results := g.Screen(context.Background(), "text", spec(""))
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want a refusal", results)
	}
	if !strings.Contains(results[0].Err.Error(), "entry point") {
		t.Fatalf("err = %v, want it to name the missing entry point", results[0].Err)
	}
	if fg.hits() != 0 {
		t.Fatalf("a call without an entry point reached the guard %d times", fg.hits())
	}
}

func TestGateUnknownEntryPointRefused(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("m") })
	g := NewGate(New(fg.srv.URL, ""))
	results := g.Screen(context.Background(), "text", spec(Entry("schedule")))
	if len(results) != 1 || results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "unknown entry point") {
		t.Fatalf("results = %+v, want an unknown-entry-point refusal", results)
	}
}

// TestGateEntryPointsTable covers the closed set: each entry builds and
// issues its call.
func TestGateEntryPointsTable(t *testing.T) {
	for _, entry := range []Entry{EntryEvent, EntryPackage, EntryLoadout} {
		fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") })
		g := NewGate(New(fg.srv.URL, ""))
		results := g.Screen(context.Background(), "text", spec(entry))
		if len(results) != 1 || results[0].Err != nil {
			t.Fatalf("entry %q: results = %+v", entry, results)
		}
		if results[0].Entry != entry {
			t.Fatalf("entry %q: recorded %q", entry, results[0].Entry)
		}
		if fg.hits() != 1 {
			t.Fatalf("entry %q: guard hit %d times", entry, fg.hits())
		}
	}
}

func TestGateHashBeforeRedaction(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") })
	g := NewGate(New(fg.srv.URL, ""))
	text := "issue body: Authorization: token ghp_abc123 please screen me"
	results := g.Screen(context.Background(), text, spec(EntryEvent))
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	bodies, _, _ := fg.received()
	if len(bodies) != 1 {
		t.Fatalf("guard saw %d bodies", len(bodies))
	}
	if strings.Contains(bodies[0], "ghp_abc123") {
		t.Fatal("the guard received the unredacted credential")
	}
	// The hash is computed by the TEST, from the unredacted text — never
	// taken from the code under test.
	sum := sha256.Sum256([]byte(text))
	wantHash := hex.EncodeToString(sum[:])
	if results[0].Hash != wantHash {
		t.Fatalf("hash = %s, want the SHA-256 of the unredacted text %s", results[0].Hash, wantHash)
	}
	if len(results[0].Hash) != 64 || results[0].Hash != strings.ToLower(results[0].Hash) {
		t.Fatalf("hash %q is not 64 lowercase hex digits", results[0].Hash)
	}
}

func TestGateModelMismatchEvenWithValidLogprobs(t *testing.T) {
	// REQ-6 scenario: the guard's model is Shieldstral, the response names
	// qwen3-30b-a3b and carries valid logprobs.
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("qwen3-30b-a3b") })
	g := NewGate(New(fg.srv.URL, ""))
	results := g.Screen(context.Background(), "text", spec(EntryEvent))
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v, want model_mismatch", results)
	}
	if !errors.Is(results[0].Err, ErrModelMismatch) {
		t.Fatalf("err = %v, want errors.Is ErrModelMismatch", results[0].Err)
	}
	if results[0].ServedModel != "qwen3-30b-a3b" {
		t.Fatalf("served_model = %q, want the response's model", results[0].ServedModel)
	}
}

func TestGateServedModelSanitized(t *testing.T) {
	long := strings.Repeat("q", 280) + "\x00\x01" + strings.Repeat("z", 20) // 302 bytes with control chars
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody(long) })
	g := NewGate(New(fg.srv.URL, ""))
	results := g.Screen(context.Background(), "text", spec(EntryEvent))
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("results = %+v (the mismatch is expected; the sanitization is the subject)", results)
	}
	s := results[0].ServedModel
	if len(s) > 256 {
		t.Fatalf("served_model is %d bytes, want at most 256", len(s))
	}
	if strings.ContainsAny(s, "\x00\x01") {
		t.Fatalf("served_model %q still carries control characters", s)
	}
}

func TestGateNoLogprobsWhenRequested(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) {
		return 200, `{"model":"mistralai/Shieldstral-1.0-3B","choices":[{"message":{"content":"no"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	})
	g := NewGate(New(fg.srv.URL, ""))
	results := g.Screen(context.Background(), "text", spec(EntryEvent))
	if len(results) != 1 || !errors.Is(results[0].Err, ErrNoLogprobs) {
		t.Fatalf("results = %+v, want no_logprobs", results)
	}
}

func TestGateParseClasses(t *testing.T) {
	t.Run("invalid json", func(t *testing.T) {
		fg := newFakeGuard(t, func() (int, string) { return 200, `<html>nope</html>` })
		g := NewGate(New(fg.srv.URL, ""))
		results := g.Screen(context.Background(), "text", spec(EntryEvent))
		if len(results) != 1 || !errors.Is(results[0].Err, ErrParse) {
			t.Fatalf("results = %+v, want parse", results)
		}
	})
	t.Run("no choices", func(t *testing.T) {
		fg := newFakeGuard(t, func() (int, string) {
			return 200, `{"model":"mistralai/Shieldstral-1.0-3B","choices":[],"usage":{}}`
		})
		g := NewGate(New(fg.srv.URL, ""))
		results := g.Screen(context.Background(), "text", spec(EntryEvent))
		if len(results) != 1 || !errors.Is(results[0].Err, ErrParse) {
			t.Fatalf("results = %+v, want parse", results)
		}
	})
	t.Run("body over cap", func(t *testing.T) {
		fg := newFakeGuard(t, func() (int, string) {
			return 200, `{"model":"m","choices":[{"message":{"content":"` + strings.Repeat("a", 1<<20) + `"}}]}`
		})
		g := NewGate(New(fg.srv.URL, ""))
		results := g.Screen(context.Background(), "text", spec(EntryEvent))
		if len(results) != 1 || !errors.Is(results[0].Err, ErrParse) {
			t.Fatalf("results = %+v, want parse (body over 1 MiB)", results)
		}
	})
}

func TestGateHTTPStatusClass(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) { return 500, `{"error":{}}` })
	g := NewGate(New(fg.srv.URL, ""))
	results := g.Screen(context.Background(), "text", spec(EntryEvent))
	if len(results) != 1 || !errors.Is(results[0].Err, ErrHTTPStatus) {
		t.Fatalf("results = %+v, want http_status", results)
	}
	// Never retried within a screen (REQ-6).
	if fg.hits() != 1 {
		t.Fatalf("guard saw %d requests, want 1", fg.hits())
	}
}

func TestGateTransportClass(t *testing.T) {
	// A server that is closed: every dial fails.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	g := NewGate(New(url, ""))
	results := g.Screen(context.Background(), "text", spec(EntryEvent))
	if len(results) != 1 || !errors.Is(results[0].Err, ErrTransport) {
		t.Fatalf("results = %+v, want transport", results)
	}
}

func TestGateTimeoutClass(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") })
	release := fg.hold()
	defer release()
	g := NewGate(New(fg.srv.URL, ""))
	s := spec(EntryEvent)
	s.Timeout = 300 * time.Millisecond
	start := time.Now()
	results := g.Screen(context.Background(), "text", s)
	elapsed := time.Now().Sub(start)
	if len(results) != 1 || !errors.Is(results[0].Err, ErrTimeout) {
		t.Fatalf("results = %+v, want timeout", results)
	}
	// Tolerance: 300ms deadline, measured from the screen's start, robust
	// on a loaded CI runner.
	if elapsed > s.Timeout+tolerance {
		t.Fatalf("call lived %s, want at most timeout %s + tolerance %s", elapsed, s.Timeout, tolerance)
	}
}

func TestGateExpiredDeadlineIsTimeoutNotInterrupted(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") })
	release := fg.hold()
	defer release()
	g := NewGate(New(fg.srv.URL, ""))
	s := spec(EntryEvent)
	s.Timeout = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(2 * time.Second) // well past the deadline; never fires first
		cancel()
	}()
	results := g.Screen(ctx, "text", s)
	if len(results) != 1 || !errors.Is(results[0].Err, ErrTimeout) {
		t.Fatalf("results = %+v, want the expired deadline to read as timeout", results)
	}
}

func TestGateCancelledParentIsInterrupted(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") })
	release := fg.hold()
	defer release()
	g := NewGate(New(fg.srv.URL, ""))
	s := spec(EntryEvent)
	s.Timeout = 10 * time.Second // long enough that only the cancel ends it
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	results := g.Screen(ctx, "text", s)
	if len(results) != 1 || !errors.Is(results[0].Err, ErrInterrupted) {
		t.Fatalf("results = %+v, want a cancelled parent to read as interrupted", results)
	}
}

func TestGateShutdownIsInterrupted(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") })
	release := fg.hold()
	defer release()
	g := NewGate(New(fg.srv.URL, ""))
	s := spec(EntryEvent)
	s.Timeout = 10 * time.Second
	go func() { time.Sleep(150 * time.Millisecond); g.Shutdown() }()
	results := g.Screen(context.Background(), "text", s)
	if len(results) != 1 || !errors.Is(results[0].Err, ErrInterrupted) {
		t.Fatalf("results = %+v, want shutdown to read as interrupted, never timeout", results)
	}
	var gce *GateCallError
	if !errors.As(results[0].Err, &gce) || gce.Class() != "interrupted" {
		t.Fatalf("err = %v, want class interrupted", results[0].Err)
	}
}

// TestGateSlotWaitCountsAgainstTimeout is REQ-8's three-call scenario: a
// guard with max_concurrency = 1 and timeout 2s behind a server taking 1.5s
// per call, asked three calls in one screen. The first succeeds, the other
// two end in timeout about 2s after the screen started, the screen ends
// within about 2s, and no call outlives timeout + tolerance measured from
// the screen's start.
func TestGateSlotWaitCountsAgainstTimeout(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) {
		time.Sleep(1500 * time.Millisecond)
		return 200, yesBody("mistralai/Shieldstral-1.0-3B")
	})
	g := NewGate(New(fg.srv.URL, ""))
	s := spec(EntryEvent)
	s.Timeout = 2 * time.Second
	s.MaxConcurrency = 1
	start := time.Now()
	results := g.Screen(context.Background(), "text", s, s, s)
	elapsed := time.Now().Sub(start)

	ok, timeouts := 0, 0
	for _, r := range results {
		switch {
		case r.Err == nil:
			ok++
		case errors.Is(r.Err, ErrTimeout):
			timeouts++
		default:
			t.Fatalf("unexpected result: %+v", r)
		}
	}
	if ok != 1 || timeouts != 2 {
		t.Fatalf("ok = %d, timeouts = %d, want 1 and 2", ok, timeouts)
	}
	if elapsed > s.Timeout+tolerance {
		t.Fatalf("screen ran %s, want at most timeout %s + tolerance %s", elapsed, s.Timeout, tolerance)
	}
	// Server-side arrival times: the first two calls reached the server (the
	// second was abandoned mid-request at its deadline); the third never
	// got a slot and never arrived.
	_, _, arrivals := fg.received()
	if len(arrivals) != 2 {
		t.Fatalf("server saw %d arrivals, want 2 (the third never got a slot)", len(arrivals))
	}
	for _, at := range arrivals {
		if at.Sub(start) > s.Timeout+tolerance {
			t.Fatalf("a call reached the server %s after the screen started, beyond timeout + tolerance", at.Sub(start))
		}
	}
}

// TestGateDurationBeginsAtSlotRequest shows the duration observation starts
// when the call asks for its slot: the only slot is held by an in-flight
// probe, so the gate call's whole wait for it is inside its duration.
func TestGateDurationBeginsAtSlotRequest(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) {
		time.Sleep(400 * time.Millisecond)
		return 200, yesBody("mistralai/Shieldstral-1.0-3B")
	})
	g := NewGate(New(fg.srv.URL, ""))
	s := spec(EntryEvent)
	s.MaxConcurrency = 1
	s.Timeout = 3 * time.Second

	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		if r := g.Probe(context.Background(), s, "probe text"); r.Err != nil {
			t.Errorf("probe: %v", r.Err)
		}
	}()
	time.Sleep(50 * time.Millisecond) // let the probe take the only slot
	results := g.Screen(context.Background(), "text", s)
	<-probeDone
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	// The gate call waited ~350ms for the slot before its ~400ms request:
	// a duration measured from the REQUEST would miss the wait.
	if results[0].Duration < 700*time.Millisecond {
		t.Fatalf("duration = %s, want the slot wait (about 350ms) counted from the slot request", results[0].Duration)
	}
	snap := g.Counters("g1")
	if snap.LastDuration < 700*time.Millisecond {
		t.Fatalf("counter duration = %s, want the slot wait included", snap.LastDuration)
	}
}

func TestGateCountersMatchScriptedMix(t *testing.T) {
	mix := []struct {
		name      string
		respond   func() (int, string)
		wantClass string
	}{
		{"ok", func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") }, ""},
		{"http_status", func() (int, string) { return 503, `{}` }, "http_status"},
		{"model_mismatch", func() (int, string) { return 200, yesBody("other-model") }, "model_mismatch"},
		{"no_logprobs", func() (int, string) {
			return 200, `{"model":"mistralai/Shieldstral-1.0-3B","choices":[{"message":{"content":"yes"}}]}`
		}, "no_logprobs"},
		{"ok", func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") }, ""},
	}
	for _, m := range mix {
		fg := newFakeGuard(t, m.respond)
		g := NewGate(New(fg.srv.URL, ""))
		results := g.Screen(context.Background(), "text", spec(EntryEvent))
		if len(results) != 1 {
			t.Fatalf("%s: results = %+v", m.name, results)
		}
		if m.wantClass == "" && results[0].Err != nil {
			t.Fatalf("%s: unexpected err %v", m.name, results[0].Err)
		}
		if m.wantClass != "" {
			var gce *GateCallError
			if !errors.As(results[0].Err, &gce) || gce.Class() != m.wantClass {
				t.Fatalf("%s: err = %v, want class %s", m.name, results[0].Err, m.wantClass)
			}
		}
		snap := g.Counters("g1")
		wantCalls := int64(1)
		if m.wantClass != "" {
			if snap.CallsByResult["error"] != wantCalls || snap.CallsByResult["ok"] != 0 {
				t.Fatalf("%s: calls = %v", m.name, snap.CallsByResult)
			}
			if snap.ErrorsByClass[m.wantClass] != wantCalls {
				t.Fatalf("%s: classes = %v", m.name, snap.ErrorsByClass)
			}
		} else {
			if snap.CallsByResult["ok"] != wantCalls || snap.CallsByResult["error"] != 0 {
				t.Fatalf("%s: calls = %v", m.name, snap.CallsByResult)
			}
			if len(snap.ErrorsByClass) != 0 {
				t.Fatalf("%s: classes = %v", m.name, snap.ErrorsByClass)
			}
		}
	}
}

func TestGateCountersWithoutUsage(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) {
		return 200, `{"model":"mistralai/Shieldstral-1.0-3B","choices":[{"message":{"content":"yes"},"logprobs":{"content":[{"token":"yes","top_logprobs":[{"token":"yes","logprob":-0.1}]}]}}]}`
	})
	g := NewGate(New(fg.srv.URL, ""))
	s := spec(EntryEvent)
	s.Logprobs = nil // a labels-shaped call, still fine
	if r := g.Screen(context.Background(), "text", s); len(r) != 1 || r[0].Err != nil {
		t.Fatalf("results = %+v", r)
	}
	snap := g.Counters("g1")
	if snap.PromptTokens != 0 || snap.CompletionTokens != 0 {
		t.Fatalf("tokens = %d/%d, want unchanged (response carried no usage)", snap.PromptTokens, snap.CompletionTokens)
	}
}

func TestGateCountersTokensFromUsage(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") })
	g := NewGate(New(fg.srv.URL, ""))
	if r := g.Screen(context.Background(), "text", spec(EntryEvent)); len(r) != 1 || r[0].Err != nil {
		t.Fatalf("results = %+v", r)
	}
	snap := g.Counters("g1")
	if snap.PromptTokens != 7 || snap.CompletionTokens != 1 {
		t.Fatalf("tokens = %d/%d, want 7/1 from usage", snap.PromptTokens, snap.CompletionTokens)
	}
}

// TestGateProbeIsNotAGateCall: the probe path uses the guard's pool and
// counters, needs no entry point, and counts in its own probe series.
func TestGateProbeIsNotAGateCall(t *testing.T) {
	fg := newFakeGuard(t, func() (int, string) { return 200, yesBody("mistralai/Shieldstral-1.0-3B") })
	g := NewGate(New(fg.srv.URL, ""))
	s := spec("")
	r := g.Probe(context.Background(), s, "probe text")
	if r.Err != nil {
		t.Fatalf("probe err = %v (a probe takes no entry point)", r.Err)
	}
	if fg.hits() != 1 {
		t.Fatalf("guard saw %d requests, want 1", fg.hits())
	}
	snap := g.Counters("g1")
	if snap.CallsByResult["probe:ok"] != 1 || snap.CallsByResult["ok"] != 0 {
		t.Fatalf("calls = %v, want the probe in its own series", snap.CallsByResult)
	}
}

func TestSanitizeModelDirect(t *testing.T) {
	straddle := strings.Repeat("a", 255) + "éxyz" // the cap lands inside the é
	cases := []struct {
		in   string
		want string
	}{
		{"m1", "m1"},
		{"a\x00b", "ab"},
		{"a\nb\x1fb", "abb"},
		{strings.Repeat("q", 300), strings.Repeat("q", 256)},
		{strings.Repeat("é", 200), strings.Repeat("é", 128)}, // 256 bytes exactly, on a rune boundary
		{straddle, strings.Repeat("a", 255)},                 // the split rune is dropped whole
		{"", ""},
	}
	for _, c := range cases {
		if got := SanitizeModel(c.in); got != c.want {
			t.Fatalf("SanitizeModel = %q (len %d), want %q (len %d)", got, len(got), c.want, len(c.want))
		}
	}
}
