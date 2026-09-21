// Turn State Watcher
//
// The daemon-side live turn state graceful shutdown closes on (SPEC-0012 REQ
// "Turn State Signal"): for every followed harness it keeps the time of the
// latest attributed trace event, whether the reader reports turn boundaries for
// the agent, and, when it does, whether the latest turn has ended.
//
// It follows only harnesses a graceful close is waiting on, or that are within
// a minute of one, so the rest of the day costs no trace I/O at all; the
// attribution rule is Attribute's, extended with the resumed-session credit
// SPEC-0006 REQ "Run Correlation" adds, so turn state can never come from a
// session `harness logs` would refuse, and a session another harness could
// have written contributes nothing.
//
// Turn markers: no agent-trace reader reports turn boundaries yet —
// claude-code's stop_reason, codex's task_complete and crush's finish part are
// all unparsed as of v0.2.1 — so TurnMarkers is false today and every close
// falls back to the quiet period (design.md § Risks). The gap is filed as
// stump.wtf/agent-trace#102; when a reader ships the boundary, derive
// TurnMarkers/TurnEnded in sample from the boundary mark and the closes light
// up without further caller changes.
//
// Governing: ADR-0019 (operating hours), SPEC-0012 REQ "Turn State Signal",
// REQ "Graceful Shutdown"; SPEC-0006 REQ "Run Correlation", REQ "Live Turn
// State"; design.md § "Turn state from a daemon-side watcher".
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#384.

package runtrace

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/stump-wtf/agent-trace/tail"
)

// TurnState is one harness's live agent-activity signal, sampled from its
// attributed sessions. LastEventAt is zero when the run has no in-window event
// yet; TurnEnded is meaningful only when TurnMarkers.
type TurnState struct {
	LastEventAt time.Time
	TurnMarkers bool
	TurnEnded   bool
}

const (
	// refreshTimeout bounds one attribution pass: local SQLite and JSONL
	// reads, but a store locked by a wedged writer must not wedge the
	// follower.
	refreshTimeout = 10 * time.Second

	// DefaultWatchPoll is how often a follower re-reads its attributed
	// sessions. The close decision it feeds runs on a one-second tick; a
	// slower sample would make the settle and quiet windows lag, a faster
	// one burns reads for nothing.
	DefaultWatchPoll = 2 * time.Second

	// lingerAfterClose is how long a follower keeps polling past the close
	// it was armed for when no close ever begins — an operator stop or a
	// reload removed the reason — before retiring itself. The manager
	// unfollows explicitly on every path it sees; this is the backstop for
	// the paths it does not.
	lingerAfterClose = 5 * time.Minute
)

// Watcher is the daemon-owned turn-state watch. Each followed harness gets a
// polling loop; nothing else here reads trace stores, so a watcher with no
// followers performs no trace I/O — the property SPEC-0012's acceptance
// criteria assert.
type Watcher struct {
	poll time.Duration

	mu        sync.Mutex
	followers map[string]*follower
}

// follower is one harness's watch: the correlation inputs frozen at arm time
// and the freshest turn state the loop has sampled.
type follower struct {
	target  Scope
	window  Window
	peers   []Scope
	closeAt time.Time

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	state TurnState
	ok    bool
	why   string
}

// NewWatcher starts a watcher whose followers sample every poll. poll <= 0
// disables the background loop entirely: tests then drive Refresh by hand,
// which keeps turn-state tests off the clock.
func NewWatcher(poll time.Duration) *Watcher {
	return &Watcher{poll: poll, followers: map[string]*follower{}}
}

