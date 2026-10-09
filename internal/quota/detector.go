// Package quota is SPEC-0021's park detector: it watches one harness's model
// outcomes and says when the harness should be parked on an exhausted
// provider quota, and for how long.
//
// # The Park Detector
//
// A provider that refuses every call for an exhausted quota cannot be fixed
// by restarting the agent, and restarting it into the same refusal is how a
// resident walked through backoff into `failed` on 2026-09-19, and how a
// scheduled review failed 58 times in a row on 2026-10-04. The detector turns
// what the agent's transcript says (error marks, classified by
// internal/modelerr, and tool calls, each a successful model call) into one
// verdict: park, until when, by which rule.
//
// The rules are SPEC-0021 REQ-12's:
//
//   - a quota error that names a reset time the daemon can parse parks the
//     harness until then (modelerr.ResetAfter clamps it to 8 days and refuses
//     one under a minute away);
//   - a resident is stuck on quota after StuckErrors quota errors with no
//     successful call between them, within StuckWindow of the newest;
//   - a one-shot run is stuck when its last classified model outcome is a
//     quota error;
//   - a stuck harness with no reset time parks for quota_backoff, doubled for
//     each consecutive park without an intervening success, up to
//     quota_backoff_max (Backoff);
//   - a successful call resets both the stuck count and the backoff step, and
//     nothing but the quota class ever parks.
//
// The window is measured on the marks' own clock, between the oldest and the
// newest error it holds, so the rule needs no "now" and reads the same
// whichever goroutine asks.
//
// The package is pure: no clock, no I/O, no locks. The supervisor Manager owns
// one Detector per harness under its own lock, feeds it from the observer's
// "budget" subscription, asks it at every exit before the restart policy
// counts the exit, and applies what it answers (manager_quota.go). Error text
// never leaves it: a Park carries the matched rule's name (ADR-0008).
//
// Governing: ADR-0027; SPEC-0021 REQ-11 "Quota detection", REQ-12 "Parking",
// REQ-13 "Park effects"; design.md § "The park detector".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#477.
package quota

import (
	"slices"
	"time"

	"github.com/stump-wtf/harness/internal/modelerr"
)

const (
	// StuckErrors is how many quota errors, with no success between them,
	// leave a resident stuck on quota (SPEC-0021 REQ-12; design.md "Settled
	// Questions": constants in the first cut).
	StuckErrors = 3
	// StuckWindow is the span those errors must fall within.
	StuckWindow = 10 * time.Minute
	// maxErrs bounds what a detector keeps of a flood of errors: the rule
	// only ever needs the newest StuckErrors, and a few more keep a late,
	// out-of-order mark from evicting one that counts.
	maxErrs = 4 * StuckErrors
)

// Outcome is one model outcome the detector is fed: a successful call (a tool
// event), or an error mark with its class.
type Outcome struct {
	// At is the outcome's own time, from the transcript.
	At time.Time
	// Success is a model call that worked; the fields below are then unset.
	Success bool
	// Class and Rule are what modelerr made of the error's note. Rule is a
	// fixed name, never the note (ADR-0008).
	Class modelerr.Class
	Rule  string
	// Reset is when the note says the quota comes back, zero when it names
	// no usable time; Clamped marks one cut to 8 days.
	Reset   time.Time
	Clamped bool
}

// Success is a successful model call at at.
func Success(at time.Time) Outcome { return Outcome{At: at, Success: true} }

// ErrorOutcome classifies an error mark's note with the one classifier
// (SPEC-0021 REQ-11) and, for the quota class only, reads the reset time it
// names as of now. now is the decision clock the reset is judged against: a
// relative "try again in 2h" counts from it, and a clock time with no zone is
// read in its location.
func ErrorOutcome(adapter, note string, at, now time.Time) Outcome {
	class, rule, _ := modelerr.ClassifyRule(adapter, note)
	o := Outcome{At: at, Class: class, Rule: rule}
	if class == modelerr.ClassQuota {
		o.Reset, o.Clamped, _ = modelerr.ResetAfter(adapter, note, now)
	}
	return o
}

// Park is a park the detector calls for. The caller turns it into an instant
// and applies it: Until when the refusal named one, otherwise now plus
// Backoff for Step.
type Park struct {
	// Until is the reset instant the error named, zero when it named none
	// (or none still ahead).
	Until time.Time
	// Clamped marks an Until cut to 8 days.
	Clamped bool
	// Rule names the classifier rule that matched the deciding error.
	Rule string
	// Step is the backoff step a park with no Until lasts for: how many
	// parks came before this one with no successful call in between.
	Step int
}

