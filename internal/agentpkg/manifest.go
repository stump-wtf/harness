// Manifest is the decoded, validated package.toml (SPEC-0026 REQ-3). The
// loader is strict by construction: any table beyond [package], [harness],
// [requests] and [[env]] (env.go), any harness key outside the value-key allowlist, any forbidden
// key, and any string containing "${" (the ADR-0038 secret-reference grammar)
// fails the load naming the key and the manifest's path.
//
// Governing: ADR-0044, SPEC-0026 REQ-3, Error Handling Standards.
package agentpkg

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// PackageMeta is the [package] table: identity plus optional provenance
// strings.
type PackageMeta struct {
	Name        string
	Version     string
	Description string
	Author      string
	Homepage    string
	// License is the SPDX license expression as written, validated against
	// the vendored SPDX License List (license.go). Empty means the package
	// declares none, which is a low finding, package.no-license.
	License string
}

// HarnessValues is the per-harness value subset a manifest may carry. The
// fields are exactly the keys today's harness schema parses (rawHarness in
// internal/config); model_pin, skill_paths, use_default_skill_paths,
// mcp_bridge, mcp_exclusive and mcp_policy join this struct in the PRs that
// add them to the schema itself — until then they fail as unknown keys, which
// is the honest error.
type HarnessValues struct {
	Harness    string
	Args       []string
	Argv       []string
	Model      string
	AutoAccept *bool
	MaxTurns   *int
	Quiet      *bool
	// Prompt, PromptFile, PromptTemplate and PromptTemplateFile are the
	// one-shot prompt sources (SPEC-0006 REQ "Prompt Source", SPEC-0017
	// REQ-5). A manifest carries at most one; the two file forms resolve
	// against the pin directory like system_prompt_file.
	Prompt             string
	PromptFile         string
	PromptTemplate     string
	PromptTemplateFile string
	SystemPromptFile   string
	MCPConfig          string
	AllowedTools       []string
	// SkillPaths is the package's additional skill roots (SPEC-0006 REQ
	// "Skill Path Configuration"), resolved against the pin directory at
	// config load. Values here point INTO the bundle — the merge tier they
	// contribute at is #816's.
	SkillPaths []string
}

// Requests is the itemized [requests] table (REQ-4 renders every entry).
type Requests struct {
	SkillPaths *bool
	MCPAllow   []string
	Network    *bool
}

// Manifest is a fully validated package.toml.
type Manifest struct {
	Package  PackageMeta
	Harness  HarnessValues
	Requests Requests
	// Env is the [[env]] declarations, in manifest order: names the harness
	// expects in its environment, never their values (env.go).
	Env []EnvVar
}

// harnessAllowlist is the only set of keys [harness] accepts, matched against
// the decoded keys in sorted order so errors are deterministic.
var harnessAllowlist = map[string]bool{
	"harness":              true,
	"args":                 true,
	"argv":                 true,
	"model":                true,
	"auto_accept":          true,
	"max_turns":            true,
	"quiet":                true,
	"prompt":               true,
	"prompt_file":          true,
	"prompt_template":      true,
	"prompt_template_file": true,
	"system_prompt_file":   true,
	"mcp_config":           true,
	"allowed_tools":        true,
	"skill_paths":          true,
}

// harnessForbidden is the explicit denylist SPEC-0026 REQ-3 names, so its
// error can say what the key is rather than a generic "unknown". Every deny
// reason is one line of guidance.
var harnessForbidden = map[string]string{
	"env_file":        "a package must not reference secrets; use the installing harness's own env_file",
	"secrets_env":     "a package must not reference secrets; use the installing harness's own secrets",
	"workdir":         "a package cannot choose a workdir; set it on the harness table after install",
	"enabled":         "a package cannot choose autostart; set enabled on the harness table",
	"restart":         "restart policy is supervisor state, not package content",
	"restart_delay":   "restart policy is supervisor state, not package content",
	"operating_hours": "operating hours belong to the machine, not the package",
	"schedule":        "a package cannot schedule itself; set schedule on the harness table",
	"triggers":        "a package cannot bind itself to event sources; set triggers on the harness table",
}

var requestsAllowlist = map[string]bool{
	"skill_paths": true,
	"mcp_allow":   true,
	"network":     true,
}

// LoadManifest reads and validates package.toml at path. Every failure wraps
// ErrManifestViolation (schema violations) and names the offending key plus
// the path.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agentpkg: read manifest: %w", err)
	}
	return ParseManifest(raw, path)
}

