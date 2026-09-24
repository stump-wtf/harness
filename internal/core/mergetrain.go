package core

// Merge Train Configuration
//
// The parsed [mergetrain] table (SPEC-0025 REQ-1). A capability that merges
// code ships off: Enabled defaults to false, and even when enabled the mode
// defaults to "report", which builds and tests trains but writes nothing to
// any pull request. The forge token is never here — ForgeTokenEnv names the
// environment variable that holds it (ADR-0008).
//
// Governing: ADR-0032, SPEC-0025 design.md "Configuration".

import "time"

// Merge train defaults.
const (
	DefaultMergeTrainMode         = "report"
	DefaultMergeTrainBaseBranch   = "main"
	DefaultMergeTrainPollInterval = 60 * time.Second
	DefaultMergeTrainCITimeout    = 30 * time.Minute
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
	}
}
