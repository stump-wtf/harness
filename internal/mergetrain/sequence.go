package mergetrain

// Sequence Collision Detection
//
// ADR, spec and migration files carry their identity in a number in the file
// name, and two pull requests that each add one of those files with the same
// number merge cleanly: the paths differ, so nothing conflicts, and the base
// branch ends up holding two ADR-0050 files. Renaming one afterwards is a
// push, which dismisses approvals and re-runs CI, so the renumbering happens
// by hand instead (ADR-0052). The train judges a sequence claim the way it
// judges a content conflict: on the train commit, before anything merges.
//
// A file claims a sequence number when the first run of two or more digits
// in its base name says so, and the claim is scoped to the file's directory,
// so an ADR and a migration may share a number. A pair already sitting on the
// base branch is not this attempt's doing, and refusing it would block the
// train forever, so only a collision the attempt introduces is a verdict.
//
// Governing: ADR-0032 (the attempt), ADR-0052 (sequence claims), SPEC-0025.

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/stump-wtf/harness/internal/forge"
)

// sequenceDigitsRE matches the digit run a file name claims: two digits or
// more, so a plain "v2" in a name is not read as a claim, while a number
// written without its zero padding still is.
var sequenceDigitsRE = regexp.MustCompile(`\d{2,}`)

// SequenceClaim is one file's claim on a sequence number, and who brings it.
type SequenceClaim struct {
	Path string
	// PR is the pull request that adds the file. 0 means the train could not
	// say which head brought it.
	PR int
	// OnBase marks a file already on the base branch, which no PR of this
	// attempt can rename.
	OnBase bool
}

// SequenceCollision is one sequence number two or more files of one train
// commit claim under a watched path.
type SequenceCollision struct {
	Dir string
	// Number is the claimed number without leading zeros; Digits is how the
	// first claimant spells it.
	Number string
	Digits string
	// Claims holds every file that claims the number, sorted by path.
	Claims []SequenceClaim
}

// Introduced returns the colliding paths this attempt brings, which are the
// ones an author can rename.
func (c SequenceCollision) Introduced() []string {
	var out []string
	for _, cl := range c.Claims {
		if !cl.OnBase {
			out = append(out, cl.Path)
		}
	}
	return out
}

// sequenceClaimKey scopes a claimed number to the directory holding it.
type sequenceClaimKey struct {
	dir, number string
}

// sequenceClaim reports the claim path makes: its directory, the number
// without leading zeros, the digits as spelled, and whether the name claims a
// number at all.
func sequenceClaim(p string) (dir, number, digits string, ok bool) {
	digits = sequenceDigitsRE.FindString(path.Base(p))
	if digits == "" {
		return "", "", "", false
	}
	number = strings.TrimLeft(digits, "0")
	if number == "" {
		number = "0"
	}
	dir = path.Dir(p)
	if dir == "." {
		dir = ""
	}
	return dir, number, digits, true
}

// inWatched reports whether p lies under one of the watched prefixes. A
// prefix names a directory: written without its trailing slash it still
// matches the directory, never a file that merely starts with the same
// letters. An empty list watches the whole tree.
func inWatched(p string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		prefix = strings.TrimSuffix(prefix, "/")
		if prefix == "" {
			return true
		}
		if strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// indexSequenceClaims groups the watched paths by the claim each makes.
func indexSequenceClaims(paths []string, prefixes []string) map[sequenceClaimKey][]string {
	claims := map[sequenceClaimKey][]string{}
	for _, p := range paths {
		if !inWatched(p, prefixes) {
			continue
		}
		dir, number, _, ok := sequenceClaim(p)
		if !ok {
			continue
		}
		key := sequenceClaimKey{dir: dir, number: number}
		claims[key] = append(claims[key], p)
	}
	return claims
}

// sortedClaimKeys returns keys in a stable order, so a report never depends
// on map iteration.
func sortedClaimKeys(claims map[sequenceClaimKey][]string) []sequenceClaimKey {
	keys := make([]sequenceClaimKey, 0, len(claims))
	for k := range claims {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].dir != keys[j].dir {
			return keys[i].dir < keys[j].dir
		}
		return keys[i].number < keys[j].number
	})
	return keys
}

