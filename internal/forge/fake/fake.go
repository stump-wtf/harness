// Package fake is an in-memory forge.Forge for tests. It holds pull requests,
// branches, statuses, files, trees and comments in maps under one mutex,
// records every call in order, and can be told to fail any one method.
//
// Governing: SPEC-0025 design.md "The fake"; #599.
package fake

// In-Memory Forge
//
// The fake models just enough of a forge to drive the merge train's state
// machine: a train build creates a branch and records the train's tree; a
// squash merge moves the base branch to a new commit that, by default, has the
// train's tree and files — the forge agreeing with the train. Tests that need
// the forge to disagree (a different merged tree, a status that changes over
// time, a conflicting build) use the On* hooks.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"

	"github.com/stump-wtf/harness/internal/forge"
)

// Call is one recorded method call. Args are the call's arguments after the
// context, in order.
type Call struct {
	Method string
	Args   []any
}

type prState struct {
	pr     forge.PullRequest
	merged bool
}

type repoState struct {
	prs       map[int]*prState
	branches  map[string]string            // name → sha
	statuses  map[string]string            // sha → combined state
	files     map[string]map[string][]byte // sha or branch → path → bytes
	trees     map[string]string            // sha → tree
	comments  map[int][]string
	lastTrain map[int]forge.TrainBranch // pr → the last train built for it
}

// Fake is an in-memory forge.Forge. The zero value is not usable; call New.
type Fake struct {
	// BaseBranch is the branch SquashMerge moves. Default "main".
	BaseBranch string

	mu       sync.Mutex
	repos    map[string]*repoState
	calls    []Call
	errs     map[string]error
	polls    map[string]int
	seq      int
	onStatus func(sha string, n int) string
	onTrain  func(forge.TrainSpec) (forge.TrainBranch, error)
	onMerge  func(pr int, train forge.TrainBranch) (tree string)
}

var _ forge.Forge = (*Fake)(nil)

// New returns an empty fake whose base branch is "main".
func New() *Fake {
	return &Fake{
		BaseBranch: "main",
		repos:      map[string]*repoState{},
		errs:       map[string]error{},
		polls:      map[string]int{},
	}
}

// repo returns repo's state, creating it. Callers hold f.mu.
func (f *Fake) repo(name string) *repoState {
	r, ok := f.repos[name]
	if !ok {
		r = &repoState{
			prs:       map[int]*prState{},
			branches:  map[string]string{},
			statuses:  map[string]string{},
			files:     map[string]map[string][]byte{},
			trees:     map[string]string{},
			comments:  map[int][]string{},
			lastTrain: map[int]forge.TrainBranch{},
		}
		f.repos[name] = r
	}
	return r
}

// begin records a call and returns the error forced for method, if any, or
// the context's error: like a real forge, the fake refuses work on a
// cancelled context. Callers hold f.mu.
func (f *Fake) begin(ctx context.Context, method string, args ...any) error {
	f.calls = append(f.calls, Call{Method: method, Args: args})
	if err := f.errs[method]; err != nil {
		return err
	}
	return ctx.Err()
}

// nextSHA returns a unique 40-hex-character synthetic SHA. Callers hold f.mu.
func (f *Fake) nextSHA() string {
	f.seq++
	return fmt.Sprintf("%040x", 0xf00d0000+f.seq)
}

// --- setters -------------------------------------------------------------

// AddPR adds or replaces an open pull request.
func (f *Fake) AddPR(repo string, pr forge.PullRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo(repo).prs[pr.Number] = &prState{pr: clonePR(pr)}
}

// SetStatus sets the combined CI state of sha.
func (f *Fake) SetStatus(repo, sha, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo(repo).statuses[sha] = state
}

// SetFile sets path's content at ref, which may be a SHA or a branch name.
func (f *Fake) SetFile(repo, ref, path string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repo(repo)
	if r.files[ref] == nil {
		r.files[ref] = map[string][]byte{}
	}
	r.files[ref][path] = slices.Clone(content)
}

