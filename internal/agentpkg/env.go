// The [[env]] manifest table: a package declares the environment variables
// its harness expects, by NAME only, so `harness doctor` and `describe` can
// tell an operator a variable is missing before the first run quietly does
// nothing (issue #930). A declaration never carries a value — not a literal,
// not a default, and not a ${NAME} reference (ADR-0038): the value lives in
// the installing harness's own env_file or the daemon's environment, which a
// package can never name.
//
// Governing: ADR-0044 (agent package stables), ADR-0038 (secret references),
// SPEC-0026 REQ-3 (manifest schema), REQ-12 (CLI visibility and doctor).
//
// @joestump-agent 10/09/2026 - Added for harness#930.
package agentpkg

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// EnvNamePattern is the grammar an [[env]] name must match: a POSIX-style
// upper-case environment variable name.
var EnvNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// EnvVar is one [[env]] declaration. It names a variable and says how the
// package uses it; it never carries the variable's value.
type EnvVar struct {
	// Name is the variable's name, matching EnvNamePattern.
	Name string
	// Required marks a variable the harness cannot work without: doctor
	// fails the row when it is unset. An optional one only warns.
	Required bool
	// Secret marks a credential. Display surfaces say so; nothing ever
	// prints the value either way.
	Secret bool
	// Description says what the variable is for, one line.
	Description string
}

// envAllowlist is the only set of keys an [[env]] entry accepts.
var envAllowlist = map[string]bool{
	"name":        true,
	"required":    true,
	"secret":      true,
	"description": true,
}

// envValueKeys are keys that would carry a value. They get their own error
// so the author learns why rather than reading "unknown key".
var envValueKeys = map[string]bool{
	"value":   true,
	"default": true,
	"example": true,
}

// decodeEnv validates the [[env]] array of tables into m.Env. Absent is
// fine; anything else that is not a list of tables, an entry with a key
// outside the allowlist, a value-like key, a bad or duplicate name, a
// non-boolean flag, or a "${" anywhere fails the load naming the entry.
func decodeEnv(v any, path string, m *Manifest) error {
	if v == nil {
		return nil
	}
	// [[env]] blocks decode as []map[string]any; the inline form
	// env = [{ name = "X" }] decodes as []any of tables, and is the same
	// declaration written differently.
	var entries []map[string]any
	switch l := v.(type) {
	case []map[string]any:
		entries = l
	case []any:
		for _, it := range l {
			t, ok := it.(map[string]any)
			if !ok {
				return violation(path, "[[env]] must be an array of tables (one [[env]] block per variable)")
			}
			entries = append(entries, t)
		}
	default:
		return violation(path, "[[env]] must be an array of tables (one [[env]] block per variable)")
	}
	seen := make(map[string]bool, len(entries))
	for i, t := range entries {
		where := fmt.Sprintf("[[env]] #%d", i+1)
		if n, ok := t["name"].(string); ok && n != "" {
			where = fmt.Sprintf("[[env]] %q", n)
		}
		for _, k := range sortedKeys(t) {
			if envValueKeys[k] {
				return violation(path, "%s: %q is not permitted — a package declares variable names, never values; the value belongs in the installing harness's env_file", where, k)
			}
			if !envAllowlist[k] {
				return violation(path, "%s: %q is not an allowed key (name, required, secret, description)", where, k)
			}
			if err := checkNoSecretRef(t[k], path, where+"."+k); err != nil {
				return err
			}
		}
		raw, present := t["name"]
		if !present {
			return violation(path, "%s: name is required", where)
		}
		name, ok := raw.(string)
		if !ok {
			return violation(path, "%s: name must be a string", where)
		}
		if !EnvNamePattern.MatchString(name) {
			return violation(path, "%s: name must match %s", where, EnvNamePattern)
		}
		if seen[name] {
			return violation(path, "%s is declared more than once", where)
		}
		seen[name] = true
		ev := EnvVar{Name: name}
		for _, f := range []struct {
			key string
			dst *bool
		}{{"required", &ev.Required}, {"secret", &ev.Secret}} {
			if _, present := t[f.key]; !present {
				continue
			}
			b, ok := t[f.key].(bool)
			if !ok {
				return violation(path, "%s: %s must be a boolean", where, f.key)
			}
			*f.dst = b
		}
		if raw, present := t["description"]; present {
			d, ok := raw.(string)
			if !ok {
				return violation(path, "%s: description must be a string", where)
			}
			// The description is printed to the operator's terminal and
			// into the env_file skeleton's comment line, so it must be one
			// line with no control characters: a newline would break the
			// skeleton out of its comment, and an escape sequence would
			// reach the terminal.
			if strings.IndexFunc(d, unicode.IsControl) >= 0 {
				return violation(path, "%s: description must be a single line without control characters", where)
			}
			ev.Description = d
		}
		m.Env = append(m.Env, ev)
	}
	return nil
}

// envFlags renders a declaration's flags for display: "required" or
// "optional", plus ", secret" for a credential.
func envFlags(ev EnvVar) string {
	s := "optional"
	if ev.Required {
		s = "required"
	}
	if ev.Secret {
		s += ", secret"
	}
	return s
}

// RenderEnv itemizes the [[env]] declarations for the install and upgrade
// confirmation and for `agent info`: one line per variable, name and flags
// and description, never a value. A package that declares none says so.
func RenderEnv(man *Manifest) []string {
	if man == nil || len(man.Env) == 0 {
		return []string{"declares no environment variables"}
	}
	lines := make([]string, 0, len(man.Env))
	for _, ev := range man.Env {
		line := fmt.Sprintf("%s (%s)", ev.Name, envFlags(ev))
		if ev.Description != "" {
			line += ": " + ev.Description
		}
		lines = append(lines, line)
	}
	return lines
}

// EnvFileSkeleton is the ready-to-copy env_file text install prints last:
// each variable as a description comment and a bare NAME= line, with an
// optional variable's line commented out so pasting the skeleton never sets
// it to an empty value over one the daemon's environment already provides.
// Names only — there is no value here to print. Nil when nothing is
// declared.
func EnvFileSkeleton(man *Manifest) []string {
	if man == nil || len(man.Env) == 0 {
		return nil
	}
	var lines []string
	for _, ev := range man.Env {
		comment := "# " + envFlags(ev)
		if ev.Description != "" {
			comment += ": " + ev.Description
		}
		lines = append(lines, comment)
		if ev.Required {
			lines = append(lines, ev.Name+"=")
		} else {
			lines = append(lines, "# "+ev.Name+"=")
		}
	}
	return lines
}
