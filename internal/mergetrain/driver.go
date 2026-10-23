package mergetrain

// The Driver
//
// One driver per repo, holding the repo's lock for its lifetime. Each tick it
// takes the first eligible PR, in Order, that it has not already attempted at
// the same (head, base), and runs one train attempt. With batch above 1
// (SPEC-0025 REQ-17) it takes up to batch of them and puts their heads on one
// train commit, bisecting a red batch by halves; every per-PR guarantee is
// kept inside the batch. On green, and only in merge mode, it re-reads the PR
// and the base branch, merges with the head pinned, and verifies — real tree
// ids first, then bytes. A PR-level failure gets exactly one comment per
// head, which is the author's todo; a verification failure halts the driver,
// because an untested tree may be on the base branch and nothing after that
// can be trusted. A cancelled context is not a verification failure: a
// shutdown landing between the merge and its verification reports the
// cancellation and stops, rather than halting.
//
// No code path here opens, closes or edits a pull request, or pushes to a PR's
// branch: the Forge interface offers no way to (SPEC-0025 REQ-13).
//
// Governing: ADR-0032 (the loop, failure handling, singleton, author todo,
// bypass audit), SPEC-0025 REQ-1, REQ-7..REQ-11, REQ-13..REQ-17.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/forge"
)

// Mode says whether the driver merges.
type Mode string

// Driver modes (SPEC-0025 REQ-1).
const (
	// ModeReport builds and tests trains and logs what it would merge and
	// what it would comment. It writes nothing to pull requests.
	ModeReport Mode = "report"
	// ModeMerge merges green trains.
	ModeMerge Mode = "merge"
)

// ErrHalted wraps the reason a driver stopped attempting merges.
var ErrHalted = errors.New("mergetrain: halted")

// minPoll is the floor on the CI poll interval (SPEC-0025 REQ-5).
const minPoll = 5 * time.Second

// DriverConfig configures one repo's driver.
type DriverConfig struct {
	Repo         string // "owner/name"
	BaseBranch   string // default "main"
	Mode         Mode   // default ModeReport
	PollInterval time.Duration
	CITimeout    time.Duration
	LockDir      string
	Log          Logger

	// Batch is the number of PRs per train commit (SPEC-0025 REQ-17). 0 or 1
	// keeps the one-PR-per-train behaviour byte-identical; above 1 a tick
	// folds up to Batch eligible PRs onto one train commit and bisects a red
	// batch by halves.
	Batch int

	// TrainPollMin overrides the derived first CI poll wait
	// (max(PollInterval/4, 5s)); tests use it to run in milliseconds.
	TrainPollMin time.Duration
	// RetryPause is the pause between retries of cleanup and verification
	// reads. Default 1 s.
	RetryPause time.Duration
}

type attemptKey struct {
	pr         int
	head, base string
}

// pendingComment is a failure the author has not been told about yet, because
// posting the comment failed. It is retried at the start of each tick.
type pendingComment struct {
	pr     PullRequest
	cause  string
	detail string
}

// Driver is one repo's merge train.
type Driver struct {
	cfg  DriverConfig
	f    forge.Forge
	log  Logger
	lock *Lock

	attempted map[attemptKey]Outcome
	pending   map[attemptKey]pendingComment
	lastMain  string // the base branch head last seen or produced
	lastQueue string
	halted    error
}

// NewDriver validates cfg and takes the repo's lock. It makes no forge call.
func NewDriver(cfg DriverConfig, f forge.Forge) (*Driver, error) {
	if cfg.BaseBranch == "" {
		cfg.BaseBranch = "main"
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeReport
	}
	if cfg.Mode != ModeReport && cfg.Mode != ModeMerge {
		return nil, fmt.Errorf("mergetrain: mode %q is neither %q nor %q", cfg.Mode, ModeReport, ModeMerge)
	}
	if cfg.PollInterval <= 0 || cfg.CITimeout <= 0 {
		return nil, errors.New("mergetrain: poll interval and CI timeout must be positive")
	}
	if cfg.Batch < 0 {
		return nil, errors.New("mergetrain: batch must be at least 1")
	}
	if cfg.LockDir == "" {
		return nil, errors.New("mergetrain: lock dir is required")
	}
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	if cfg.RetryPause <= 0 {
		cfg.RetryPause = time.Second
	}
	lock, err := AcquireLock(cfg.LockDir, cfg.Repo)
	if err != nil {
		return nil, err
	}
	d := &Driver{
		cfg:       cfg,
		f:         f,
		log:       cfg.Log,
		lock:      lock,
		attempted: map[attemptKey]Outcome{},
		pending:   map[attemptKey]pendingComment{},
	}
	d.log.Info(event("started"), "repo", cfg.Repo, "mode", string(cfg.Mode), "base_branch", cfg.BaseBranch)
	return d, nil
}

