package mergetrain

// Lock Tests
//
// Governing tests: SPEC-0025 REQ-11 — a second holder is refused without
// waiting, Release frees the lock, and different repos do not contend.

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestLockSingleton(t *testing.T) {
	dir := t.TempDir()
	a, err := AcquireLock(dir, "stump.wtf/harness")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := AcquireLock(dir, "stump.wtf/harness"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second AcquireLock = %v, want ErrLocked", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("the second holder waited %v instead of failing fast", waited)
	}
	other, err := AcquireLock(dir, "stump.wtf/switchboard")
	if err != nil {
		t.Fatalf("a different repo contended: %v", err)
	}
	_ = other.Release()
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(); err != nil {
		t.Fatalf("second Release = %v", err)
	}
	b, err := AcquireLock(dir, "stump.wtf/harness")
	if err != nil {
		t.Fatalf("lock not freed by Release: %v", err)
	}
	_ = b.Release()
	if fi, err := os.Stat(LockPath(dir, "stump.wtf/harness")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file = %v, %v; want mode 0600", fi, err)
	}
}
