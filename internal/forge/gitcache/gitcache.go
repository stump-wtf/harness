// Package gitcache is the content-addressed git plumbing every forge backend
// needs and no forge's REST API can do: build the train commit without a work
// tree, and read a real tree id.
//
// A forge reports a commit's "tree" as the commit id, so the only way to know
// that the tree CI tested is the tree that landed is to ask git (ADR-0032
// "Why tested == merged"). Building the train commit is the same story: the
// three-way merge is computed with `git merge-tree --write-tree`, which needs
// no checkout, and pushed as a new branch. Both want a repository on disk, so
// each backend keeps one bare clone per repo in a cache directory.
//
// The credential reaches git as an http.extraHeader through GIT_CONFIG_COUNT /
// GIT_CONFIG_KEY_0 / GIT_CONFIG_VALUE_0 in the child's environment — never in
// argv (where ps shows it) and never in a URL (where git may echo it). The
// user's global and system git config are ignored, so a credential helper or
// hook on the host cannot change what the train does.
//
// Governing: ADR-0032 option A1, ADR-0061; SPEC-0025 REQ-4, REQ-8, REQ-12.
package gitcache

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

// Identity is who a train commit is authored and committed by.
type Identity struct{ Name, Email string }

// Options configures a Cache.
type Options struct {
	// Prefix names the calling backend in an error, e.g. "gitea", so a failed
	// git child reads the same as it did when the plumbing lived there.
	Prefix string
	// AuthHeader is the full HTTP header the cache clone sends for
	// authentication, e.g. "Authorization: token …". It never appears in argv
	// or a URL.
	AuthHeader string
	// CacheDir holds one bare clone per repo. Required.
	CacheDir string
	// GitBinary is the git executable. Default "git".
	GitBinary string
	// GitURL returns the clone URL for repo ("owner/name"). Tests point it at
	// a local bare repository.
	GitURL func(repo string) string
}

// Cache runs git in a directory of bare clones, one per repo.
type Cache struct {
	Prefix     string
	AuthHeader string
	CacheDir   string
	GitBinary  string
	GitURL     func(repo string) string

	mu sync.Map // repo → *sync.Mutex, serialising git per clone
}

// New returns a Cache with the defaults filled in. It makes no call.
func New(o Options) *Cache {
	bin := o.GitBinary
	if bin == "" {
		bin = "git"
	}
	gitURL := o.GitURL
	if gitURL == nil {
		gitURL = func(repo string) string { return "https://" + repo + ".git" }
	}
	return &Cache{Prefix: o.Prefix, AuthHeader: o.AuthHeader, CacheDir: o.CacheDir, GitBinary: bin, GitURL: gitURL}
}

// Error is a failed git invocation. It names the subcommand and exit code;
// stderr is kept out of it, since a remote's error can echo a header.
type Error struct {
	Prefix string
	Sub    string
	Code   int
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: git %s exited %d", e.Prefix, e.Sub, e.Code)
}

// lockRepo serialises git in one repo's clone.
func (c *Cache) lockRepo(repo string) func() {
	m, _ := c.mu.LoadOrStore(repo, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// git runs one git command in dir and returns its stdout. id, when non-zero,
// becomes the author and committer.
func (c *Cache) git(ctx context.Context, dir string, id Identity, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.GitBinary, args...)
	cmd.Dir = dir
	cmd.Env = c.env(id)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil // discarded; see Error
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return stdout.Bytes(), ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.Bytes(), &Error{Prefix: c.Prefix, Sub: args[0], Code: ee.ExitCode()}
		}
		return stdout.Bytes(), fmt.Errorf("%s: git %s: %w", c.Prefix, args[0], err)
	}
	return stdout.Bytes(), nil
}

// env is the child's environment: the parent's, minus any git config injection
// it carried, plus ours.
func (c *Cache) env(id Identity) []string {
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
		"GIT_CONFIG_VALUE_0="+c.AuthHeader,
	)
	if id.Name != "" {
		env = append(env,
			"GIT_AUTHOR_NAME="+id.Name, "GIT_AUTHOR_EMAIL="+id.Email,
			"GIT_COMMITTER_NAME="+id.Name, "GIT_COMMITTER_EMAIL="+id.Email,
		)
	}
	return env
}

