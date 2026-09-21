package telemetry

// Test Fixtures
//
// A fake observer (non-blocking fan-out, like the real one), a fake Manager,
// and an httptest OTLP receiver that decodes what arrives, so assertions are
// on the bytes a collector would read, not on the Records that produced them.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// fakeObserver fans events out without ever blocking, counting drops per
// subscriber, the way observe.Observer does.
type fakeObserver struct {
	mu      sync.Mutex
	subs    map[string]chan observe.Event
	dropped map[string]uint64
	names   []string
}

func newFakeObserver() *fakeObserver {
	return &fakeObserver{subs: map[string]chan observe.Event{}, dropped: map[string]uint64{}}
}

func (f *fakeObserver) Subscribe(name string, buf int) (<-chan observe.Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan observe.Event, buf)
	f.subs[name] = ch
	f.names = append(f.names, name)
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			delete(f.subs, name)
			close(ch)
		})
	}
}

func (f *fakeObserver) Stats() observe.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := map[string]uint64{}
	for k, v := range f.dropped {
		d[k] = v
	}
	return observe.Stats{Dropped: d}
}

func (f *fakeObserver) publish(evs ...observe.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ev := range evs {
		for name, ch := range f.subs {
			select {
			case ch <- ev:
			default:
				f.dropped[name]++
			}
		}
	}
}

// pending is how many events wait unread in a subscription's buffer.
func (f *fakeObserver) pending(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs[name])
}

func (f *fakeObserver) subscriptions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...)
}

// fakeSource is a Manager stand-in whose definitions a test can change, as a
// reload would.
type fakeSource struct {
	mu     sync.Mutex
	defs   map[string]core.Harness
	states map[string]core.State
}

func newFakeSource(defs ...core.Harness) *fakeSource {
	s := &fakeSource{defs: map[string]core.Harness{}, states: map[string]core.State{}}
	for _, h := range defs {
		s.defs[h.Name] = h
		s.states[h.Name] = core.StateRunning
	}
	return s
}

func (s *fakeSource) HarnessDef(name string) (core.Harness, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.defs[name]
	return h, ok
}

func (s *fakeSource) Snapshot(name string) (supervisor.Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[name]
	return supervisor.Snapshot{Name: name, State: st}, ok
}

func (s *fakeSource) set(h core.Harness) {
	s.mu.Lock()
	s.defs[h.Name] = h
	s.mu.Unlock()
}

func (s *fakeSource) setState(name string, st core.State) {
	s.mu.Lock()
	s.states[name] = st
	s.mu.Unlock()
}

func optIn(name string) core.Harness {
	yes := true
	return core.Harness{Name: name, Adapter: "crush", ExportTelemetry: &yes}
}

// --- decoded OTLP ---

type wireKV struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

type wireLog struct {
	TimeUnixNano         string         `json:"timeUnixNano"`
	ObservedTimeUnixNano string         `json:"observedTimeUnixNano"`
	SeverityNumber       int            `json:"severityNumber"`
	SeverityText         string         `json:"severityText"`
	Body                 map[string]any `json:"body"`
	Attributes           []wireKV       `json:"attributes"`
	TraceID              string         `json:"traceId"`
	SpanID               string         `json:"spanId"`
}

func (l wireLog) attr(k string) any { return attrOf(l.Attributes, k) }
func (l wireLog) body() string      { s, _ := l.Body["stringValue"].(string); return s }

type wireSpan struct {
	TraceID      string   `json:"traceId"`
	SpanID       string   `json:"spanId"`
	ParentSpanID string   `json:"parentSpanId"`
	Name         string   `json:"name"`
	Kind         int      `json:"kind"`
	Start        string   `json:"startTimeUnixNano"`
	End          string   `json:"endTimeUnixNano"`
	Attributes   []wireKV `json:"attributes"`
	Status       struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
}

func (s wireSpan) attr(k string) any { return attrOf(s.Attributes, k) }

// attrOf unwraps an AnyValue to a Go value.
func attrOf(kvs []wireKV, k string) any {
	for _, kv := range kvs {
		if kv.Key != k {
			continue
		}
		for typ, v := range kv.Value {
			switch typ {
			case "arrayValue":
				var out []string
				for _, item := range v.(map[string]any)["values"].([]any) {
					out = append(out, item.(map[string]any)["stringValue"].(string))
				}
				return out
			default:
				return v
			}
		}
	}
	return nil
}

