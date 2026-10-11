// The [defaults] manifest table: supervision values a package *suggests*
// for the operator's own harness table (issue #931, ADR-0044 amendment of
// 2026-10-09). A suggestion is inert: config load never applies it, and it
// reaches a harness only when the operator chooses it at install or upgrade,
// where it is written onto the table as the operator's own key. The keys it
// may carry are exactly the supervision keys REQ-3 forbids under [harness]
// — a package suggests them here, and nowhere else. env_file, secrets_env
// and enabled can never be suggested: a credential reference (ADR-0038) and
// autostart intent are not things a package may propose at all.
//
// Each value is validated at manifest load by the same parser config load
// applies to that key on a [harness.*] table, so an invalid suggestion
// fails where the author is looking rather than the operator's next load.
//
// Governing: ADR-0044 (agent package stables, amendment of 2026-10-09),
// SPEC-0026 REQ-3 (manifest schema), REQ-6 (install choice), REQ-8 (upgrade
// never moves an operator value), Error Handling Standards.
//
// @joestump-agent 10/10/2026 - Added for harness#931.
package agentpkg

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
)

// DefaultsKeyOrder is the order REQ-3 lists the suggestible keys in. Every
// surface that offers or reports defaults walks this order, so two runs of
// the same manifest ask the same questions in the same order.
var DefaultsKeyOrder = []string{
	"schedule",
	"triggers",
	"timeout",
	"on_overlap",
	"catch_up",
	"operating_hours",
	"restart",
	"restart_delay",
	"workdir",
}

// defaultsAllowed is the set form of DefaultsKeyOrder.
var defaultsAllowed = func() map[string]bool {
	m := make(map[string]bool, len(DefaultsKeyOrder))
	for _, k := range DefaultsKeyOrder {
		m[k] = true
	}
	return m
}()

// defaultsForbidden names the keys that may never be suggested, each with
// the reason, so the author learns why rather than reading "unknown key".
var defaultsForbidden = map[string]string{
	"env_file":    "a suggested env_file would be a secret reference (ADR-0038); the operator's own env_file stays theirs",
	"secrets_env": "a suggested secrets_env would be a secret reference (ADR-0038); the operator's own secrets stay theirs",
	"enabled":     "a package must never start itself; enabling a harness is autostart intent, which is the operator's alone",
}

// Defaults holds a manifest's suggestions, keyed by harness-table key name.
// The values are validated at load; their types are the manifest types
// (string, bool, int64, []string), which is also what a table write needs.
type Defaults struct {
	Values map[string]any
}

