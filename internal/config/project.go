package config

// Governing: ADR-0009 (project-scoped config and compose commands), SPEC-0004
// REQ "Project File Discovery" and REQ "Project File Schema". This file
// implements client-side discovery of a repo-root harness.toml and its parsing
// into the existing core domain types — producing the parsed project definition
// and the sentinel-error contract that the daemon-registration (#30) and
// command-surface (#31) stories build on.
//
// A project file reuses the [harness.*] schema but MUST NOT contain [server]
// or [profile.*] tables. Relative workdir values resolve against the project
// root, not the daemon cwd. An optional [project] table sets the project name.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// ---- Sentinel errors (SPEC-0004 REQ "Error Handling Standards") -----------

// Project-level sentinel errors. Callers use errors.Is to branch on these
// without parsing the human message.
var (
	// ErrNoProjectFound is returned when discovery walks to home/root without
	// finding a harness.toml.
	ErrNoProjectFound = errors.New("no harness.toml found")
	// ErrProjectNameCollision is returned when a project name would shadow an
	// existing bare (global) harness name. (Exercised by #30 at registration
	// time; defined here so both packages share the sentinel.)
	ErrProjectNameCollision = errors.New("project name collides with an existing harness")
	// ErrUnknownProject is returned when a control op targets a project the
	// daemon has no record of. (Exercised by #30; defined here for the same
	// shared-sentinel reason.)
	ErrUnknownProject = errors.New("unknown project")
)

// forbiddenTableErr wraps a config.Error for the [server]/[profile.*] rejection
// so callers can errors.As it while still getting the source line.
func forbiddenTableErr(filename, table string, line int) *Error {
	return newError(filename, line, "project file must not contain [%s] (global-only concern)", table)
}

// ---- Project types -------------------------------------------------------

// rawProject mirrors the optional [project] TOML table.
type rawProject struct {
	Name string `toml:"name"`
}

// Project is a fully parsed project-scoped harness.toml: the project name,
// the project root directory, and the harnesses parsed from [harness.*].
// It reuses core.Harness for each entry so downstream code (supervisor,
// daemon registration) needs no new types.
type Project struct {
	// Name is the project name: [project].name if present, else the sanitized
	// basename of Root.
	Name string
	// Root is the absolute path to the directory containing the project
	// harness.toml.
	Root string
	// ConfigPath is the absolute path to the project harness.toml.
	ConfigPath string
	// Harnesses is the parsed config (only Harnesses and HarnessOrder are
	// populated; Profiles and Server are always zero because they are rejected
	// at parse time).
	Config *core.Config
}

// ---- Discovery (SPEC-0004 REQ "Project File Discovery") ------------------

// DiscoverProject walks upward from cwd to the first ancestor harness.toml,
// resolving the project root. It MUST NOT adopt the daemon's global config file
// (config.DefaultPath()) as a project file.
//
// Returns ErrNoProjectFound (wrapping the last directory checked) when no
// project file exists before the user's home or filesystem root.
func DiscoverProject() (*Project, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("project discovery: getwd: %w", err)
	}
	globalPath := DefaultPath()

	dir := cwd
	for {
		candidate := filepath.Join(dir, "harness.toml")

		// Skip the global config — it is never a project file (SPEC-0004).
		if !samePath(candidate, globalPath) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return LoadProject(candidate)
			}
		}

		// Stop at the user's home directory or filesystem root.
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root.
			return nil, fmt.Errorf("%w (searched from %s to %s)", ErrNoProjectFound, cwd, dir)
		}
		home, _ := os.UserHomeDir()
		if dir == home {
			return nil, fmt.Errorf("%w (searched from %s to %s)", ErrNoProjectFound, cwd, dir)
		}
		dir = parent
	}
}

// LoadProject loads and parses a project harness.toml from path. The
// containing directory is the project root. Relative workdir values resolve
// against the project root.
func LoadProject(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("project load %s: %w", path, err)
	}
	return ParseProject(data, path)
}

