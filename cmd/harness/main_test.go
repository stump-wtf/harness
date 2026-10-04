package main

import (
	"os"
	"testing"

	"github.com/stump-wtf/harness/internal/ledger"
)

// TestMain turns off the run ledger's fdatasync for this binary: every run
// these tests wait on is read back from the ledger, and none of them tests
// durability. See ledger.SkipSyncForTesting.
//
// Run with standInAgentEnv set, the binary is a stand-in agent instead
// (daemon_quota_test.go): it writes a model outcome to its session store and
// exits before any test runs.
func TestMain(m *testing.M) {
	if plan := os.Getenv(standInAgentEnv); plan != "" {
		os.Exit(runStandInAgent(plan))
	}
	ledger.SkipSyncForTesting()
	os.Exit(m.Run())
}