// clone returns repo's bare cache clone, creating it on first use.
func (c *Cache) clone(ctx context.Context, repo string) (string, error) {
	if c.CacheDir == "" {
		return "", fmt.Errorf("%s: CacheDir is not set; git operations are unavailable", c.Prefix)
	}
	dir := filepath.Join(c.CacheDir, filepath.FromSlash(repo)+".git")
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("%s: create cache clone: %w", c.Prefix, err)
	}
	if _, err := c.git(ctx, dir, Identity{}, "init", "--quiet", "--bare"); err != nil {
		return "", err
	}
	return dir, nil
}

// CreateTrainBranch builds spec.BaseSHA plus a squash of every pinned head,
// folded in order, as one new commit and pushes it to the new branch
// spec.Branch. With one head this is exactly the single-PR train commit; with
// several (SPEC-0025 REQ-17) head i is folded onto the tree the heads before it
// produced, and a rejection is attributed to that head as a
// *forge.ConflictError. id is who the resulting commit is attributed to.
func (c *Cache) CreateTrainBranch(ctx context.Context, repo string, spec forge.TrainSpec, id Identity) (forge.TrainBranch, error) {
	heads := spec.Heads
	if len(heads) == 0 {
		heads = []forge.PRHead{{PR: spec.PR, HeadSHA: spec.HeadSHA}}
	}
	if spec.Branch == "" || spec.BaseSHA == "" {
		return forge.TrainBranch{}, errors.New(c.Prefix + ": CreateTrainBranch needs Branch and BaseSHA")
	}
	for _, h := range heads {
		if h.PR <= 0 || h.HeadSHA == "" {
			return forge.TrainBranch{}, errors.New(c.Prefix + ": CreateTrainBranch needs a PR number and head SHA for every head")
		}
	}
	unlock := c.lockRepo(repo)
	defer unlock()
	dir, err := c.clone(ctx, repo)
	if err != nil {
		return forge.TrainBranch{}, err
	}
	remote := c.GitURL(repo)

	// Fetch every branch head and every pinned PR head into private refs.
	// Fetching the base by SHA would need uploadpack.allowReachableSHA1InWant
	// on the server; the base is main's head, so fetching heads always brings
	// it.
	refspecs := make([]string, 0, len(heads)+1)
	refspecs = append(refspecs, headsRefspec)
	for _, h := range heads {
		refspecs = append(refspecs, "+refs/pull/"+strconv.Itoa(h.PR)+"/head:"+pullRef(h.PR))
	}
	if _, err := c.git(ctx, dir, Identity{}, append([]string{"fetch", "--quiet", "--no-tags", "--prune", remote}, refspecs...)...); err != nil {
		return forge.TrainBranch{}, err
	}
	if _, err := c.git(ctx, dir, Identity{}, "cat-file", "-e", spec.BaseSHA+"^{commit}"); err != nil {
		return forge.TrainBranch{}, fmt.Errorf("%s: base %s is not on the remote: %w", c.Prefix, spec.BaseSHA, forge.ErrStale)
	}
	for _, h := range heads {
		got, err := c.revParse(ctx, dir, pullRef(h.PR))
		if err != nil {
			return forge.TrainBranch{}, err
		}
		if got != h.HeadSHA {
			return forge.TrainBranch{}, forge.ErrStale
		}
	}

	// Fold the heads in order. merge-tree takes commits, not trees, so the
	// tree folded so far travels in a local commit-tree whose parent is the
	// base: merge-base(that commit, the next head) is then merge-base(base,
	// the next head), which is exactly the squash the forge will apply onto
	// the growing base. These fold commits are local plumbing; only the final
	// commit is pushed.
	tree := ""
	for i, h := range heads {
		side1 := spec.BaseSHA
		if i > 0 {
			// The bot identity, same as the final commit below: the child
			// environment carries no git config (GIT_CONFIG_GLOBAL=/dev/null),
			// so a commit-tree with an empty identity depends on the runner's
			// fallback email auto-detection and dies where that is absent.
			out, err := c.git(ctx, dir, id, "commit-tree", tree, "-p", spec.BaseSHA, "-m", "merge train fold")
			if err != nil {
				return forge.TrainBranch{}, err
			}
			side1 = firstLine(out)
		}
		out, err := c.git(ctx, dir, Identity{}, "merge-tree", "--write-tree", "--no-messages", side1, pullRef(h.PR))
		var ge *Error
		if errors.As(err, &ge) && ge.Code == 1 {
			if len(heads) == 1 {
				return forge.TrainBranch{}, forge.ErrConflict
			}
			return forge.TrainBranch{}, &forge.ConflictError{PR: h.PR}
		}
		if err != nil {
			return forge.TrainBranch{}, err
		}
		tree = firstLine(out)
	}

	msg := spec.Message
	if msg == "" {
		msg = "merge train: #" + strconv.Itoa(spec.PR)
	}
	out, err := c.git(ctx, dir, id, "commit-tree", tree, "-p", spec.BaseSHA, "-m", msg)
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
	// deletes any leftover train branch before building.
	if _, err := c.git(ctx, dir, Identity{}, "push", "--quiet", remote, sha+":refs/heads/"+spec.Branch); err != nil {
		return forge.TrainBranch{}, err
	}
	return forge.TrainBranch{SHA: sha, Tree: tree, Changed: changed, Deleted: deleted}, nil
}