// ParseManifest validates manifest bytes. path is used only for error
// messages.
func ParseManifest(raw []byte, path string) (*Manifest, error) {
	var doc map[string]any
	md, err := toml.Decode(string(raw), &doc)
	if err != nil {
		return nil, fmt.Errorf("agentpkg: %s: %w", path, err)
	}
	_ = md

	for _, key := range sortedKeys(doc) {
		switch key {
		case "package", "harness", "requests", "env":
		default:
			return nil, violation(path, "table [%s] is not allowed in a package manifest (only [package], [harness], [requests], [[env]])", key)
		}
	}

	m := &Manifest{}
	if err := decodePackage(doc["package"], path, m); err != nil {
		return nil, err
	}
	if err := decodeHarness(doc["harness"], path, m); err != nil {
		return nil, err
	}
	if err := decodeRequests(doc["requests"], path, m); err != nil {
		return nil, err
	}
	if err := decodeEnv(doc["env"], path, m); err != nil {
		return nil, err
	}
	return m, nil
}

func violation(path, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrManifestViolation, path, fmt.Sprintf(format, args...))
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func decodePackage(v any, path string, m *Manifest) error {
	if v == nil {
		return violation(path, "[package] table is required")
	}
	t, ok := v.(map[string]any)
	if !ok {
		return violation(path, "[package] must be a table")
	}
	str := func(k string) (string, bool) {
		s, ok := t[k].(string)
		return s, ok
	}
	for _, k := range sortedKeys(t) {
		switch k {
		case "name", "version", "description", "author", "homepage", "license":
			if _, ok := t[k].(string); !ok {
				return violation(path, "[package].%s must be a string", k)
			}
		default:
			return violation(path, "[package].%s is not an allowed key (name, version, description, author, homepage, license)", k)
		}
	}
	name, _ := str("name")
	if name == "" {
		return violation(path, "[package].name is required")
	}
	if !NamePattern.MatchString(name) {
		return violation(path, "[package].name %q must match %s", name, NamePattern)
	}
	m.Package = PackageMeta{
		Name:        name,
		Version:     mustStr(t, "version"),
		Description: mustStr(t, "description"),
		Author:      mustStr(t, "author"),
		Homepage:    mustStr(t, "homepage"),
	}
	// A present license must be a valid SPDX expression; an absent one is
	// the package.no-license finding, not a load error (SPEC-0026 REQ-3).
	if _, present := t["license"]; present {
		lic := mustStr(t, "license")
		if err := ValidateLicense(lic); err != nil {
			return violation(path, "[package].license: %v", err)
		}
		m.Package.License = lic
	}
	return nil
}

func mustStr(t map[string]any, k string) string {
	s, _ := t[k].(string)
	return s
}

func decodeHarness(v any, path string, m *Manifest) error {
	if v == nil {
		return violation(path, "[harness] table is required")
	}
	t, ok := v.(map[string]any)
	if !ok {
		return violation(path, "[harness] must be a table")
	}
	for _, k := range sortedKeys(t) {
		if reason, denied := harnessForbidden[k]; denied {
			return violation(path, "[harness].%s is not permitted in a package manifest: %s", k, reason)
		}
		if !harnessAllowlist[k] {
			return violation(path, "[harness].%s is not an allowed key in a package manifest", k)
		}
		if err := checkNoSecretRef(t[k], path, "[harness]."+k); err != nil {
			return err
		}
	}

	hv := &m.Harness
	hv.Harness = mustStr(t, "harness")
	if strings.TrimSpace(hv.Harness) == "" {
		return violation(path, "[harness].harness is required (the adapter to run)")
	}
	var err error
	if hv.Args, err = optStrList(t, "args", path); err != nil {
		return err
	}
	if hv.Argv, err = optStrList(t, "argv", path); err != nil {
		return err
	}
	hv.Model = mustStr(t, "model")
	var prompts []string
	for _, p := range []struct {
		key string
		dst *string
	}{
		{"prompt", &hv.Prompt},
		{"prompt_file", &hv.PromptFile},
		{"prompt_template", &hv.PromptTemplate},
		{"prompt_template_file", &hv.PromptTemplateFile},
	} {
		if _, present := t[p.key]; !present {
			continue
		}
		s, ok := t[p.key].(string)
		if !ok {
			return violation(path, "[harness].%s must be a string", p.key)
		}
		*p.dst = s
		prompts = append(prompts, p.key)
	}
	// The four prompt sources exclude one another on any harness table
	// (SPEC-0017 REQ-5); a manifest that names two could never load, so say
	// so here, against the manifest, rather than at every install.
	if len(prompts) > 1 {
		return violation(path, "[harness] carries more than one prompt source (%s); a package supplies at most one", strings.Join(prompts, ", "))
	}
	hv.SystemPromptFile = mustStr(t, "system_prompt_file")
	hv.MCPConfig = mustStr(t, "mcp_config")
	if hv.AllowedTools, err = optStrList(t, "allowed_tools", path); err != nil {
		return err
	}
	if hv.SkillPaths, err = optStrList(t, "skill_paths", path); err != nil {
		return err
	}
	// Every path a manifest names is read at config load or spawn and fed to
	// the agent, so it must name a file the package ships: a path that is
	// absolute or climbs out of the package would hand the agent any file on
	// the installing machine (a prompt_file of ~/.ssh/id_ed25519 is the
	// agent's instruction) without the content scan ever seeing it.
	for _, pv := range []struct{ key, val string }{
		{"prompt_file", hv.PromptFile},
		{"prompt_template_file", hv.PromptTemplateFile},
		{"system_prompt_file", hv.SystemPromptFile},
		{"mcp_config", hv.MCPConfig},
	} {
		if err := checkPackagePath(pv.val, path, "[harness]."+pv.key); err != nil {
			return err
		}
	}
	for _, sp := range hv.SkillPaths {
		if err := checkPackagePath(sp, path, "[harness].skill_paths"); err != nil {
			return err
		}
	}
	if b, ok := t["auto_accept"].(bool); ok {
		hv.AutoAccept = &b
	} else if _, present := t["auto_accept"]; present {
		return violation(path, "[harness].auto_accept must be a boolean")
	}
	if i, ok := t["max_turns"].(int64); ok {
		n := int(i)
		hv.MaxTurns = &n
	} else if _, present := t["max_turns"]; present {
		return violation(path, "[harness].max_turns must be an integer")
	}
	if b, ok := t["quiet"].(bool); ok {
		hv.Quiet = &b
	} else if _, present := t["quiet"]; present {
		return violation(path, "[harness].quiet must be a boolean")
	}
	return nil
}

