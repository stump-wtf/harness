package ledger

import (
	"os"
	"testing"
)

// SkipSyncForTesting makes every Ledger in this test binary write its lines
// without fdatasync. The ordering does not change: a line still commits, and
// becomes visible to Records, Get and Query, only once the writer has written
// it and its (now instant) sync has returned.
//
// No test can observe what fdatasync adds: that is only visible after a power
// loss, and a killed process leaves the page cache behind. What tests did
// observe was the runner's disk. Every run lifecycle the supervisor, daemon
// and cmd/harness tests wait on is read back from the ledger, and so waited on
// an fsync; on a shared CI runner that disk is contended, and run 13777 logged
// syncs past the 2s SyncTimeout and shutdown drains still queued 5s later,
// failing tests on budgets the code under test never used. This package's own
// ordering, query and import tests failed the same way, with a synced Append
// reporting ErrLedgerUnavailable. Its sync handling (a failing, a stalled
// sync) is tested by installing a syncFileFn that fails or stalls, which this
// does not prevent.
//
// Call it from TestMain, before any Ledger opens. It panics outside a test
// binary, so it cannot turn a daemon's durability off.
func SkipSyncForTesting() {
	if !testing.Testing() {
		panic("ledger: SkipSyncForTesting called outside a test binary")
	}
	syncFileFn = func(*os.File) error { return nil }
}
