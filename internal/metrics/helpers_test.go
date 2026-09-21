package metrics

// Test Helpers
//
// Every assertion in this package reads the exposition a scraper would read:
// the registry is served through Handler, the body is parsed with
// prometheus/common/expfmt, and a series is looked up by name and exact label
// set. String matching on the body would pass for a series that exists under
// the wrong labels, or one that is present when it should be absent.
//
// @joestump-agent 09/21/2026 - Added for harness#356.

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// clock is a settable time source.
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

// fakeSource stands in for the Manager: declared harnesses, their snapshots,
// and a real lifecycle Bus. cmd/harness covers the real Manager.
type fakeSource struct {
	mu    sync.Mutex
	order []string
	snaps map[string]supervisor.Snapshot
	defs  map[string]core.Harness
	bus   *supervisor.Bus
	panic bool
}

func newFakeSource() *fakeSource {
	return &fakeSource{snaps: map[string]supervisor.Snapshot{}, defs: map[string]core.Harness{}, bus: supervisor.NewBus()}
}

func (f *fakeSource) Snapshots() []supervisor.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panic {
		panic("snapshot read failed")
	}
	out := make([]supervisor.Snapshot, 0, len(f.order))
	for _, n := range f.order {
		out = append(out, f.snaps[n])
	}
	return out
}

// HarnessRecord lets the fake double as the observer's Source.
func (f *fakeSource) HarnessRecord(name string) (core.Harness, string, bool) {
	h, ok := f.HarnessDef(name)
	return h, "", ok
}

func (f *fakeSource) HarnessDef(name string) (core.Harness, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.defs[name]
	return h, ok
}

func (f *fakeSource) EventsCounted() (<-chan supervisor.Event, func(), func() uint64) {
	return f.bus.SubscribeCounted()
}

func (f *fakeSource) add(h core.Harness, snap supervisor.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap.Name = h.Name
	if _, ok := f.defs[h.Name]; !ok {
		f.order = append(f.order, h.Name)
	}
	f.defs[h.Name] = h
	f.snaps[h.Name] = snap
}

func (f *fakeSource) remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.defs, name)
	delete(f.snaps, name)
	for i, n := range f.order {
		if n == name {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
}

func (f *fakeSource) setPanic(v bool) {
	f.mu.Lock()
	f.panic = v
	f.mu.Unlock()
}

// fakeFeed is a hand-driven observer feed for the unit tests; the outage test
// uses the real observer.
type fakeFeed struct {
	mu     sync.Mutex
	ch     chan observe.Event
	stats  observe.Stats
	closed bool
}

func newFakeFeed() *fakeFeed { return &fakeFeed{ch: make(chan observe.Event, 64)} }

func (f *fakeFeed) Subscribe(string, int) (<-chan observe.Event, func()) {
	return f.ch, f.close
}

func (f *fakeFeed) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.ch)
	}
}

func (f *fakeFeed) Stats() observe.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.stats
	st.Dropped = map[string]uint64{}
	for k, v := range f.stats.Dropped {
		st.Dropped[k] = v
	}
	return st
}

func (f *fakeFeed) setStats(st observe.Stats) {
	f.mu.Lock()
	f.stats = st
	f.mu.Unlock()
}

// crushHarness is an observable harness definition.
func crushHarness(name string) core.Harness {
	return core.Harness{Name: name, Adapter: "crush", Workdir: "/work/" + name}
}

func runningSnap() supervisor.Snapshot {
	return supervisor.Snapshot{State: core.StateRunning, Enabled: true, PID: 4242}
}

func toolEvent(harness, session string, at time.Time) observe.Event {
	ev := observe.Event{Harness: harness, Adapter: "crush", Kind: observe.KindTool, Time: at, ObservedAt: at}
	ev.Session.Key = "crush:" + session
	ev.Session.ID = session
	ev.Tool.Tool = "view"
	return ev
}

func errorEvent(harness, session, note string, at time.Time) observe.Event {
	ev := observe.Event{Harness: harness, Adapter: "crush", Kind: observe.KindMark, Time: at, ObservedAt: at}
	ev.Session.Key = "crush:" + session
	ev.Session.ID = session
	ev.Mark.Type = "error"
	ev.Mark.Note = note
	return ev
}

// families is one parsed scrape.
type families map[string]*dto.MetricFamily

// scrapeHandler GETs /metrics from h and parses the text exposition.
func scrapeHandler(t *testing.T, h http.Handler) families {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: status %d: %s", rec.Code, rec.Body.String())
	}
	return parse(t, rec.Body.String())
}

func scrape(t *testing.T, m *Metrics) families {
	t.Helper()
	return scrapeHandler(t, m.Handler())
}

func parse(t *testing.T, body string) families {
	t.Helper()
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse exposition: %v\n%s", err, body)
	}
	return fams
}

// labels is an exact label set; "harness=worker,state=running" style.
func lbls(kv ...string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// get returns the value of the series name{want} and whether it exists. The
// label set must match exactly.
func (f families) get(name string, want map[string]string) (float64, bool) {
	fam, ok := f[name]
	if !ok {
		return 0, false
	}
	for _, m := range fam.GetMetric() {
		if len(m.GetLabel()) != len(want) {
			continue
		}
		match := true
		for _, lp := range m.GetLabel() {
			if want[lp.GetName()] != lp.GetValue() {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		switch {
		case m.Counter != nil:
			return m.GetCounter().GetValue(), true
		case m.Gauge != nil:
			return m.GetGauge().GetValue(), true
		case m.Untyped != nil:
			return m.GetUntyped().GetValue(), true
		}
	}
	return 0, false
}

// must returns the series value, failing when it is absent.
func (f families) must(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	v, ok := f.get(name, want)
	if !ok {
		t.Fatalf("series %s%v absent; have %v", name, want, f.series(name))
	}
	return v
}

// series lists name's label sets, for failure messages.
func (f families) series(name string) []string {
	fam, ok := f[name]
	if !ok {
		return nil
	}
	var out []string
	for _, m := range fam.GetMetric() {
		var parts []string
		for _, lp := range m.GetLabel() {
			parts = append(parts, lp.GetName()+"="+lp.GetValue())
		}
		out = append(out, "{"+strings.Join(parts, ",")+"}")
	}
	sort.Strings(out)
	return out
}

// harnessValues returns the distinct harness label values of name.
func (f families) harnessValues(name string) []string {
	seen := map[string]bool{}
	if fam, ok := f[name]; ok {
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "harness" {
					seen[lp.GetValue()] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
