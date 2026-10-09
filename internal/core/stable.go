// Stable Tables
//
// The [stable.<name>] table is the trust ledger for SPEC-0026 agent package
// stables: a git remote the operator explicitly trusts, registered only by
// `harness stable add` and cloned under the agent state root. The shape
// is deliberately skill_repo's — `remote` plus `public`, nothing else — with
// the same global-only rule.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-1 (stable
// registration and global-only trust).
//
// @joestump-agent 10/02/2026 - Added for harness#811.
package core

// Stable is one declared [stable.<name>] table: a git remote whose clone
// under $XDG_STATE_HOME/harness/agents/stables/<name>/ holds installable
// agent packages. Only `harness stable add|remove` writes it; only
// `harness stable update` fetches it (SPEC-0026 REQ-1).
type Stable struct {
	// Name is the table suffix, unique across the config, matching
	// ^[a-z][a-z0-9-]*$.
	Name string
	// Remote is the git URL `harness stable add` clones.
	Remote string
	// Public is true when unset: an unset `public` is treated as true, the
	// skill_repo convention.
	Public bool
}

// OrderedStables returns the declared stables in file order.
func (c *Config) OrderedStables() []Stable {
	if len(c.StableOrder) == 0 {
		return nil
	}
	out := make([]Stable, 0, len(c.StableOrder))
	for _, name := range c.StableOrder {
		out = append(out, c.Stables[name])
	}
	return out
}