// Detector is one harness's park detector. The zero value is ready to use.
// It is not safe for concurrent use; its owner locks around it.
type Detector struct {
	// errs are the quota errors since the last success, oldest first, the
	// oldest within StuckWindow of the newest.
	errs []Outcome
	// last is the newest model outcome of any kind: a one-shot parks when
	// its run's last outcome is a quota error.
	last Outcome
	// step counts the parks since the last success (Backoff).
	step int
}

// Observe feeds one outcome. A success clears the stuck count and the
// backoff step; a quota error joins the count. Outcomes may arrive out of
// order across sessions: each is placed by its own time.
func (d *Detector) Observe(o Outcome) {
	if o.At.IsZero() {
		return
	}
	if d.last.At.IsZero() || !o.At.Before(d.last.At) {
		d.last = o
	}
	if o.Success {
		// SPEC-0021 REQ-12: a successful model call resets the stuck count
		// and the backoff step. An error dated after it still counts.
		d.errs = slices.DeleteFunc(d.errs, func(e Outcome) bool { return !e.At.After(o.At) })
		d.step = 0
		return
	}
	if o.Class != modelerr.ClassQuota {
		return
	}
	i, _ := slices.BinarySearchFunc(d.errs, o.At, func(e Outcome, t time.Time) int { return e.At.Compare(t) })
	d.errs = slices.Insert(d.errs, i, o)
	newest := d.errs[len(d.errs)-1].At
	cut := 0
	for cut < len(d.errs) && newest.Sub(d.errs[cut].At) > StuckWindow {
		cut++
	}
	if n := len(d.errs) - cut; n > maxErrs {
		cut = len(d.errs) - maxErrs
	}
	d.errs = slices.Delete(d.errs, 0, cut)
}

// Resident answers whether a resident should be parked as of now: its
// newest quota error names a reset still ahead, or it is stuck on quota
// (SPEC-0021 REQ-12).
func (d *Detector) Resident(now time.Time) (Park, bool) {
	if len(d.errs) == 0 {
		return Park{}, false
	}
	newest := d.errs[len(d.errs)-1]
	if newest.Reset.After(now) {
		return Park{Until: newest.Reset, Clamped: newest.Clamped, Rule: newest.Rule, Step: d.step}, true
	}
	if len(d.errs) >= StuckErrors {
		return Park{Rule: newest.Rule, Step: d.step}, true
	}
	return Park{}, false
}

// OneShot answers whether a one-shot run that started at since (less the
// caller's attribution slack) and has ended should be parked as of now: its
// last classified model outcome is a quota error (SPEC-0021 REQ-12). Whether
// the run's exit status allows a park at all (a zero exit never parks,
// REQ-11) is the caller's.
func (d *Detector) OneShot(since, now time.Time) (Park, bool) {
	l := d.last
	if l.Success || l.Class != modelerr.ClassQuota || l.At.Before(since) {
		return Park{}, false
	}
	p := Park{Rule: l.Rule, Step: d.step}
	if l.Reset.After(now) {
		p.Until, p.Clamped = l.Reset, l.Clamped
	}
	return p, true
}

// Parked records that a park was applied: the errors behind it are spent,
// so a released harness parks again only on new evidence, and the next park
// without a success in between lasts twice as long.
func (d *Detector) Parked() {
	d.errs = nil
	d.step++
}

// Step is how many parks came before the next one with no success between.
func (d *Detector) Step() int { return d.step }

// SetStep seeds the backoff step, from a park restored at boot.
func (d *Detector) SetStep(n int) {
	if n > d.step {
		d.step = n
	}
}

// Backoff is how long a park with no reset time lasts at step: base doubled
// step times, capped at max (SPEC-0021 REQ-12 Scenario "Backoff grows, then
// resets": 15m, 30m, 1h, 1h with a 1h cap).
func Backoff(step int, base, maxDur time.Duration) time.Duration {
	if base <= 0 {
		return maxDur
	}
	d := base
	for range step {
		if d >= maxDur || d > maxDur/2 {
			return maxDur
		}
		d *= 2
	}
	return min(d, maxDur)
}
