# Design: Merge Train

## Context

ADR-0032 decides the shape: one flock-held driver per repo, git plumbing for
the train commit, a head-pinned API squash merge, tree-id verification, and a
marker comment as the author's todo. This document fixes the code layout and
the one interface every story builds against, so each story in #540 can cite
it.

## Goals / Non-Goals

### Goals

- One `Forge` interface through which every forge interaction passes, with an
  in-memory fake that the whole state machine is tested against.
- Pure, I/O-free eligibility and ordering.
- A Gitea implementation that never lets the token reach argv, a log line or
  an error string.

### Non-Goals

- A GitHub implementation. Gitea is where our CI gates merges.
- Batching several PRs into one train commit.
- Writing Switchboard todos (ADR-0032 E1).

## Decisions

### Package layout, and why the PR types live in `internal/forge`

```
internal/forge/            types.go   PullRequest, Review
                           forge.go   Forge, TrainSpec, TrainBranch, errors
internal/forge/fake/       fake.go    in-memory Forge
internal/forge/gitea/      gitea.go   REST
                           git.go     bare cache clone: build, fetch, tree ids
internal/mergetrain/       types.go   aliases of the forge types
                           eligibility.go order.go verify.go
                           train.go   one attempt: build, wait, delete
                           driver.go  the loop, failure handling, comments
                           lock.go    the per-repo flock
                           marker.go  the comment marker
```

The stories were first sketched with `PullRequest` declared in
`internal/mergetrain` and `Forge.ListOpenPRs` returning
`[]mergetrain.PullRequest`. That cannot compile once `mergetrain` calls the
forge — `VerifyLanded`, the train and the driver all take a `forge.Forge` — as
`forge` and `mergetrain` would import each other. So the structs are declared
in `internal/forge` (a leaf: it imports only the standard library), and
`internal/mergetrain/types.go` re-exports them as aliases:

```go
type PullRequest = forge.PullRequest
type Review = forge.Review
```

Every signature in the stories keeps its spelling: `mergetrain.PullRequest`,
`mergetrain.Eligible(pr)`, `forge.Forge`, `VerifyLanded(ctx, f forge.Forge, …)`.
Tests in `internal/mergetrain` that use the fake are in the external
`mergetrain_test` package, because `forge/fake` imports `forge`, which a
`package mergetrain` test importing the fake would otherwise cycle through.

### The types

```go
package forge

type Review struct {
	Author      string
	State       string // "APPROVED", "REQUEST_CHANGES", "COMMENT"
	CommitID    string // head SHA the review was left on
	SubmittedAt time.Time
}

type PullRequest struct {
	Number    int
	Author    string
	HeadSHA   string
	Reviews   []Review
	CIState   string // "success", "pending", "failure", "error"
	Draft     bool
	Mergeable bool
}
```

### The `Forge` interface

```go
type Forge interface {
	ListOpenPRs(ctx context.Context, repo string) ([]PullRequest, error)
	BranchHead(ctx context.Context, repo, branch string) (sha string, err error)
	CreateTrainBranch(ctx context.Context, repo string, spec TrainSpec) (TrainBranch, error)
	DeleteBranch(ctx context.Context, repo, name string) error
	CombinedStatus(ctx context.Context, repo, sha string) (string, error) // "success"|"pending"|"failure"|"error"
	SquashMerge(ctx context.Context, repo string, pr int, headSHA, title, message string) (mergedSHA string, err error)
	Comment(ctx context.Context, repo string, pr int, body string) error
	ListComments(ctx context.Context, repo string, pr int) ([]string, error)
	FileContentAtRef(ctx context.Context, repo, path, ref string) ([]byte, error)
	TreeOf(ctx context.Context, repo, sha string) (string, error)
}

type TrainSpec struct {
	Branch  string // "train/<pr>"
	BaseSHA string // the base branch head the train is built on
	PR      int
	HeadSHA string // the PR head; the build fails with ErrStale if the forge disagrees
	Message string // the train commit's message
}

type TrainBranch struct {
	SHA     string   // the train commit
	Tree    string   // its git tree id
	Changed []string // paths added or modified relative to BaseSHA, sorted
	Deleted []string // paths deleted relative to BaseSHA, sorted
}

var (
	ErrConflict = errors.New("forge: merge conflict")
	ErrStale    = errors.New("forge: pull request head moved")
	ErrNotFound = errors.New("forge: not found")
)

// StatusError is a non-2xx response. It never carries the response body.
type StatusError struct {
	Method string // the Forge method, e.g. "SquashMerge"
	Status int
	Repo   string
}

// Transient reports whether err is worth retrying on a later tick: a 5xx,
// a 429, or a transport error. A 4xx other than 429 is a refusal.
func Transient(err error) bool
```

Departures from the interface sketched in #599, each forced by ADR-0032:

| Change | Why |
|---|---|
| `CreateBranch(name, fromSHA)` → `CreateTrainBranch(spec)` | the train commit does not exist on the forge until the train makes it; a branch-at-SHA call cannot create a merge (ADR-0032 A1) |
| `SquashMerge` gains `headSHA` | pins the head in the merge call itself, so the forge refuses a merge of a head that moved (REQ-7) |
| `+ BranchHead` | `base` is read before the build and re-read before the merge (REQ-4, REQ-7, REQ-8) |
| `+ ListComments` | the one-comment-per-head dedupe lives on the forge and survives a restart (REQ-9) |
| `+ TreeOf` | tree equality after the merge; the Gitea REST API reports the commit id as the tree id, so this is answered by git (REQ-8) |

