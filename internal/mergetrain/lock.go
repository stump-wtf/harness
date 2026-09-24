package mergetrain

// Per-Repo Singleton
//
// A driver holds an exclusive, non-blocking lock on a per-repo file for its
// whole lifetime, so a second driver for the same repo — in this daemon or in
// another on the same host — is refused, never raced. The lock dies with the
// process, so a crash leaves nothing stale. Cross-host exclusion is
// operational: the train is enabled in one daemon's config only.
//
// Governing: ADR-0032 option D1, SPEC-0025 REQ-11.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrLocked means another merge train already holds the repo.
var ErrLocked = errors.New("mergetrain: another merge train holds this repo")

// Lock is a held per-repo lock.
type Lock struct {
	f    *os.File
	path string
}

// LockPath is the lock file for repo under dir: "<owner>_<name>.lock".
func LockPath(dir, repo string) string {
	return filepath.Join(dir, strings.ReplaceAll(repo, "/", "_")+".lock")
}

// AcquireLock takes repo's lock under dir without waiting. It returns an
// error wrapping ErrLocked when another holder has it.
func AcquireLock(dir, repo string) (*Lock, error) {
	if repo == "" || strings.Count(repo, "/") != 1 {
		return nil, fmt.Errorf("mergetrain: repo %q is not owner/name", repo)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mergetrain: lock dir: %w", err)
	}
	path := LockPath(dir, repo)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mergetrain: open lock: %w", err)
	}
	if err := tryLock(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, fmt.Errorf("%w: %s (%s)", ErrLocked, repo, path)
		}
		return nil, fmt.Errorf("mergetrain: lock %s: %w", path, err)
	}
	return &Lock{f: f, path: path}, nil
}

// Release drops the lock. It is safe to call more than once.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close() // closing the descriptor releases the flock
	l.f = nil
	return err
}