type receiver struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	raw      [][]byte
	logs     []wireLog
	spans    []wireSpan
	requests []*http.Request
	resource []wireKV
	scope    map[string]string
	// respond, when set, decides the reply; nil is 200.
	respond func(n int, w http.ResponseWriter) bool
	hits    int
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{t: t}
	r.srv = httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) handle(w http.ResponseWriter, req *http.Request) {
	var body io.Reader = req.Body
	if req.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(req.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		body = zr
	}
	raw, _ := io.ReadAll(body)
	r.mu.Lock()
	r.hits++
	n := r.hits
	respond := r.respond
	r.mu.Unlock()
	if respond != nil && respond(n, w) {
		return
	}
	var env struct {
		ResourceLogs []struct {
			Resource  struct{ Attributes []wireKV } `json:"resource"`
			ScopeLogs []struct {
				Scope      map[string]string `json:"scope"`
				LogRecords []wireLog         `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
		ResourceSpans []struct {
			Resource   struct{ Attributes []wireKV } `json:"resource"`
			ScopeSpans []struct {
				Scope map[string]string `json:"scope"`
				Spans []wireSpan        `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		r.t.Errorf("receiver: undecodable OTLP JSON: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.raw = append(r.raw, raw)
	r.requests = append(r.requests, req)
	for _, rl := range env.ResourceLogs {
		r.resource = rl.Resource.Attributes
		for _, sl := range rl.ScopeLogs {
			r.scope = sl.Scope
			r.logs = append(r.logs, sl.LogRecords...)
		}
	}
	for _, rs := range env.ResourceSpans {
		r.resource = rs.Resource.Attributes
		for _, ss := range rs.ScopeSpans {
			r.scope = ss.Scope
			r.spans = append(r.spans, ss.Spans...)
		}
	}
}

func (r *receiver) snapshotLogs() []wireLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]wireLog(nil), r.logs...)
}

func (r *receiver) snapshotSpans() []wireSpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]wireSpan(nil), r.spans...)
}

func (r *receiver) allRaw() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var s string
	for _, b := range r.raw {
		s += string(b)
	}
	return s
}

func (r *receiver) hitCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- resolved configs and events ---

func testResolved(url string, mutate func(*core.TelemetryConfig)) *Resolved {
	cfg := core.DefaultTelemetryConfig()
	cfg.Logs, cfg.Traces = true, true
	cfg.Endpoint = url
	cfg.BatchInterval = 20 * time.Millisecond
	cfg.IdleFlush = time.Hour
	if mutate != nil {
		mutate(&cfg)
	}
	res, err := Resolve(cfg, Env{Getenv: func(string) string { return "" }, Hostname: func() (string, error) { return "box1", nil }, Version: "v9.9.9"})
	if err != nil {
		panic(err)
	}
	return res
}

func quietLogger() *log.Logger { return log.New(io.Discard) }

var t0 = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

func session(key string) tail.SessionMeta {
	return tail.SessionMeta{Key: key, ID: "sess-" + key, Harness: tail.HarnessCrush, Cwd: "/work", Model: "glm-5.3", Title: "fix the build"}
}

func markEv(harness, key string, seq int, typ, note string, at time.Time) observe.Event {
	return observe.Event{
		Harness: harness, Adapter: "crush", Session: session(key), Kind: observe.KindMark,
		Mark: classify.Mark{Seq: seq, Type: typ, Note: note, Timestamp: at.Format(time.RFC3339Nano)},
		Time: at, ObservedAt: at.Add(time.Second),
	}
}

func toolEv(harness, key string, seq int, summary string, isErr bool, at time.Time) observe.Event {
	return observe.Event{
		Harness: harness, Adapter: "crush", Session: session(key), Kind: observe.KindTool,
		Tool: classify.Event{
			Seq: seq, Tool: "bash", Action: classify.ActionExec, Summary: summary, IsError: isErr,
			ResultBytes: 42, Timestamp: at.Format(time.RFC3339Nano),
			Targets: []classify.Target{{Path: "main.go", Touch: classify.TouchRead}},
		},
		Time: at, ObservedAt: at.Add(time.Second),
	}
}
