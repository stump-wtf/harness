package ledger

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestSkipSyncForTestingCommitsWithoutTheDisk: behind a disk whose sync never
// returns, a synced append still commits, promptly, and reads back — and the
// sync was never asked for. Without the skip this append would wait out
// SyncTimeout and fail ErrLedgerUnavailable, the "not synced within 2s" CI
// logged under load.
func TestSkipSyncForTestingCommitsWithoutTheDisk(t *testing.T) {
	realSync := syncFileFn
	t.Cleanup(func() { syncFileFn = realSync })
	var synced atomic.Int32
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	syncFileFn = func(*os.File) error {
		synced.Add(1)
		<-stuck
		return nil
	}

	SkipSyncForTesting()
	l, err := Open(t.TempDir(), Options{SyncTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close(time.Second) })

	began := time.Now()
	if _, err := l.Append(opened("a", 1, time.Now()), true); err != nil {
		t.Fatalf("synced append: %v", err)
	}
	if took := time.Since(began); took >= 5*time.Second {
		t.Errorf("synced append took %v: it waited on the disk", took)
	}
	if _, ok, err := l.Get("a", 1); err != nil || !ok {
		t.Fatalf("Get after a committed append = %v, %v; want the record", ok, err)
	}
	if n := synced.Load(); n != 0 {
		t.Errorf("the disk was asked to sync %d times, want 0", n)
	}
}
