// Package stallguard recovers a persistent harness that is alive and making no
// progress.
//
// # The Stall Watchdog
//
// `restart` fires on process exit, so a worker that stays alive and blocks —
// inside a tool call, on a hung child, on a network call to a peer that stopped
// answering — is never recovered, and `harness list` keeps reporting `running`
// (issue #959). The run `timeout` cannot cover it either: applied to a
// long-lived worker it is a wall-clock ceiling that kills healthy agents on a
// schedule, and not applied it never notices a wedge.
//
// So this guard measures progress instead of wall time. "Activity" is the same
// signal `harness_last_successful_call_timestamp` exports (issue #958 puts it
// on list/describe): a classified tool call delivered by the agent-event
// observer, which is what an agent does to affect the world. A task that runs
// for nine hours and calls tools all nine hours never trips it; a two-hour
// block does.
//
// The stall clock for a harness runs from the later of its current process's
// start and its last observed call, so a worker that has not been asked to do
// anything yet is not accused of stalling at boot. A harness the observer never
// reports on — a `generic` command with no transcripts — therefore has no
// progress signal at all, and its clock runs from process start alone. That is
// the operator asking for a wall-clock restart period by another name, which is
// why `RestartLimit` exists: after that many consecutive stalls with no observed
// call between them the guard stops restarting and only reports, because a
// worker that wedges before it ever calls a tool is not being fixed by another
// restart. (Crash-loop give-up cannot be that bound: a stall that follows hours
// of good work is not a crash, and the existing latch correctly refuses to
// treat it as one.)
//
// Each stall episode is acted on once per process. Without that, a `notify`
// harness would page every interval and a `restart` harness would be killed
// again the moment the guard next looked. The episode ends when the process is
// replaced (a new start time) or when progress resumes.
//
// Governing: ADR-0045; issue #959.
package stallguard

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/proctree"
	"github.com/stump-wtf/harness/internal/supervisor"
)

const (
	// DefaultInterval is how often the guard samples. It is the blind window:
	// a harness can be stalled for up to this long before anyone notices, and
	// sampling more often than a minute buys nothing against a timeout
	// measured in tens of minutes.
	DefaultInterval = time.Minute
	// DefaultRestartLimit matches the supervisor's crash-loop give-up
	// (Policy.MaxRestarts): the same "we have tried this many times" bound,
	// applied to the recovery path that latch cannot see.
	DefaultRestartLimit = 5
	// DefaultTreeLimit caps how many processes one stall dump names.
	DefaultTreeLimit = proctree.DefaultLimit
	// subscriberName and subscriberBuffer are the observer subscription.
	// Losing events to a full buffer can only delay a finding: an event that
	// never arrives leaves the progress clock where it was, which the guard
	// reads as idle, and the episode latch keeps one finding from acting
	// twice.
	subscriberName   = "stallguard"
	subscriberBuffer = 256
)

// Subscriber is the observer surface the guard reads. *observe.Observer
// satisfies it.
type Subscriber interface {
	Subscribe(name string, buf int) (<-chan observe.Event, func())
}

// Supervised is the Manager surface the guard acts through. *supervisor.Manager
// satisfies it.
type Supervised interface {
	Snapshots() []supervisor.Snapshot
	HarnessRecord(name string) (core.Harness, string, bool)
	// ReportStall hands the finding to the harness's own actor goroutine,
	// naming the process start it was measured against.
	ReportStall(name string, startedAt time.Time) bool
	LogLifecycle(name, msg string, kv ...any) bool
}

// Options configures a Guard. The zero value is production.
type Options struct {
	// Interval is the sampling period (default DefaultInterval).
	Interval time.Duration
	// RestartLimit is how many consecutive stalls of one harness may restart
	// it before the guard gives up on that and reports only (default
	// DefaultRestartLimit; zero or negative takes the default).
	RestartLimit int
	// Logger receives the WARN line (default log.Default()).
	Logger *log.Logger
	// Now is the clock (default time.Now); tests inject it.
	Now func() time.Time
	// OnStall, when set, is called once per stall episode after it is logged
	// and acted on — the daemon's notify hook (SPEC-0003 REQ "Operator
	// Notification"). `stall_action = "none"` never reaches it: silence is
	// what that value asks for. It runs on the guard's goroutine and must not
	// block.
	OnStall func(Stall)
}

