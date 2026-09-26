// Package loopguard stops a supervised harness whose agent has fallen into a
// runaway tool loop.
//
// # Runaway Tool-Loop Guard
//
// A process that loops never exits, so the supervisor's state machine sees a
// healthy harness. On 2026-09-21 a pool worker called the same MCP tool with
// the same arguments 608 times in a row over two hours — tool_call, then
// tool_result, then the same tool_call again, with no text or reasoning
// between — posting 608 identical comments before anyone noticed. The only
// record of that is the agent's transcript, which the agent event observer
// (internal/observe) already reads, so the guard is one more subscriber on it.
//
// It keys on the tool name and classify.Event.InputDigest, a fingerprint of
// the call's arguments, never the tool name alone: an agent commenting on ten
// different issues is working, not looping. And it counts identical calls in
// an unbroken streak, not across the whole run: a long-lived worker repeats
// identical reads all day (`make test` in an edit-test cycle, a queue poll per
// doorbell), and those always have other calls or a new prompt between them.
// The streak resets on any different tool call, on a user message (a new
// prompt is new work), and on a new session. On the Threshold-th identical
// call in a row the guard stops the harness through the Manager — a stop, not
// a restart, because a restarted crush resumes the looping session — logs an
// ERROR naming the harness, the tool and the count to the daemon log and the
// harness's own log, and starts counting again from zero.
//
// Known limit: a loop that alternates between two or more calls (A, B, A, B)
// never builds a streak and is not caught. The incident was a single call.
//
// Delivery never blocks the observer or its other subscribers (ADR-0007): a
// full buffer drops events for this subscriber alone, which can only break a
// streak, never fabricate one — a lost event undercounts, so the guard errs
// toward not stopping.
//
// Governing: stumpcloud/stumpcloud#469; issue #390 (the observer); ADR-0007.
//
// @joestump-agent 09/23/2026 - Added for stumpcloud/stumpcloud#469. Replaces
// observe.LoopGuard (#621), which keyed on the classified call (tool, action,
// summary, targets) — for an MCP tool that is the tool name alone, whatever
// the arguments — and counted over the session's whole life, so a worker's
// eighth call to any one MCP tool in a session stopped it.
package loopguard

import (
	"sync"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/observe"
)

// DefaultThreshold is how many identical calls in a row stop a harness. The
// incident ran to 608; an agent retrying one failed call a few times, or
// re-reading a file it just edited, stays well under it.
const DefaultThreshold = 8

// subscriberName and subscriberBuffer are the guard's observer subscription.
// The buffer absorbs a burst of polled events while the guard is issuing a
// stop; losing events only breaks streaks.
const (
	subscriberName   = "loopguard"
	subscriberBuffer = 256
)

// Subscriber is the observer surface the guard reads. *observe.Observer
// satisfies it.
type Subscriber interface {
	Subscribe(name string, buf int) (<-chan observe.Event, func())
}

// Stopper stops a harness and writes to its own log. *supervisor.Manager
// satisfies it.
type Stopper interface {
	Stop(name string) bool
	LogLifecycle(name, msg string, kv ...any) bool
}

// Options configures a Guard. The zero value is production.
type Options struct {
	// Threshold is the identical-call streak that stops a harness (default
	// DefaultThreshold; values below 2 take the default, since a streak of
	// one is every call).
	Threshold int
	// Logger receives the ERROR line (default log.Default()).
	Logger *log.Logger
}

// Trip is one stop the guard issued.
type Trip struct {
	Harness string
	Session string
	Tool    string
	Count   int
}

// streak is one harness's current run of identical calls.
type streak struct {
	session string
	key     string // tool name + NUL + input digest
	count   int
}

// Guard watches observer events for runaway tool loops. Create it with New.
type Guard struct {
	stop      Stopper
	threshold int
	log       *log.Logger

	// streaks is touched only by the consuming goroutine (or a test driving
	// handle directly).
	streaks map[string]*streak

	mu     sync.Mutex
	trips  []Trip
	seen   uint64 // events handled
	cancel func()
	done   chan struct{}
}

// New builds a guard that stops harnesses through stop. Start subscribes it.
func New(stop Stopper, opts Options) *Guard {
	if opts.Threshold < 2 {
		opts.Threshold = DefaultThreshold
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	return &Guard{
		stop:      stop,
		threshold: opts.Threshold,
		log:       opts.Logger,
		streaks:   make(map[string]*streak),
	}
}

// Start subscribes to sub and consumes its events until Close. Calling it
// twice does nothing.
func (g *Guard) Start(sub Subscriber) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done != nil {
		return
	}
	events, cancel := sub.Subscribe(subscriberName, subscriberBuffer)
	g.cancel, g.done = cancel, make(chan struct{})
	go func() {
		defer close(g.done)
		for ev := range events {
			g.handle(ev)
		}
	}()
}

// Close unsubscribes and waits for the consumer to finish. Idempotent, and
// safe without Start.
func (g *Guard) Close() {
	g.mu.Lock()
	cancel, done := g.cancel, g.done
	g.cancel = nil
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Seen is how many events the guard has handled.
func (g *Guard) Seen() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen
}

// Trips returns the stops issued so far, oldest first.
func (g *Guard) Trips() []Trip {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Trip(nil), g.trips...)
}

// handle advances ev's harness's streak and stops the harness when it reaches
// the threshold. It reports whether it issued a stop.
func (g *Guard) handle(ev observe.Event) bool {
	// Counted once handling, stop included, is done: Seen is what a caller
	// waits on before asserting what the guard did.
	defer func() {
		g.mu.Lock()
		g.seen++
		g.mu.Unlock()
	}()
	st := g.streaks[ev.Harness]
	if st == nil || st.session != ev.Session.Key {
		st = &streak{session: ev.Session.Key}
		g.streaks[ev.Harness] = st
	}
	switch ev.Kind {
	case observe.KindMark:
		if ev.Mark.Type == "user-message" {
			st.key, st.count = "", 0
		}
		return false
	case observe.KindTool:
	default:
		return false
	}
	if ev.Tool.InputDigest == "" {
		// Nothing to compare: an unencodable input, or an adapter that did
		// not pass one. Treat it as a different call.
		st.key, st.count = "", 0
		return false
	}
	key := ev.Tool.Tool + "\x00" + ev.Tool.InputDigest
	if key != st.key {
		st.key, st.count = key, 0
	}
	st.count++
	if st.count < g.threshold {
		return false
	}
	trip := Trip{Harness: ev.Harness, Session: ev.Session.ID, Tool: ev.Tool.Tool, Count: st.count}
	st.key, st.count = "", 0
	g.fire(trip, ev)
	return true
}

// fire stops the harness and says so, loudly, in both logs.
func (g *Guard) fire(t Trip, ev observe.Event) {
	g.mu.Lock()
	g.trips = append(g.trips, t)
	g.mu.Unlock()
	stopped := g.stop.Stop(t.Harness)
	kv := []any{
		"harness", t.Harness,
		"tool", t.Tool,
		"count", t.Count,
		"session", t.Session,
		"input_digest", shortDigest(ev.Tool.InputDigest),
		"summary", ev.Tool.Summary,
		"stopped", stopped,
	}
	g.log.Error("runaway tool loop: stopping harness", kv...)
	g.stop.LogLifecycle(t.Harness, "runaway tool loop: stopped by the daemon", kv[2:]...)
}

// shortDigest is enough of a digest to match log lines against each other.
func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
