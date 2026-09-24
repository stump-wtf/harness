package source

// What the source manager reports about itself: per-outcome counters, the
// ordered stream of state changes `trigger_source_changed` is built from, and
// the scrubbing that keeps a credential out of a reported error.
//
// Governing: ADR-0021; SPEC-0014 REQ "Trigger Visibility", REQ "Trigger
// Metrics", REQ "Error Handling Standards", REQ "Credential Resolution".
//
// @joestump 09/24/2026 - Introduced with `harness triggers` (#476).

import (
	"net/url"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
)

// sourceStats is one source's counters and last event.
type sourceStats struct {
	counts    map[trigger.Outcome]int
	lastEvent time.Time
}

// NoteOutcome counts one outcome for a source. `fired` also stamps the
// source's last event.
//
// Exported because not every outcome is decided here: a webhook delivery is
// rejected — unauthorized, too large, rate limited — by the listener before
// it ever reaches Fire, and it is the listener that has to say so.
func (m *Manager) NoteOutcome(ref string, o trigger.Outcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.stats[ref]
	if s == nil {
		s = &sourceStats{counts: map[trigger.Outcome]int{}}
		m.stats[ref] = s
	}
	s.counts[o]++
	if o == trigger.OutcomeFired {
		s.lastEvent = time.Now()
	}
}

// withStatsLocked returns st with its counters filled in: every outcome,
// zeros included. Caller holds m.mu.
func (m *Manager) withStatsLocked(st Status) Status {
	st.Counts = make(map[trigger.Outcome]int, len(trigger.Outcomes))
	for _, o := range trigger.Outcomes {
		st.Counts[o] = 0
	}
	if s := m.stats[st.Source]; s != nil {
		for o, n := range s.counts {
			st.Counts[o] = n
		}
		st.LastEvent = s.lastEvent
	}
	st.Events = st.Counts[trigger.OutcomeFired]
	st.Invalid = st.Counts[trigger.OutcomeInvalid]
	return st
}

// SetOnState installs the state-change handler, replacing Options.OnState.
//
// The daemon needs it because of its boot order: the source manager starts
// before the protocol server that broadcasts `trigger_source_changed` exists.
// Changes before the handler is installed are not replayed — nobody could
// have subscribed to them — and Status still reports the current state.
func (m *Manager) SetOnState(fn func(Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onState = fn
}

// publishLocked queues a state change for OnState. Caller holds m.mu.
//
// One drainer delivers the queue in order, off the lock. Order is the whole
// point: a subscriber to `trigger_source_changed` takes the last change it
// saw as the current state, so two changes to one source that arrive swapped
// leave it believing the wrong one for as long as nothing else changes. A
// goroutine per change could not promise that; a synchronous call under the
// lock could deadlock a handler that reads Status back.
func (m *Manager) publishLocked(st Status) {
	if m.onState == nil {
		return
	}
	m.pending = append(m.pending, m.withStatsLocked(st))
	if m.draining {
		return
	}
	m.draining = true
	go m.drain()
}

// drain delivers queued changes until the queue is empty.
func (m *Manager) drain() {
	for {
		m.mu.Lock()
		batch, fn := m.pending, m.onState
		m.pending = nil
		if len(batch) == 0 || fn == nil {
			m.draining = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		for _, st := range batch {
			fn(st)
		}
	}
}

// scrubReason removes what a channel source's error text must never carry:
// the URL's query, and any resolved header value.
//
// The query first. Go's HTTP client quotes the full request URL in its
// errors — `Get "https://sb.example/mcp?token=…": dial tcp: …` — and a vended
// endpoint's query can be its credential. REQ "Trigger Visibility" shows a
// channel's `url` with its query removed for the same reason.
//
// Header values second, and defensively: nothing in the client echoes one
// today, but a reason is shown to every subscriber, and an echo added later
// should not be the first anyone hears of it.
func scrubReason(reason string, src core.ChannelSource) string {
	if u, err := url.Parse(src.URL); err == nil && (u.RawQuery != "" || u.Fragment != "") {
		for _, form := range []string{src.URL, u.String()} {
			reason = strings.ReplaceAll(reason, form, StripQuery(src.URL))
		}
		if u.RawQuery != "" {
			reason = strings.ReplaceAll(reason, u.RawQuery, "***")
		}
	}
	for _, v := range src.Headers {
		if raw := v.Reveal(); len(raw) >= 4 {
			reason = strings.ReplaceAll(reason, raw, "***")
		}
	}
	return reason
}

// StripQuery returns a URL without its query or fragment — the form of a
// channel's `url` any report shows. An unparseable URL is cut at the first
// '?' or '#', so a malformed one still cannot leak a query.
func StripQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			return raw[:i]
		}
		return raw
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	return u.String()
}