// SetBranch points name at sha.
func (f *Fake) SetBranch(repo, name, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo(repo).branches[name] = sha
}

// SetTree sets the git tree id of commit sha.
func (f *Fake) SetTree(repo, sha, tree string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repo(repo).trees[sha] = tree
}

// Err forces method to return err until it is cleared with a nil err. The
// method's call is still recorded.
func (f *Fake) Err(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errs, method)
		return
	}
	f.errs[method] = err
}

// OnStatus makes CombinedStatus return fn(sha, n), where n counts that SHA's
// polls from 1. It overrides SetStatus.
func (f *Fake) OnStatus(fn func(sha string, n int) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onStatus = fn
}

// OnTrain makes CreateTrainBranch build what fn returns. A returned error
// creates no branch. The head check (ErrStale) still runs first.
func (f *Fake) OnTrain(fn func(forge.TrainSpec) (forge.TrainBranch, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onTrain = fn
}

// OnMerge makes SquashMerge give the merge commit the tree fn returns,
// instead of the tree of the PR's last train — a forge that disagrees.
func (f *Fake) OnMerge(fn func(pr int, train forge.TrainBranch) (tree string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onMerge = fn
}

// --- inspection ----------------------------------------------------------

// Calls returns a copy of every call so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Methods returns the method name of every call so far, in order.
func (f *Fake) Methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.Method
	}
	return out
}

// Branches returns a copy of repo's branches.
func (f *Fake) Branches(repo string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.repo(repo).branches)
}

// Comments returns the comments posted on pr.
func (f *Fake) Comments(repo string, pr int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.repo(repo).comments[pr])
}

// Merged reports whether pr was merged.
func (f *Fake) Merged(repo string, pr int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.repo(repo).prs[pr]
	return ok && s.merged
}

// --- forge.Forge ---------------------------------------------------------

// ListOpenPRs returns the unmerged pull requests, by number.
func (f *Fake) ListOpenPRs(ctx context.Context, repo string) ([]forge.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "ListOpenPRs", repo); err != nil {
		return nil, err
	}
	out := []forge.PullRequest{}
	for _, s := range f.repo(repo).prs {
		if !s.merged {
			out = append(out, clonePR(s.pr))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// BranchHead returns the SHA branch points at.
func (f *Fake) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "BranchHead", repo, branch); err != nil {
		return "", err
	}
	sha, ok := f.repo(repo).branches[branch]
	if !ok {
		return "", fmt.Errorf("fake: branch %s: %w", branch, forge.ErrNotFound)
	}
	return sha, nil
}

// CreateTrainBranch checks the head, then builds the train OnTrain describes,
// or a synthetic one, and points spec.Branch at it.
func (f *Fake) CreateTrainBranch(ctx context.Context, repo string, spec forge.TrainSpec) (forge.TrainBranch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "CreateTrainBranch", repo, spec); err != nil {
		return forge.TrainBranch{}, err
	}
	r := f.repo(repo)
	if _, exists := r.branches[spec.Branch]; exists {
		return forge.TrainBranch{}, &forge.StatusError{Method: "CreateTrainBranch", Status: 409, Repo: repo}
	}
	s, ok := r.prs[spec.PR]
	if !ok || s.merged {
		return forge.TrainBranch{}, fmt.Errorf("fake: pull %d: %w", spec.PR, forge.ErrNotFound)
	}
	if s.pr.HeadSHA != spec.HeadSHA {
		return forge.TrainBranch{}, forge.ErrStale
	}
	var tb forge.TrainBranch
	if f.onTrain != nil {
		var err error
		if tb, err = f.onTrain(spec); err != nil {
			return forge.TrainBranch{}, err
		}
	} else {
		tb.SHA = f.nextSHA()
		tb.Tree = "tree-" + tb.SHA
	}
	r.branches[spec.Branch] = tb.SHA
	r.trees[tb.SHA] = tb.Tree
	r.lastTrain[spec.PR] = tb
	return tb, nil
}

