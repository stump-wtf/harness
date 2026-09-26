package main

import (
	"os"
	"testing"

	"github.com/stump-wtf/harness/internal/ledger"
)

// TestMain turns off the run ledger's fdatasync for this binary: every run
// these tests wait on is read back from the ledger, and none of them tests
// durability. See ledger.SkipSyncForTesting.
func TestMain(m *testing.M) {
	ledger.SkipSyncForTesting()
	os.Exit(m.Run())
}
