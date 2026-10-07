// The upgrade review's effective diff: which manifest-suppliable keys and
// requested scopes actually differ between a harness's current effective
// values and the upgrade candidate (issue #882). A diff is review-worthy
// only when it changes behavior: a manifest-supplied key whose value moves,
// a local override the new pin contradicts, or a requested mcp_allow scope
// the table does not already grant. Package metadata (version,
// description) never triggers review — it changes nothing the harness runs.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-8 (upgrade),
// REQ-4 (capability requests).
//
// @joestump-agent 10/02/2026 - Added for harness#882.
//
// @joestump-agent 10/04/2026 - Review fixes: package-supplied keys compare
// old manifest to new (a pin-relative path no longer reads as moved on
// every upgrade); a scalar at its zero is keepable rather than "added", so a
// local auto_accept = false is never overwritten; values compare with
// reflect.DeepEqual; the mcp_allow row grants the union.
//
// @joestump-agent 10/04/2026 - A local override is a row only when the new
// pin moves its key; an override the package left alone no longer blocks
// every --yes upgrade.
package agentpkg

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/stump-wtf/harness/internal/core"
)

// EffectiveChange is one reviewable difference. Kind is "value" for a
// manifest-suppliable key or "request" for an mcp_allow grant.
type EffectiveChange struct {
	// Key is the TOML key on the harness table ("model", "mcp_allow", …).
	Key string
	// Kind is "value" or "request".
	Kind string
	// Old is the current effective value (nil = unset), rendered by Render.
	Old any
	// New is the candidate's value (nil = the new pin drops it).
	New any
	// OldLocal is true when Old came from the operator's table rather than
	// the old pin — the case where "take the new value" must REMOVE the
	// local override for the choice to be honest.
	OldLocal bool
	// Added is true when Old is nil and nothing local can preserve it: the
	// new pin introduces the key, and the only choice is to accept it.
	Added bool
}

// Render formats a value for the review line: lists bracketed, unset as
// "unset".
func (c EffectiveChange) Render(v any) string {
	if v == nil {
		return "unset"
	}
	if l, ok := v.([]string); ok {
		return "[" + strings.Join(l, ", ") + "]"
	}
	return fmt.Sprintf("%v", v)
}

