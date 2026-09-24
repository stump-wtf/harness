//go:build !unix

package mergetrain

import (
	"errors"
	"os"
)

var errWouldBlock = errors.New("would block")

// tryLock fails closed where flock is unavailable: without a singleton the
// train must not run (SPEC-0025 REQ-11).
func tryLock(*os.File) error {
	return errors.New("mergetrain: per-repo locking is not supported on this platform")
}