// Ordered returns the present keys in DefaultsKeyOrder order.
func (d *Defaults) Ordered() []string {
	if d == nil {
		return nil
	}
	var out []string
	for _, k := range DefaultsKeyOrder {
		if _, ok := d.Values[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// Get returns the suggested value for key and whether it is suggested.
func (d *Defaults) Get(key string) (any, bool) {
	if d == nil {
		return nil, false
	}
	v, ok := d.Values[key]
	return v, ok
}

// decodeDefaults validates the [defaults] table into m.Defaults. Absent is
// fine. A key outside the suggestible set fails naming the set; env_file,
// secrets_env and enabled fail with their reason; every value is validated
// by the same parser config load applies to that key, naming it under
// [defaults].
func decodeDefaults(v any, path string, m *Manifest) error {
	if v == nil {
		return nil
	}
	t, ok := v.(map[string]any)
	if !ok {
		return violation(path, "[defaults] must be a table")
	}
	vals := make(map[string]any, len(t))
	for _, k := range sortedKeys(t) {
		if reason, denied := defaultsForbidden[k]; denied {
			return violation(path, "[defaults].%s can never be suggested: %s", k, reason)
		}
		if !defaultsAllowed[k] {
			return violation(path, "[defaults].%s is not a suggestible key (%s)", k, DefaultsKeyList())
		}
		if err := checkNoSecretRef(t[k], path, "[defaults]."+k); err != nil {
			return err
		}
		if err := ValidateDefault(k, t[k]); err != nil {
			return violation(path, "[defaults].%s: %v", k, err)
		}
		canonical, err := canonicalDefault(k, t[k])
		if err != nil {
			return violation(path, "[defaults].%s: %v", k, err)
		}
		vals[k] = canonical
	}
	m.Defaults = &Defaults{Values: vals}
	return nil
}

// canonicalDefault puts a validated value in the shape a harness table write
// needs. TOML decodes an array as []any and the editor cannot encode that,
// so a triggers suggestion would fail at the write instead of here.
func canonicalDefault(key string, v any) (any, error) {
	switch key {
	case "triggers":
		return defaultTriggerList(key, v)
	case "catch_up":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%q must be a boolean", key)
		}
		return b, nil
	case "restart_delay":
		n, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("%q must be an integer number of seconds", key)
		}
		return n, nil
	default:
		return defaultString(key, v)
	}
}

// ValidateDefault validates one suggested value with the same parser config
// load applies to that key on a [harness.*] table. Manifest-typed values
// (string, bool, int64, []string) go straight to the parser; a raw string
// from an interactive override is parsed into shape first (triggers as a
// comma list, catch_up as a bool, restart_delay as an integer).
func ValidateDefault(key string, value any) error {
	switch key {
	case "schedule":
		s, err := defaultString(key, value)
		if err != nil {
			return err
		}
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%q must not be blank", "schedule")
		}
		if _, err := cron.ParseStandard(s); err != nil {
			return fmt.Errorf("invalid schedule %q: %v", s, err)
		}
	case "triggers":
		refs, err := defaultTriggerList(key, value)
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			return fmt.Errorf("%q must not be empty", "triggers")
		}
		seen := map[string]bool{}
		for _, r := range refs {
			if strings.TrimSpace(r) == "" {
				return fmt.Errorf("%q entries must not be blank", "triggers")
			}
			if _, err := core.ParseTriggerRef(r); err != nil {
				return err
			}
			if seen[r] {
				return fmt.Errorf("%q names %q twice", "triggers", r)
			}
			seen[r] = true
		}
	case "timeout":
		s, err := defaultString(key, value)
		if err != nil {
			return err
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid timeout %q: %v", s, err)
		}
		if d < 0 {
			return fmt.Errorf("timeout %q must not be negative", s)
		}
	case "on_overlap":
		s, err := defaultString(key, value)
		if err != nil {
			return err
		}
		switch s {
		case "skip", "queue", "replace":
		default:
			return fmt.Errorf("invalid on_overlap %q (want \"skip\", \"queue\", or \"replace\")", s)
		}
	case "catch_up":
		if _, ok := value.(bool); !ok {
			if s, isStr := value.(string); isStr {
				if _, err := parseDefaultBool(s); err != nil {
					return err
				}
				return nil
			}
			return fmt.Errorf("%q must be a boolean", "catch_up")
		}
	case "operating_hours":
		s, err := defaultString(key, value)
		if err != nil {
			return err
		}
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%q must not be blank", "operating_hours")
		}
		if _, err := hours.Parse(s); err != nil {
			return fmt.Errorf("invalid operating_hours %q: %v", s, err)
		}
	case "restart":
		s, err := defaultString(key, value)
		if err != nil {
			return err
		}
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%q must not be blank", "restart")
		}
		if !core.RestartPolicy(s).Valid() {
			return fmt.Errorf("invalid restart %q (want \"no\", \"always\", \"unless-stopped\", or \"on-failure\")", s)
		}
	case "restart_delay":
		n, ok := value.(int64)
		if !ok {
			if s, isStr := value.(string); isStr {
				p, err := parseDefaultInt(s)
				if err != nil {
					return err
				}
				n = p
			} else {
				return fmt.Errorf("%q must be an integer number of seconds", "restart_delay")
			}
		}
		if n < 0 {
			return fmt.Errorf("%q must not be negative", "restart_delay")
		}
	case "workdir":
		s, err := defaultString(key, value)
		if err != nil {
			return err
		}
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%q must not be blank", "workdir")
		}
		// workdir is the operator's path, never a package path: the
		// package-relative path rule deliberately does not apply here.
	default:
		return fmt.Errorf("unknown default key %q", key)
	}
	return nil
}