// Close releases the repo's lock.
func (d *Driver) Close() error {
	d.log.Info(event("stopped"), "repo", d.cfg.Repo)
	return d.lock.Release()
}

// Run ticks immediately and then every PollInterval until ctx is done (nil)
// or the driver halts (an error wrapping ErrHalted).
func (d *Driver) Run(ctx context.Context) error {
	t := time.NewTicker(d.cfg.PollInterval)
	defer t.Stop()
	for {
		if err := d.Tick(ctx); errors.Is(err, ErrHalted) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (d *Driver) kv(extra ...any) []any {
	return append([]any{"repo", d.cfg.Repo}, extra...)
}

// Tick runs at most one attempt, which carries up to Batch PRs when
// batching. Its error is informational — the tick is abandoned and the next
// retries — except ErrHalted, which is permanent.
func (d *Driver) Tick(ctx context.Context) error {
	if d.halted != nil {
		return d.halted
	}
	d.flushPending(ctx)

	prs, err := d.f.ListOpenPRs(ctx, d.cfg.Repo)
	if err != nil {
		return d.tickFailed(ctx, err)
	}
	base, err := d.f.BranchHead(ctx, d.cfg.Repo, d.cfg.BaseBranch)
	if err != nil {
		return d.tickFailed(ctx, err)
	}
	if d.lastMain != "" && base != d.lastMain {
		d.log.Warn(event("bypass detected"), d.kv("previous", d.lastMain, "current", base)...)
	}
	d.lastMain = base
	d.forget(prs)

	queue := Order(prs, d.cfg.BaseBranch)
	if q := queueString(queue); q != d.lastQueue {
		d.log.Info(event("queue"), d.kv("base", base, "prs", q)...)
		d.lastQueue = q
	}

	targets := d.targets(queue, base)
	if len(targets) == 0 {
		return nil
	}
	if len(targets) == 1 {
		return d.attemptSingle(ctx, targets[0], base)
	}
	_, err = d.resolveBatch(ctx, targets, base)
	return err
}

// targets returns the first not-yet-attempted PRs of the queue, up to the
// batch size (SPEC-0025 REQ-17). A PR already attempted at this (head, base)
// is skipped, never retried in this tick.
func (d *Driver) targets(queue []PullRequest, base string) []PullRequest {
	n := max(d.cfg.Batch, 1)
	var out []PullRequest
	for _, p := range queue {
		if _, done := d.attempted[attemptKey{p.Number, p.HeadSHA, base}]; done {
			continue
		}
		out = append(out, p)
		if len(out) == n {
			break
		}
	}
	return out
}

// attemptSingle runs and judges one train attempt for pr: the REQ-1 through
// REQ-16 path, unchanged by batching.
func (d *Driver) attemptSingle(ctx context.Context, pr PullRequest, base string) error {
	res := RunTrain(ctx, d.f, d.trainConfig(), pr, base)
	_, err := d.attemptResult(ctx, pr, base, res)
	return err
}

// attemptResult judges a finished single-PR attempt and returns whether the
// PR landed. Landed means merged and verified; a report-mode "would merge"
// is not a landing.
func (d *Driver) attemptResult(ctx context.Context, pr PullRequest, base string, res Result) (bool, error) {
	key := attemptKey{pr.Number, pr.HeadSHA, base}
	switch res.Outcome {
	case OutcomeGreen:
	case OutcomeConflict:
		d.attempted[key] = res.Outcome
		d.fail(ctx, key, pr, CauseConflict, fmt.Sprintf(
			"`%s` does not merge cleanly onto `%s` at `%s`. Rebase it (the forge's rebase update, or locally) and push; the train picks it up again on the new head.",
			short(pr.HeadSHA), d.cfg.BaseBranch, short(base)))
		return false, nil
	case OutcomeRed:
		d.attempted[key] = res.Outcome
		d.fail(ctx, key, pr, CauseRed, fmt.Sprintf(
			"CI was `%s` on the train commit `%s` — this PR at `%s` squashed onto `%s` at `%s`. The PR's own CI was green, so this is an interaction with what landed since, or a flake; the train tries again when `%s` or this PR's head moves.",
			res.Status, short(res.Train.SHA), short(pr.HeadSHA), d.cfg.BaseBranch, short(base), d.cfg.BaseBranch))
		return false, nil
	case OutcomeTimeout:
		d.attempted[key] = res.Outcome
		d.fail(ctx, key, pr, CauseTimeout, fmt.Sprintf(
			"CI on the train commit `%s` did not finish within %s. The train tries again when `%s` or this PR's head moves.",
			short(res.Train.SHA), d.cfg.CITimeout, d.cfg.BaseBranch))
		return false, nil
	case OutcomeStale:
		return false, nil
	case OutcomeCancelled:
		return false, ctx.Err()
	default: // forge error, already logged by RunTrain
		return false, res.Err
	}

	if d.cfg.Mode == ModeReport {
		d.attempted[key] = res.Outcome
		d.log.Info(event("would merge"), d.kv("pr", pr.Number, "head", pr.HeadSHA, "base", base, "train", res.Train.SHA, "tree", res.Train.Tree)...)
		return false, nil
	}
	if err := d.merge(ctx, key, pr, res); err != nil {
		return false, err
	}
	return d.attempted[key] == OutcomeGreen, nil
}

// resolveBatch carries cur, which must be non-empty and in merge order, on
// one train commit (SPEC-0025 REQ-17). On green every PR merges, in order,
// each with its own REQ-7 re-check and REQ-8 verification. On red or
// timeout it bisects by halves and recurses: a half that goes green lands,
// and the recursion narrows to the half that stayed red until a single PR
// takes the usual REQ-9 comment and REQ-10 memory. On a build conflict the
// first rejecting head gets the usual conflict comment and the batch shrinks
// to the heads before it. It reports whether every PR in cur landed; a half
// that did not land was judged on its own (a commented leaf, a stale or
// failed build) and whatever came after it waits for the next tick.
func (d *Driver) resolveBatch(ctx context.Context, cur []PullRequest, base string) (bool, error) {
	if len(cur) == 1 {
		res := RunTrain(ctx, d.f, d.trainConfig(), cur[0], base)
		return d.attemptResult(ctx, cur[0], base, res)
	}
	res := RunTrainBatch(ctx, d.f, d.trainConfig(), cur, base)
	switch res.Outcome {
	case OutcomeGreen:
		for _, p := range cur {
			d.attempted[attemptKey{p.Number, p.HeadSHA, base}] = res.Outcome
		}
		if d.cfg.Mode == ModeReport {
			d.log.Info(event("would merge"), d.kv("prs", queueString(cur), "base", base, "train", res.Train.SHA, "tree", res.Train.Tree)...)
			return false, nil
		}
		return d.mergeBatch(ctx, cur, base, res)
	case OutcomeConflict:
		idx := conflictingIndex(cur, res.ConflictPR)
		pr := cur[idx]
		key := attemptKey{pr.Number, pr.HeadSHA, base}
		d.attempted[key] = res.Outcome
		d.fail(ctx, key, pr, CauseConflict, fmt.Sprintf(
			"`%s` does not merge cleanly onto `%s` at `%s`, or onto the batched tree of the heads before it. Rebase it (the forge's rebase update, or locally) and push; the train picks it up again on the new head.",
			short(pr.HeadSHA), d.cfg.BaseBranch, short(base)))
		if idx == 0 {
			return false, nil
		}
		return d.resolveBatch(ctx, cur[:idx], base)
	case OutcomeRed, OutcomeTimeout:
		first, rest := halves(cur)
		landed, err := d.resolveBatch(ctx, first, base)
		if err != nil {
			return false, err
		}
		if !landed {
			return false, nil
		}
		return d.resolveBatch(ctx, rest, d.lastMain)
	case OutcomeStale:
		return false, nil
	case OutcomeCancelled:
		return false, ctx.Err()
	default: // forge error, already logged by RunTrainBatch
		return false, res.Err
	}
}

// mergeBatch lands a green batch, one PR at a time in merge order. Each
// merge keeps its pinned head, its REQ-7 re-check and its REQ-8
// verification; REQ-8's tree and content checks can only be conclusive on
// the last merge, whose resulting tree is the batch tree CI tested, so an
// intermediate merge is checked against the base branch head alone.
func (d *Driver) mergeBatch(ctx context.Context, cur []PullRequest, base string, res Result) (bool, error) {
	wantBase := res.Base
	for i, pr := range cur {
		final := i == len(cur)-1
		key := attemptKey{pr.Number, pr.HeadSHA, base}
		kv := d.kv("pr", pr.Number, "head", pr.HeadSHA, "base", wantBase, "train", res.Train.SHA, "batch", len(cur))

		// REQ-7 per PR: still open, still at head, still eligible, and the
		// base branch still exactly where the last step put it — the tested
		// base for the first PR, our own merge for every one after it.
		prs, err := d.f.ListOpenPRs(ctx, d.cfg.Repo)
		if err != nil {
			return false, d.tickFailed(ctx, err)
		}
		cand, open := findPR(prs, pr.Number)
		now, err := d.f.BranchHead(ctx, d.cfg.Repo, d.cfg.BaseBranch)
		if err != nil {
			return false, d.tickFailed(ctx, err)
		}
		var why string
		switch {
		case !open:
			why = "no longer open"
		case cand.HeadSHA != pr.HeadSHA:
			why = "head moved to " + cand.HeadSHA
		case now != wantBase:
			why = d.cfg.BaseBranch + " moved to " + now
		default:
			if ok, reason := Eligible(cand, d.cfg.BaseBranch); !ok {
				why = "no longer eligible: " + reason
			}
		}
		if why != "" {
			d.log.Info(event("stale"), staleKV(kv, cand, why)...)
			return false, nil
		}

		msg := fmt.Sprintf("Merge-Train: tested as %s (tree %s) on %s", res.Train.SHA, res.Train.Tree, res.Base)
		merged, err := d.f.SquashMerge(ctx, d.cfg.Repo, pr.Number, pr.HeadSHA, "", msg)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if !forge.Transient(err) {
				d.attempted[key] = OutcomeForgeError
				d.log.Warn(event("merge refused"), append(kv, "err", err)...)
				d.fail(ctx, key, pr, CauseMergeRefused, d.mergeRefusedDetail(
					"batch train commit `"+short(res.Train.SHA)+"`", err, "Nothing was merged for this PR."))
				return false, nil
			}
			// A 5xx on a merge is ambiguous. The last merge's proof is the
			// tested tree, as in the single train; an intermediate merge has
			// no tree evidence to lean on, so abandon the tick and let the
			// next one read the forge.
			head, herr := d.f.BranchHead(ctx, d.cfg.Repo, d.cfg.BaseBranch)
			if herr != nil || head == wantBase {
				return false, d.tickFailed(ctx, err)
			}
			if !final {
				d.log.Warn(event("merge refused"), append(kv, "err", err, "cause", "ambiguous; nothing to verify an intermediate merge against")...)
				return false, d.tickFailed(ctx, err)
			}
			d.log.Warn(event("merge refused"), append(kv, "err", err, "cause", "ambiguous; base branch moved, verifying")...)
			merged = head
		}
		d.attempted[key] = OutcomeGreen
		d.lastMain = merged
		d.log.Info(event("merged"), append(kv, "merged", merged)...)

		if err := d.verifyMerge(ctx, res, merged, final); err != nil {
			if ctx.Err() != nil {
				d.log.Warn(event("shutting down"), append(kv, "merged", merged, "err", err)...)
				return false, ctx.Err()
			}
			d.halted = fmt.Errorf("%w: pr #%d: %v", ErrHalted, pr.Number, err)
			d.log.Error(event("halted"), append(kv, "merged", merged, "err", err)...)
			d.fail(ctx, key, pr, CauseVerifyFailed, fmt.Sprintf(
				"This PR was merged as `%s`, but verification failed: %v. The tree that landed may not be the tree CI tested. **The merge train has halted** until its daemon is restarted; check `%s` and revert if needed.",
				short(merged), err, d.cfg.BaseBranch))
			return false, d.halted
		}
		if final {
			d.log.Info(event("verified"), append(kv, "merged", merged, "tree", res.Train.Tree)...)
		} else {
			d.log.Info(event("verified"), append(kv, "merged", merged)...)
		}
		wantBase = merged
	}
	return true, nil
}

// halves splits a batch for the bisect: the first half is at least one PR
// and strictly smaller than the batch, so the recursion always narrows.
func halves(prs []PullRequest) (first, rest []PullRequest) {
	mid := (len(prs) + 1) / 2
	return prs[:mid], prs[mid:]
}

// conflictingIndex locates the head the fold rejected. A forge that cannot
// say which head rejected loses the batch's last head only, so the heads
// before it still get their batch.
func conflictingIndex(prs []PullRequest, pr int) int {
	if pr > 0 {
		for i, p := range prs {
			if p.Number == pr {
				return i
			}
		}
	}
	return len(prs) - 1
}

// merge re-checks, merges and verifies one green train.
func (d *Driver) merge(ctx context.Context, key attemptKey, pr PullRequest, res Result) error {
	kv := d.kv("pr", pr.Number, "head", pr.HeadSHA, "base", res.Base, "train", res.Train.SHA)

	// REQ-7: the PR and the base branch must be exactly as they were tested.
	prs, err := d.f.ListOpenPRs(ctx, d.cfg.Repo)
	if err != nil {
		return d.tickFailed(ctx, err)
	}
	cur, open := findPR(prs, pr.Number)
	now, err := d.f.BranchHead(ctx, d.cfg.Repo, d.cfg.BaseBranch)
	if err != nil {
		return d.tickFailed(ctx, err)
	}
	var why string
	switch {
	case !open:
		why = "no longer open"
	case cur.HeadSHA != pr.HeadSHA:
		why = "head moved to " + cur.HeadSHA
	case now != res.Base:
		why = d.cfg.BaseBranch + " moved to " + now
	default:
		if ok, reason := Eligible(cur, d.cfg.BaseBranch); !ok {
			why = "no longer eligible: " + reason
		}
	}
	if why != "" {
		d.log.Info(event("stale"), staleKV(kv, cur, why)...)
		return nil
	}

	// An empty title lets the forge use its default, "<PR title> (#n)", so
	// main's history reads as it does for a human merge. The body records
	// what was tested.
	msg := fmt.Sprintf("Merge-Train: tested as %s (tree %s) on %s", res.Train.SHA, res.Train.Tree, res.Base)
	merged, err := d.f.SquashMerge(ctx, d.cfg.Repo, pr.Number, pr.HeadSHA, "", msg)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !forge.Transient(err) {
			d.attempted[key] = OutcomeForgeError
			d.log.Warn(event("merge refused"), append(kv, "err", err)...)
			d.fail(ctx, key, pr, CauseMergeRefused, d.mergeRefusedDetail(
				"train commit `"+short(res.Train.SHA)+"`", err, "Nothing was merged."))
			return nil
		}
		// A 5xx on a merge is ambiguous: it may have landed. If the base
		// branch moved, treat its new head as the merge and verify it — the
		// tree check proves or disproves it was ours.
		head, herr := d.f.BranchHead(ctx, d.cfg.Repo, d.cfg.BaseBranch)
		if herr != nil || head == res.Base {
			return d.tickFailed(ctx, err)
		}
		d.log.Warn(event("merge refused"), append(kv, "err", err, "cause", "ambiguous; base branch moved, verifying")...)
		merged = head
	}
	d.attempted[key] = OutcomeGreen
	d.lastMain = merged
	d.log.Info(event("merged"), append(kv, "merged", merged)...)

	if err := d.verify(ctx, res, merged); err != nil {
		// A cancelled context is not a verification failure. A clean stop that
		// lands between the merge and its verification makes verify's reads
		// fail with context.Canceled; treating that as a halt would stop the
		// driver permanently for a shutdown, and tell the author their merge
		// was unverified when the daemon simply went away. Report the
		// cancellation and let Run return; the merge itself stands.
		if ctx.Err() != nil {
			d.log.Warn(event("shutting down"), append(kv, "merged", merged, "err", err)...)
			return ctx.Err()
		}
		d.halted = fmt.Errorf("%w: pr #%d: %v", ErrHalted, pr.Number, err)
		d.log.Error(event("halted"), append(kv, "merged", merged, "err", err)...)
		d.fail(ctx, key, pr, CauseVerifyFailed, fmt.Sprintf(
			"This PR was merged as `%s`, but verification failed: %v. The tree that landed may not be the tree CI tested. **The merge train has halted** until its daemon is restarted; check `%s` and revert if needed.",
			short(merged), err, d.cfg.BaseBranch))
		return d.halted
	}
	d.log.Info(event("verified"), append(kv, "merged", merged, "tree", res.Train.Tree)...)
	return nil
}

