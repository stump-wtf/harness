// Package mergetrain lands approved pull requests one at a time, testing the
// exact tree that will become the base branch before merging it.
//
// Governing: ADR-0032 (merge train), SPEC-0025.
package mergetrain

// Domain Types
//
// The pull request types are declared in internal/forge and aliased here
// (SPEC-0025 design.md "Package layout"): mergetrain depends on the Forge
// interface, so the types cannot live in this package without an import cycle.
// An alias, not a new type, so a forge.PullRequest needs no conversion.

import "github.com/stump-wtf/harness/internal/forge"

// Review is one review left on a pull request.
type Review = forge.Review

// PullRequest is an open pull request as the train sees it.
type PullRequest = forge.PullRequest

// Review states, as the forge reports them.
const (
	StateApproved       = "APPROVED"
	StateRequestChanges = "REQUEST_CHANGES"
)

// CISuccess is the only combined CI state that lets a pull request, or a
// train commit, through.
const CISuccess = "success"
