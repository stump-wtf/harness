package mergetrain

// One Train Attempt
//
// RunTrain builds train/<pr> = base + a squash of the PR head as a new branch,
// waits for CI on that exact commit, and deletes the branch — on every path,
// including a panic and a cancelled context. It never merges; the driver
// decides that from the typed Result.
//
// The branch is deleted with a context detached from the attempt's, so a
// daemon shutting down mid-poll still cleans up. A failed deletion is retried
// within a 30 s budget and logged; it never changes the attempt's outcome.
//
// Governing: ADR-0032 (A1, failure handling), SPEC-0025 REQ-4, REQ-5, REQ-6.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/stump-wtf/harness/internal/forge"
)

// Outcome is how a train attempt ended.
type Outcome string

// Train outcomes.
const (
	OutcomeGreen      Outcome = "green"       // CI passed on the train commit
	OutcomeRed        Outcome = "red"         // CI failed or errored
	OutcomeConflict   Outcome = "conflict"    // the head does not merge onto base
	OutcomeTimeout    Outcome = "timeout"     // CI did not finish in time
	OutcomeStale      Outcome = "stale"       // the head or base moved; retry silently
	OutcomeForgeError Outcome = "forge error" // the forge failed; retry next tick
	OutcomeCancelled  Outcome = "cancelled"   // the context was cancelled
)

// Combined CI states.
const (
	ciPending = "pending"
	ciFailure = "failure"
	ciError   = "error"
)

// deleteBudget bounds branch cleanup, independent of the attempt's context.
const deleteBudget = 30 * time.Second

// TrainConfig tunes one attempt.
type TrainConfig struct {
	Repo    string
	PollMin time.Duration // first wait before polling CI
	PollMax time.Duration // backoff cap
	Timeout time.Duration // overall CI deadline, from the branch's creation
	// DeleteRetry is the pause between cleanup attempts. Default 1 s.
	DeleteRetry time.Duration
	Log         Logger
}

// Result is what one attempt produced.
type Result struct {
	Outcome Outcome
	Base    string            // the base the train was built on
	Train   forge.TrainBranch // zero unless the branch was built
	Status  string            // the last combined status read
	// Want holds, on green, the bytes of every path the train commit adds or
	// modifies, read from the train commit before its branch was deleted.
	// VerifyLanded checks them on the base branch after the merge.
	Want map[string][]byte
	Err  error
}

// TrainBranchName is the branch an attempt for pr uses.
func TrainBranchName(pr int) string { return "train/" + strconv.Itoa(pr) }

