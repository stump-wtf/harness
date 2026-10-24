package mergetrain

// One Train Attempt
//
// RunTrain builds train/<pr> = base + a squash of the PR head as a new branch,
// waits for CI on that exact commit, and deletes the branch — on every path,
// including a panic and a cancelled context. It never merges; the driver
// decides that from the typed Result. RunTrainBatch is the same attempt with
// up to batch heads folded onto one train commit in merge order
// (SPEC-0025 REQ-17); with one PR it is byte-identical to RunTrain.
//
// The branch is deleted with a context detached from the attempt's, so a
// daemon shutting down mid-poll still cleans up. A failed deletion is retried
// within a 30 s budget and logged; it never changes the attempt's outcome.
//
// Governing: ADR-0032 (A1, failure handling), SPEC-0025 REQ-4, REQ-5, REQ-6,
// REQ-17.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	// OutcomeSequenceConflict says two files of the train commit claim the
	// same sequence number, so landing it would duplicate an ADR, a spec or a
	// migration (ADR-0052).
	OutcomeSequenceConflict Outcome = "sequence conflict"
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
	// SequencePaths are the directories whose files carry a sequence number
	// in their names; empty skips the collision check (ADR-0052).
	SequencePaths []string
	Log           Logger
}

// Result is what one attempt produced.
type Result struct {
	Outcome Outcome
	Base    string            // the base the train was built on
	Train   forge.TrainBranch // zero unless the branch was built
	Status  string            // the last combined status read
	// ConflictPR is the PR whose head the in-order fold rejected first on a
	// batch build that ended OutcomeConflict (SPEC-0025 REQ-17); 0 when the
	// forge did not say or the train carried one PR.
	ConflictPR int
	// Collisions lists the sequence numbers two files of the train commit
	// claim on an OutcomeSequenceConflict (ADR-0052).
	Collisions []SequenceCollision
	// Want holds, on green, the bytes of every path the train commit adds or
	// modifies, read from the train commit before its branch was deleted.
	// VerifyLanded checks them on the base branch after the merge.
	Want map[string][]byte
	Err  error
}

// TrainBranchName is the branch an attempt for pr uses.
func TrainBranchName(pr int) string { return "train/" + strconv.Itoa(pr) }

// trainBranchName is the branch an attempt for prs uses: the per-PR name for
// one PR, a name naming every PR in the set for a batch, so a batch never
// shares a ref with a single-PR train.
func trainBranchName(prs []PullRequest) string {
	if len(prs) == 1 {
		return TrainBranchName(prs[0].Number)
	}
	nums := make([]string, len(prs))
	for i, p := range prs {
		nums[i] = strconv.Itoa(p.Number)
	}
	return "train/batch/" + strings.Join(nums, "-")
}

// trainMessage is the train commit's message. For one PR it is the message
// the unbatched train has always used; a batch names every PR and head it
// carries.
func trainMessage(prs []PullRequest) string {
	if len(prs) == 1 {
		return fmt.Sprintf("merge train: #%d at %s", prs[0].Number, short(prs[0].HeadSHA))
	}
	nums := make([]string, len(prs))
	shas := make([]string, len(prs))
	for i, p := range prs {
		nums[i] = "#" + strconv.Itoa(p.Number)
		shas[i] = short(p.HeadSHA)
	}
	return fmt.Sprintf("merge train: %s at %s", strings.Join(nums, ","), strings.Join(shas, ","))
}

// headsOf pins every PR head of a batch, in merge order.
func headsOf(prs []PullRequest) []forge.PRHead {
	hs := make([]forge.PRHead, len(prs))
	for i, p := range prs {
		hs[i] = forge.PRHead{PR: p.Number, HeadSHA: p.HeadSHA}
	}
	return hs
}

// RunTrain runs one attempt for pr on base. It is RunTrainBatch with one PR,
// the spelling SPEC-0025 REQ-17 keeps byte-identical.
func RunTrain(ctx context.Context, f forge.Forge, cfg TrainConfig, pr PullRequest, base string) Result {
	return RunTrainBatch(ctx, f, cfg, []PullRequest{pr}, base)
}

// RunTrainBatch runs one attempt carrying every PR in prs, which must be in
// merge order, on one train commit (SPEC-0025 REQ-17). One PR behaves exactly
// as RunTrain always did.
func RunTrainBatch(ctx context.Context, f forge.Forge, cfg TrainConfig, prs []PullRequest, base string) (res Result) {
	log := cfg.Log
	if log == nil {
		log = nopLogger{}
	}
	branch := trainBranchName(prs)
	kv := []any{"repo", cfg.Repo, "pr", prs[0].Number, "head", prs[0].HeadSHA, "base", base}
	if len(prs) > 1 {
		nums := make([]string, len(prs))
		for i, p := range prs {
			nums[i] = strconv.Itoa(p.Number)
		}
		kv = append(kv, "batch", strings.Join(nums, ","))
	}
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
	spec := forge.TrainSpec{
		Branch:  branch,
		BaseSHA: base,
		PR:      prs[0].Number,
		HeadSHA: prs[0].HeadSHA,
		Message: trainMessage(prs),
	}
	if len(prs) > 1 {
		spec.Heads = headsOf(prs)
	}
	tb, err := f.CreateTrainBranch(ctx, cfg.Repo, spec)
	switch {
	case err == nil:
	case errors.Is(err, forge.ErrConflict):
		log.Info(event("conflict"), kv...)
		res := Result{Outcome: OutcomeConflict, Base: base, Err: err}
		var ce *forge.ConflictError
		if errors.As(err, &ce) {
			res.ConflictPR = ce.PR
		}
		return res
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

	// A colliding sequence number merges cleanly and lands a duplicate, so
	// it is refused here, on the built commit, before CI is asked to spend
	// itself on a tree that will not merge (ADR-0052).
	if len(cfg.SequencePaths) > 0 {
		collisions, err := checkSequences(ctx, f, cfg.Repo, cfg.SequencePaths, prs, tb.SHA, base)
		switch {
		case err != nil && ctx.Err() != nil:
			return Result{Outcome: OutcomeCancelled, Base: base, Train: tb, Err: ctx.Err()}
		case err != nil:
			log.Warn(event("tick failed"), append(kv, "err", err)...)
			return Result{Outcome: OutcomeForgeError, Base: base, Train: tb, Err: err}
		case len(collisions) > 0:
			log.Info(event("sequence conflict"), append(kv, "collisions", len(collisions))...)
			return Result{Outcome: OutcomeSequenceConflict, Base: base, Train: tb, Collisions: collisions}
		}
	}

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