// Stall is one stall episode the guard acted on.
type Stall struct {
	Harness      string
	Idle         time.Duration
	StallTimeout time.Duration
	Action       string
	PID          int
	// Tree is the process tree at the moment of the stall, root first, so the
	// post-mortem says what the worker was stuck on. Empty when the process
	// table could not be read.
	Tree []proctree.Line
	// Restarts counts this harness's consecutive stall restarts, this one
	// included.
	Restarts int
	// GaveUp marks the restart limit reached: the guard stopped reviving this
	// harness and is reporting only.
	GaveUp bool
	// Reported marks a finding the Manager refused because the process it
	// measured is already gone.
	Reported bool
}

// String renders the stall's cause for a log line or a notify payload.
func (s Stall) Cause() string {
	return "no successful model call for " + s.Idle.Round(time.Second).String() +
		" (stall_timeout " + s.StallTimeout.String() + ")"
}

// TreeText renders the process tree for a log line or a notify payload.
func (s Stall) TreeText() string {
	parts := make([]string, 0, len(s.Tree))
	for _, l := range s.Tree {
		parts = append(parts, l.String())
	}
	return strings.Join(parts, "; ")
}

// watch is the guard's state for one harness.
type watch struct {
	// lastCall is the last observed successful call; zero until the first.
	lastCall time.Time
	// actedOn is the start time of the process this episode was acted on;
	// zero when nothing has been acted on.
	actedOn time.Time
	// restarts counts consecutive stalls with no observed call between them.
	restarts int
	gaveUp   bool
}

// Guard watches persistent harnesses for lost progress. Create it with New and
// run it with Start.
type Guard struct {
	sup          Supervised
	interval     time.Duration
	restartLimit int
	log          *log.Logger
	now          func() time.Time
	onStall      func(Stall)

	mu      sync.Mutex
	watches map[string]*watch

	events <-chan observe.Event
	unsub  func()
	stop   chan struct{}
	done   chan struct{}
}

// New builds a guard over the Manager. Run it with Start, which subscribes it
// to the observer.
func New(sup Supervised, opts Options) *Guard {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	limit := opts.RestartLimit
	if limit <= 0 {
		limit = DefaultRestartLimit
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Guard{
		sup:          sup,
		interval:     interval,
		restartLimit: limit,
		log:          logger,
		now:          now,
		onStall:      opts.OnStall,
		watches:      map[string]*watch{},
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
}

// Start subscribes to the observer and launches the sampling loop. A nil
// observer means nothing has a progress signal yet: the guard still runs, and a
// harness it never reports on is read as idle, which is the honest reading of a
// worker that has called nothing.
func (g *Guard) Start(obs Subscriber) {
	if obs != nil {
		g.events, g.unsub = obs.Subscribe(subscriberName, subscriberBuffer)
	}
	go g.loop()
}

// Close stops the loop, waits for it, and drops the observer subscription.
func (g *Guard) Close() {
	close(g.stop)
	<-g.done
	if g.unsub != nil {
		g.unsub()
	}
}

// Restarts reports the guard's consecutive stall restarts for one harness, for
// tests and for a describe surface that wants the number.
func (g *Guard) Restarts(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if w := g.watches[name]; w != nil {
		return w.restarts
	}
	return 0
}

func (g *Guard) loop() {
	defer close(g.done)
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
			g.sample()
		case ev, ok := <-g.events:
			if !ok {
				g.events = nil // subscription closed; sampling continues
				continue
			}
			g.note(ev)
		}
	}
}

