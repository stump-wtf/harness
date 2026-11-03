package github

// Reading a File at a Commit
//
// VerifyLanded compares the bytes the tested tree had against the bytes the
// base branch has now, so this has to return exactly the file, at exactly that
// commit, for every path the train commit touched.
//
// GitHub's contents endpoint answers a request with the raw media type for
// anything up to 1 MB and refuses a larger file with 403. A lockfile in a real
// repository clears that easily, and "we could not read it" is not an answer
// the train can act on, so a refusal falls back to the object route: resolve
// the path's blob id in the commit's recursive tree listing and read that blob
// raw, which has no size cap of its own. The tree listing is kept for the
// commit it was read for, so a train that reads twenty paths lists the tree
// once.
//
// Governing: SPEC-0025 REQ-8; ADR-0061.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/stump-wtf/harness/internal/forge"
)

const rawAccept = "application/vnd.github.raw+json"

// treeCache remembers one commit's path→blob map.
type treeCache struct {
	mu   sync.Mutex
	key  string
	path map[string]string
}

// FileContentAtRef returns path's bytes at ref (a branch name or a SHA), or
// forge.ErrNotFound.
func (c *Client) FileContentAtRef(ctx context.Context, repo, path, ref string) ([]byte, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	p := strings.TrimPrefix(path, "/")
	b, err := c.raw(ctx, "FileContentAtRef", repo, rp+"/contents/"+escapeSegments(p)+"?ref="+url.QueryEscape(ref))
	if err == nil {
		return b, nil
	}
	var se *forge.StatusError
	if !errors.As(err, &se) {
		return nil, err
	}
	if se.Status == http.StatusNotFound {
		return nil, err
	}
	if se.Status != http.StatusForbidden && se.Status != http.StatusRequestEntityTooLarge {
		return nil, err
	}
	// Too large for the contents endpoint: read the blob instead.
	sha, err := c.blobSHA(ctx, repo, rp, ref, p)
	if err != nil {
		return nil, err
	}
	return c.raw(ctx, "FileContentAtRef", repo, rp+"/git/blobs/"+url.PathEscape(sha))
}

// blobSHA resolves path to a blob id in ref's tree, listing the tree at most
// once per commit.
func (c *Client) blobSHA(ctx context.Context, repo, rp, ref, path string) (string, error) {
	key := repo + "\000" + ref
	c.trees.mu.Lock()
	defer c.trees.mu.Unlock()
	if c.trees.key != key || c.trees.path == nil {
		var t struct {
			Truncated bool `json:"truncated"`
			Tree      []struct {
				Path string `json:"path"`
				Type string `json:"type"`
				SHA  string `json:"sha"`
			} `json:"tree"`
		}
		if err := c.do(ctx, "FileContentAtRef", repo, http.MethodGet,
			rp+"/git/trees/"+escapeSegments(ref)+"?recursive=1", nil, &t); err != nil {
			return "", err
		}
		if t.Truncated {
			return "", fmt.Errorf("github: %s: tree of %s is truncated; the file cannot be located", repo, ref)
		}
		m := make(map[string]string, len(t.Tree))
		for _, e := range t.Tree {
			if e.Type == "blob" {
				m[e.Path] = e.SHA
			}
		}
		c.trees.key, c.trees.path = key, m
	}
	sha, ok := c.trees.path[path]
	if !ok {
		return "", fmt.Errorf("github: FileContentAtRef on %s: %s is not in %s: %w", repo, path, ref, forge.ErrNotFound)
	}
	return sha, nil
}

// raw performs a GET that returns bytes rather than JSON.
func (c *Client) raw(ctx context.Context, method, repo, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api+path, nil)
	if err != nil {
		return nil, fmt.Errorf("github: %s on %s: build request: %w", method, repo, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", rawAccept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("github: %s on %s: %w", method, repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, forge.BodyLimit))
		return nil, notFound(&forge.StatusError{Method: method, Status: resp.StatusCode, Repo: repo, Body: forge.SanitizeBody(c.token, body)})
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("github: %s on %s: read: %w", method, repo, err)
	}
	return b, nil
}
