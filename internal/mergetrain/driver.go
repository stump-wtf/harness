package mergetrain

// The Driver
//
// One driver per repo, holding the repo's lock for its lifetime. Each tick it
// takes the first eligible PR, in Order, that it has not already attempted at
// the same (head, base), and runs one train attempt. On green, and only in
// merge mode, it re-reads the PR and the base branch, merges with the head
// pinned, and verifies — real tree ids first, then bytes. A PR-level failure
// gets exactly one comment per head, which is the author's todo; a
// verification failure halts the driver, because an untested tree may be on
// the base branch and nothing after that can be trusted. A cancelled context
// is not a verification failure: a shutdown landing between the merge and its
// verification reports the cancellation and stops, rather than halting.
//
// No code path here opens, closes or edits a pull request, or pushes to a PR's
// branch: the Forge interface offers no way to (SPEC-0025 REQ-13).
//
// Governing: ADR-0032 (the loop, failure handling, singleton, author todo,
// bypass audit), SPEC-0025 REQ-1, REQ-7..REQ-11, REQ-13..REQ-16.

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

// Tick runs at most one attempt. Its error is informational — the tick is
// abandoned and the next retries — except ErrHalted, which is permanent.
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

	queue := Order(prs)
	if q := queueString(queue); q != d.lastQueue {
		d.log.Info(event("queue"), d.kv("base", base, "prs", q)...)
		d.lastQueue = q
	}

	var pr PullRequest
	found := false
	for _, p := range queue {
		if _, done := d.attempted[attemptKey{p.Number, p.HeadSHA, base}]; !done {
			pr, found = p, true
			break
		}
	}
	if !found {
		return nil
	}
	key := attemptKey{pr.Number, pr.HeadSHA, base}

	res := RunTrain(ctx, d.f, d.trainConfig(), pr, base)
	switch res.Outcome {
	case OutcomeGreen:
	case OutcomeConflict:
		d.attempted[key] = res.Outcome
		d.fail(ctx, key, pr, CauseConflict, fmt.Sprintf(
			"`%s` does not merge cleanly onto `%s` at `%s`. Rebase it (the forge's rebase update, or locally) and push; the train picks it up again on the new head.",
			short(pr.HeadSHA), d.cfg.BaseBranch, short(base)))
		return nil
	case OutcomeRed:
		d.attempted[key] = res.Outcome
		d.fail(ctx, key, pr, CauseRed, fmt.Sprintf(
			"CI was `%s` on the train commit `%s` — this PR at `%s` squashed onto `%s` at `%s`. The PR's own CI was green, so this is an interaction with what landed since, or a flake; the train tries again when `%s` or this PR's head moves.",
			res.Status, short(res.Train.SHA), short(pr.HeadSHA), d.cfg.BaseBranch, short(base), d.cfg.BaseBranch))
		return nil
	case OutcomeTimeout:
		d.attempted[key] = res.Outcome
		d.fail(ctx, key, pr, CauseTimeout, fmt.Sprintf(
			"CI on the train commit `%s` did not finish within %s. The train tries again when `%s` or this PR's head moves.",
			short(res.Train.SHA), d.cfg.CITimeout, d.cfg.BaseBranch))
		return nil
	case OutcomeStale:
		return nil
	case OutcomeCancelled:
		return ctx.Err()
	default: // forge error, already logged by RunTrain
		return res.Err
	}

	if d.cfg.Mode == ModeReport {
		d.attempted[key] = res.Outcome
		d.log.Info(event("would merge"), d.kv("pr", pr.Number, "head", pr.HeadSHA, "base", base, "train", res.Train.SHA, "tree", res.Train.Tree)...)
		return nil
	}
	return d.merge(ctx, key, pr, res)
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
		if ok, reason := Eligible(cur); !ok {
			why = "no longer eligible: " + reason
		}
	}
	if why != "" {
		d.log.Info(event("stale"), append(kv, "cause", why)...)
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
			d.fail(ctx, key, pr, CauseMergeRefused, fmt.Sprintf(
				"CI passed on the train commit `%s`, but the forge refused the squash merge (%s). Nothing was merged.",
				short(res.Train.SHA), err))
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

// verify is SPEC-0025 REQ-8: the base branch is at the merge commit, the merge
// commit's tree is the tested tree, and the changed paths' bytes landed.
func (d *Driver) verify(ctx context.Context, res Result, merged string) error {
	head, err := retry(ctx, d.cfg.RetryPause, func() (string, error) {
		return d.f.BranchHead(ctx, d.cfg.Repo, d.cfg.BaseBranch)
	})
	if err != nil {
		return fmt.Errorf("could not read %s: %w", d.cfg.BaseBranch, err)
	}
	if head != merged {
		return fmt.Errorf("%s is at %s, not at the merge commit %s", d.cfg.BaseBranch, head, merged)
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
