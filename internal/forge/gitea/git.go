package gitea

// Git Plumbing
//
// The two Forge methods REST cannot answer. CreateTrainBranch computes the
// train commit with `git merge-tree --write-tree` (no work tree; exit 1 means
// conflict) and `git commit-tree`, then pushes it to a new branch. TreeOf
// reads a real tree id, which Gitea's REST API does not expose: its commit and
// tree endpoints both report the commit id (ADR-0032 "Why tested == merged").
//
// The token reaches git as an http.extraHeader through GIT_CONFIG_COUNT /
// GIT_CONFIG_KEY_0 / GIT_CONFIG_VALUE_0 in the child's environment — never in
// argv (where ps shows it) and never in a URL (where git may echo it). The
// user's global and system git config are ignored, so a credential helper or
// hook on the host cannot change what the train does.
//
// Governing: ADR-0032 option A1, SPEC-0025 REQ-4, REQ-8, REQ-12.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/stump-wtf/harness/internal/forge"
)

// headsRefspec mirrors the remote's branches into a namespace of the cache
// clone's own, so nothing the train does locally is mistaken for a branch.
const headsRefspec = "+refs/heads/*:refs/mergetrain/heads/*"

// identity is who the train commit is authored and committed by.
type identity struct {
	name, email string
}

// gitError is a failed git invocation. It names the subcommand and exit code;
// stderr is kept out of it, since a remote's error can echo a header.
type gitError struct {
	sub  string
	code int
}

func (e *gitError) Error() string {
	return fmt.Sprintf("gitea: git %s exited %d", e.sub, e.code)
}

// lockRepo serialises git in one repo's clone.
func (c *Client) lockRepo(repo string) func() {
	m, _ := c.gitMu.LoadOrStore(repo, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// git runs one git command in dir and returns its stdout. id, when non-zero,
// becomes the author and committer.
func (c *Client) git(ctx context.Context, dir string, id identity, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.gitBin, args...)
	cmd.Dir = dir
	cmd.Env = c.gitEnv(id)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil // discarded; see gitError
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.Bytes(), &gitError{sub: args[0], code: ee.ExitCode()}
		}
		return stdout.Bytes(), fmt.Errorf("gitea: git %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

// gitEnv is the child's environment: the parent's, minus any git config
// injection it carried, plus ours.
func (c *Client) gitEnv(id identity) []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_CONFIG") || strings.HasPrefix(kv, "GIT_AUTHOR_") ||
			strings.HasPrefix(kv, "GIT_COMMITTER_") || strings.HasPrefix(kv, "GIT_DIR=")
	})
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: token "+c.token,
	)
	if id.name != "" {
		env = append(env,
			"GIT_AUTHOR_NAME="+id.name, "GIT_AUTHOR_EMAIL="+id.email,
			"GIT_COMMITTER_NAME="+id.name, "GIT_COMMITTER_EMAIL="+id.email,
		)
	}
	return env
}

// clone returns repo's bare cache clone, creating it on first use.
func (c *Client) clone(ctx context.Context, repo string) (string, error) {
	if c.cache == "" {
		return "", errors.New("gitea: CacheDir is not set; git operations are unavailable")
	}
	if _, err := repoPath(repo); err != nil {
		return "", err
	}
	dir := filepath.Join(c.cache, filepath.FromSlash(repo)+".git")
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("gitea: create cache clone: %w", err)
	}
	if _, err := c.git(ctx, dir, identity{}, "init", "--quiet", "--bare"); err != nil {
		return "", err
	}
	return dir, nil
}