// verify is SPEC-0025 REQ-8 for a single-PR train: the base branch is at
// the merge commit, the merge commit's tree is the tested tree, and the
// changed paths' bytes landed.
func (d *Driver) verify(ctx context.Context, res Result, merged string) error {
	return d.verifyMerge(ctx, res, merged, true)
}

// verifyMerge is SPEC-0025 REQ-8. The base branch must be at the merge
// commit whatever the train carried; the tree and content checks compare
// against the tested tree and need checkTree, which a batch's intermediate
// merges do not have — their trees are fold prefixes of the tested batch
// tree, and the final merge's tree check is the conclusive one.
func (d *Driver) verifyMerge(ctx context.Context, res Result, merged string, checkTree bool) error {
	head, err := retry(ctx, d.cfg.RetryPause, func() (string, error) {
		return d.f.BranchHead(ctx, d.cfg.Repo, d.cfg.BaseBranch)
	})
	if err != nil {
		return fmt.Errorf("could not read %s: %w", d.cfg.BaseBranch, err)
	}
	if head != merged {
		return fmt.Errorf("%s is at %s, not at the merge commit %s", d.cfg.BaseBranch, head, merged)
	}
	if !checkTree {
		return nil
	}
	tree, err := retry(ctx, d.cfg.RetryPause, func() (string, error) {
		return d.f.TreeOf(ctx, d.cfg.Repo, merged)
	})
	if err != nil {
		return fmt.Errorf("could not read the tree of %s: %w", merged, err)
	}
	if tree != res.Train.Tree {
		return fmt.Errorf("merge commit %s has tree %s, but CI tested tree %s", merged, tree, res.Train.Tree)
	}
	ok, mismatched, err := VerifyLanded(ctx, d.f, d.cfg.Repo, d.cfg.BaseBranch, res.Want)
	if !ok {
		if err != nil {
			return fmt.Errorf("content check failed for %s: %w", strings.Join(mismatched, ", "), err)
		}
		return fmt.Errorf("content differs from the tested tree for %s", strings.Join(mismatched, ", "))
	}
	return nil
}

