package core

// Merge Train Configuration
//
// The parsed [mergetrain] table (SPEC-0025 REQ-1). A capability that merges
// code ships off: Enabled defaults to false, and even when enabled the mode
// defaults to "report", which builds and tests trains but writes nothing to
// any pull request. The forge token is never here — ForgeTokenEnv names the
// environment variable that holds it (ADR-0008).
//
// Batch is SPEC-0025 REQ-17's PRs-per-train size: above 1 a tick folds up to
// that many eligible heads onto one train commit and bisects a red batch by
// halves.
//
// Governing: ADR-0032, SPEC-0025 design.md "Configuration", REQ-17.
//
// @joestump-agent 09/28/2026 - Added Batch: the strict loader refused the
// documented `batch` key, so a [mergetrain] table copied from the spec failed
// the whole config load.

import "time"

// Merge train defaults.
const (
	DefaultMergeTrainMode         = "report"
	DefaultMergeTrainBaseBranch   = "main"
	DefaultMergeTrainPollInterval = 60 * time.Second
	DefaultMergeTrainCITimeout    = 30 * time.Minute
	// DefaultMergeTrainBatch is one PR per train commit: the behaviour
	// REQ-1 through REQ-16 describe, unchanged by REQ-17.
	DefaultMergeTrainBatch = 1
	// MinMergeTrainPollInterval is the floor on poll_interval; the train's CI
	// polling never goes below 5 s either (SPEC-0025 REQ-5).
	MinMergeTrainPollInterval = 5 * time.Second
)

// MergeTrainConfig is the global [mergetrain] table.
type MergeTrainConfig struct {
	Enabled      bool
	Mode         string   // "report" or "merge"
	Repos        []string // "owner/name", each driven by its own driver
	BaseBranch   string
	PollInterval time.Duration
	CITimeout    time.Duration
	// Batch is the PRs-per-train size (SPEC-0025 REQ-17), at least 1. Above
	// 1 a tick batches that many heads onto one train commit.
	Batch int
	// ForgeBaseURL is the forge's root, e.g. "https://gitea.stump.rocks".
	ForgeBaseURL string
	// ForgeTokenEnv is the NAME of the environment variable holding the
	// forge token, never the token.
	ForgeTokenEnv string
}

// DefaultMergeTrainConfig is the table's value when harness.toml omits it:
// disabled, report mode, the documented intervals.
func DefaultMergeTrainConfig() MergeTrainConfig {
	return MergeTrainConfig{
		Mode:         DefaultMergeTrainMode,
		BaseBranch:   DefaultMergeTrainBaseBranch,
		PollInterval: DefaultMergeTrainPollInterval,
		CITimeout:    DefaultMergeTrainCITimeout,
		Batch:        DefaultMergeTrainBatch,
	}
}