### The fake

`internal/forge/fake` keeps PRs, branches (`name → sha`), statuses, files
(`ref → path → bytes`), trees (`sha → tree`) and comments in maps under one
`sync.Mutex`. It records every call, in order, as `Call{Method, Args}`, and
`Err(method, err)` forces the named method to fail until cleared. Hooks let a
test script behaviour over time: `OnStatus(func(sha string, n int) string)`
returns the status on the n-th poll, and `OnTrain(func(TrainSpec) (TrainBranch, error))`
decides what a build produces. `SquashMerge` of an unknown PR errors; of a known
open PR at the given head it marks it merged, moves the base branch to a
synthetic SHA, and returns that SHA; `TreeOf` of that SHA returns the tree the
test set (by default, the tree of the last train built for the PR).

### The Gitea implementation

REST, all under `<base>/api/v1`, with `Authorization: token <t>` and a 30 s
per-request timeout:

| Method | Call |
|---|---|
| `ListOpenPRs` | `GET /repos/{r}/pulls?state=open&limit=50` (paged), then per PR `GET /repos/{r}/pulls/{n}/reviews` and `GET /repos/{r}/commits/{head}/status` |
| `BranchHead` | `GET /repos/{r}/branches/{branch}` → `commit.id` |
| `CombinedStatus` | `GET /repos/{r}/commits/{sha}/status` → `state`; `total_count == 0` → `pending` |
| `DeleteBranch` | `DELETE /repos/{r}/branches/{name}` (the name path-escaped); 404 is success |
| `SquashMerge` | `POST /repos/{r}/pulls/{n}/merge` `{"Do":"squash","MergeTitleField":…,"MergeMessageField":…,"head_commit_id":…}`, then `GET /repos/{r}/pulls/{n}` → `merge_commit_sha` |
| `Comment` | `POST /repos/{r}/issues/{n}/comments` `{"body":…}` |
| `ListComments` | `GET /repos/{r}/issues/{n}/comments` (paged) |
| `FileContentAtRef` | `GET /repos/{r}/raw/{path}?ref={ref}`; 404 → `ErrNotFound` |

`raw` takes the ref as a query parameter, not a path segment, because a branch
name may contain `/` (`train/12`) and the path form is then ambiguous.

git, in a bare clone at `<cache>/<owner>/<name>.git`, authenticated by
`GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_0=http.extraHeader`/`GIT_CONFIG_VALUE_0=Authorization: token <t>`
in the child's environment — never a URL with credentials, never argv:

| Method | Commands |
|---|---|
| `CreateTrainBranch` | `fetch origin <base> refs/pull/<n>/head`; check `FETCH_HEAD` of the PR equals `HeadSHA` else `ErrStale`; `merge-tree --write-tree <base> <head>` (exit 1 → `ErrConflict`); `commit-tree <tree> -p <base> -m <msg>`; `diff-tree -r --name-status <base> <commit>`; `push origin <commit>:refs/heads/<branch>` |
| `TreeOf` | `fetch origin <sha>` if absent; `rev-parse <sha>^{tree}` |

Commits are authored and committed as the token's user, read once from
`GET /user` at construction and passed as `GIT_AUTHOR_*`/`GIT_COMMITTER_*`.

### Logging

`mergetrain` takes a small interface that `charmbracelet/log`'s `*Logger`
satisfies, so the daemon passes its own logger and tests pass a recorder:

```go
type Logger interface {
	Info(msg interface{}, keyvals ...interface{})
	Warn(msg interface{}, keyvals ...interface{})
	Error(msg interface{}, keyvals ...interface{})
}
```

### Configuration (#604)

```toml
[mergetrain]
enabled = false                 # REQ-1
mode = "report"                 # or "merge"
repos = ["stump.wtf/harness"]
base_branch = "main"
poll_interval = "60s"
ci_timeout = "30m"
forge_base_url = "https://gitea.stump.rocks"
forge_token_env = "HARNESS_MERGETRAIN_TOKEN"   # the variable's NAME
```

A change to `[mergetrain]` takes effect at the next daemon restart, like
`[telemetry]`.

## Risks / Trade-offs

- **An untested tree can land in a race** (a bypass in the milliseconds
  between the re-read and the merge). Bounded to one merge, detected by
  REQ-8, and the train halts.
- **Head-of-line blocking by a slow pipeline.** One attempt per tick, one PR
  per attempt. `ci_timeout` bounds it.
- **The Gitea `mergeable` flag lags.** It is recomputed asynchronously after a
  push; a PR may read unmergeable for a few seconds. It is simply re-evaluated
  next tick.

## Migration Plan

1. Land the code with the train off (#597–#604).
2. Add `train/**` to the pipeline's `push` trigger and a `train/*` protection
   rule allowing only `joestump-agent` to push (#605).
3. Pilot in `report` mode on `stump.wtf/harness`, with
   `block_on_outdated_branch` still on (#605).
4. In one change: `mode = "merge"` and `block_on_outdated_branch: false` on
   harness only. Rollback is the reverse API call, recorded on #605.