// findSequenceCollisions reports the claims of trainPaths that more than one
// file shares and that the attempt introduces: a pair already complete in
// basePaths is the base branch's own cleanup, not a reason to refuse every
// future attempt. Claims come back unattributed; attributeClaims fills in the
// pull requests.
func findSequenceCollisions(trainPaths, basePaths, prefixes []string) []SequenceCollision {
	onBase := make(map[string]bool, len(basePaths))
	for _, p := range basePaths {
		onBase[p] = true
	}
	claims := indexSequenceClaims(trainPaths, prefixes)
	var out []SequenceCollision
	for _, key := range sortedClaimKeys(claims) {
		group := slices.Clone(claims[key])
		if len(group) < 2 {
			continue
		}
		slices.Sort(group)
		introduced := false
		var grouped []SequenceClaim
		for _, p := range group {
			grouped = append(grouped, SequenceClaim{Path: p, OnBase: onBase[p]})
			introduced = introduced || !onBase[p]
		}
		if !introduced {
			continue
		}
		_, number, digits, _ := sequenceClaim(group[0])
		out = append(out, SequenceCollision{Dir: key.dir, Number: number, Digits: digits, Claims: grouped})
	}
	return out
}

// attributeClaims records which pull request of the attempt brings each
// colliding file. A file already on the base branch names none; with one PR
// in the attempt that PR is the only candidate; with a batch, each head's
// colliding directory is read once, so the extra forge reads happen on the
// failure path alone.
func attributeClaims(collisions []SequenceCollision, prs []PullRequest, holds func(pr int, p string) (bool, error)) error {
	for i := range collisions {
		for j := range collisions[i].Claims {
			claim := &collisions[i].Claims[j]
			if claim.OnBase {
				continue
			}
			if len(prs) == 1 {
				claim.PR = prs[0].Number
				continue
			}
			for _, pr := range prs {
				held, err := holds(pr.Number, claim.Path)
				if err != nil {
					return err
				}
				if held {
					claim.PR = pr.Number
					break
				}
			}
		}
	}
	return nil
}

// checkSequences reads the watched directories at the train commit and at the
// base, and reports the sequence collisions this attempt introduces, naming
// the pull request behind each file.
func checkSequences(ctx context.Context, f forge.Forge, repo string, prefixes []string, prs []PullRequest, trainSHA, base string) ([]SequenceCollision, error) {
	trainPaths, err := f.TreePaths(ctx, repo, trainSHA, prefixes)
	if err != nil {
		return nil, fmt.Errorf("mergetrain: read train commit %s: %w", short(trainSHA), err)
	}
	basePaths, err := f.TreePaths(ctx, repo, base, prefixes)
	if err != nil {
		return nil, fmt.Errorf("mergetrain: read base commit %s: %w", short(base), err)
	}
	collisions := findSequenceCollisions(trainPaths, basePaths, prefixes)
	if len(collisions) == 0 {
		return nil, nil
	}
	heads := make(map[int]string, len(prs))
	for _, pr := range prs {
		heads[pr.Number] = pr.HeadSHA
	}
	holds := func(pr int, p string) (bool, error) {
		paths, err := f.TreePaths(ctx, repo, heads[pr], []string{path.Dir(p)})
		if err != nil {
			return false, fmt.Errorf("mergetrain: read head of PR %d: %w", pr, err)
		}
		for _, q := range paths {
			if q == p {
				return true, nil
			}
		}
		return false, nil
	}
	if err := attributeClaims(collisions, prs, holds); err != nil {
		return nil, err
	}
	return collisions, nil
}

// CollisionDetail is the body of a sequence-conflict comment: every colliding
// file, who brings it, and what to do about it.
func CollisionDetail(collisions []SequenceCollision, baseBranch string) string {
	var b strings.Builder
	for i, c := range collisions {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("Sequence number ")
		b.WriteString(c.Digits)
		b.WriteString(" is claimed twice under `")
		b.WriteString(c.Dir)
		b.WriteString("/`:\n")
		for _, cl := range c.Claims {
			b.WriteString("- `")
			b.WriteString(cl.Path)
			b.WriteString("`")
			switch {
			case cl.OnBase:
				b.WriteString(" (already on `")
				b.WriteString(baseBranch)
				b.WriteString("`)")
			case cl.PR != 0:
				b.WriteString(" (PR #")
				b.WriteString(strconv.Itoa(cl.PR))
				b.WriteString(")")
			default:
				b.WriteString(" (brought by this train commit)")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nThe train will not land a commit that puts two files with the same sequence number on the base branch: the rename that fixes it is a push, which would cost the pull request its approvals and its CI run. Give one of them the next free number and push; the train picks it up on the new head.")
	return strings.TrimRight(b.String(), "\n")
}
