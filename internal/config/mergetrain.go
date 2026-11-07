package config

// Merge Train Table
//
// Parses and validates the global [mergetrain] table (SPEC-0025 REQ-1) into
// core.MergeTrainConfig. Everything static is checked at load with the
// offending key's line. The token is resolved later, by the daemon, from the
// environment variable forge_token_env names — harness.toml carries no
// credential (ADR-0008), so a `token` key is refused outright rather than
// ignored. The table is global-only: a project file or harness_d drop-in
// must not be able to turn on a component that merges code.
//
// `batch` (SPEC-0025 REQ-17) is decoded and range-checked: a whole number of
// at least 1, where a value above 1 batches that many heads onto one train
// commit.
//
// `flake_cool_down` (SPEC-0025 REQ-18) is how long a red attempt waits for its
// one retry when every failed job reported no runner. It is the one duration
// here that accepts 0, because 0 is the honest way to turn the retry off — and
// it is the default until ADR-0056 is accepted, so the retry is opt-in.
//
// Governing: ADR-0032, ADR-0056; SPEC-0025 REQ-1, REQ-12, REQ-17, REQ-18.
//
// @joestump-agent 09/28/2026 - Accept `batch`. It was absent from
// rawMergeTrain, so the strict decode refused it as an unknown key and the
// documented config failed to load at all.

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// rawMergeTrain mirrors [mergetrain] before validation. Tuning keys are
// pointers so an absent key takes its default and an explicit zero is refused.
type rawMergeTrain struct {
	Enabled      bool     `toml:"enabled"`
	Mode         *string  `toml:"mode"`
	Repos        []string `toml:"repos"`
	BaseBranch   *string  `toml:"base_branch"`
	PollInterval *string  `toml:"poll_interval"`
	CITimeout    *string  `toml:"ci_timeout"`
	Batch        *int64   `toml:"batch"`
	// SequencePaths replaces the watched directories of the sequence check
	// (ADR-0063). A pointer, like the other tuning keys: an absent key keeps
	// the defaults, an explicit empty list disables the check (REQ-19).
	SequencePaths *[]string `toml:"sequence_paths"`
	FlakeCoolDown *string   `toml:"flake_cool_down"`
	ForgeBaseURL  string    `toml:"forge_base_url"`
	ForgeTokenEnv string    `toml:"forge_token_env"`

	// GitHub is the train's second forge, decoded flat rather than as a table
	// so a repo's forge stays one prefix on its repos entry (ADR-0061).
	GitHubBaseURL  string `toml:"github_base_url"`
	GitHubTokenEnv string `toml:"github_token_env"`
	GitHubCIGate   string `toml:"github_ci_gate"`

	// Token is decoded only so its presence can be refused.
	Token any `toml:"token"`
}

var (
	envNameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	// sequencePathRe is a repo-relative directory: no leading slash, no
	// backslash, nothing empty.
	sequencePathRe = regexp.MustCompile(`^[^/.\\][^\\]*$`)
)

// hasDotSegment reports whether p contains a "." or ".." path segment.
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || seg == "" {
			return true
		}
	}
	return false
}