// staleKV is a stale line's fields. The cause stays word for word what
// SPEC-0025 REQ-2 spells; the extra note is for the one ineligibility a reader
// cannot diagnose from that cause alone: a PR whose approval is still standing
// on the head but which the forge no longer counts, which a review re-request
// produces without anyone touching the code (#977). Without the note the only
// honest reading of "no approval on current head" is that nobody has reviewed
// it yet, and the lane re-verifies instead of re-approving.
func staleKV(kv []any, pr PullRequest, why string) []any {
	out := append(kv, "cause", why)
	if !strings.HasSuffix(why, ReasonNoApproval) {
		return out
	}
	if r, ok := UnofficialApproval(pr); ok {
		out = append(out, "note", "approval by "+r.Author+" on this head is not official; a reviewer needs to approve again")
	}
	return out
}

// mergeRefusedDetail is the body of a merge-refused comment: what was tested,
// what the forge answered, and, when the answer is the approvals refusal, what
// actually fixes it. That case is named apart because re-verifying cannot fix
// it: the forge counts only approvals it marks official, and requesting review
// from a reviewer clears that mark on the reviews they had already left, so an
// APPROVED review can be sitting on the head counting nothing. The (pr, head,
// base) triple is remembered as refused at this point, so a fresh approval
// alone will not re-trigger the merge either (#977, ADR-0032 REQ-10).
func (d *Driver) mergeRefusedDetail(tested string, err error, nothing string) string {
	detail := fmt.Sprintf("CI passed on the %s, but the forge refused the squash merge (%s). %s", tested, err, nothing)
	if !forge.InsufficientApprovals(err) {
		return detail
	}
	return detail + " The forge counts only approvals it marks official, and it clears that mark when review is requested from the reviewer who left one, so an approval on this head may be counting nothing even though the review is still there. A reviewer needs to post a fresh APPROVED review; reviewing or re-verifying the change again will not help. This head is remembered as refused, so the fresh approval alone will not re-trigger the merge: it needs a new commit on the PR, or `" + d.cfg.BaseBranch + "` to move."
}