// CreateTrainBranch builds spec.BaseSHA + a squash of the PR head as one new
// commit and pushes it to the new branch spec.Branch.
func (c *Client) CreateTrainBranch(ctx context.Context, repo string, spec forge.TrainSpec) (forge.TrainBranch, error) {
	if spec.Branch == "" || spec.BaseSHA == "" || spec.HeadSHA == "" || spec.PR <= 0 {
		return forge.TrainBranch{}, errors.New("gitea: CreateTrainBranch needs Branch, BaseSHA, HeadSHA and PR")
	}
	id, err := c.whoami(ctx)
	if err != nil {
		return forge.TrainBranch{}, err
	}
	unlock := c.lockRepo(repo)
	defer unlock()
	dir, err := c.clone(ctx, repo)
	if err != nil {
		return forge.TrainBranch{}, err
	}
	remote := c.gitURL(repo)
	pullRef := "refs/mergetrain/pull/" + strconv.Itoa(spec.PR)

	// Fetch every branch head and the PR head into private refs. Fetching
	// the base by SHA would need uploadpack.allowReachableSHA1InWant on the
	// server; the base is main's head, so fetching heads always brings it.
	if _, err := c.git(ctx, dir, identity{}, "fetch", "--quiet", "--no-tags", "--prune", remote,
		headsRefspec, "+refs/pull/"+strconv.Itoa(spec.PR)+"/head:"+pullRef); err != nil {
		return forge.TrainBranch{}, err
	}
	if _, err := c.git(ctx, dir, identity{}, "cat-file", "-e", spec.BaseSHA+"^{commit}"); err != nil {
		return forge.TrainBranch{}, fmt.Errorf("gitea: base %s is not on the remote: %w", spec.BaseSHA, forge.ErrStale)
	}
	got, err := c.revParse(ctx, dir, pullRef)
	if err != nil {
		return forge.TrainBranch{}, err
	}
	if got != spec.HeadSHA {
		return forge.TrainBranch{}, forge.ErrStale
	}

	out, err := c.git(ctx, dir, identity{}, "merge-tree", "--write-tree", "--no-messages", spec.BaseSHA, spec.HeadSHA)
	var ge *gitError
	if errors.As(err, &ge) && ge.code == 1 {
		return forge.TrainBranch{}, forge.ErrConflict
	}
	if err != nil {
		return forge.TrainBranch{}, err
	}
	tree := firstLine(out)

	msg := spec.Message
	if msg == "" {
		msg = "merge train: #" + strconv.Itoa(spec.PR)
	}
	out, err = c.git(ctx, dir, id, "commit-tree", tree, "-p", spec.BaseSHA, "-m", msg)
	if err != nil {
		return forge.TrainBranch{}, err
	}
	sha := firstLine(out)

	changed, deleted, err := c.diff(ctx, dir, spec.BaseSHA, sha)
	if err != nil {
		return forge.TrainBranch{}, err
	}

	// A plain push creates the branch. It cannot rewrite one: a branch that
	// already exists and is not an ancestor rejects it, and the driver
	// deletes any leftover train/<pr> before building.
	if _, err := c.git(ctx, dir, identity{}, "push", "--quiet", remote, sha+":refs/heads/"+spec.Branch); err != nil {
		return forge.TrainBranch{}, err
	}
	return forge.TrainBranch{SHA: sha, Tree: tree, Changed: changed, Deleted: deleted}, nil
}

// diff lists paths from base to commit: added, modified or type-changed ones
// in changed, deleted ones in deleted, both sorted. Renames are split into a
// delete and an add.
func (c *Client) diff(ctx context.Context, dir, base, commit string) (changed, deleted []string, err error) {
	out, err := c.git(ctx, dir, identity{}, "diff-tree", "-r", "-z", "--no-renames", "--name-status", base, commit)
	if err != nil {
		return nil, nil, err
	}
	fields := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	changed, deleted = []string{}, []string{}
	for i := 0; i+1 < len(fields); i += 2 {
		switch status, path := fields[i], fields[i+1]; status {
		case "D":
			deleted = append(deleted, path)
		default:
			changed = append(changed, path)
		}
	}
	slices.Sort(changed)
	slices.Sort(deleted)
	return changed, deleted, nil
}

// TreeOf returns the git tree id of commit sha, fetching it if needed.
func (c *Client) TreeOf(ctx context.Context, repo, sha string) (string, error) {
	unlock := c.lockRepo(repo)
	defer unlock()
	dir, err := c.clone(ctx, repo)
	if err != nil {
		return "", err
	}
	if _, err := c.git(ctx, dir, identity{}, "cat-file", "-e", sha+"^{commit}"); err != nil {
		if _, err := c.git(ctx, dir, identity{}, "fetch", "--quiet", "--no-tags", "--prune", c.gitURL(repo), headsRefspec); err != nil {
			return "", err
		}
	}
	return c.revParse(ctx, dir, sha+"^{tree}")
}

func (c *Client) revParse(ctx context.Context, dir, rev string) (string, error) {
	out, err := c.git(ctx, dir, identity{}, "rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		return "", err
	}
	return firstLine(out), nil
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(s)
}
