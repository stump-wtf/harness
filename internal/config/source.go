// source.go resolves a [harness.*] table's `source` reference against the
// content-addressed pin store at config load (SPEC-0026 REQ-7). The daemon's
// one and only step: read package.toml from local disk, apply the package's
// [harness] values, and let any key present directly on the table override
// (the ADR-0011 precedence rule skill_paths already follows). No network
// request, no git operation — a missing pin fails the load and keeps the
// daemon on its last-good configuration, exactly as an unknown adapter does
// (ADR-0006).
//
// Governing: ADR-0044, SPEC-0026 REQ-7, Error Handling Standards.
package config

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sort"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

// applySource merges the pinned package's [harness] values into rh and returns
// the merged table. Local keys win on presence: a key set directly on the
// harness table overrides the package's value for that key (SPEC-0026 REQ-7).
// Relative path values from the manifest resolve against the package's own
// pin directory, never the installing harness's workdir (REQ-3).
func applySource(filename, name string, line int, rh rawHarness) (rawHarness, error) {
	src, err := agentpkg.ParseSource(rh.Source)
	if err != nil {
		return rh, newError(filename, line, "harness %q: invalid \"source\": %v", name, err)
	}
	man, err := agentpkg.LoadManifest(agentpkg.ManifestPath(src))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rh, newError(filename, line,
				"harness %q: source %q is not installed on this machine — run `harness agent install %s@%s` (no fetch is performed on your behalf)",
				name, rh.Source, src.Stable+"/"+src.Package, src.SHA)
		}
		return rh, newError(filename, line, "harness %q: source %q: %v", name, rh.Source, err)
	}
	if man.Package.Name != src.Package {
		return rh, newError(filename, line,
			"harness %q: source %q names package %q but its manifest declares %q",
			name, rh.Source, src.Package, man.Package.Name)
	}

	hv := man.Harness
	pinDir := agentpkg.PinDir(src)
	// Manifest-relative paths anchor on the pin directory; already-absolute
	// paths pass through untouched.
	manifestPath := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(pinDir, p)
	}

	// Every key filled below is the package's, not the operator's: record
	// it so describe can attribute the effective value (SPEC-0026 REQ-12).
	fromPackage := func(key string) {
		rh.PackageKeys = append(rh.PackageKeys, key)
	}
	if rh.Harness == "" && hv.Harness != "" {
		rh.Harness = hv.Harness
		fromPackage("harness")
	}
	if rh.Args == nil && hv.Args != nil {
		rh.Args = hv.Args
		fromPackage("args")
	}
	if rh.Argv == nil && hv.Argv != nil {
		rh.Argv = hv.Argv
		fromPackage("argv")
	}
	if rh.Model == "" && hv.Model != "" {
		rh.Model = hv.Model
		fromPackage("model")
	}
	if rh.AutoAccept == nil && hv.AutoAccept != nil {
		rh.AutoAccept = hv.AutoAccept
		fromPackage("auto_accept")
	}
	if rh.MaxTurns == nil && hv.MaxTurns != nil {
		rh.MaxTurns = hv.MaxTurns
		fromPackage("max_turns")
	}
	if rh.Quiet == nil && hv.Quiet != nil {
		rh.Quiet = hv.Quiet
		fromPackage("quiet")
	}
	// The four prompt sources are one choice, not four keys: they exclude
	// one another on the table (SPEC-0017 REQ-5), so a table that sets ANY
	// of them overrides the package's prompt whichever form either side
	// uses. Merging key by key would hand a table carrying `prompt` the
	// package's `prompt_file` too, and fail the load on the exclusion.
	if rh.Prompt == "" && rh.PromptFile == "" && rh.PromptTemplate == "" && rh.PromptTemplateFile == "" {
		switch {
		case hv.Prompt != "":
			rh.Prompt = hv.Prompt
			fromPackage("prompt")
		case hv.PromptFile != "":
			rh.PromptFile = manifestPath(hv.PromptFile)
			fromPackage("prompt_file")
		case hv.PromptTemplate != "":
			rh.PromptTemplate = hv.PromptTemplate
			fromPackage("prompt_template")
		case hv.PromptTemplateFile != "":
			rh.PromptTemplateFile = manifestPath(hv.PromptTemplateFile)
			fromPackage("prompt_template_file")
		}
	}
	if rh.SystemPromptFile == "" && hv.SystemPromptFile != "" {
		rh.SystemPromptFile = manifestPath(hv.SystemPromptFile)
		fromPackage("system_prompt_file")
	}
	if rh.MCPConfig == "" && hv.MCPConfig != "" {
		rh.MCPConfig = manifestPath(hv.MCPConfig)
		fromPackage("mcp_config")
	}
	if rh.AllowedTools == nil && hv.AllowedTools != nil {
		rh.AllowedTools = hv.AllowedTools
		fromPackage("allowed_tools")
	}
	// A package's skill_paths is rewritten to point at the bundle: the
	// manifest's paths resolve against the pin directory exactly like the
	// other path keys (ADR-0044; SPEC-0026 REQ-10's amendment note).
	if rh.SkillPaths == nil && hv.SkillPaths != nil {
		for _, p := range hv.SkillPaths {
			rh.SkillPaths = append(rh.SkillPaths, manifestPath(p))
		}
		fromPackage("skill_paths")
	}
	sort.Strings(rh.PackageKeys)
	return rh, nil
}