// fail tells pr's author, once per head, that the train could not land it.
// In report mode it only logs. A comment that cannot be posted is kept and
// retried next tick, so the author is never silently skipped.
func (d *Driver) fail(ctx context.Context, key attemptKey, pr PullRequest, cause, detail string) {
	if d.cfg.Mode == ModeReport {
		d.log.Info(event("would comment"), d.kv("pr", pr.Number, "head", pr.HeadSHA, "cause", cause)...)
		return
	}
	if err := d.comment(ctx, pr, cause, detail); err != nil {
		d.pending[key] = pendingComment{pr: pr, cause: cause, detail: detail}
		d.log.Warn(event("tick failed"), d.kv("pr", pr.Number, "cause", "comment not posted; will retry", "err", err)...)
	}
}

func (d *Driver) comment(ctx context.Context, pr PullRequest, cause, detail string) error {
	existing, err := d.f.ListComments(ctx, d.cfg.Repo, pr.Number)
	if err != nil {
		return err
	}
	if HasMarker(existing, pr.Number, pr.HeadSHA) {
		return nil
	}
	body := fmt.Sprintf("@%s the merge train could not land this PR at `%s` (%s).\n\n%s\n\n%s",
		pr.Author, short(pr.HeadSHA), cause, detail, Marker(pr.Number, pr.HeadSHA, cause))
	if err := d.f.Comment(ctx, d.cfg.Repo, pr.Number, body); err != nil {
		return err
	}
	d.log.Info(event("commented"), d.kv("pr", pr.Number, "head", pr.HeadSHA, "cause", cause)...)
	return nil
}

