// Runaway Tool-Loop Guard
//
// A subscriber on the agent-event observer that kills a supervised run whose
// agent session repeats one identical tool call past a threshold. The shape it
// exists for is the 2026-09-21 incident (stumpcloud/stumpcloud#469): a pool
// worker emitted 608 identical forge comment writes over two hours, each an
// assistant tool_call with no reasoning between calls. A legitimate retry
// calls the same tool twice; a runaway calls it forever — so the guard counts
// identical calls per session across the session's whole lifetime, never in a
// short window, and stops the run through the supervisor on the Nth identical
// call. The daemon keeps supervising: Stop ends the run, not the daemon, and
// a trigger may start the harness again later.
//
// The loop key is a hash of the classified call (tool, action, targets,
// summary) — the arguments as the observer sees them, redacted per ADR-0008
// and never logged. Counts live in memory only; a daemon restart starts every
// session from zero.
//
// Delivery never blocks or drops for other subscribers (ADR-0007): the guard
// is one consumer with a bounded buffer the observer may drop for, and the
// only thing it does on an event is hash, count and — once per runaway — stop
// a run.
//
// @joestump-agent 09/23/2026 - Added for harness#469 (the 608-comment
// runaway).
package observe

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"charm.land/log/v2"
)

const (
	// LoopGuardName is the guard's subscriber name, as it appears in
	// Stats.Dropped.
	LoopGuardName = "loopguard"
	// DefaultLoopThreshold is how many identical tool calls in one session
	// the guard tolerates before it kills the run. The incident ran to 608;
	// a legitimate retry is 2. Eight sits between them with room for a
	// stubborn-but-finite retry storm.
	DefaultLoopThreshold = 8
	// loopGuardBuffer is the guard subscriber's event buffer. The observer
	// drops events for a full subscriber (ADR-0007); a dropped tool call
	// undercounts by one, which only delays a kill.
	loopGuardBuffer = 256
	// loopSessionTTL is how long an idle session's counts are kept after its
	// last event, and how often the prune runs.
	loopSessionTTL    = time.Hour
	loopPruneInterval = 5 * time.Minute
)

// RunStopper kills a supervised run. *supervisor.Manager satisfies it; Stop
// ends the harness's current process and leaves the daemon supervising.
type RunStopper interface {
	Stop(name string) bool
}

// LoopGuard consumes the observer's event stream and stops a run whose
// session repeats one identical tool call DefaultLoopThreshold times (or the
// threshold StartLoopGuard was given). Create it with StartLoopGuard; stop it
// with Stop before the observer stops.
type LoopGuard struct {
	stop      RunStopper
	threshold int
	log       *log.Logger
	events    <-chan Event
	cancel    func()
	done      chan struct{}

	// counts maps a session scope to its per-shape tallies. The consumer
	// goroutine is the only writer; no lock is needed.
	counts map[string]*loopSession
	now    func() time.Time
}

type loopSession struct {
	counts   map[string]int
	lastSeen time.Time
}

// StartLoopGuard subscribes guard to obs and starts its consumer. A threshold
// of zero or less means DefaultLoopThreshold. Stop ends the consumer and
// unregisters the subscription; it does not stop the observer.
func StartLoopGuard(obs *Observer, stop RunStopper, threshold int, logger *log.Logger) *LoopGuard {
	if threshold <= 0 {
		threshold = DefaultLoopThreshold
	}
	if logger == nil {
		logger = log.Default()
	}
	events, cancel := obs.Subscribe(LoopGuardName, loopGuardBuffer)
	g := &LoopGuard{
		stop:      stop,
		threshold: threshold,
		log:       logger,
		events:    events,
		cancel:    cancel,
		done:      make(chan struct{}),
		counts:    make(map[string]*loopSession),
		now:       time.Now,
	}
	go g.consume()
	return g
}

// Stop ends the guard's consumer and unregisters its subscription. Idempotent.
func (g *LoopGuard) Stop() {
	g.cancel()
	<-g.done
}

func (g *LoopGuard) consume() {
	defer close(g.done)
	lastPrune := g.now()
	for ev := range g.events {
		if ev.Kind == KindTool {
			g.check(ev)
		}
		if now := g.now(); now.Sub(lastPrune) >= loopPruneInterval {
			g.prune(now)
			lastPrune = now
		}
	}
}

// check tallies one tool call and kills the run when its session has now made
// the threshold-th identical call. The run is killed exactly once per shape:
// later identical calls raise the count past the threshold without
// re-triggering.
func (g *LoopGuard) check(ev Event) {
	scope := ev.Harness + "\x00" + ev.Session.ID
	if ev.Session.ID == "" {
		scope = ev.Harness + "\x00" + ev.Session.Key
	}
	sess, ok := g.counts[scope]
	if !ok {
		sess = &loopSession{counts: make(map[string]int)}
		g.counts[scope] = sess
	}
	shape := loopShape(ev)
	sess.counts[shape]++
	sess.lastSeen = g.now()

	if sess.counts[shape] != g.threshold {
		return
	}
	g.log.Error("runaway tool loop detected; killing run",
		"harness", ev.Harness,
		"session", ev.Session.ID,
		"tool", ev.Tool.Tool,
		"action", ev.Tool.Action,
		"identical_calls", g.threshold,
	)
	if !g.stop.Stop(ev.Harness) {
		g.log.Warn("runaway loop guard could not stop the run",
			"harness", ev.Harness,
			"identical_calls", g.threshold,
		)
	}
}

// prune drops sessions idle past loopSessionTTL, bounding the guard's memory
// over a daemon's lifetime.
func (g *LoopGuard) prune(now time.Time) {
	for scope, sess := range g.counts {
		if now.Sub(sess.lastSeen) >= loopSessionTTL {
			delete(g.counts, scope)
		}
	}
}

// loopShape fingerprints one tool call from its classified, redacted fields —
// the parts derived from the call's arguments. Result-side fields are
// deliberately excluded: a loop that alternates error and success results is
// still one loop.
func loopShape(ev Event) string {
	h := sha256.New()
	writePart := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	writePart(ev.Tool.Tool, ev.Tool.Action, ev.Tool.Summary)
	for _, t := range ev.Tool.Targets {
		writePart(t.Path, t.Touch)
	}
	for _, o := range ev.Tool.Outside {
		writePart(o.Scope, o.Path)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
