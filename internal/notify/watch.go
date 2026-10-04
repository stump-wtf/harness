package notify

// Event Sources
//
// The Watcher turns what the daemon already knows into Notifications. Most of
// it is on the Manager's lifecycle bus: a transition into `failed` is the
// give-up, harness_flapping is the crash loop escalating, a finished run with
// a failed outcome is run_failed, and a transition back to `running` after an
// alert is `recovered`. The two guards stop and restart harnesses from outside
// the state machine and publish nothing of their own on the bus, so they call
// in directly: LoopStopped from the loop guard's OnTrip, SessionRotated from
// the session guard's OnRotate.
//
// A triggered harness is the exception to "a transition into `failed` is the
// give-up". A one-shot whose run fails lands in `failed`, and its next firing
// is the retry (SPEC-0008 REQ "Firing And Overlap"): nothing gave up, no
// restart is owed, and the restart loop's consecFailures it would quote is
// never counted for it. Its failures are run_failed's, which carries the
// run-ledger streak `harness jobs` shows. Treating the state as a give-up told
// an operator a review lane on tars "gave up after 0 consecutive failures",
// latched until a human cleared it, every ten minutes for ten hours, while the
// schedule kept firing it and each next start announced `recovered`.
//
// A park is announced from harness_hold_changed, the one signal every kind of
// park raises — a one-shot's run that ended on a quota error, a resident's
// exit or live refusal, a quota group's members: once when `quota` joins the
// harness's hold reasons, and not again until it has left them. A parked run
// ends quota_parked, not failed, so without this the park that stopped a lane
// would be the one thing the operator never heard about.
//
// The bus is lossy by design (ADR-0007), and so is this subscriber: a
// notification lost to a full buffer is a missed alert, never a false one.
// The per-harness log read that finds the cause happens here, on the
// watcher's goroutine, never on a supervisor's.
//
// Governing: SPEC-0003 REQ "Operator Notification", REQ "Lifecycle Events";
// issue #725; SPEC-0021 REQ-13, REQ-19 (parked).
//
// @joestump 10/04/2026 - A triggered harness's `failed` is not a give-up;
//   run_failed quotes the run-ledger streak.
// @joestump 10/04/2026 - parked, from harness_hold_changed.

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/loopguard"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// Source is what the Watcher reads from the supervisor. *supervisor.Manager
// satisfies it.
type Source interface {
	Events() (<-chan supervisor.Event, func())
	Snapshot(name string) (supervisor.Snapshot, bool)
	LogDir() string
	RunLogPath(name string, id int) string
	// Runs is name's run ledger, oldest first: run_failed's streak.
	Runs(name string) []supervisor.RunRecord
	// ParkOf is name's quota park: its reset, rule and group.
	ParkOf(name string) (supervisor.ParkInfo, bool)
}

// Watcher feeds a Dispatcher from the lifecycle bus and the guards.
type Watcher struct {
	src Source
	d   *Dispatcher

	mu sync.Mutex
	// open holds harnesses with an alert out — failed or loop-stopped — so
	// their next transition to running can say `recovered`.
	open map[string]string
	// parked holds harnesses whose park was announced, until it clears.
	parked map[string]bool

	cancel func()
	done   chan struct{}
}

// Watch subscribes a Watcher to src's lifecycle bus. Call it before
// Autostart, so a harness that fails during boot is seen.
func Watch(src Source, d *Dispatcher) *Watcher {
	events, cancel := src.Events()
	w := &Watcher{src: src, d: d, open: make(map[string]string), parked: make(map[string]bool), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for ev := range events {
			w.handle(ev)
		}
	}()
	return w
}

// Close unsubscribes and waits for the consumer to finish. Idempotent.
func (w *Watcher) Close() {
	w.mu.Lock()
	cancel := w.cancel
	w.cancel = nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		<-w.done
	}
}

func (w *Watcher) handle(ev supervisor.Event) {
	switch ev.Kind {
	case supervisor.EventStateChanged:
		switch ev.To {
		case core.StateFailed:
			w.failed(ev)
		case core.StateRunning:
			w.running(ev)
		}
	case supervisor.EventFlapping:
		w.flapping(ev)
	case supervisor.EventRunFinished:
		w.runFinished(ev)
	case supervisor.EventHoldChanged:
		w.holdChanged(ev)
	}
}

