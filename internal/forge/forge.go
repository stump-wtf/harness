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
// pins the head, and BranchHead, ListComments and TreeOf were added. TreePaths
// came later, for the train's sequence-collision check (ADR-0052).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// statusTooManyRequests is HTTP 429. Spelled out rather than imported: this
// package stays free of net/http (#599), which belongs to forge/gitea.
const statusTooManyRequests = 429

// statusMethodNotAllowed is HTTP 405, the status Gitea answers a squash merge
// it will not perform with. It is also the status GitHub uses for a merge it
// refuses, so matching on the status alone never identifies a reason: the body
// carries that (#977).
const statusMethodNotAllowed = 405

// approvalsRefusalRE matches the wording a forge uses to refuse a merge
// because it counts too few approvals. Gitea's is "Does not have enough
// approvals", and it is the answer a PR gets when its APPROVED reviews are no
// longer official, which is a state the reviewer can produce without touching
// the code (ADR-0050).
var approvalsRefusalRE = regexp.MustCompile(`(?i)(not enough |insufficient |does not have (enough|sufficient) )approvals`)

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
	// TreePaths returns every file path in commit sha's tree that lies under
	// one of prefixes, sorted. A prefix names a directory; an empty list
	// means the whole tree. The train needs it to see a sequence claim the
	// diff alone does not reveal (ADR-0052).
	TreePaths(ctx context.Context, repo, sha string, prefixes []string) ([]string, error)
}

// TrainSpec says what train commit to build.
type TrainSpec struct {
	Branch  string // "train/<pr>", or "train/batch/<pr>-…" when Heads carries more than one head
	BaseSHA string // the base branch head the train is built on
	PR      int    // the first PR in the batch
	HeadSHA string // the first PR's head; the build fails with ErrStale if the forge disagrees
	// Heads pins every PR in the batch, in the order their heads fold onto
	// BaseSHA (SPEC-0025 REQ-17). Empty means the single-PR spelling: exactly
	// PR and HeadSHA.
	Heads   []PRHead
	Message string // the train commit's message
}

// PRHead pins one pull request's head inside a batch TrainSpec
// (SPEC-0025 REQ-17).
type PRHead struct {
	PR      int
	HeadSHA string
}

// TrainBranch is the train commit CreateTrainBranch built.
type TrainBranch struct {
	SHA     string   // the train commit
	Tree    string   // its git tree id
	Changed []string // paths added or modified relative to BaseSHA, sorted
	Deleted []string // paths deleted relative to BaseSHA, sorted
}

// ConflictError attributes a batch build's conflict to the first PR whose
// head the in-order fold rejected (SPEC-0025 REQ-17). It unwraps to
// ErrConflict, so callers that do not care which PR rejected keep matching
// the sentinel.
type ConflictError struct {
	PR int
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("forge: merge conflict at PR %d", e.PR)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

var (
	// ErrConflict means the PR head does not merge cleanly onto the base.
	ErrConflict = errors.New("forge: merge conflict")
	// ErrStale means the PR head moved since the caller read it.
	ErrStale = errors.New("forge: pull request head moved")
	// ErrNotFound means the branch, file, commit or pull request is absent.
	ErrNotFound = errors.New("forge: not found")
)

// StatusError is a non-2xx forge response. It carries the response body only
// as a bounded, credential-redacted summary (ADR-0049, amending SPEC-0025
// REQ-12): a raw body can echo a credential.
type StatusError struct {
	Method string // the Forge method, e.g. "SquashMerge"
	Status int
	Repo   string
	Body   string // SanitizeBody of the response body; empty when there was none
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("forge: %s on %s: HTTP %d", e.Method, e.Repo, e.Status)
	}
	return fmt.Sprintf("forge: %s on %s: HTTP %d: %s", e.Method, e.Repo, e.Status, e.Body)
}

// BodyLimit bounds the response-body summary carried in a StatusError.
const BodyLimit = 512

// authSchemeRe matches a scheme and credential in free text: Bearer xyz,
// token xyz, Basic xyz.
var authSchemeRe = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+([A-Za-z0-9\-._~+/]+=*)`)

// authJSONRe matches a credential-bearing field and its value in a JSON
// object: "Authorization":"token xyz", "api_key":"xyz".
var authJSONRe = regexp.MustCompile(`(?i)"(authorization|proxy-authorization|cookie|x-api-key|api[_-]key|x-csrf-token|x-xsrf-token)"\s*:\s*("[^"]*"|[^,}\s]+)`)

// authPlainRe matches the same fields in a header or query form:
// Cookie: abc, api-key=xyz, api_key=xyz.
var authPlainRe = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|cookie|x-api-key|api[_-]key|x-csrf-token|x-xsrf-token)\s*[:=]\s*(?:"[^"]*"|'[^']*'|\S+)`)

// SanitizeBody reduces a non-2xx response body to the bounded summary a
// StatusError carries. A hostile or broken forge can echo the Authorization
// header or a query token in an error body, so the client's own token is
// replaced and credential-bearing scheme, JSON and plain field values are
// redacted (ADR-0049, amending SPEC-0025 REQ-12).
func SanitizeBody(token string, body []byte) string {
	s := string(body)
	if token != "" {
		s = strings.ReplaceAll(s, token, "[redacted]")
	}
	// The scheme first: a field value that begins with a scheme word would
	// otherwise leave the credential behind it standing.
	s = authSchemeRe.ReplaceAllString(s, "$1 [redacted]")
	s = authJSONRe.ReplaceAllString(s, `"$1":"[redacted]"`)
	s = authPlainRe.ReplaceAllString(s, "$1: [redacted]")
	return truncateBody(s)
}

// truncateBody caps s at BodyLimit without splitting a UTF-8 rune.
func truncateBody(s string) string {
	if len(s) <= BodyLimit {
		return s
	}
	cut := BodyLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
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

// InsufficientApprovals reports whether err is the forge refusing a merge
// because the approvals it counts do not meet the branch's requirement, as
// opposed to refusing it for any other reason. Only a refusal whose body says
// so matches: a 405 with no body, or with a body that says something else, is
// not an approvals refusal, because the caller's advice differs and being
// wrong about it costs a reviewer a round trip (ADR-0050).
func InsufficientApprovals(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) || se.Status != statusMethodNotAllowed {
		return false
	}
	return approvalsRefusalRE.MatchString(se.Body)
}
