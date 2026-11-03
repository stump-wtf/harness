package gitea

// Git Plumbing
//
// The two Forge methods REST cannot answer. They are the shared content-addressed
// plumbing in internal/forge/gitcache seen from Gitea: who the train commit is
// authored by comes from the token's user, and the clone authenticates with the
// same header the REST client sends.
//
// Governing: ADR-0032 option A1, SPEC-0025 REQ-4, REQ-8, REQ-12.

import (
	"context"

	"github.com/stump-wtf/harness/internal/forge"
	"github.com/stump-wtf/harness/internal/forge/gitcache"
)

// identity is who the train commit is authored and committed by.
type identity struct {
	name, email string
}

// CreateTrainBranch builds the train commit and pushes it to spec.Branch,
// authored by the token's user.
func (c *Client) CreateTrainBranch(ctx context.Context, repo string, spec forge.TrainSpec) (forge.TrainBranch, error) {
	if _, err := repoPath(repo); err != nil {
		return forge.TrainBranch{}, err
	}
	id, err := c.whoami(ctx)
	if err != nil {
		return forge.TrainBranch{}, err
	}
	return c.git.CreateTrainBranch(ctx, repo, spec, gitcache.Identity{Name: id.name, Email: id.email})
}

// TreeOf returns the git tree id of commit sha, fetching it if needed.
func (c *Client) TreeOf(ctx context.Context, repo, sha string) (string, error) {
	if _, err := repoPath(repo); err != nil {
		return "", err
	}
	return c.git.TreeOf(ctx, repo, sha)
}