func optStrList(t map[string]any, k, path string) ([]string, error) {
	if _, present := t[k]; !present {
		return nil, nil
	}
	items, ok := t[k].([]any)
	if !ok {
		return nil, violation(path, "[harness].%s must be a list of strings", k)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil, violation(path, "[harness].%s must be a list of strings", k)
		}
		out = append(out, s)
	}
	return out, nil
}

func decodeRequests(v any, path string, m *Manifest) error {
	if v == nil {
		return nil
	}
	t, ok := v.(map[string]any)
	if !ok {
		return violation(path, "[requests] must be a table")
	}
	for _, k := range sortedKeys(t) {
		if !requestsAllowlist[k] {
			return violation(path, "[requests].%s is not an allowed key (skill_paths, mcp_allow, network)", k)
		}
	}
	if b, ok := t["skill_paths"].(bool); ok {
		m.Requests.SkillPaths = &b
	} else if _, present := t["skill_paths"]; present {
		return violation(path, "[requests].skill_paths must be a boolean")
	}
	if b, ok := t["network"].(bool); ok {
		m.Requests.Network = &b
	} else if _, present := t["network"]; present {
		return violation(path, "[requests].network must be a boolean")
	}
	if items, ok := t["mcp_allow"].([]any); ok {
		out := make([]string, 0, len(items))
		for _, it := range items {
			s, ok := it.(string)
			if !ok || (s != "read" && s != "write") {
				return violation(path, "[requests].mcp_allow entries must be \"read\" and/or \"write\"")
			}
			out = append(out, s)
		}
		m.Requests.MCPAllow = out
	} else if _, present := t["mcp_allow"]; present {
		return violation(path, "[requests].mcp_allow must be a list of \"read\" and/or \"write\"")
	}
	return nil
}

// checkPackagePath rejects a manifest path that is absolute or resolves
// outside the package directory (SPEC-0026 REQ-3). Empty means unset.
func checkPackagePath(v, path, key string) error {
	if v == "" {
		return nil
	}
	if filepath.IsAbs(v) || strings.HasPrefix(v, "~") {
		return violation(path, "%s %q must be a path inside the package, relative to package.toml", key, v)
	}
	if c := filepath.Clean(v); c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
		return violation(path, "%s %q resolves outside the package directory", key, v)
	}
	return nil
}

// checkNoSecretRef rejects any "${" inside a manifest value — string or list
// of strings — naming the key (SPEC-0026 REQ-3; ADR-0038 defines ${NAME} as
// the secret-reference grammar, and a package must not carry one).
func checkNoSecretRef(v any, path, key string) error {
	switch t := v.(type) {
	case string:
		if strings.Contains(t, "${") {
			return violation(path, "%s contains \"${\" — secret references are not permitted in a package manifest", key)
		}
	case []any:
		for _, it := range t {
			if s, ok := it.(string); ok && strings.Contains(s, "${") {
				return violation(path, "%s contains \"${\" — secret references are not permitted in a package manifest", key)
			}
		}
	}
	return nil
}