// ParseProject parses raw TOML bytes into a validated Project. filename is used
// for error messages and is also resolved to an absolute path for Root.
func ParseProject(data []byte, filename string) (*Project, error) {
	absPath, err := filepath.Abs(filename)
	if err != nil {
		absPath = filename
	}
	root := filepath.Dir(absPath)

	// Reuse the existing TOML decode machinery to get header order + line
	// numbers for error attribution.
	var top map[string]toml.Primitive
	md, err := toml.Decode(string(data), &top)
	if err != nil {
		return nil, syntaxError(filename, err)
	}

	headers := scanTables(data)
	defined := definedPaths(md)
	realHeaders := headers[:0:0]
	for _, h := range headers {
		if defined[strings.Join(h.parts, ".")] {
			realHeaders = append(realHeaders, h)
		}
	}
	headers = realHeaders

	// Parse the optional [project] table.
	var projName string
	if p, ok := top["project"]; ok {
		var rp rawProject
		if err := md.PrimitiveDecode(p, &rp); err != nil {
			line := lineOf(headers, "project")
			return nil, newError(filename, line, "[project]: %v", err)
		}
		projName = strings.TrimSpace(rp.Name)
	}

	// Reject [server] and [profile.*] — they are global-only concerns.
	for _, h := range headers {
		full := strings.Join(h.parts, ".")
		if len(h.parts) == 1 && h.parts[0] == "server" {
			return nil, forbiddenTableErr(filename, "server", h.line)
		}
		if len(h.parts) >= 1 && h.parts[0] == "profile" {
			return nil, forbiddenTableErr(filename, full, h.line)
		}
		// Also reject bare [profile] namespace parent.
		if len(h.parts) == 1 && h.parts[0] == "profile" {
			return nil, forbiddenTableErr(filename, "profile", h.line)
		}
	}

	// Reuse the harness-namespace decode for [harness.*] and bare [name] tables.
	// We use a modified Parse that only accepts harness tables and skips
	// [server]/[profile.*]. Rather than modify Parse() (which the global
	// config path depends on), we do a focused decode here.
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{},
		Profiles:  map[string]core.Profile{},
	}

	var harnessNS map[string]toml.Primitive
	if p, ok := top["harness"]; ok {
		if err := md.PrimitiveDecode(p, &harnessNS); err != nil {
			return nil, newError(filename, lineOf(headers, "harness"), "[harness]: %v", err)
		}
	}

	for _, h := range headers {
		switch {
		case len(h.parts) == 1 && h.parts[0] == "harness":
			continue
		case len(h.parts) == 1 && h.parts[0] == "project":
			continue
		case len(h.parts) == 2 && h.parts[0] == "harness":
			name := h.parts[1]
			var rh rawHarness
			if err := md.PrimitiveDecode(harnessNS[name], &rh); err != nil {
				return nil, newError(filename, h.line, "[harness.%s]: %v", name, err)
			}
			if err := addProjectHarness(cfg, filename, name, h.line, rh, root); err != nil {
				return nil, err
			}

		case len(h.parts) == 1:
			// Bare [name] — backward-compatible harness (ADR-0006).
			name := h.parts[0]
			// Skip namespace parents and rejected tables (already validated above).
			if name == "server" || name == "profile" || name == "project" {
				continue
			}
			var rh rawHarness
			if err := md.PrimitiveDecode(top[name], &rh); err != nil {
				return nil, newError(filename, h.line, "[%s]: %v", name, err)
			}
			if err := addProjectHarness(cfg, filename, name, h.line, rh, root); err != nil {
				return nil, err
			}

		default:
			return nil, newError(filename, h.line, "unrecognized table [%s]", strings.Join(h.parts, "."))
		}
	}

	// Derive the project name: [project].name else sanitized basename of root.
	if projName == "" {
		projName = sanitizeProjectName(filepath.Base(root))
	}

	return &Project{
		Name:       projName,
		Root:       root,
		ConfigPath: absPath,
		Config:     cfg,
	}, nil
}

// addProjectHarness validates a raw harness table, resolves relative workdir
// against the project root, and registers it on cfg.
func addProjectHarness(cfg *core.Config, filename, name string, line int, rh rawHarness, projectRoot string) error {
	if _, exists := cfg.Harnesses[name]; exists {
		return newError(filename, line, "duplicate harness %q", name)
	}
	if strings.TrimSpace(rh.Cmd) == "" {
		return newError(filename, line, "harness %q: missing required key \"cmd\"", name)
	}

	backend := core.Backend(rh.Backend)
	if rh.Backend == "" {
		backend = core.BackendNative
	} else if !backend.Valid() {
		return newError(filename, line,
			"harness %q: invalid backend %q (want \"native\" or \"tmux\")", name, rh.Backend)
	}

	if rh.RestartDelay < 0 {
		return newError(filename, line,
			"harness %q: restart_delay must not be negative (got %d)", name, rh.RestartDelay)
	}

	enabled := false
	if rh.Enabled != nil {
		enabled = *rh.Enabled
	}

	// Resolve relative workdir against the project root (SPEC-0004).
	workdir := rh.Workdir
	if workdir != "" && !filepath.IsAbs(workdir) && !strings.HasPrefix(workdir, "~") {
		workdir = filepath.Join(projectRoot, workdir)
	}

	h := core.Harness{
		Name:         name,
		Cmd:          rh.Cmd,
		Args:         rh.Args,
		Workdir:      workdir,
		EnvFile:      resolvePath(rh.EnvFile, projectRoot),
		RestartDelay: time.Duration(rh.RestartDelay) * time.Second,
		Backend:      backend,
		Description:  rh.Description,
		Enabled:      enabled,
		TmuxSocket:   rh.TmuxSocket,
	}
	cfg.Harnesses[name] = h
	cfg.HarnessOrder = append(cfg.HarnessOrder, name)
	return nil
}

// resolvePath resolves a path that may be relative (against base), may start
// with ~, or may be absolute. Empty input returns empty.
func resolvePath(p, base string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "~") {
		return p
	}
	return filepath.Join(base, p)
}

// sanitizeProjectName lowercases, replaces non-alphanumeric runs with hyphens,
// trims leading/trailing hyphens. E.g. "My-Cool Project!" → "my-cool-project".
func sanitizeProjectName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		return "unnamed"
	}
	return result
}

// samePath returns true if two paths refer to the same file (best-effort:
// resolves symlinks and relative components). Used to skip the global config
// during discovery.
func samePath(a, b string) bool {
	absA, err := filepath.Abs(a)
	if err != nil {
		absA = a
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		absB = b
	}
	return absA == absB
}