// RunTrain runs one attempt for pr on base.
func RunTrain(ctx context.Context, f forge.Forge, cfg TrainConfig, pr PullRequest, base string) (res Result) {
	log := cfg.Log
	if log == nil {
		log = nopLogger{}
	}
	branch := TrainBranchName(pr.Number)
	kv := []any{"repo", cfg.Repo, "pr", pr.Number, "head", pr.HeadSHA, "base", base}
	res.Base = base

	// A leftover from a crashed attempt would make the push below fail. It is
	// the train's own ref, so removing it is safe; failing to is not fatal
	// here, because the build then fails loudly on its own.
	if err := f.DeleteBranch(ctx, cfg.Repo, branch); err != nil {
		log.Warn(event("delete failed"), append(kv, "train", branch, "err", err)...)
	}

	// From here on, whatever happens, the branch goes.
	defer cleanup(ctx, f, cfg, log, branch, kv)

	log.Info(event("building"), append(kv, "train", branch)...)
	tb, err := f.CreateTrainBranch(ctx, cfg.Repo, forge.TrainSpec{
		Branch:  branch,
		BaseSHA: base,
		PR:      pr.Number,
		HeadSHA: pr.HeadSHA,
		Message: fmt.Sprintf("merge train: #%d at %s", pr.Number, short(pr.HeadSHA)),
	})
	switch {
	case err == nil:
	case errors.Is(err, forge.ErrConflict):
		log.Info(event("conflict"), kv...)
		return Result{Outcome: OutcomeConflict, Base: base, Err: err}
	case errors.Is(err, forge.ErrStale):
		log.Info(event("stale"), append(kv, "err", err)...)
		return Result{Outcome: OutcomeStale, Base: base, Err: err}
	case ctx.Err() != nil:
		return Result{Outcome: OutcomeCancelled, Base: base, Err: ctx.Err()}
	default:
		log.Warn(event("tick failed"), append(kv, "err", err)...)
		return Result{Outcome: OutcomeForgeError, Base: base, Err: err}
	}
	res.Train = tb
	kv = append(kv, "train", tb.SHA, "tree", tb.Tree)
	log.Info(event("built"), kv...)

	res.Outcome, res.Status, res.Err = waitCI(ctx, f, cfg, tb.SHA)
	switch res.Outcome {
	case OutcomeGreen:
		log.Info(event("green"), kv...)
	case OutcomeRed:
		log.Info(event("red"), append(kv, "status", res.Status)...)
		return res
	case OutcomeTimeout:
		log.Info(event("timeout"), append(kv, "after", cfg.Timeout)...)
		return res
	case OutcomeForgeError:
		log.Warn(event("tick failed"), append(kv, "err", res.Err)...)
		return res
	default:
		return res
	}

	// Green: capture what the tested tree says about each changed path while
	// the branch still exists.
	res.Want = make(map[string][]byte, len(tb.Changed))
	for _, p := range tb.Changed {
		b, err := f.FileContentAtRef(ctx, cfg.Repo, p, tb.SHA)
		if err != nil {
			if ctx.Err() != nil {
				return Result{Outcome: OutcomeCancelled, Base: base, Train: tb, Err: ctx.Err()}
			}
			log.Warn(event("tick failed"), append(kv, "err", err)...)
			return Result{Outcome: OutcomeForgeError, Base: base, Train: tb, Err: err}
		}
		res.Want[p] = b
	}
	return res
}

// waitCI polls the combined status of sha with exponential backoff until it
// settles, cfg.Timeout passes, or ctx is done.
func waitCI(ctx context.Context, f forge.Forge, cfg TrainConfig, sha string) (Outcome, string, error) {
	deadline := time.Now().Add(cfg.Timeout)
	delay := cfg.PollMin
	var lastErr error
	status := ciPending
	for {
		wait := min(delay, time.Until(deadline))
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return OutcomeCancelled, status, ctx.Err()
			case <-t.C:
			}
		}
		st, err := f.CombinedStatus(ctx, cfg.Repo, sha)
		switch {
		case ctx.Err() != nil:
			return OutcomeCancelled, status, ctx.Err()
		case err != nil && !forge.Transient(err):
			return OutcomeForgeError, status, err
		case err != nil:
			// A blip during a long wait is not a verdict; keep polling.
			lastErr = err
		default:
			status, lastErr = st, nil
			switch st {
			case CISuccess:
				return OutcomeGreen, st, nil
			case ciFailure, ciError:
				return OutcomeRed, st, nil
			}
		}
		if !time.Now().Before(deadline) {
			if lastErr != nil {
				return OutcomeForgeError, status, lastErr
			}
			return OutcomeTimeout, status, nil
		}
		delay = min(delay*2, cfg.PollMax)
	}
}

// cleanup deletes the train branch on a context of its own, so neither a
// cancelled attempt nor a panic skips it. Deletion is idempotent and retried
// within deleteBudget; a final failure is logged and does not change the
// attempt's result.
func cleanup(ctx context.Context, f forge.Forge, cfg TrainConfig, log Logger, branch string, kv []any) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteBudget)
	defer cancel()
	pause := cfg.DeleteRetry
	if pause <= 0 {
		pause = time.Second
	}
	const attempts = 3
	for i := 1; ; i++ {
		err := f.DeleteBranch(dctx, cfg.Repo, branch)
		if err == nil {
			log.Info(event("deleted"), append(kv, "train", branch)...)
			return
		}
		log.Warn(event("delete failed"), append(kv, "train", branch, "attempt", i, "err", err)...)
		if i == attempts || dctx.Err() != nil {
			return
		}
		t := time.NewTimer(pause)
		select {
		case <-dctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// short abbreviates a SHA for a commit message.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