// flushPending retries comments that could not be posted earlier.
func (d *Driver) flushPending(ctx context.Context) {
	for key, p := range d.pending {
		if err := d.comment(ctx, p.pr, p.cause, p.detail); err == nil {
			delete(d.pending, key)
		}
	}
}

// forget drops attempt memory for PRs that closed or whose head moved, so the
// map does not grow without bound. Pending comments are kept: an author is
// told about a failure even if they have since pushed.
func (d *Driver) forget(prs []PullRequest) {
	heads := make(map[int]string, len(prs))
	for _, p := range prs {
		heads[p.Number] = p.HeadSHA
	}
	for k := range d.attempted {
		if heads[k.pr] != k.head {
			delete(d.attempted, k)
		}
	}
}

func (d *Driver) tickFailed(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	d.log.Warn(event("tick failed"), d.kv("err", err)...)
	return err
}

func (d *Driver) trainConfig() TrainConfig {
	pollMin := d.cfg.TrainPollMin
	if pollMin <= 0 {
		pollMin = max(d.cfg.PollInterval/4, minPoll)
	}
	return TrainConfig{
		Repo:        d.cfg.Repo,
		PollMin:     pollMin,
		PollMax:     max(d.cfg.PollInterval, pollMin),
		Timeout:     d.cfg.CITimeout,
		DeleteRetry: d.cfg.RetryPause,
		Log:         d.log,
	}
}

// retry calls fn up to three times while it fails transiently.
func retry(ctx context.Context, pause time.Duration, fn func() (string, error)) (string, error) {
	var (
		v   string
		err error
	)
	for i := 0; i < 3; i++ {
		if v, err = fn(); err == nil || !forge.Transient(err) {
			return v, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pause):
		}
	}
	return v, err
}

func findPR(prs []PullRequest, n int) (PullRequest, bool) {
	for _, p := range prs {
		if p.Number == n {
			return p, true
		}
	}
	return PullRequest{}, false
}

func queueString(prs []PullRequest) string {
	nums := make([]string, len(prs))
	for i, p := range prs {
		nums[i] = "#" + strconv.Itoa(p.Number)
	}
	return strings.Join(nums, ",")
}