// DeleteBranch deletes name; a missing branch is not an error.
func (f *Fake) DeleteBranch(ctx context.Context, repo, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "DeleteBranch", repo, name); err != nil {
		return err
	}
	delete(f.repo(repo).branches, name)
	return nil
}

// CombinedStatus returns OnStatus's answer, else the status set for sha, else
// "pending".
func (f *Fake) CombinedStatus(ctx context.Context, repo, sha string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "CombinedStatus", repo, sha); err != nil {
		return "", err
	}
	f.polls[repo+"@"+sha]++
	if f.onStatus != nil {
		return f.onStatus(sha, f.polls[repo+"@"+sha]), nil
	}
	if st, ok := f.repo(repo).statuses[sha]; ok {
		return st, nil
	}
	return "pending", nil
}

// SquashMerge marks pr merged and moves the base branch to a new commit. The
// commit's tree and files are the PR's last train's, unless OnMerge says
// otherwise.
func (f *Fake) SquashMerge(ctx context.Context, repo string, pr int, headSHA, title, message string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "SquashMerge", repo, pr, headSHA, title, message); err != nil {
		return "", err
	}
	r := f.repo(repo)
	s, ok := r.prs[pr]
	if !ok {
		return "", fmt.Errorf("fake: pull %d: %w", pr, forge.ErrNotFound)
	}
	if s.merged {
		return "", &forge.StatusError{Method: "SquashMerge", Status: 405, Repo: repo}
	}
	if s.pr.HeadSHA != headSHA {
		return "", &forge.StatusError{Method: "SquashMerge", Status: 409, Repo: repo}
	}
	s.merged = true
	sha := f.nextSHA()
	train := r.lastTrain[pr]
	tree := train.Tree
	if f.onMerge != nil {
		tree = f.onMerge(pr, train)
	}
	r.trees[sha] = tree
	files := map[string][]byte{}
	maps.Copy(files, r.files[r.branches[f.BaseBranch]])
	maps.Copy(files, r.files[train.SHA])
	r.files[sha] = files
	r.branches[f.BaseBranch] = sha
	return sha, nil
}

// Comment appends body to pr's comments.
func (f *Fake) Comment(ctx context.Context, repo string, pr int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "Comment", repo, pr, body); err != nil {
		return err
	}
	r := f.repo(repo)
	r.comments[pr] = append(r.comments[pr], body)
	return nil
}

// ListComments returns pr's comments, oldest first.
func (f *Fake) ListComments(ctx context.Context, repo string, pr int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "ListComments", repo, pr); err != nil {
		return nil, err
	}
	return slices.Clone(f.repo(repo).comments[pr]), nil
}

// FileContentAtRef reads path at ref: files set on ref itself win, then files
// at the SHA ref names as a branch.
func (f *Fake) FileContentAtRef(ctx context.Context, repo, path, ref string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "FileContentAtRef", repo, path, ref); err != nil {
		return nil, err
	}
	r := f.repo(repo)
	if b, ok := r.files[ref][path]; ok {
		return slices.Clone(b), nil
	}
	if sha, ok := r.branches[ref]; ok {
		if b, ok := r.files[sha][path]; ok {
			return slices.Clone(b), nil
		}
	}
	return nil, fmt.Errorf("fake: %s at %s: %w", path, ref, forge.ErrNotFound)
}

// TreeOf returns the tree set or built for sha.
func (f *Fake) TreeOf(ctx context.Context, repo, sha string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, "TreeOf", repo, sha); err != nil {
		return "", err
	}
	tree, ok := f.repo(repo).trees[sha]
	if !ok {
		return "", fmt.Errorf("fake: commit %s: %w", sha, forge.ErrNotFound)
	}
	return tree, nil
}

// clonePR deep-copies pr so a caller cannot mutate the fake's state.
func clonePR(pr forge.PullRequest) forge.PullRequest {
	pr.Reviews = slices.Clone(pr.Reviews)
	return pr
}