// buildMergeTrain validates rm into a core.MergeTrainConfig.
func buildMergeTrain(filename string, data []byte, line int, rm rawMergeTrain) (core.MergeTrainConfig, error) {
	keyLine := func(key string) int {
		if l := lineOfKeyInTable(data, "mergetrain", key); l > 0 {
			return l
		}
		return line
	}
	fail := func(key, format string, args ...any) (core.MergeTrainConfig, error) {
		return core.MergeTrainConfig{}, newError(filename, keyLine(key), "[mergetrain] %q: "+format, append([]any{key}, args...)...)
	}

	if rm.Token != nil {
		return fail("token", "is not allowed in harness.toml — a forge token is a credential (ADR-0008); put it in the daemon's environment and name the variable with forge_token_env")
	}

	mc := core.DefaultMergeTrainConfig()
	mc.Enabled = rm.Enabled
	var hasGitHubRepo, hasDefaultRepo bool

	if rm.Mode != nil {
		m := strings.TrimSpace(*rm.Mode)
		if m != "report" && m != "merge" {
			return fail("mode", "must be \"report\" or \"merge\" (got %q)", *rm.Mode)
		}
		mc.Mode = m
	}
	if rm.BaseBranch != nil {
		b := strings.TrimSpace(*rm.BaseBranch)
		if b == "" || strings.HasPrefix(b, "train/") {
			return fail("base_branch", "must name a branch other than a train/ branch (got %q)", *rm.BaseBranch)
		}
		mc.BaseBranch = b
	}

	if rm.Batch != nil {
		if *rm.Batch < 1 {
			return fail("batch", "must be a whole number of at least 1 (got %d)", *rm.Batch)
		}
		mc.Batch = int(*rm.Batch)
	}

	for _, r := range rm.Repos {
		r = strings.TrimSpace(r)
		forgeName := ""
		if kind, rest, ok := strings.Cut(r, ":"); ok {
			if kind != "github" {
				return fail("repos", "names forge %q, which [mergetrain] has no configuration for; the only forge a repos entry can name is \"github:\" (got %q)", kind, r)
			}
			forgeName, r = kind, strings.TrimSpace(rest)
		}
		if !repoNameRe.MatchString(r) {
			return fail("repos", "each entry must be \"owner/name\", or \"github:owner/name\" to use the GitHub forge (got %q)", r)
		}
		if slices.Contains(mc.Repos, r) {
			return fail("repos", "lists %q twice", r)
		}
		mc.Repos = append(mc.Repos, r)
		if mc.RepoForge == nil {
			mc.RepoForge = map[string]string{}
		}
		mc.RepoForge[r] = forgeName
		switch forgeName {
		case "github":
			hasGitHubRepo = true
		default:
			hasDefaultRepo = true
		}
	}

	if rm.SequencePaths != nil {
		var paths []string
		for _, p := range *rm.SequencePaths {
			p = strings.TrimSuffix(strings.TrimSpace(p), "/")
			if p == "" || !sequencePathRe.MatchString(p) || hasDotSegment(p) {
				return fail("sequence_paths", "must be a repo-relative directory such as \"docs/adrs\", with no leading slash and no \".\" or \"..\" segment (got %q)", p)
			}
			if slices.Contains(paths, p+"/") {
				return fail("sequence_paths", "lists %q twice", p)
			}
			paths = append(paths, p+"/")
		}
		mc.SequencePaths = paths
	}

	durations := []struct {
		key string
		raw *string
		dst *time.Duration
		min time.Duration
	}{
		{"poll_interval", rm.PollInterval, &mc.PollInterval, core.MinMergeTrainPollInterval},
		{"ci_timeout", rm.CITimeout, &mc.CITimeout, time.Minute},
		// flake_cool_down is the one duration allowed to be zero: 0 turns the
		// infra retry off rather than meaning "immediately" (SPEC-0025 REQ-18).
		{"flake_cool_down", rm.FlakeCoolDown, &mc.FlakeCoolDown, 0},
	}
	for _, d := range durations {
		if d.raw == nil {
			continue
		}
		v, err := time.ParseDuration(strings.TrimSpace(*d.raw))
		if err != nil || v < d.min {
			return fail(d.key, "must be a duration of at least %s (got %q)", d.min, *d.raw)
		}
		*d.dst = v
	}

	if ep := strings.TrimSpace(rm.ForgeBaseURL); ep != "" {
		u, err := url.Parse(ep)
		switch {
		case err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			return fail("forge_base_url", "must be an absolute http or https URL with a host (got %q)", redactURL(ep))
		case u.User != nil:
			return fail("forge_base_url", "must not carry userinfo — a credential in the URL is a credential in harness.toml (ADR-0008)")
		}
		mc.ForgeBaseURL = strings.TrimRight(ep, "/")
	}
	if name := strings.TrimSpace(rm.ForgeTokenEnv); name != "" {
		if !envNameRe.MatchString(name) {
			return fail("forge_token_env", "must be the NAME of an environment variable, such as \"HARNESS_MERGETRAIN_TOKEN\" — never the token itself")
		}
		mc.ForgeTokenEnv = name
	}

	gh := &core.MergeTrainGitHub{BaseURL: strings.TrimRight(strings.TrimSpace(rm.GitHubBaseURL), "/"), CIGate: core.GitHubGateDraftPR}
	if gh.BaseURL != "" {
		u, err := url.Parse(gh.BaseURL)
		switch {
		case err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			return fail("github_base_url", "must be an absolute http or https URL with a host (got %q)", redactURL(gh.BaseURL))
		case u.User != nil:
			return fail("github_base_url", "must not carry userinfo — a credential in the URL is a credential in harness.toml (ADR-0008)")
		}
	}
	if gate := strings.TrimSpace(rm.GitHubCIGate); gate != "" {
		if gate != core.GitHubGateDraftPR && gate != core.GitHubGateTrainPush {
			return fail("github_ci_gate", "must be %q or %q (got %q)", core.GitHubGateDraftPR, core.GitHubGateTrainPush, gate)
		}
		gh.CIGate = gate
	}
	if name := strings.TrimSpace(rm.GitHubTokenEnv); name != "" {
		if !envNameRe.MatchString(name) {
			return fail("github_token_env", "must be the NAME of an environment variable, such as \"HARNESS_MERGETRAIN_GITHUB_TOKEN\" — never the token itself")
		}
		gh.TokenEnv = name
	}
	if !hasGitHubRepo && gh.BaseURL == "" && gh.TokenEnv == "" && strings.TrimSpace(rm.GitHubCIGate) == "" {
		gh = nil // nothing names the GitHub forge, so the train has only one
	}
	mc.GitHub = gh

	if mc.Enabled {
		switch {
		case len(mc.Repos) == 0:
			return fail("repos", "must list at least one \"owner/name\" when enabled = true")
		case hasDefaultRepo && mc.ForgeBaseURL == "":
			return fail("forge_base_url", "is required when enabled = true")
		case hasDefaultRepo && mc.ForgeTokenEnv == "":
			return fail("forge_token_env", "is required when enabled = true")
		case hasGitHubRepo && (gh == nil || gh.TokenEnv == ""):
			return fail("github_token_env", "is required when a repos entry names the GitHub forge")
		}
	}
	return mc, nil
}
