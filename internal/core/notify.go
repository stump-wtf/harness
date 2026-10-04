package core

// Notify Configuration
//
// The parsed [notify] table: one operator-owned program the daemon runs when a
// harness needs a human — it gave up into `failed`, is crash-looping, was
// stopped by the runaway tool-loop guard, or had its session rotated. The
// program is a daemon-side hook, not a supervised harness: it gets no PTY, no
// restart policy, no run record and no trace, and it runs only when the daemon
// has something to say. ADR-0033's rule that harness supervises agents and not
// arbitrary processes is about harnesses; this is the daemon's own outbound
// alert, configured once, globally, by the operator.
//
// Governing: SPEC-0003 REQ "Operator Notification"; issue #725.

import (
	"slices"
	"time"
)

// Notify event names: the values of `events`, HARNESS_NOTIFY_EVENT and the
// payload's "event".
const (
	// NotifyFailed fires when a harness gives up into `failed` (SPEC-0003
	// REQ "Backoff Give-Up"). Never for a triggered harness: its failed run
	// lands it in `failed`, but its next firing retries it, so nothing gave
	// up; run_failed reports that run.
	NotifyFailed = "failed"
	// NotifyFlapping fires when crash-loop backoff escalates.
	NotifyFlapping = "flapping"
	// NotifyLoopStopped fires when the runaway tool-loop guard stops a
	// harness. The stop clears the harness's enabled intent, so it stays
	// down until someone starts it.
	NotifyLoopStopped = "loop_stopped"
	// NotifySessionRotated fires when the session guard rotates a harness
	// whose session is wedged on context-limit errors.
	NotifySessionRotated = "session_rotated"
	// NotifyRunFailed fires when a scheduled or triggered run ends failed
	// or timed out. Opt-in: a job that fails on purpose would otherwise page.
	NotifyRunFailed = "run_failed"
	// NotifyIntentLost fires when a daemon boot finds an autostart member
	// left down by a persisted enabled=false (issue #835): the intent
	// flipped at some point without notice, and a later restart made the
	// change stick.
	NotifyIntentLost = "intent_lost"
	// NotifyParked fires once per park, when a harness is parked on an
	// exhausted provider quota (SPEC-0021 REQ-13): its runs are refused
	// until the reset, and it is released by itself then. A parked run ends
	// quota_parked rather than failed, so neither `failed` nor `run_failed`
	// would ever say so.
	NotifyParked = "parked"
	// NotifyRecovered fires when a harness that was reported failed or
	// loop-stopped is running again, so the alert thread can close.
	NotifyRecovered = "recovered"
	// NotifyTest is what `harness doctor --notify-test` sends. It is never
	// listed in `events`; it always runs.
	NotifyTest = "test"
)

// NotifyEvents is every event `events` may name, in documentation order.
var NotifyEvents = []string{
	NotifyFailed, NotifyFlapping, NotifyLoopStopped, NotifySessionRotated, NotifyRunFailed, NotifyIntentLost, NotifyParked, NotifyRecovered,
}

// DefaultNotifyEvents is `events` when the table omits it: everything that
// means a harness needs a human, plus the recovery that closes it. run_failed
// is left out because a scheduled job's failure already lands in `harness
// jobs`, and some jobs fail as their normal "nothing to do" answer. parked is
// in: a provider refusing an account for quota or credits is usually
// something only a person can top up.
var DefaultNotifyEvents = []string{
	NotifyFailed, NotifyFlapping, NotifyLoopStopped, NotifySessionRotated, NotifyIntentLost, NotifyParked, NotifyRecovered,
}

// Notify defaults and bounds.
const (
	DefaultNotifyTimeout  = 15 * time.Second
	MinNotifyTimeout      = time.Second
	MaxNotifyTimeout      = 5 * time.Minute
	DefaultNotifyCooldown = 15 * time.Minute
	MaxNotifyCooldown     = 24 * time.Hour
)

// NotifyConfig is the global [notify] table. The zero value is off.
type NotifyConfig struct {
	// Command is the hook's argv: Command[0] is an absolute path, exec'd
	// without a shell. Empty means notify is off.
	Command []string
	// Events is the set of events that run the hook.
	Events []string
	// Timeout bounds one delivery; the hook's process group is killed when
	// it passes.
	Timeout time.Duration
	// Cooldown suppresses a repeat of the same (harness, event) inside the
	// window. Zero disables de-duplication.
	Cooldown time.Duration
}

// Enabled reports whether a hook is configured.
func (n NotifyConfig) Enabled() bool { return len(n.Command) > 0 }

// Wants reports whether event runs the hook. The test event always does.
func (n NotifyConfig) Wants(event string) bool {
	return n.Enabled() && (event == NotifyTest || slices.Contains(n.Events, event))
}

// Equal reports whether two tables configure the same hook.
func (n NotifyConfig) Equal(o NotifyConfig) bool {
	return slices.Equal(n.Command, o.Command) && slices.Equal(n.Events, o.Events) &&
		n.Timeout == o.Timeout && n.Cooldown == o.Cooldown
}
