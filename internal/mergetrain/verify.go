package mergetrain

// Landed-by-Content Verification
//
// A squash merge makes `git merge-base --is-ancestor` answer "no", and an
// armed auto-merge plus a late push can merge the armed SHA and silently drop
// a commit. So the PR's merged flag is not evidence that its change is on the
// base branch; the bytes are. VerifyLanded reads each expected path at a ref
// and compares it exactly.
//
// Governing: SPEC-0025 REQ-8 (check 3); #601.

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/stump-wtf/harness/internal/forge"
)

// VerifyLanded reports whether every path in want has the expected bytes on ref.
// mismatched lists the paths that did not match, sorted.
func VerifyLanded(ctx context.Context, f forge.Forge, repo, ref string, want map[string][]byte) (ok bool, mismatched []string, err error) {
	if len(want) == 0 {
		return true, nil, nil
	}
	paths := make([]string, 0, len(want))
	for p := range want {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	var errs []error
	for _, p := range paths {
		got, rerr := f.FileContentAtRef(ctx, repo, p, ref)
		if rerr != nil {
			mismatched = append(mismatched, p)
			errs = append(errs, rerr)
			continue
		}
		if !bytes.Equal(got, want[p]) {
			mismatched = append(mismatched, p)
		}
	}
	return len(mismatched) == 0, mismatched, errors.Join(errs...)
}