// note records observed progress: a tool call is the signal the metric exports
// as a successful call, and it is also what releases the restart latch — a
// worker that called a tool since its last stall is making progress, however it
// got there.
func (g *Guard) note(ev observe.Event) {
	if ev.Kind != observe.KindTool {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	w := g.watches[ev.Harness]
	if w == nil {
		w = &watch{}
		g.watches[ev.Harness] = w
	}
	if ev.Time.After(w.lastCall) {
		w.lastCall = ev.Time
	}
	w.restarts = 0
	w.gaveUp = false
}

// sample measures every harness the watchdog is configured for. Only a RUNNING
// harness can be wedged: anything else is down, starting, or being stopped, and
// says so.
func (g *Guard) sample() {
	now := g.now()
	for _, snap := range g.sup.Snapshots() {
		if snap.State != core.StateRunning || snap.Triggered {
			continue
		}
		h, _, ok := g.sup.HarnessRecord(snap.Name)
		if !ok || h.StallTimeout <= 0 {
			continue
		}
		g.measure(snap, h, now)
	}
}

// measure decides one harness's episode edge: idle past the timeout and not yet
// acted on for this process means act; anything else closes the episode.
func (g *Guard) measure(snap supervisor.Snapshot, h core.Harness, now time.Time) {
	g.mu.Lock()
	w := g.watches[snap.Name]
	if w == nil {
		w = &watch{}
		g.watches[snap.Name] = w
	}
	idle := now.Sub(snap.LastStarted)
	if w.lastCall.After(snap.LastStarted) {
		idle = now.Sub(w.lastCall)
	}
	if idle < h.StallTimeout {
		w.actedOn = time.Time{}
		g.mu.Unlock()
		return
	}
	if !w.actedOn.IsZero() && w.actedOn.Equal(snap.LastStarted) {
		g.mu.Unlock()
		return // this episode is already acted on
	}
	w.actedOn = snap.LastStarted
	action := h.StallAction
	if action == "" {
		action = core.DefaultStallAction
	}
	st := Stall{
		Harness:      snap.Name,
		Idle:         idle,
		StallTimeout: h.StallTimeout,
		Action:       action,
		PID:          snap.PID,
	}
	if action == core.StallRestart {
		if w.gaveUp {
			st.GaveUp = true
		} else {
			w.restarts++
			st.Restarts = w.restarts
			if w.restarts >= g.restartLimit {
				w.gaveUp = true
			}
		}
	}
	g.mu.Unlock()

	g.act(snap, st)
}

// act logs the episode with the process tree, then does what `stall_action`
// asked for. The tree is captured before the restart because after it there is
// nothing left to ask.
func (g *Guard) act(snap supervisor.Snapshot, st Stall) {
	if st.PID > 0 {
		tree, err := proctree.Tree(st.PID, DefaultTreeLimit)
		if err != nil {
			g.log.Debug("stall post-mortem: process tree unavailable", "harness", st.Harness, "pid", st.PID, "err", err)
		} else {
			st.Tree = tree
		}
	}
	g.log.Warn("harness stalled", "harness", st.Harness, "state", string(snap.State),
		"idle", st.Idle.Round(time.Second).String(), "stall_timeout", st.StallTimeout.String(),
		"action", st.Action, "restarts", st.Restarts, "gave_up", st.GaveUp)
	if len(st.Tree) > 0 {
		g.log.Warn("stall process tree", "harness", st.Harness, "tree", st.TreeText())
	}
	msg := "stalled: " + st.Cause()
	switch st.Action {
	case core.StallRestart:
		switch {
		case st.GaveUp:
			msg += " — restart limit reached, not restarting again; this worker needs a human"
		case st.Restarts > 0:
			msg += " — restarting it (stall " + strconv.Itoa(st.Restarts) + " in a row)"
		}
		if !st.GaveUp {
			st.Reported = g.sup.ReportStall(st.Harness, snap.LastStarted)
			if !st.Reported {
				msg += " — the harness was already gone before the restart landed"
			}
		}
	case core.StallNotify:
		msg += " — left running; `stall_action = \"restart\"` would revive it"
	}
	g.sup.LogLifecycle(st.Harness, msg, "idle", st.Idle.Round(time.Second).String())

	if g.onStall != nil && st.Action != core.StallNone {
		g.onStall(st)
	}
}
