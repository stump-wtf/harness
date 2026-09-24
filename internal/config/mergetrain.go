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
// Governing: ADR-0032; SPEC-0025 REQ-1, REQ-12.

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
	Enabled       bool     `toml:"enabled"`
	Mode          *string  `toml:"mode"`
	Repos         []string `toml:"repos"`
	BaseBranch    *string  `toml:"base_branch"`
	PollInterval  *string  `toml:"poll_interval"`
	CITimeout     *string  `toml:"ci_timeout"`
	ForgeBaseURL  string   `toml:"forge_base_url"`
	ForgeTokenEnv string   `toml:"forge_token_env"`

	// Token is decoded only so its presence can be refused.
	Token any `toml:"token"`
}

var (
	envNameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

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

	for _, r := range rm.Repos {
		r = strings.TrimSpace(r)
		if !repoNameRe.MatchString(r) {
			return fail("repos", "each entry must be \"owner/name\" (got %q)", r)
		}
		if slices.Contains(mc.Repos, r) {
			return fail("repos", "lists %q twice", r)
		}
		mc.Repos = append(mc.Repos, r)
	}

	durations := []struct {
		key string
		raw *string
		dst *time.Duration
		min time.Duration
	}{
		{"poll_interval", rm.PollInterval, &mc.PollInterval, core.MinMergeTrainPollInterval},
		{"ci_timeout", rm.CITimeout, &mc.CITimeout, time.Minute},
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

	if mc.Enabled {
		switch {
		case len(mc.Repos) == 0:
			return fail("repos", "must list at least one \"owner/name\" when enabled = true")
		case mc.ForgeBaseURL == "":
			return fail("forge_base_url", "is required when enabled = true")
		case mc.ForgeTokenEnv == "":
			return fail("forge_token_env", "is required when enabled = true")
		}
	}
	return mc, nil
}