// Follow arms the watch for name, freezing the correlation inputs — the
// harness's own scope, its run's window, and the peer scopes that make a
// session ambiguous. closeAt is the instant the harness goes out of hours,
// which bounds the follower's linger. Following an already-followed name
// re-arms in place. The first sample is taken synchronously, so the first
// close decision after the arm already has something to read.
func (w *Watcher) Follow(name string, target Scope, window Window, peers []Scope, closeAt time.Time) {
	w.mu.Lock()
	if old, ok := w.followers[name]; ok {
		old.cancel()
		delete(w.followers, name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &follower{target: target, window: window, peers: peers, closeAt: closeAt, ctx: ctx, cancel: cancel}
	w.followers[name] = f
	w.mu.Unlock()

	f.refresh() // synchronous first sample
	if w.poll <= 0 {
		return // manual mode: the caller drives every further refresh
	}
	go w.loop(f)
}

// Unfollow tears name's watch down. Unknown names are fine.
func (w *Watcher) Unfollow(name string) {
	w.mu.Lock()
	f, ok := w.followers[name]
	if ok {
		delete(w.followers, name)
	}
	w.mu.Unlock()
	if ok {
		f.cancel()
	}
}

// Turn reports name's freshest turn state, or ok=false when the watch has
// nothing attributable to the run — no session at all, or only ones
// correlation excludes. Unavailable names the reason for the caller's log.
func (w *Watcher) Turn(name string) (TurnState, bool) {
	f := w.get(name)
	if f == nil {
		return TurnState{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.ok
}

// Unavailable returns why the last Turn for name came back false. Empty when
// the last sample was good or the harness is not followed.
func (w *Watcher) Unavailable(name string) string {
	f := w.get(name)
	if f == nil {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.why
}

// Refresh re-samples name now. The follower loops call it on their own
// cadence; tests in manual mode (poll <= 0) call it to move the world
// forward. Unknown names are a no-op.
func (w *Watcher) Refresh(name string) {
	if f := w.get(name); f != nil {
		f.refresh()
	}
}

func (w *Watcher) get(name string) *follower {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.followers[name]
}

// loop samples on the watcher's cadence until the follower is cancelled — or,
// as a backstop, until lingerAfterClose past the close it armed for, in case
// no one ever unfollows.
func (w *Watcher) loop(f *follower) {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	deadline := f.closeAt.Add(lingerAfterClose)
	for {
		select {
		case <-f.ctx.Done():
			return
		case <-ticker.C:
			if !time.Now().Before(deadline) {
				w.Unfollow(f.target.Name)
				return
			}
			f.refresh()
		}
	}
}

// refresh re-attributes the run and stores the freshest turn state.
func (f *follower) refresh() {
	ctx, cancel := context.WithTimeout(f.ctx, refreshTimeout)
	defer cancel()
	state, why, ok := f.sample(ctx)
	f.mu.Lock()
	f.state, f.why, f.ok = state, why, ok
	f.mu.Unlock()
}

// sample is one attribution pass over the frozen inputs: the run's ordinary
// sessions through Attribute, plus any session the resumed-session rule
// credits, reduced to a single TurnState.
func (f *follower) sample(ctx context.Context) (TurnState, string, bool) {
	var zero TurnState
	now := time.Now()
	att, err := Attribute(ctx, f.target, f.window, f.peers, now)
	if err != nil {
		return zero, err.Error(), false
	}
	sessions := att.Sessions
	if len(sessions) == 0 {
		// Nothing attributable the ordinary way. A session that started
		// before the run but was resumed by it (claude-code --continue,
		// crush's stored session) still credits its in-window events to
		// this run — SPEC-0006 REQ "Run Correlation", resumed sessions.
		resumed, rerr := f.resumed(ctx, now)
		if rerr != nil {
			return zero, rerr.Error(), false
		}
		sessions = resumed
	}
	if len(sessions) == 0 {
		return zero, "no agent-trace session is attributable to this run", false
	}
	return TurnState{LastEventAt: f.latestEvent(ctx, sessions, now)}, "", true
}

// latestEvent is the newest in-window event across sessions. A session that
// fails to parse hides no others. Zero when none of them has an in-window
// event yet — the close reads that as maximally quiet.
func (f *follower) latestEvent(ctx context.Context, sessions []Session, now time.Time) time.Time {
	var latest time.Time
	for _, s := range sessions {
		entries, err := sessionEntries(ctx, s, f.window, now)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Time.After(latest) {
				latest = e.Time
			}
		}
	}
	return latest
}

// resumed finds sessions that started before the run but were resumed by it,
// crediting them per SPEC-0006's resumed-session rule: the session is in the
// harness's own store (by construction — it is listed from this scope's own
// sources), its cwd is the harness's workdir, and no other harness could have
// written events at those times. That last check is applied at the window's
// start, the earliest moment a resumed session could contribute, and a peer
// that could have been running then excludes the session: a resumed session
// with a sibling is credited to no one, exactly as a fresh one would be.
func (f *follower) resumed(ctx context.Context, now time.Time) ([]Session, error) {
	work := clean(f.target.Workdir)
	if work == "" || f.window.Start.IsZero() {
		return nil, nil
	}
	sources, err := Sources(f.target)
	if err != nil {
		return nil, err
	}
	lo, _ := f.window.bounds(now)
	targetStore := Store(f.target)
	peerStores := make([]string, len(f.peers))
	for i, p := range f.peers {
		peerStores[i] = Store(p)
	}
	var out []Session
	for _, a := range sources {
		metas, err := tail.ListSessionsFiltered(ctx, a, tail.SessionFilter{Cwd: work})
		if err != nil {
			return nil, fmt.Errorf("runtrace: list %s sessions for %s: %w", f.target.Adapter, f.target.Name, err)
		}
		for _, m := range metas {
			started, ok := m.Started()
			if !ok || !started.Before(f.window.Start) || clean(m.Cwd) != work {
				continue
			}
			if names := claimants(f.target, targetStore, f.peers, peerStores, work, lo, now); len(names) > 1 {
				continue // ambiguous at the contribution window: credited to no one
			}
			out = append(out, Session{Meta: m, Started: started, adapter: a})
		}
	}
	return out, nil
}