// holdChanged announces a park once: when quota joins name's hold reasons.
// Its leaving re-arms the announcement for the next park.
func (w *Watcher) holdChanged(ev supervisor.Event) {
	parked := ev.Holds.Has(core.HoldQuota)
	w.mu.Lock()
	was := w.parked[ev.Name]
	if parked {
		w.parked[ev.Name] = true
	} else {
		delete(w.parked, ev.Name)
	}
	w.mu.Unlock()
	if !parked || was {
		return
	}
	p, ok := w.src.ParkOf(ev.Name)
	until := p.Until
	if !ok || until.IsZero() {
		until = ev.HoldNext
	}
	msg := ev.Name + " parked"
	if !until.IsZero() {
		msg += " until " + until.Local().Format("Jan 2 15:04 MST")
	}
	msg += ": its provider refused it for quota"
	if p.Rule != "" {
		msg += " (" + p.Rule + ")"
	}
	if p.Group != "" {
		msg += fmt.Sprintf(", which parks quota group %s", p.Group)
		if p.By != "" && p.By != ev.Name {
			msg += " after " + p.By + " was refused"
		}
	}
	msg += ". Its runs are skipped until then and it is released by itself — see `harness logs " + ev.Name + "`"
	snap, _ := w.src.Snapshot(ev.Name)
	w.send(Notification{
		Event: core.NotifyParked, Harness: ev.Name, State: string(snap.State),
		Message: msg,
		Cause:   p.Rule,
		Hint:    "harness logs " + ev.Name,
		Time:    ev.Time,
		Until:   until,
	}, false)
}

func (w *Watcher) failed(ev supervisor.Event) {
	snap, _ := w.src.Snapshot(ev.Name)
	if snap.Triggered {
		return // its next firing retries; run_failed reported this run
	}
	cause := w.lastLine(ev.Name)
	code := snap.LastExitCode
	msg := fmt.Sprintf("%s failed: gave up after %d consecutive failures (last exit %d)", ev.Name, snap.ConsecutiveFailures, code)
	if snap.ConsecutiveFailures == 0 {
		// Not a give-up: a command that never came up under a restart
		// policy that does not retry it lands here with no streak.
		msg = fmt.Sprintf("%s failed and will not restart on its own (last exit %d)", ev.Name, code)
	}
	w.send(Notification{
		Event: core.NotifyFailed, Harness: ev.Name, State: string(core.StateFailed),
		Message:  withCause(msg, cause) + " — restart with `harness restart " + ev.Name + "`; see `harness logs " + ev.Name + "`",
		Cause:    cause,
		Hint:     "harness logs " + ev.Name,
		Time:     ev.Time,
		ExitCode: &code,
		Restarts: snap.RestartCount,
	}, true)
}

func (w *Watcher) running(ev supervisor.Event) {
	w.mu.Lock()
	was, ok := w.open[ev.Name]
	delete(w.open, ev.Name)
	w.mu.Unlock()
	if !ok {
		return
	}
	w.send(Notification{
		Event: core.NotifyRecovered, Harness: ev.Name, State: string(core.StateRunning),
		Message: fmt.Sprintf("%s is running again (was %s)", ev.Name, strings.ReplaceAll(was, "_", "-")),
		Cause:   was,
		Hint:    "harness describe " + ev.Name,
		Time:    ev.Time,
	}, false)
}

func (w *Watcher) flapping(ev supervisor.Event) {
	snap, _ := w.src.Snapshot(ev.Name)
	cause := w.lastLine(ev.Name)
	code := snap.LastExitCode
	msg := fmt.Sprintf("%s is crash-looping: %d restarts, next retry in %s (last exit %d)",
		ev.Name, ev.Restarts, ev.NextRetryIn.Round(time.Second), code)
	w.send(Notification{
		Event: core.NotifyFlapping, Harness: ev.Name, State: string(snap.State),
		Message:  withCause(msg, cause) + " — see `harness logs " + ev.Name + "`",
		Cause:    cause,
		Hint:     "harness logs " + ev.Name,
		Time:     ev.Time,
		ExitCode: &code,
		Restarts: ev.Restarts,
	}, false)
}