// RenderDefault renders one suggested value the way a harness table would
// spell it, for reports and prompts.
func RenderDefault(key string, value any) string {
	switch v := value.(type) {
	case []string:
		parts := make([]string, len(v))
		copy(parts, v)
		slices.Sort(parts)
		return "[" + strings.Join(parts, ", ") + "]"
	case []any:
		parts := make([]string, 0, len(v))
		for _, it := range v {
			parts = append(parts, fmt.Sprint(it))
		}
		slices.Sort(parts)
		return "[" + strings.Join(parts, ", ") + "]"
	case bool:
		if v {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func defaultString(key string, value any) (string, error) {
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%q must be a string", key)
	}
	return s, nil
}

func defaultTriggerList(key string, value any) ([]string, error) {
	switch l := value.(type) {
	case []string:
		return l, nil
	case []any:
		out := make([]string, 0, len(l))
		for _, it := range l {
			s, ok := it.(string)
			if !ok {
				return nil, fmt.Errorf("%q must be a list of strings", key)
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		// An interactive override types the list on one line.
		var out []string
		for _, part := range strings.Split(l, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%q must be a list of strings", key)
	}
}

func parseDefaultBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%q must be a boolean", "catch_up")
	}
}

// parseDefaultInt accepts only a whole number written as one: "5m" is a
// duration, not 5, and a suggestion half-parsed into a different unit would
// be a silent lie about how long the operator waits.
func parseDefaultInt(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q must be an integer number of seconds", "restart_delay")
	}
	return n, nil
}

// TriggerRefs returns the references a suggested triggers value names, nil
// when the value is not a list of them. Whether each source exists is the
// caller's question (REQ-6), not this one's.
func TriggerRefs(value any) []string {
	refs, err := defaultTriggerList("triggers", value)
	if err != nil {
		return nil
	}
	return refs
}

// NormalizeDefault parses a typed line — an interactive override (REQ-6) —
// into the manifest shape of key, so a written key reads identically whether
// the author or the operator wrote it, and validates it with the same
// parser REQ-3 applies.
func NormalizeDefault(key, raw string) (any, error) {
	var v any
	switch key {
	case "catch_up":
		b, err := parseDefaultBool(raw)
		if err != nil {
			return nil, err
		}
		v = b
	case "restart_delay":
		n, err := parseDefaultInt(raw)
		if err != nil {
			return nil, err
		}
		v = n
	case "triggers":
		refs, err := defaultTriggerList(key, raw)
		if err != nil {
			return nil, err
		}
		v = refs
	default:
		v = strings.TrimSpace(raw)
	}
	if err := ValidateDefault(key, v); err != nil {
		return nil, err
	}
	return v, nil
}

// RenderDefaults describes the suggestions for the confirmation report and
// `harness agent info` (REQ-3): each line says the package suggests the
// value, never that the harness runs with it.
func RenderDefaults(man *Manifest) []string {
	var d *Defaults
	if man != nil {
		d = man.Defaults
	}
	keys := d.Ordered()
	if len(keys) == 0 {
		return []string{"suggests no supervision defaults"}
	}
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		v, _ := man.Defaults.Get(k)
		lines = append(lines, fmt.Sprintf("suggests %s = %s (a suggestion you choose at install or upgrade)", k, RenderDefault(k, v)))
	}
	return lines
}

// DiffDefaults compares two suggestion sets: added, changed and removed
// keys, walked in DefaultsKeyOrder order so a report is deterministic
// (REQ-8).
type DefaultsDiff struct {
	Added   []string
	Changed []string
	Removed []string
}

func DiffDefaults(oldDefaults, newDefaults *Defaults) DefaultsDiff {
	var d DefaultsDiff
	for _, k := range DefaultsKeyOrder {
		ov, had := oldDefaults.Get(k)
		nv, has := newDefaults.Get(k)
		switch {
		case had && !has:
			d.Removed = append(d.Removed, k)
		case !had && has:
			d.Added = append(d.Added, k)
		case had && has && !sameDefault(ov, nv):
			d.Changed = append(d.Changed, k)
		}
	}
	return d
}

// DiffLines renders the REQ-8 report: every suggestion the new pin adds,
// changes or drops, each with its old and new value. A dropped suggestion
// says plainly that the operator's table keeps whatever it holds — upgrade
// never moves a written key.
func DiffLines(oldDefaults, newDefaults *Defaults) []string {
	var lines []string
	for _, k := range DefaultsKeyOrder {
		ov, had := oldDefaults.Get(k)
		nv, has := newDefaults.Get(k)
		switch {
		case had && !has:
			lines = append(lines, fmt.Sprintf("dropped suggestion %s: %s (your table keeps what it holds)", k, RenderDefault(k, ov)))
		case !had && has:
			lines = append(lines, fmt.Sprintf("new suggestion %s: %s", k, RenderDefault(k, nv)))
		case had && has && !sameDefault(ov, nv):
			lines = append(lines, fmt.Sprintf("changed suggestion %s: %s -> %s", k, RenderDefault(k, ov), RenderDefault(k, nv)))
		}
	}
	return lines
}

func sameDefault(a, b any) bool {
	return RenderDefault("", a) == RenderDefault("", b)
}

// DefaultsKeyList is the human-readable suggestible-key list for errors.
func DefaultsKeyList() string { return strings.Join(slices.Clone(DefaultsKeyOrder), ", ") }
