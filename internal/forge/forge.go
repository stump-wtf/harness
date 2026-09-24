// Package forge is the one door between the merge train and a code forge.
// Every forge interaction the train makes goes through the Forge interface, so
// the whole train runs in tests against internal/forge/fake with no network.
//
// Governing: ADR-0032 (merge train), SPEC-0025 REQ-12, design.md "The Forge
// interface".
package forge

// Forge Interface
//
// The interface first sketched in #599 is amended by SPEC-0025 design.md, each
// change forced by ADR-0032: CreateBranch became CreateTrainBranch (the train
// commit does not exist on the forge until the train makes it), SquashMerge
// pins the head, and BranchHead, ListComments and TreeOf were added.

import (
	"context"
	"errors"
	"fmt"
)

// statusTooManyRequests is HTTP 429. Spelled out rather than imported: this
// package stays free of net/http (#599), which belongs to forge/gitea.
const statusTooManyRequests = 429

// Forge is what the merge train needs from a code forge. repo is always
// "owner/name".
type Forge interface {
	// ListOpenPRs returns every open pull request with its reviews and the
	// combined CI state of its head.
	ListOpenPRs(ctx context.Context, repo string) ([]PullRequest, error)
	// BranchHead returns the SHA branch points at, or ErrNotFound.
	BranchHead(ctx context.Context, repo, branch string) (sha string, err error)
	// CreateTrainBranch creates spec.Branch pointing at a new commit whose
	// parent is spec.BaseSHA and whose tree is the three-way merge of
	// spec.BaseSHA and spec.HeadSHA. It returns ErrConflict, creating nothing,
	// when the merge conflicts, and ErrStale when the PR's head is no longer
	// spec.HeadSHA. It never writes any other branch.
	CreateTrainBranch(ctx context.Context, repo string, spec TrainSpec) (TrainBranch, error)
	// DeleteBranch deletes name. Deleting a branch that does not exist is not
	// an error.
	DeleteBranch(ctx context.Context, repo, name string) error
	// CombinedStatus returns "success", "pending", "failure" or "error". A
	// commit with no statuses at all is "pending".
	CombinedStatus(ctx context.Context, repo, sha string) (string, error) // "success"|"pending"|"failure"|"error"
	// SquashMerge squash-merges pr, refusing if its head is no longer headSHA,
	// and returns the SHA of the commit it created on the base branch.
	SquashMerge(ctx context.Context, repo string, pr int, headSHA, title, message string) (mergedSHA string, err error)
	// Comment posts body as a new comment on pr.
	Comment(ctx context.Context, repo string, pr int, body string) error
	// ListComments returns the bodies of every comment on pr, oldest first.
	ListComments(ctx context.Context, repo string, pr int) ([]string, error)
	// FileContentAtRef returns path's bytes at ref (a branch name or a SHA),
	// or ErrNotFound.
	FileContentAtRef(ctx context.Context, repo, path, ref string) ([]byte, error)
	// TreeOf returns the git tree id of commit sha. It must come from git
	// itself: Gitea's REST API reports the commit id in its tree fields.
	TreeOf(ctx context.Context, repo, sha string) (string, error)
}

// TrainSpec says what train commit to build.
type TrainSpec struct {
	Branch  string // "train/<pr>"
	BaseSHA string // the base branch head the train is built on
	PR      int
	HeadSHA string // the PR head; the build fails with ErrStale if the forge disagrees
	Message string // the train commit's message
}

// TrainBranch is the train commit CreateTrainBranch built.
type TrainBranch struct {
	SHA     string   // the train commit
	Tree    string   // its git tree id
	Changed []string // paths added or modified relative to BaseSHA, sorted
	Deleted []string // paths deleted relative to BaseSHA, sorted
}

var (
	// ErrConflict means the PR head does not merge cleanly onto the base.
	ErrConflict = errors.New("forge: merge conflict")
	// ErrStale means the PR head moved since the caller read it.
	ErrStale = errors.New("forge: pull request head moved")
	// ErrNotFound means the branch, file, commit or pull request is absent.
	ErrNotFound = errors.New("forge: not found")
)

// StatusError is a non-2xx forge response. It deliberately carries no
// response body: a body can echo a credential (SPEC-0025 REQ-12).
type StatusError struct {
	Method string // the Forge method, e.g. "SquashMerge"
	Status int
	Repo   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("forge: %s on %s: HTTP %d", e.Method, e.Repo, e.Status)
}

// Transient reports whether err is worth retrying on a later tick rather than
// reporting to a pull request's author: a 5xx, a 429, a transport failure, or
// anything unrecognised. The refusals — a 4xx other than 429, ErrConflict,
// ErrStale, ErrNotFound — and a cancelled context are not transient.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status >= 500 || se.Status == statusTooManyRequests
	}
	switch {
	case errors.Is(err, ErrConflict), errors.Is(err, ErrStale), errors.Is(err, ErrNotFound),
		errors.Is(err, context.Canceled):
		return false
	}
	return true
}
