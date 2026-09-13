package supervisor

// SessionGuard — recover a harness whose agent session has outgrown the
// model's context window.
//
// A long-running crush harness keeps one session for its whole life, and
// that session only grows. Past the context window every assistant turn
// fails immediately with a provider context-limit error while the process
// and the daemon report perfectly healthy: the harness accepts events and
// answers none of them. Nothing in the state machine notices, because the
// state machine watches process exit, and the process never exits. A
// restart does not help — crush resumes the oversized session from its
// store. The observed recovery is to move the store aside so the next start
// begins a fresh one.
//
// The guard therefore polls each RUNNING harness's crush store (detection
// lives in internal/sessionguard: a session is stalled when every assistant
// turn inside the lookback window failed with a context-limit error — never
// a message count, because total tokens and not turn count is the variable)
// and rotates on the stalled edge: stop, archive the store (kept, as
// evidence and the only record of what the worker was doing), start. The
// rotation is logged loudly; a self-healing harness that heals invisibly
// just moves the outage somewhere harder to find.
//
// Governing: issue #347.
//
// @joestump-agent 09/13/2026 - Initial guard: detect, rotate, surface on
// Snapshot.

import (
	"sync"
	"time"

	clog "github.com/charmbracelet/log"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/sessionguard"
)

// guardLog is the guard's logger over the daemon log.
var guardLog = clog.New(nil)

const (
	// DefaultSessionGuardInterval is how often the guard samples stores.
	// A wedged worker answers nothing from the moment it wedges, so the
	// interval is the blind window; two minutes bounds it without turning
	// the check into a busy loop over every store.
	DefaultSessionGuardInterval = 2 * time.Minute
	// DefaultSessionGuardLookback is the window of assistant turns the
	// detector classifies. Wide enough to hold several failed turns at the
	// observed failure cadence (immediate errors), narrow enough that a
	// recovered turn ages out.
	DefaultSessionGuardLookback = 15 * time.Minute
)

// sessionFlag is the per-harness observation the guard overlays onto
// Snapshots, so `harness list` can say what process state cannot: this
// harness was accepting events and answering none of them.
type sessionFlag struct {
	stalled   bool // wedged; stays set until a healthy check clears it
	rotations int  // rotations performed this daemon lifetime
}

// SessionGuard periodically samples running crush harnesses for stores
// wedged on context-limit errors and rotates the session when one stalls.
type SessionGuard struct {
	mgr      *Manager
	log      *clog.Logger
	interval time.Duration
	lookback time.Duration

	mu    sync.Mutex
	flags map[string]sessionFlag

	stop chan struct{}
	done chan struct{}
}

// NewSessionGuard builds a guard over mgr; Start runs it. A zero interval
// or lookback takes the default.
func NewSessionGuard(mgr *Manager, interval, lookback time.Duration) *SessionGuard {
	if interval <= 0 {
		interval = DefaultSessionGuardInterval
	}
	if lookback <= 0 {
		lookback = DefaultSessionGuardLookback
	}
	return &SessionGuard{
		mgr:      mgr,
		log:      guardLog,
		interval: interval,
		lookback: lookback,
		flags:    make(map[string]sessionFlag),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start launches the sampling loop.
func (g *SessionGuard) Start() {
	go g.loop()
}

// Close stops the loop and waits for it.
func (g *SessionGuard) Close() {
	close(g.stop)
	<-g.done
}

// Flag reports the guard's observation for one harness (zero value when the
// guard has never flagged it), for Snapshot overlays.
func (g *SessionGuard) Flag(name string) (stalled bool, rotations int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.flags[name]
	return f.stalled, f.rotations
}

func (g *SessionGuard) loop() {
	defer close(g.done)
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
			g.sample()
		}
	}
}

// sample checks every harness the manager knows. A harness that is not
// running cannot be wedged, so only RUNNING states are examined.
func (g *SessionGuard) sample() {
	for _, snap := range g.mgr.Snapshots() {
		g.check(snap)
	}
}

func (g *SessionGuard) check(snap Snapshot) {
	if snap.State != core.StateRunning {
		return
	}
	h, _, ok := g.mgr.HarnessRecord(snap.Name)
	if !ok {
		return
	}
	db := sessionguard.StorePath(Workdir(h))
	if db == "" {
		return
	}
	status, err := sessionguard.Stalled(db, g.lookback, time.Now())
	if err != nil {
		// An unreadable store is not evidence of a wedge: crush holds the
		// DB open and readers busy-time out against it. Skip quietly.
		g.log.Debug("store unreadable; skipping", "harness", snap.Name, "err", err)
		return
	}

	g.mu.Lock()
	flag := g.flags[snap.Name]
	if !status.Stalled {
		if flag.stalled {
			g.log.Info("session healthy again after rotation", "harness", snap.Name)
		}
		delete(g.flags, snap.Name)
		g.mu.Unlock()
		return
	}
	if flag.stalled {
		// Already rotating — or rotated and still wedged inside the same
		// lookback window, where the archived failures still read as
		// recent. Either way our own stale evidence must not rotate again.
		g.mu.Unlock()
		return
	}
	flag.stalled = true
	flag.rotations++
	g.flags[snap.Name] = flag
	g.mu.Unlock()

	g.rotate(snap.Name, db, status)
}

// rotate performs the stop → archive → start cycle once per stall episode.
// Any step failing leaves the harness where it is rather than half-rotated;
// the flag stays set, the warning stands in the log, and the next healthy
// check (or an operator) resolves it.
func (g *SessionGuard) rotate(name, db string, status sessionguard.GuardStatus) {
	g.log.Warn("session wedged on context-limit errors; rotating session",
		"harness", name, "turns", status.Turns, "errors", status.Errors, "store", db)

	if !g.mgr.Stop(name) {
		g.log.Error("could not stop harness for rotation; leaving store untouched", "harness", name)
		return
	}
	archived, err := sessionguard.Archive(db, time.Now())
	if err != nil {
		g.log.Error("could not archive session store; NOT restarting onto the same wedged session", "harness", name, "err", err)
		return
	}
	g.log.Info("session store archived", "harness", name, "archive", archived)

	if !g.mgr.Start(name) {
		g.log.Error("could not restart harness after rotation", "harness", name)
		return
	}
	_, rotations := g.Flag(name)
	g.log.Info("harness restarted on a fresh session", "harness", name, "rotations", rotations)
}