// TreeOf returns the git tree id of commit sha, fetching it if needed.
func (c *Cache) TreeOf(ctx context.Context, repo, sha string) (string, error) {
	unlock := c.lockRepo(repo)
	defer unlock()
	dir, err := c.clone(ctx, repo)
	if err != nil {
		return "", err
	}
	if _, err := c.git(ctx, dir, Identity{}, "cat-file", "-e", sha+"^{commit}"); err != nil {
		if _, err := c.git(ctx, dir, Identity{}, "fetch", "--quiet", "--no-tags", "--prune", c.GitURL(repo), headsRefspec); err != nil {
			return "", err
		}
	}
	return c.revParse(ctx, dir, sha+"^{tree}")
}

// TreePaths lists the files under prefixes at commit sha, sorted. The train
// uses it to read a whole watched directory, which a diff of one head cannot
// show: the other claimant of a sequence number is usually not in the diff at
// all (ADR-0052).
func (c *Cache) TreePaths(ctx context.Context, repo, sha string, prefixes []string) ([]string, error) {
	unlock := c.lockRepo(repo)
	defer unlock()
	dir, err := c.clone(ctx, repo)
	if err != nil {
		return nil, err
	}
	if _, err := c.git(ctx, dir, Identity{}, "cat-file", "-e", sha+"^{commit}"); err != nil {
		if _, err := c.git(ctx, dir, Identity{}, "fetch", "--quiet", "--no-tags", "--prune", c.GitURL(repo), headsRefspec); err != nil {
			return nil, err
		}
	}
	args := []string{"ls-tree", "-r", "-z", "--name-only", sha}
	if len(prefixes) > 0 {
		args = append(args, "--")
		for _, p := range prefixes {
			args = append(args, strings.TrimSuffix(p, "/"))
		}
	}
	out, err := c.git(ctx, dir, Identity{}, args...)
	if err != nil {
		return nil, err
	}
	paths := strings.FieldsFunc(string(out), func(r rune) bool { return r == 0 })
	for i, p := range paths {
		paths[i] = strings.TrimSuffix(p, "\n")
	}
	slices.Sort(paths)
	return paths, nil
}

// pullRef is the private ref a PR head is fetched into.
func pullRef(pr int) string { return "refs/mergetrain/pull/" + strconv.Itoa(pr) }

// diff lists paths from base to commit: added, modified or type-changed ones
// in changed, deleted ones in deleted, both sorted. Renames are split into a
// delete and an add.
func (c *Cache) diff(ctx context.Context, dir, base, commit string) (changed, deleted []string, err error) {
	out, err := c.git(ctx, dir, Identity{}, "diff-tree", "-r", "-z", "--no-renames", "--name-status", base, commit)
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

func (c *Cache) revParse(ctx context.Context, dir, rev string) (string, error) {
	out, err := c.git(ctx, dir, Identity{}, "rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		return "", err
	}
	return firstLine(out), nil
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(s)
}