func (w *Watcher) runFinished(ev supervisor.Event) {
	r := ev.Run
	if r.Outcome != supervisor.OutcomeFailed && r.Outcome != supervisor.OutcomeTimedOut {
		return
	}
	path := r.Log
	if path == "" {
		path = w.src.RunLogPath(ev.Name, r.RunID)
	}
	cause := ""
	if path != "" {
		cause = supervisor.LastOutputLine(path)
	}
	what := "failed"
	if r.Outcome == supervisor.OutcomeTimedOut {
		what = "timed out"
	}
	msg := fmt.Sprintf("%s run #%d %s", ev.Name, r.RunID, what)
	if r.ExitCode != nil {
		msg += fmt.Sprintf(" (exit %d)", *r.ExitCode)
	}
	// The journal closes a run before it is published, so the ledger
	// already holds this one: the same count `harness jobs` shows as FAILS.
	if n := supervisor.ConsecutiveFailures(w.src.Runs(ev.Name)); n > 1 {
		msg += fmt.Sprintf(", %d in a row", n)
	}
	hint := fmt.Sprintf("harness logs %s --run %d", ev.Name, r.RunID)
	w.send(Notification{
		Event: core.NotifyRunFailed, Harness: ev.Name, State: string(r.Outcome),
		Message:  withCause(msg, cause) + " — see `" + hint + "`",
		Cause:    cause,
		Hint:     hint,
		Time:     ev.Time,
		ExitCode: r.ExitCode,
		RunID:    r.RunID,
	}, false)
}

// LoopStopped reports a runaway tool-loop guard stop. It is the loop guard's
// OnTrip.
func (w *Watcher) LoopStopped(t loopguard.Trip) {
	cause := fmt.Sprintf("%s called %d times in a row with identical arguments", t.Tool, t.Count)
	w.send(Notification{
		Event: core.NotifyLoopStopped, Harness: t.Harness, State: string(core.StateStopped),
		Message: fmt.Sprintf("%s stopped by the runaway tool-loop guard: %s — it stays down until `harness start %s`; see `harness logs %s`",
			t.Harness, cause, t.Harness, t.Harness),
		Cause: cause,
		Hint:  "harness logs " + t.Harness,
		Tool:  t.Tool,
		Count: t.Count,
	}, true)
}

// IntentLost reports autostart members a boot left down by persisted intent
// (issue #835): the intent flipped to disabled at some point without a trace,
// and this daemon's restart made the change stick. The daemon's boot path
// calls it directly — a boot finding is not a lifecycle event, so there is
// nothing for the bus watcher to pick up. Each harness gets its own
// notification, and like a give-up it opens the alert: starting the harness
// reports it recovered.
func (w *Watcher) IntentLost(harnesses []string) {
	for _, name := range harnesses {
		w.send(Notification{
			Event:   core.NotifyIntentLost,
			Harness: name,
			State:   string(core.StateStopped),
			Message: fmt.Sprintf("%s left down at boot by persisted intent (autostart member, enabled = false) — start it with `harness start %s`", name, name),
			Cause:   "persisted intent is disabled",
			Hint:    "harness describe " + name,
			Time:    time.Now(),
		}, true)
	}
}

// SessionRotated reports a session guard rotation. It is the session guard's
// OnRotate.
func (w *Watcher) SessionRotated(r supervisor.SessionRotation) {
	cause := fmt.Sprintf("%d of %d recent turns failed on context-limit errors", r.Errors, r.Turns)
	state, msg := string(core.StateRunning), fmt.Sprintf("%s session rotated: %s; the store was archived", r.Harness, cause)
	if r.Archive != "" {
		msg += " to " + filepath.Base(r.Archive)
	}
	if r.Failed != "" {
		state = string(core.StateStopped)
		msg = fmt.Sprintf("%s session rotation failed: %s; %s — start it with `harness start %s`", r.Harness, cause, r.Failed, r.Harness)
	} else {
		msg += " and the harness restarted on a fresh session"
	}
	w.send(Notification{
		Event: core.NotifySessionRotated, Harness: r.Harness, State: state,
		Message: msg + "; see `harness logs " + r.Harness + "`",
		Cause:   cause,
		Hint:    "harness logs " + r.Harness,
	}, r.Failed != "")
}

// send hands n to the dispatcher. opens marks an alert that a later
// transition to running should close with `recovered` — only when the event
// is one the operator asked for, or they would be told of a recovery from
// something they never heard about.
func (w *Watcher) send(n Notification, opens bool) {
	if opens && w.d.Wants(n.Event) {
		w.mu.Lock()
		w.open[n.Harness] = n.Event
		w.mu.Unlock()
	}
	w.d.Notify(n)
}

// lastLine is the last thing name's agent printed to its durable log.
func (w *Watcher) lastLine(name string) string {
	dir := w.src.LogDir()
	if dir == "" {
		return ""
	}
	return supervisor.LastOutputLine(filepath.Join(dir, name+".log"))
}

// withCause appends the quoted cause to msg, when there is one.
func withCause(msg, cause string) string {
	if cause == "" {
		return msg
	}
	return msg + ": " + fmt.Sprintf("%q", cause)
}