// EffectiveChanges diffs the harness's current effective values against the
// candidate manifest. Provenance comes from h.PackageKeys and the installed
// pin's manifest: a key PackageKeys lists was the old pin's; a key the old
// pin supplied but PackageKeys omits, or any other set key, is the
// operator's own (applySource fills only keys absent from the table). Only
// behavioral differences surface — a local override whose key the new pin
// moves to a value contradicting it (the effective value is the operator's
// either way, but the package changed underneath it), a package-supplied
// value the new pin moves or drops (the effective value would silently
// follow), and a requested mcp_allow scope the table does not grant.
func EffectiveChanges(h *core.Harness, oldMan, newMan *Manifest) []EffectiveChange {
	var out []EffectiveChange
	packageKey := func(key string) bool {
		for _, k := range h.PackageKeys {
			if k == key {
				return true
			}
		}
		return false
	}

	type field struct {
		key string
		cur any  // the current effective value; nil = unset
		set bool // cur is non-empty (non-zero for a scalar)
		old any  // the installed pin's manifest value; nil = not supplied
		new any  // the candidate's manifest value; nil = not supplied
		// zero is the unset value of a scalar key (false, 0) and nil for
		// the rest. A scalar always has an effective value, so a new pin
		// supplying one is never "added": keeping it pins the zero.
		zero any
	}
	ov, nv := oldMan.Harness, newMan.Harness
	fields := []field{
		{"harness", strOrNil(h.Adapter), h.Adapter != "", strOrNil(ov.Harness), strOrNil(nv.Harness), nil},
		{"args", strSliceOrNil(h.Args), h.Args != nil, strSliceOrNil(ov.Args), strSliceOrNil(nv.Args), nil},
		{"argv", strSliceOrNil(h.Argv), h.Argv != nil, strSliceOrNil(ov.Argv), strSliceOrNil(nv.Argv), nil},
		{"model", strOrNil(h.Model), h.Model != "", strOrNil(ov.Model), strOrNil(nv.Model), nil},
		{"auto_accept", h.AutoAccept, h.AutoAccept, boolOrNil(ov.AutoAccept), boolOrNil(nv.AutoAccept), false},
		{"max_turns", h.MaxTurns, h.MaxTurns != 0, intOrNil(ov.MaxTurns), intOrNil(nv.MaxTurns), 0},
		{"quiet", h.Quiet, h.Quiet, boolOrNil(ov.Quiet), boolOrNil(nv.Quiet), false},
		{"prompt", strOrNil(h.Prompt), h.Prompt != "", strOrNil(ov.Prompt), strOrNil(nv.Prompt), nil},
		{"prompt_file", strOrNil(h.PromptFile), h.PromptFile != "", strOrNil(ov.PromptFile), strOrNil(nv.PromptFile), nil},
		{"prompt_template", strOrNil(h.PromptTemplate), h.PromptTemplate != "", strOrNil(ov.PromptTemplate), strOrNil(nv.PromptTemplate), nil},
		{"prompt_template_file", strOrNil(h.PromptTemplateFile), h.PromptTemplateFile != "", strOrNil(ov.PromptTemplateFile), strOrNil(nv.PromptTemplateFile), nil},
		{"system_prompt_file", strOrNil(h.SystemPromptFile), h.SystemPromptFile != "", strOrNil(ov.SystemPromptFile), strOrNil(nv.SystemPromptFile), nil},
		{"mcp_config", strOrNil(h.MCPConfig), h.MCPConfig != "", strOrNil(ov.MCPConfig), strOrNil(nv.MCPConfig), nil},
		{"allowed_tools", strSliceOrNil(h.AllowedTools), h.AllowedTools != nil, strSliceOrNil(ov.AllowedTools), strSliceOrNil(nv.AllowedTools), nil},
		{"skill_paths", strSliceOrNil(h.SkillPaths), h.SkillPaths != nil, strSliceOrNil(ov.SkillPaths), strSliceOrNil(nv.SkillPaths), nil},
	}

	// The prompt sources are one choice (applySource): when the operator's
	// table sets its own, the package's prompt never applies, so nothing
	// the new pin does to its prompt keys changes what the harness runs.
	localPrompt := false
	for _, p := range []struct {
		key string
		set bool
	}{
		{"prompt", h.Prompt != ""},
		{"prompt_file", h.PromptFile != ""},
		{"prompt_template", h.PromptTemplate != ""},
		{"prompt_template_file", h.PromptTemplateFile != ""},
	} {
		if p.set && !packageKey(p.key) {
			localPrompt = true
		}
	}

	for _, f := range fields {
		if localPrompt && promptKeys[f.key] {
			continue
		}
		fromPackage := packageKey(f.key)
		local := !fromPackage && (f.set || f.old != nil)
		switch {
		case f.new == nil:
			// A key the new pin does not supply: a package-supplied value
			// being dropped moves the effective value to unset (review); a
			// local one simply stays — nothing the upgrade does touches it.
			if fromPackage && !reflect.DeepEqual(f.cur, f.zero) {
				out = append(out, EffectiveChange{Key: f.key, Kind: "value", Old: f.cur})
			}
		case fromPackage:
			// The old pin supplied it: compare the two manifests, not the
			// effective value. A manifest path resolves under its own pin
			// directory, so an unchanged "system.md" reads as a different
			// absolute path on every upgrade. The effective value silently
			// follows the pin unless the operator pins the old one — the
			// auto-accept danger the review guards.
			if !reflect.DeepEqual(f.old, f.new) {
				out = append(out, EffectiveChange{Key: f.key, Kind: "value", Old: f.cur, New: f.new})
			}
		case local:
			// A local override the package moved under: the new pin changes
			// the key from what the installed pin said (or introduces it),
			// and its value contradicts the operator's. The effective value
			// is the operator's either way, but a moved package value is
			// worth a look — taking it removes the override. An override the
			// package did not move is the operator's settled choice: no row.
			if !reflect.DeepEqual(f.old, f.new) && !reflect.DeepEqual(f.cur, f.new) {
				out = append(out, EffectiveChange{Key: f.key, Kind: "value", Old: f.cur, New: f.new, OldLocal: true})
			}
		case f.zero != nil:
			// A scalar at its zero: unset, or an explicit zero on the table
			// — the loaded config cannot tell them apart. Either way the
			// current value is the zero, so the row is keepable, and
			// keeping it pins the zero rather than letting the new value in.
			if !reflect.DeepEqual(f.cur, f.new) {
				out = append(out, EffectiveChange{Key: f.key, Kind: "value", Old: f.cur, New: f.new})
			}
		default:
			// The new pin introduces the key: the only choice is to accept
			// it — there is nothing local to preserve.
			out = append(out, EffectiveChange{Key: f.key, Kind: "value", New: f.new, Added: true})
		}
	}

	// The requested-scope row: the grant the table would carry with every
	// scope the new pin requests added to what it already grants — never
	// fewer, so taking the row cannot revoke a scope the operator granted.
	// Package metadata never lands here.
	if len(newMan.Requests.MCPAllow) > 0 {
		granted := map[string]bool{}
		for _, s := range h.MCPAllow {
			granted[strings.ToLower(s)] = true
		}
		grant := append([]string(nil), h.MCPAllow...)
		for _, s := range newMan.Requests.MCPAllow {
			if !granted[strings.ToLower(s)] {
				granted[strings.ToLower(s)] = true
				grant = append(grant, s)
			}
		}
		if len(grant) > len(h.MCPAllow) {
			out = append(out, EffectiveChange{
				Key:  "mcp_allow",
				Kind: "request",
				Old:  h.MCPAllow,
				New:  grant,
			})
		}
	}
	return out
}

// promptKeys are the four mutually exclusive prompt sources.
var promptKeys = map[string]bool{
	"prompt":               true,
	"prompt_file":          true,
	"prompt_template":      true,
	"prompt_template_file": true,
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strSliceOrNil(s []string) any {
	if s == nil {
		return nil
	}
	return s
}

func boolOrNil(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func intOrNil(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}
